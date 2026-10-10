// Package alerts stores Board alerts (circuit breaker trips, quota warnings)
// in the board_alerts table so they reach the web UI.
//
// Alerts are raised by more than one process: the daemon and the
// `staypoint hook` CLI both write the same SQLite file, and the daemon's
// server polls ListSeenSince to push new rows over SSE. macOS notifications
// stay a best-effort side channel (STA-705).
package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// Severity orders how loudly the Board UI shows an alert.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Alert is one row of board_alerts.
type Alert struct {
	ID             int64      `json:"id"`
	Kind           string     `json:"kind"`
	Severity       Severity   `json:"severity"`
	Title          string     `json:"title"`
	Message        string     `json:"message"`
	DedupeKey      string     `json:"dedupe_key,omitempty"`
	Occurrences    int        `json:"occurrences"`
	CreatedAt      time.Time  `json:"created_at"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
}

// ErrNotFound is returned by Acknowledge for an unknown id.
var ErrNotFound = errors.New("alert not found")

const (
	maxTitleRunes   = 120
	maxMessageRunes = 500
)

// timeLayout is RFC3339 with a fixed nine-digit fraction. RFC3339Nano drops
// trailing zeros, which breaks TEXT comparison ("…:05.1Z" > "…:05.12Z"), and
// ListSeenSince compares last_seen_at as TEXT.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

const columns = `id, kind, severity, title, message, COALESCE(dedupe_key, ''), occurrences, created_at, last_seen_at, acknowledged_at`

// Record stores a. When a.DedupeKey matches an unacknowledged alert, that row
// is updated instead (occurrences+1, new last_seen_at, title and message) and
// keeps its ID. Title and message are cut to 120 and 500 runes.
func Record(db *sql.DB, a Alert) (Alert, error) {
	if a.Severity == "" {
		a.Severity = SeverityInfo
	}
	switch a.Severity {
	case SeverityCritical, SeverityWarning, SeverityInfo:
	default:
		return Alert{}, fmt.Errorf("alerts: unknown severity %q", a.Severity)
	}
	a.Title = truncate(a.Title, maxTitleRunes)
	a.Message = truncate(a.Message, maxMessageRunes)

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return Alert{}, fmt.Errorf("alerts: conn: %w", err)
	}
	defer conn.Close()

	// BEGIN IMMEDIATE takes the write lock before the dedupe lookup, so the
	// daemon and the hook CLI can't both miss the row and insert twice.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return Alert{}, fmt.Errorf("alerts: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	now := formatTime(time.Now())
	var id int64
	if a.DedupeKey != "" {
		err := conn.QueryRowContext(ctx,
			`SELECT id FROM board_alerts WHERE dedupe_key = ? AND acknowledged_at IS NULL ORDER BY id DESC LIMIT 1`,
			a.DedupeKey).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Alert{}, fmt.Errorf("alerts: dedupe lookup: %w", err)
		}
	}
	if id != 0 {
		if _, err := conn.ExecContext(ctx,
			`UPDATE board_alerts SET occurrences = occurrences + 1, last_seen_at = ?, title = ?, message = ? WHERE id = ?`,
			now, a.Title, a.Message, id); err != nil {
			return Alert{}, fmt.Errorf("alerts: update: %w", err)
		}
	} else {
		var dedupe any
		if a.DedupeKey != "" {
			dedupe = a.DedupeKey
		}
		res, err := conn.ExecContext(ctx,
			`INSERT INTO board_alerts (kind, severity, title, message, dedupe_key, occurrences, created_at, last_seen_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			a.Kind, string(a.Severity), a.Title, a.Message, dedupe, now, now)
		if err != nil {
			return Alert{}, fmt.Errorf("alerts: insert: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return Alert{}, fmt.Errorf("alerts: insert id: %w", err)
		}
	}

	out, err := scanAlert(conn.QueryRowContext(ctx, `SELECT `+columns+` FROM board_alerts WHERE id = ?`, id))
	if err != nil {
		return Alert{}, fmt.Errorf("alerts: reload: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return Alert{}, fmt.Errorf("alerts: commit: %w", err)
	}
	committed = true
	return out, nil
}

// ListUnacknowledged returns unacknowledged alerts, newest last_seen_at first.
// limit <= 0 means no limit.
func ListUnacknowledged(db *sql.DB, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = -1
	}
	return query(db,
		`SELECT `+columns+` FROM board_alerts WHERE acknowledged_at IS NULL ORDER BY last_seen_at DESC, id DESC LIMIT ?`,
		limit)
}

// ListSeenSince returns alerts (acknowledged or not) with last_seen_at after
// since, oldest first. The daemon's poller uses it to publish new alerts.
func ListSeenSince(db *sql.DB, since time.Time) ([]Alert, error) {
	return query(db,
		`SELECT `+columns+` FROM board_alerts WHERE last_seen_at > ? ORDER BY last_seen_at ASC, id ASC`,
		formatTime(since))
}

// Acknowledge marks an alert as dismissed. Acknowledging twice is not an
// error and keeps the first acknowledged_at; an unknown id is ErrNotFound.
func Acknowledge(db *sql.DB, id int64) error {
	res, err := db.Exec(
		`UPDATE board_alerts SET acknowledged_at = COALESCE(acknowledged_at, ?) WHERE id = ?`,
		formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("alerts: acknowledge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alerts: acknowledge: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AcknowledgeKey dismisses the open alert with dedupeKey, if any, once the
// thing it asked for is dealt with.
func AcknowledgeKey(db *sql.DB, dedupeKey string) error {
	_, err := db.Exec(
		`UPDATE board_alerts SET acknowledged_at = ? WHERE dedupe_key = ? AND acknowledged_at IS NULL`,
		formatTime(time.Now()), dedupeKey)
	if err != nil {
		return fmt.Errorf("alerts: acknowledge key: %w", err)
	}
	return nil
}

func query(db *sql.DB, q string, args ...any) ([]Alert, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("alerts: query: %w", err)
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("alerts: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAlert(s scanner) (Alert, error) {
	var a Alert
	var severity, created, lastSeen string
	var acked sql.NullString
	if err := s.Scan(&a.ID, &a.Kind, &severity, &a.Title, &a.Message, &a.DedupeKey,
		&a.Occurrences, &created, &lastSeen, &acked); err != nil {
		return Alert{}, err
	}
	a.Severity = Severity(severity)
	var err error
	if a.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Alert{}, fmt.Errorf("created_at: %w", err)
	}
	if a.LastSeenAt, err = time.Parse(time.RFC3339Nano, lastSeen); err != nil {
		return Alert{}, fmt.Errorf("last_seen_at: %w", err)
	}
	if acked.Valid {
		t, err := time.Parse(time.RFC3339Nano, acked.String)
		if err != nil {
			return Alert{}, fmt.Errorf("acknowledged_at: %w", err)
		}
		a.AcknowledgedAt = &t
	}
	return a, nil
}

// truncate cuts s to max runes, ending in "…" when cut.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
