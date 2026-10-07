package main

import (
	"bytes"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

func agentEnv(k string) string {
	if k == "CLAUDECODE" {
		return "1"
	}
	return ""
}

func TestOrgHoldCLI_BoardOnlyAndCannotLift(t *testing.T) {
	trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()
	var out bytes.Buffer

	if err := runOrgHold(&out, conn, "Managed Solution", false, agentEnv, true); err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Fatalf("agent session: err = %v, want refusal", err)
	}
	if err := runOrgHold(&out, conn, "Managed Solution", false, noEnv, false); err == nil {
		t.Fatal("no terminal: want refusal")
	}
	if governance.OrgHeld(conn, "Managed Solution") {
		t.Fatal("a refused command placed the hold")
	}
	if err := runOrgHold(&out, conn, "Managed Solution", false, noEnv, true); err != nil {
		t.Fatalf("Board terminal hold: %v", err)
	}
	if !governance.OrgHeld(conn, "Managed Solution") {
		t.Fatal("hold not placed")
	}
	// Lifting needs Touch ID: the CLI refuses even for the Board.
	if err := runOrgHold(&out, conn, "Managed Solution", true, noEnv, true); err == nil || !strings.Contains(err.Error(), "Touch ID") {
		t.Fatalf("CLI release: err = %v, want Touch ID refusal", err)
	}
	if !governance.OrgHeld(conn, "Managed Solution") {
		t.Fatal("CLI lifted the hold")
	}
}

func TestCLITaskOriginAndBoardStage(t *testing.T) {
	work, _ := trackingEnv(t)
	if got := cliTaskOrigin(agentEnv); got != meshContext.OriginAgent {
		t.Errorf("origin in agent session = %s, want agent", got)
	}
	if got := cliTaskOrigin(noEnv); got != meshContext.OriginNative {
		t.Errorf("origin at the Board terminal = %s, want native", got)
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()
	task, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: "agent work", RepoPath: work, GitBranch: "main", Origin: meshContext.OriginAgent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliBoardStage(conn, task.ID, "todo", agentEnv, true); err == nil {
		t.Error("agent session moving an agent task to todo: want refusal")
	}
	if ok, err := cliBoardStage(conn, task.ID, "todo", noEnv, true); err != nil || !ok {
		t.Errorf("Board terminal: ok=%v err=%v", ok, err)
	}
	if ok, err := cliBoardStage(conn, task.ID, "cancelled", agentEnv, false); err != nil || ok {
		t.Errorf("closing needs no Board: ok=%v err=%v", ok, err)
	}
	prod, _ := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: "ship to prod", RepoPath: work, GitBranch: "main", Origin: meshContext.OriginAgent})
	if _, err := cliBoardStage(conn, prod.ID, "todo", noEnv, true); err == nil || !strings.Contains(err.Error(), "Touch ID") {
		t.Errorf("prod agent task from CLI: err = %v, want Touch ID refusal", err)
	}
}
