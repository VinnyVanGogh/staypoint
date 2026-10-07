package context

import (
	"sort"
	"testing"
)

// VisibleTasksSQL must hide exactly the rows IsHiddenByDefault hides, so SQL
// aggregators (fleet overview, spend totals) and slice filters agree.
func TestVisibleTasksSQL_MatchesIsHiddenByDefault(t *testing.T) {
	database := setupTestDB(t)
	rows := []struct{ id, origin, stage string }{
		{"native-todo", OriginNative, "todo"},
		{"native-done", OriginNative, "done"},
		{"import-open", OriginPaperclipImport, "in_progress"},
		{"import-backlog", OriginPaperclipImport, "backlog"},
		{"import-done", OriginPaperclipImport, "done"},
		{"import-cancelled", OriginPaperclipImport, "cancelled"},
		{"legacy-todo", OriginLegacy, "todo"},
		{"legacy-done", OriginLegacy, "done"},
	}
	for _, r := range rows {
		if _, err := database.Exec(`INSERT INTO tasks (id, name, repo_path, git_branch, status, account_role, execution_stage, origin)
			VALUES (?, ?, '/repo/x', 'main', 'active', 'personal', ?, ?)`, r.id, r.id, r.stage, r.origin); err != nil {
			t.Fatal(err)
		}
	}

	query := func(includeHidden bool) []string {
		t.Helper()
		res, err := database.Query(`SELECT id FROM tasks t WHERE ` + VisibleTasksSQL("t", includeHidden))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Close()
		var ids []string
		for res.Next() {
			var id string
			if err := res.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}

	var want []string
	for _, r := range rows {
		if !IsHiddenByDefault(Task{Origin: r.origin, ExecutionStage: r.stage}) {
			want = append(want, r.id)
		}
	}
	sort.Strings(want)
	if got := query(false); !equalStrings(got, want) {
		t.Errorf("visible rows = %v, want %v", got, want)
	}
	if got := query(true); len(got) != len(rows) {
		t.Errorf("includeHidden rows = %v, want all %d", got, len(rows))
	}
	// Unaliased form works against a bare tasks table too.
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM tasks WHERE ` + HiddenByDefaultSQL("")).Scan(&n); err != nil || n != 4 {
		t.Errorf("hidden count = %d (err %v), want 4", n, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
