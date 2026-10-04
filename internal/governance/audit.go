package governance

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// AuditEvent types for the governance_audit_log.
const (
	AuditReviewerAssigned  = "reviewer_assigned"
	AuditReviewerRemoved   = "reviewer_removed"
	AuditApproverAssigned  = "approver_assigned"
	AuditApproverRemoved   = "approver_removed"
	AuditWatchdogAssigned  = "watchdog_assigned"
	AuditReviewSubmitted   = "review_submitted"
	AuditApprovalVote      = "approval_vote"
	AuditWatchdogEval      = "watchdog_eval"
	AuditStateTransition   = "state_transition"
	AuditGovernanceUpdated = "governance_updated"

	AuditBoardAction  = "board_action"
	AuditPasskeyEvent = "passkey_event"
)

// AuditEntry is a single row from governance_audit_log.
type AuditEntry struct {
	ID        int64   `json:"id"`
	TaskID    string  `json:"task_id"`
	ActorID   string  `json:"actor_id"`
	EventType string  `json:"event_type"`
	FromState *string `json:"from_state,omitempty"`
	ToState   *string `json:"to_state,omitempty"`
	Payload   any     `json:"payload,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// LogEvent writes a governance event to the durable audit log.
func LogEvent(db *sql.DB, taskID, actorID, eventType string, fromState, toState *string, payload any) error {
	var payloadJSON *string
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("governance: marshal audit payload: %w", err)
		}
		s := string(b)
		payloadJSON = &s
	}
	_, err := db.Exec(
		`INSERT INTO governance_audit_log (task_id, actor_id, event_type, from_state, to_state, payload)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, actorID, eventType, fromState, toState, payloadJSON,
	)
	return err
}

// LogGateEvent writes a security gate decision to security_gate_audit_log.
// Unlike LogEvent, gate_id is not a foreign key to tasks — gate requests have
// their own UUID namespace in security_gate_requests.
func LogGateEvent(db *sql.DB, gateID, actorID, eventType string, fromStatus, toStatus *string, payload any) error {
	var payloadJSON *string
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("governance: marshal gate audit payload: %w", err)
		}
		s := string(b)
		payloadJSON = &s
	}
	_, err := db.Exec(
		`INSERT INTO security_gate_audit_log (gate_id, actor_id, event_type, from_status, to_status, payload)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		gateID, actorID, eventType, fromStatus, toStatus, payloadJSON,
	)
	return err
}

// ListGateAuditLog returns all audit events for a given gate request ID.
func ListGateAuditLog(db *sql.DB, gateID string) ([]map[string]any, error) {
	rows, err := db.Query(
		`SELECT id, gate_id, actor_id, event_type, from_status, to_status, payload, created_at
		 FROM security_gate_audit_log WHERE gate_id = ? ORDER BY id ASC`,
		gateID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var (
			id, gateID2, actorID2, eventType2, createdAt string
			fromStatus, toStatus, payload                sql.NullString
		)
		if err := rows.Scan(&id, &gateID2, &actorID2, &eventType2, &fromStatus, &toStatus, &payload, &createdAt); err != nil {
			return nil, err
		}
		entry := map[string]any{
			"id": id, "gate_id": gateID2, "actor_id": actorID2,
			"event_type": eventType2, "created_at": createdAt,
		}
		if fromStatus.Valid {
			entry["from_status"] = fromStatus.String
		}
		if toStatus.Valid {
			entry["to_status"] = toStatus.String
		}
		if payload.Valid {
			var v any
			if json.Unmarshal([]byte(payload.String), &v) == nil {
				entry["payload"] = v
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// GetAuditLog returns the combined audit trail for a task (governance events +
// activity-log entries), sorted newest first.
func GetAuditLog(db *sql.DB, taskID string) ([]AuditEntry, error) {
	govRows, err := db.Query(
		`SELECT id, task_id, actor_id, event_type, from_state, to_state, payload, created_at
		 FROM governance_audit_log
		 WHERE task_id = ?
		 ORDER BY created_at DESC, id DESC`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer govRows.Close()

	var entries []AuditEntry
	for govRows.Next() {
		var e AuditEntry
		var fromState, toState, payloadStr sql.NullString
		if err := govRows.Scan(&e.ID, &e.TaskID, &e.ActorID, &e.EventType, &fromState, &toState, &payloadStr, &e.CreatedAt); err != nil {
			return nil, err
		}
		if fromState.Valid {
			e.FromState = &fromState.String
		}
		if toState.Valid {
			e.ToState = &toState.String
		}
		if payloadStr.Valid {
			var v any
			if err := json.Unmarshal([]byte(payloadStr.String), &v); err == nil {
				e.Payload = v
			}
		}
		entries = append(entries, e)
	}
	if err := govRows.Err(); err != nil {
		return nil, err
	}

	// Merge activity_log (comments, interactions, run events) into the audit trail.
	actRows, err := db.Query(
		`SELECT id, task_id, event_type, details, created_at
		 FROM activity_log
		 WHERE task_id = ?`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer actRows.Close()

	for actRows.Next() {
		var e AuditEntry
		var details string
		if err := actRows.Scan(&e.ID, &e.TaskID, &e.EventType, &details, &e.CreatedAt); err != nil {
			return nil, err
		}
		if details != "" {
			e.Payload = map[string]string{"details": details}
		}
		entries = append(entries, e)
	}
	if err := actRows.Err(); err != nil {
		return nil, err
	}

	// Sort combined slice newest first.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt > entries[j].CreatedAt
		}
		return entries[i].ID > entries[j].ID
	})

	return entries, nil
}
