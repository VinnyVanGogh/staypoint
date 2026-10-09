package alerts

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite :memory:: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })

	_, err = database.Exec(`
	CREATE TABLE IF NOT EXISTS board_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		severity TEXT NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
		title TEXT NOT NULL,
		message TEXT NOT NULL,
		dedupe_key TEXT,
		occurrences INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		acknowledged_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_board_alerts_unack ON board_alerts (acknowledged_at, last_seen_at DESC);
	CREATE INDEX IF NOT EXISTS idx_board_alerts_seen ON board_alerts (last_seen_at);
	`)
	if err != nil {
		t.Fatalf("create board_alerts table: %v", err)
	}
	return database
}

func TestRecordAndListUnacknowledged(t *testing.T) {
	db := setupTestDB(t)

	a1 := Alert{
		Kind:      "quota_warning",
		Severity:  SeverityWarning,
		Title:     "Warning 1",
		Message:   "Approaching quota limit",
		DedupeKey: "key-1",
	}
	rec1, err := Record(db, a1)
	if err != nil {
		t.Fatalf("Record a1: %v", err)
	}
	if rec1.ID == 0 {
		t.Errorf("expected non-zero ID for rec1")
	}

	time.Sleep(10 * time.Millisecond)

	a2 := Alert{
		Kind:      "circuit_breaker_tripped",
		Severity:  SeverityCritical,
		Title:     "Critical 2",
		Message:   "Breaker tripped",
		DedupeKey: "key-2",
	}
	rec2, err := Record(db, a2)
	if err != nil {
		t.Fatalf("Record a2: %v", err)
	}

	list, err := ListUnacknowledged(db, 50)
	if err != nil {
		t.Fatalf("ListUnacknowledged: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 unacknowledged alerts, got %d", len(list))
	}
	// Newest last_seen_at first
	if list[0].ID != rec2.ID {
		t.Errorf("expected newest alert (rec2) first, got ID=%d", list[0].ID)
	}
	if list[1].ID != rec1.ID {
		t.Errorf("expected older alert (rec1) second, got ID=%d", list[1].ID)
	}
}

func TestRecordDeduplication(t *testing.T) {
	db := setupTestDB(t)

	a1 := Alert{
		Kind:      "circuit_breaker_tripped",
		Severity:  SeverityCritical,
		Title:     "Breaker 1",
		Message:   "First message",
		DedupeKey: "breaker:session-1",
	}
	first, err := Record(db, a1)
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if first.Occurrences != 1 {
		t.Errorf("expected occurrences=1, got %d", first.Occurrences)
	}

	time.Sleep(10 * time.Millisecond)

	a2 := Alert{
		Kind:      "circuit_breaker_tripped",
		Severity:  SeverityCritical,
		Title:     "Breaker 1 Updated",
		Message:   "Updated message",
		DedupeKey: "breaker:session-1",
	}
	second, err := Record(db, a2)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}

	// Same unacknowledged DedupeKey updates the existing row instead of inserting:
	// same ID, Occurrences == 2, newer LastSeenAt, updated message.
	if second.ID != first.ID {
		t.Errorf("expected same ID=%d, got %d", first.ID, second.ID)
	}
	if second.Occurrences != 2 {
		t.Errorf("expected occurrences=2, got %d", second.Occurrences)
	}
	if !second.LastSeenAt.After(first.LastSeenAt) {
		t.Errorf("expected newer LastSeenAt: first=%v, second=%v", first.LastSeenAt, second.LastSeenAt)
	}
	if second.Message != "Updated message" {
		t.Errorf("expected updated message, got %q", second.Message)
	}

	list, err := ListUnacknowledged(db, 10)
	if err != nil {
		t.Fatalf("ListUnacknowledged: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 unacknowledged alert, got %d", len(list))
	}
}

func TestAcknowledgeAndReinsert(t *testing.T) {
	db := setupTestDB(t)

	a1 := Alert{
		Kind:      "quota_locked",
		Severity:  SeverityWarning,
		Title:     "Quota Locked",
		Message:   "Pool locked",
		DedupeKey: "quota:pool-1:locked",
	}
	rec1, err := Record(db, a1)
	if err != nil {
		t.Fatalf("Record a1: %v", err)
	}

	// After Acknowledge, the same key inserts a new row with a new ID
	if err := Acknowledge(db, rec1.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	unack, err := ListUnacknowledged(db, 10)
	if err != nil {
		t.Fatalf("ListUnacknowledged: %v", err)
	}
	if len(unack) != 0 {
		t.Fatalf("expected 0 unacknowledged alerts after ack, got %d", len(unack))
	}

	a2 := Alert{
		Kind:      "quota_locked",
		Severity:  SeverityWarning,
		Title:     "Quota Locked Again",
		Message:   "Pool locked again",
		DedupeKey: "quota:pool-1:locked",
	}
	rec2, err := Record(db, a2)
	if err != nil {
		t.Fatalf("Record a2: %v", err)
	}
	if rec2.ID == rec1.ID {
		t.Errorf("expected new ID after acknowledge, got same ID=%d", rec2.ID)
	}
	if rec2.Occurrences != 1 {
		t.Errorf("expected occurrences=1 for new row, got %d", rec2.Occurrences)
	}
}

func TestAcknowledgeIdempotentAndNotFound(t *testing.T) {
	db := setupTestDB(t)

	rec, err := Record(db, Alert{
		Kind:     "quota_ready",
		Severity: SeverityInfo,
		Title:    "Ready",
		Message:  "Pool ready",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Acknowledge is idempotent
	if err := Acknowledge(db, rec.ID); err != nil {
		t.Fatalf("first Acknowledge: %v", err)
	}
	if err := Acknowledge(db, rec.ID); err != nil {
		t.Fatalf("second Acknowledge (idempotent): %v", err)
	}

	// An unknown id returns ErrNotFound (use errors.Is)
	err = Acknowledge(db, 999999)
	if err == nil {
		t.Fatalf("expected error for unknown id, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected errors.Is(err, ErrNotFound), got %v", err)
	}
}

func TestTitleAndMessageTruncation(t *testing.T) {
	db := setupTestDB(t)

	// A 200-rune title is cut to 120 runes ending in …
	longTitle := strings.Repeat("日", 200)
	// A 1000-rune message cut to 500 ending in …
	longMsg := strings.Repeat("本", 1000)

	rec, err := Record(db, Alert{
		Kind:     "info_test",
		Severity: SeverityInfo,
		Title:    longTitle,
		Message:  longMsg,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	titleRuneCount := utf8.RuneCountInString(rec.Title)
	if titleRuneCount != 120 {
		t.Errorf("expected title length of 120 runes, got %d", titleRuneCount)
	}
	if !strings.HasSuffix(rec.Title, "…") {
		t.Errorf("expected title to end in …, got %q", rec.Title)
	}

	msgRuneCount := utf8.RuneCountInString(rec.Message)
	if msgRuneCount != 500 {
		t.Errorf("expected message length of 500 runes, got %d", msgRuneCount)
	}
	if !strings.HasSuffix(rec.Message, "…") {
		t.Errorf("expected message to end in …, got %q", rec.Message)
	}
}

func TestSeverityValidation(t *testing.T) {
	db := setupTestDB(t)

	// Empty severity defaults to info
	rec, err := Record(db, Alert{
		Kind:     "default_test",
		Severity: "",
		Title:    "No Severity",
		Message:  "Should default to info",
	})
	if err != nil {
		t.Fatalf("Record with empty severity: %v", err)
	}
	if rec.Severity != SeverityInfo {
		t.Errorf("expected SeverityInfo (%q), got %q", SeverityInfo, rec.Severity)
	}

	// An unknown severity returns an error
	_, err = Record(db, Alert{
		Kind:     "bad_severity_test",
		Severity: Severity("catastrophic"),
		Title:    "Bad Severity",
		Message:  "Invalid",
	})
	if err == nil {
		t.Errorf("expected error for unknown severity, got nil")
	}
}

func TestListSeenSince(t *testing.T) {
	db := setupTestDB(t)

	rec1, err := Record(db, Alert{
		Kind:     "quota_warning",
		Severity: SeverityWarning,
		Title:    "Alert 1",
		Message:  "First alert",
	})
	if err != nil {
		t.Fatalf("Record rec1: %v", err)
	}

	t1 := rec1.LastSeenAt
	time.Sleep(10 * time.Millisecond)

	rec2, err := Record(db, Alert{
		Kind:     "quota_warning",
		Severity: SeverityWarning,
		Title:    "Alert 2",
		Message:  "Second alert",
	})
	if err != nil {
		t.Fatalf("Record rec2: %v", err)
	}

	// ListSeenSince(t) returns only rows with last_seen_at > t
	sinceT1, err := ListSeenSince(db, t1)
	if err != nil {
		t.Fatalf("ListSeenSince(t1): %v", err)
	}
	if len(sinceT1) != 1 {
		t.Fatalf("expected 1 row since t1, got %d", len(sinceT1))
	}
	if sinceT1[0].ID != rec2.ID {
		t.Errorf("expected rec2 (ID=%d), got ID=%d", rec2.ID, sinceT1[0].ID)
	}

	// ListSeenSince before rec1 returns both in oldest-first order
	beforeAll := t1.Add(-1 * time.Minute)
	all, err := ListSeenSince(db, beforeAll)
	if err != nil {
		t.Fatalf("ListSeenSince(beforeAll): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(all))
	}
	if all[0].ID != rec1.ID || all[1].ID != rec2.ID {
		t.Errorf("expected oldest first (rec1 then rec2), got [%d, %d]", all[0].ID, all[1].ID)
	}
}
