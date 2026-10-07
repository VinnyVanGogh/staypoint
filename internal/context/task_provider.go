package context

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
)

// ErrInvalidProvider is returned for a provider other than "", "claude" or
// "gemini".
var ErrInvalidProvider = errors.New("invalid provider")

// IsValidTaskProvider reports whether p is a stored provider value: "" (the
// default, Claude), "claude" or "gemini". Callers normalise user input with
// router.NormalizeRouteChoice first.
func IsValidTaskProvider(p string) bool {
	switch p {
	case "", "claude", "gemini":
		return true
	}
	return false
}

// SetTaskProvider records the Board's explicit provider/model choice for a
// task (STA-838). provider and model must already be normalised
// (router.NormalizeRouteChoice); "" for both restores the default (Claude).
// The next run routes with it.
func SetTaskProvider(db *sql.DB, taskID, provider, model string) (*Task, error) {
	if !IsValidTaskProvider(provider) {
		return nil, fmt.Errorf("%w %q", ErrInvalidProvider, provider)
	}
	t, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	model = strings.TrimSpace(model)
	if _, err := db.Exec(
		`UPDATE tasks SET provider = ?, model_override = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`,
		provider, model, t.ID,
	); err != nil {
		return nil, fmt.Errorf("set task provider: %w", err)
	}
	// A run held for (or refused by) a Gemini-code approval is released when
	// the Board changes the choice; the next run re-evaluates it.
	_, _ = db.Exec(
		`UPDATE tasks SET is_blocked = 0, block_reason = '' WHERE id = ? AND (block_reason LIKE ? OR block_reason LIKE ?)`,
		t.ID, geminiapproval.WaitingReason+"%", geminiapproval.DeniedReason+"%")
	label := provider
	if label == "" {
		label = "default (claude)"
	}
	if model != "" {
		label += " / " + model
	}
	_ = LogActivity(db, t.ID, "provider_set", "provider: "+label)
	return GetTask(db, t.ID)
}

// ErrRunInProgress is returned when a change must wait for the task's run to
// finish (or be stopped).
var ErrRunInProgress = errors.New("a run is in progress")

// SetTaskWorkKind changes a task's work_kind after creation (STA-861). kind
// must be one of ValidWorkKinds. It is refused with ErrRunInProgress while a
// run holds the task (checked out, in_progress or paused): the live run was
// routed for the old kind. The caller checks the provider rules
// (router.ValidateTaskChoice) for the new kind first.
func SetTaskWorkKind(db *sql.DB, taskID, kind string) (*Task, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !IsValidWorkKind(kind) {
		return nil, fmt.Errorf("%w %q: must be one of %s", ErrInvalidWorkKind, kind, strings.Join(validWorkKinds, ", "))
	}
	t, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	res, err := db.Exec(
		`UPDATE tasks SET work_kind = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		  WHERE id = ? AND checkout_run_id IS NULL AND execution_stage NOT IN ('in_progress', 'paused')`,
		kind, t.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("set task work kind: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: stop it before changing the kind of task %s", ErrRunInProgress, t.ID)
	}
	_ = LogActivity(db, t.ID, "work_kind_set", "work kind: "+kind)
	return GetTask(db, t.ID)
}
