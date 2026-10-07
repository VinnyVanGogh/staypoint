package main

import (
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func TestFilterTaskList(t *testing.T) {
	tasks := []meshContext.Task{
		{ID: "a", Origin: meshContext.OriginNative, ExecutionStage: "backlog"},
		{ID: "b", Origin: meshContext.OriginLegacy, ExecutionStage: "todo"},
		{ID: "c", Origin: meshContext.OriginPaperclipImport, ExecutionStage: "todo"},
	}
	if got := filterTaskList(tasks, false, ""); len(got) != 2 {
		t.Fatalf("default hides legacy: %+v", got)
	}
	if got := filterTaskList(tasks, true, "todo"); len(got) != 2 || got[0].ID != "b" {
		t.Fatalf("legacy+todo: %+v", got)
	}
	if got := filterTaskList(tasks, false, "BACKLOG"); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("stage filter: %+v", got)
	}
}
