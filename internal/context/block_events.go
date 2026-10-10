package context

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Activity-log event types for a manual block toggle.
const (
	EventTaskBlocked   = "task_blocked"
	EventTaskUnblocked = "task_unblocked"
)

// BlockEvent records who blocked or unblocked a task by hand, and from where.
// At is the activity_log created_at of the entry.
type BlockEvent struct {
	Blocked bool   `json:"blocked"`
	By      string `json:"by"`
	Via     string `json:"via"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at"`
}

type blockEventDetails struct {
	By     string `json:"by"`
	Via    string `json:"via"`
	Reason string `json:"reason,omitempty"`
}

// LogBlockEvent appends a task_blocked / task_unblocked entry naming the actor.
func LogBlockEvent(db *sql.DB, taskID string, blocked bool, by, via, reason string) error {
	details, err := json.Marshal(blockEventDetails{By: by, Via: via, Reason: reason})
	if err != nil {
		return err
	}
	eventType := EventTaskUnblocked
	if blocked {
		eventType = EventTaskBlocked
	}
	return LogActivity(db, taskID, eventType, string(details))
}

// LatestBlockEvent returns the most recent manual block toggle for a task, or
// nil when none was ever logged.
func LatestBlockEvent(db *sql.DB, taskID string) (*BlockEvent, error) {
	var eventType, details, at string
	err := db.QueryRow(`SELECT event_type, details, created_at FROM activity_log
		WHERE task_id = ? AND event_type IN (?, ?) ORDER BY id DESC LIMIT 1`,
		taskID, EventTaskBlocked, EventTaskUnblocked).Scan(&eventType, &details, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d blockEventDetails
	_ = json.Unmarshal([]byte(details), &d)
	return &BlockEvent{
		Blocked: eventType == EventTaskBlocked,
		By:      d.By,
		Via:     d.Via,
		Reason:  d.Reason,
		At:      at,
	}, nil
}
