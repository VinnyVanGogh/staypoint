package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// STA-861 #1: `task create --org` makes an interactive task the daemon never
// claims; Run Now (in_progress) still runs it.
func TestCreateOrgTask_IsNeverClaimedByDaemon(t *testing.T) {
	work, _ := trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	var out bytes.Buffer
	if err := createLocalOrgTask(&out, conn, "Managed Solution", "", "Interactive fix", work, "sess-861", trackgate.ClientClaude, 0, 0); err != nil {
		t.Fatalf("create --org: %v", err)
	}
	taskID, err := trackgate.SessionTask(conn, "sess-861")
	if err != nil || taskID == "" {
		t.Fatalf("session not attached: %q %v", taskID, err)
	}
	task, _ := meshContext.GetTask(conn, taskID)
	if task.ExecutionStage != "backlog" {
		t.Fatalf("interactive task stage = %s, want backlog", task.ExecutionStage)
	}
	if !strings.Contains(out.String(), "the daemon will not run it") {
		t.Errorf("create output should say the task is parked: %s", out.String())
	}

	h := &orchestrator.Harness{DB: conn}
	if err := h.Claim(context.Background(), taskID, "run-1", "agent"); !errors.Is(err, orchestrator.ErrNotRunnable) {
		t.Fatalf("daemon claim on interactive task: err = %v, want ErrNotRunnable", err)
	}

	// Board Run Now: backlog -> todo -> in_progress, then the claim succeeds.
	if err := meshContext.SetTaskExecutionStageWithOptions(conn, taskID, "in_progress", meshContext.DoneOptions{BoardStage: true}); err != nil {
		t.Fatalf("run now: %v", err)
	}
	if err := h.Claim(context.Background(), taskID, "run-2", "agent"); err != nil {
		t.Fatalf("claim after Run Now: %v", err)
	}
	h.Release(taskID, "run-2")
}

// STA-861 #3: done succeeds after a PR is registered from the CLI, and the
// --pr shortcut registers and closes in one step.
func TestTaskProductAddAndDonePR(t *testing.T) {
	work, _ := trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	mk := func(name string) string {
		t.Helper()
		task, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: name, RepoPath: work, GitBranch: "main", ExecutionStage: "backlog"})
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}

	var out bytes.Buffer
	a := mk("a")
	if err := markTaskDoneCLI(&out, conn, a, ""); !errors.Is(err, meshContext.ErrNoWorkProduct) {
		t.Fatalf("done without product: err = %v", err)
	}
	if err := addTaskProduct(&out, conn, a, "pr", "https://github.com/o/r/pull/1"); err != nil {
		t.Fatalf("product add: %v", err)
	}
	if err := addTaskProduct(&out, conn, a, "nope", "x"); !errors.Is(err, meshContext.ErrInvalidWorkProduct) {
		t.Errorf("bad type: err = %v", err)
	}
	if err := markTaskDoneCLI(&out, conn, a, ""); err != nil {
		t.Fatalf("done after product add: %v", err)
	}

	b := mk("b")
	if err := markTaskDoneCLI(&out, conn, b, "https://github.com/o/r/pull/2"); err != nil {
		t.Fatalf("done --pr: %v", err)
	}
	products, _ := meshContext.GetTaskWorkProducts(conn, b)
	if len(products) != 1 || products[0].ProductType != "pull_request" || products[0].Reference != "https://github.com/o/r/pull/2" {
		t.Errorf("done --pr products = %+v", products)
	}
	for _, id := range []string{a, b} {
		if task, _ := meshContext.GetTask(conn, id); task.Status != "done" {
			t.Errorf("%s status = %s, want done", id, task.Status)
		}
	}
}

// set-kind validates the kind, refuses mid-run, and applies the #231 rule:
// a gemini task cannot take a code kind in a work repo.
func TestSetTaskKindCLI(t *testing.T) {
	work, personal := trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	mk := func(repo, provider string) string {
		t.Helper()
		task, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: "k", RepoPath: repo, GitBranch: "main", WorkKind: "docs", Provider: provider, ExecutionStage: "backlog"})
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	var out bytes.Buffer
	id := mk(personal, "")
	if err := setTaskKind(&out, conn, id, "review"); err != nil {
		t.Fatalf("set-kind review: %v", err)
	}
	if task, _ := meshContext.GetTask(conn, id); task.WorkKind != "review" {
		t.Errorf("kind = %s", task.WorkKind)
	}
	if err := setTaskKind(&out, conn, id, "poetry"); err == nil {
		t.Error("invalid kind accepted")
	}

	gem := mk(work, "gemini")
	if err := setTaskKind(&out, conn, gem, "coding"); !errors.Is(err, router.ErrGeminiCodeKind) {
		t.Errorf("gemini + coding in work repo: err = %v, want ErrGeminiCodeKind", err)
	}
	if err := setTaskKind(&out, conn, gem, "planning"); err != nil {
		t.Errorf("gemini + planning in work repo: %v", err)
	}

	if _, err := conn.Exec(`UPDATE tasks SET execution_stage = 'in_progress', checkout_run_id = 'run-1' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := setTaskKind(&out, conn, id, "docs"); !errors.Is(err, meshContext.ErrRunInProgress) {
		t.Errorf("set-kind mid-run: err = %v, want ErrRunInProgress", err)
	}
}
