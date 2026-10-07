package board

import (
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/context"
)

func TestMapTaskToColumn_Backlog(t *testing.T) {
	if got := MapTaskToColumn(context.Task{Status: "active", ExecutionStage: "backlog"}); got != ColBacklog {
		t.Fatalf("backlog task in %s", got)
	}
	if AllColumns[0] != ColBacklog {
		t.Fatalf("backlog must be the first column, got %v", AllColumns)
	}
}

func TestRebuildColumns_HidesLegacy(t *testing.T) {
	m := &Model{columns: map[ColumnType][]context.Task{}}
	m.allTasks = []context.Task{
		{ID: "task-native1", Status: "active", ExecutionStage: "backlog", Origin: context.OriginNative},
		{ID: "task-legacy1", Status: "active", ExecutionStage: "todo", Origin: context.OriginLegacy},
	}
	m.rebuildColumns()
	if len(m.columns[ColBacklog]) != 1 || len(m.columns[ColTodo]) != 0 {
		t.Fatalf("columns: backlog=%d todo=%d", len(m.columns[ColBacklog]), len(m.columns[ColTodo]))
	}
}
