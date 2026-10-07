package db

import (
	"database/sql"
	"fmt"
	"os"
)

// OpenReadOnly opens an existing database read-only and applies no
// migrations, so a preview (staypoint import paperclip --dry-run) built from
// a newer branch never changes the schema the running daemon expects. The
// caller must cope with columns that this build adds but the file lacks.
func OpenReadOnly(dbPath string) (*sql.DB, error) {
	if err := refuseLiveDBUnderTest(dbPath); err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open read-only: %w", err)
	}
	return conn, nil
}

// HasColumn reports whether table has column (false on any error).
func HasColumn(conn *sql.DB, table, column string) bool {
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
