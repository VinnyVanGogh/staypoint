package main

import (
	"database/sql"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/taskref"
	"github.com/spf13/cobra"
)

// taskRefCommands are the top-level commands whose first positional argument
// names a task. Under them, a reference (STA-123) is accepted wherever a task
// id is.
var taskRefCommands = map[string]bool{"task": true, "ship-review": true, "run": true}

// freeTextCommands take free text, not a task, as their first argument.
var freeTextCommands = map[string]bool{"create": true, "add": true, "list": true, "tree": true}

// allArgsTaskCommands take a task id in every argument (the task, then the
// tasks it is blocked by).
var allArgsTaskCommands = map[string]bool{"block": true, "unblock": true}

// taskRefFlags are flags that take a task id.
var taskRefFlags = []string{"parent", "task-id", "task"}

// resolveTaskRefArgs rewrites task references in cmd's arguments and task
// flags to task ids in place, before the command runs; the commands then only
// ever see ids. A reference the database does not know is left as typed, so
// the command reports it as not found.
func resolveTaskRefArgs(cmd *cobra.Command, args []string, dbPath string) {
	top := cmd
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	if !top.HasParent() || (!taskRefCommands[top.Name()] && top.Name() != "chat") {
		return
	}
	var idx []int
	switch {
	case top.Name() == "chat":
	case allArgsTaskCommands[cmd.Name()] && cmd.Parent() == top:
		for i := range args {
			idx = append(idx, i)
		}
	case !freeTextCommands[cmd.Name()] && len(args) > 0:
		idx = []int{0}
	}
	var flags []string
	for _, name := range taskRefFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			flags = append(flags, name)
		}
	}
	needs := false
	for _, i := range idx {
		if _, _, ok := taskref.Parse(args[i]); ok {
			needs = true
		}
	}
	for _, name := range flags {
		v, _ := cmd.Flags().GetString(name)
		if _, _, ok := taskref.Parse(v); ok {
			needs = true
		}
	}
	if !needs {
		return
	}
	conn, err := db.OpenReadOnly(dbPath)
	if err != nil {
		return
	}
	defer conn.Close()
	rewriteTaskRefs(conn, cmd, args, idx, flags)
}

func rewriteTaskRefs(conn *sql.DB, cmd *cobra.Command, args []string, idx []int, flags []string) {
	for _, i := range idx {
		args[i] = meshContext.ResolveTaskID(conn, args[i])
	}
	for _, name := range flags {
		v, _ := cmd.Flags().GetString(name)
		if id := meshContext.ResolveTaskID(conn, v); id != v {
			_ = cmd.Flags().Set(name, id)
		}
	}
}
