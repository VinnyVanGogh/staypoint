package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDB_ConcurrentOpen_PendingMigrations(t *testing.T) {
	origMigrations := Migrations
	defer func() { Migrations = origMigrations }()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent_open.db")

	// 1. Prepare a database at current schema version with existing data
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open failed: %v", err)
	}

	_, err = store.DB().Exec(`
		INSERT INTO accounts (account_key, email_domain, role, label, plan_tier)
		VALUES ('pre-existing-key', 'example.com', 'work', 'Existing Label', 'pro');
	`)
	if err != nil {
		t.Fatalf("failed to insert pre-existing row: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	// 2. Add pending migrations with Up instrumentation
	var upCount1, upCount2 atomic.Int32
	pendingVersion1 := 998
	pendingVersion2 := 999

	Migrations = append(Migrations,
		Migration{
			Version: pendingVersion1,
			Name:    "add_test_concurrent_column_1",
			Up: func(conn *sql.DB) error {
				upCount1.Add(1)
				// Small sleep ensures concurrent openers overlap during migration execution
				time.Sleep(20 * time.Millisecond)
				_, err := conn.Exec("ALTER TABLE accounts ADD COLUMN concurrent_test_col_1 TEXT DEFAULT 'migrated_val_1';")
				return err
			},
		},
		Migration{
			Version: pendingVersion2,
			Name:    "add_test_concurrent_column_2",
			Up: func(conn *sql.DB) error {
				upCount2.Add(1)
				time.Sleep(20 * time.Millisecond)
				_, err := conn.Exec("ALTER TABLE accounts ADD COLUMN concurrent_test_col_2 TEXT DEFAULT 'migrated_val_2';")
				return err
			},
		},
	)

	// 3. Launch 4 concurrent goroutines opening the same database
	const numGoroutines = 4
	var startWg sync.WaitGroup
	var doneWg sync.WaitGroup
	startWg.Add(1)

	stores := make([]*Store, numGoroutines)
	errs := make([]error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		doneWg.Add(1)
		go func(idx int) {
			defer doneWg.Done()
			startWg.Wait() // Synchronize all goroutines to call Open concurrently
			s, openErr := Open(dbPath)
			stores[idx] = s
			errs[idx] = openErr
			if s != nil {
				// Immediate write to verify writes are not clobbered by concurrent openers or restore
				_, _ = s.DB().Exec(
					"INSERT INTO wire_messages (channel, author, content, expires_at) VALUES (?, ?, ?, datetime('now', '+1 hour'));",
					"concurrent-open", fmt.Sprintf("worker-%d", idx), "msg",
				)
			}
		}(i)
	}

	// Release all goroutines at the same time
	startWg.Done()
	doneWg.Wait()

	defer func() {
		for _, s := range stores {
			if s != nil {
				_ = s.Close()
			}
		}
	}()

	// Assertion 1: All Open calls succeed without error or duplicate column failure
	for i, openErr := range errs {
		if openErr != nil {
			t.Errorf("goroutine %d Open failed: %v", i, openErr)
			if strings.Contains(openErr.Error(), "duplicate column name") {
				t.Errorf("goroutine %d failed with duplicate column error", i)
			}
			if strings.Contains(openErr.Error(), "database restored from backup") {
				t.Errorf("goroutine %d triggered restore path: %v", i, openErr)
			}
		}
	}

	// Assertion 2: Each pending migration Up function runs exactly once
	if count1 := upCount1.Load(); count1 != 1 {
		t.Errorf("pending migration %d Up ran %d times, want exactly 1", pendingVersion1, count1)
	}
	if count2 := upCount2.Load(); count2 != 1 {
		t.Errorf("pending migration %d Up ran %d times, want exactly 1", pendingVersion2, count2)
	}

	// Assertion 3: Restore path is not triggered and no writes/files are clobbered
	backupPath := dbPath + ".bak"
	if fi, err := os.Stat(backupPath); err == nil && !fi.IsDir() {
		t.Errorf("backup file %s was left behind or still exists", backupPath)
	}

	for i, s := range stores {
		if s == nil {
			continue
		}
		var label, col1, col2 string
		err := s.DB().QueryRow("SELECT label, concurrent_test_col_1, concurrent_test_col_2 FROM accounts WHERE account_key = 'pre-existing-key';").Scan(&label, &col1, &col2)
		if err != nil {
			t.Errorf("store %d failed to query migrated table: %v", i, err)
		} else {
			if label != "Existing Label" {
				t.Errorf("store %d: pre-existing data clobbered, got %q, want %q", i, label, "Existing Label")
			}
			if col1 != "migrated_val_1" || col2 != "migrated_val_2" {
				t.Errorf("store %d: migrated columns incorrect, got col1=%q col2=%q", i, col1, col2)
			}
		}
	}

	// Verify post-condition with a fresh Open: schema versions, migrations, and all worker writes
	finalStore, err := Open(dbPath)
	if err != nil {
		t.Fatalf("final Open failed: %v", err)
	}
	defer finalStore.Close()

	for _, v := range []int{pendingVersion1, pendingVersion2} {
		var verCount int
		if err := finalStore.DB().QueryRow("SELECT count(*) FROM schema_versions WHERE version = ?;", v).Scan(&verCount); err != nil {
			t.Errorf("failed to query schema_versions for version %d: %v", v, err)
		} else if verCount != 1 {
			t.Errorf("schema_versions for version %d count = %d, want 1", v, verCount)
		}
	}

	var messageCount int
	if err := finalStore.DB().QueryRow("SELECT count(*) FROM wire_messages WHERE channel = 'concurrent-open';").Scan(&messageCount); err != nil {
		t.Errorf("failed to query wire_messages: %v", err)
	} else if messageCount != numGoroutines {
		t.Errorf("worker writes clobbered: expected %d wire_messages, found %d", numGoroutines, messageCount)
	}
}
