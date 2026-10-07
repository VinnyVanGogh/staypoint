package main

import (
	"path/filepath"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// 2026-10-07: an interactive session in agent-mesh was blocked because a
// different, stopped task in the same repo (macmaint) was over its turn
// budget. Only the session's own task may gate it.
func TestBudgetTaskForSession_OnlyOwnTask(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()
	repo := t.TempDir()
	other, err := meshContext.CreateTask(conn, "macmaint", repo, "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Exec(`UPDATE tasks SET execution_stage='stopped', max_turns=40, spent_turns=41 WHERE id=?`, other.ID)
	mine, err := meshContext.CreateTask(conn, "mine", repo, "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) string { return "" }

	if got := budgetTaskForSession(conn, "sess-unattached", noEnv); got != nil {
		t.Fatalf("unattached session must not inherit a repo task's budget, got %s", got.ID)
	}
	if err := trackgate.Attach(conn, "sess-attached", mine.ID, trackgate.ClientClaude, repo); err != nil {
		t.Fatal(err)
	}
	if got := budgetTaskForSession(conn, "sess-attached", noEnv); got == nil || got.ID != mine.ID {
		t.Fatalf("attached session must use its own task, got %v", got)
	}
	runEnv := func(k string) string {
		if k == "STAYPOINT_TASK_ID" {
			return other.ID
		}
		return ""
	}
	if got := budgetTaskForSession(conn, "sess-attached", runEnv); got == nil || got.ID != other.ID {
		t.Fatalf("daemon run must use STAYPOINT_TASK_ID, got %v", got)
	}
}
