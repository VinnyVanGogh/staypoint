package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A daemon agent run cannot propose a Board action plan from the CLI; the
// refusal comes before the plan file or any token is read.
func TestBoardPlanPropose_RefusedInAgentRun(t *testing.T) {
	t.Setenv("STAYPOINT_TASK_ID", "task-agent-run")
	plan := filepath.Join(t.TempDir(), "plan.json")
	_ = os.WriteFile(plan, []byte(`{"actions":[{"task_id":"task-1","action":"run_now"}]}`), 0o600)
	if err := boardPlanProposeCmd.Flags().Set("file", plan); err != nil {
		t.Fatal(err)
	}
	err := boardPlanProposeCmd.RunE(boardPlanProposeCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "agent run") {
		t.Fatalf("want agent-run refusal, got %v", err)
	}
}

func TestBoardPlanCmd_Registered(t *testing.T) {
	for _, c := range boardCmd.Commands() {
		if c.Name() == "plan" {
			for _, sub := range c.Commands() {
				if sub.Name() == "propose" {
					return
				}
			}
		}
	}
	t.Fatal("staypoint board plan propose not registered")
}
