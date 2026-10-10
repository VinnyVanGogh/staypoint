package opstools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Board review #4: run tokens are random, held in memory, bound to one task
// and run, and dead once the run ends.
func TestRunTokens(t *testing.T) {
	r := NewRunTokens()
	if _, err := r.Check("task-a", "anything"); err == nil {
		t.Fatal("an unissued token checked out")
	}
	tok, revoke, err := r.Issue("task-a", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token %q is not 32 random bytes in hex", tok)
	}
	ref, err := r.Check("task-a", tok)
	if err != nil || ref != (RunRef{TaskID: "task-a", RunID: "run-1"}) {
		t.Fatalf("issued token: %+v, %v", ref, err)
	}
	again, revoke2, _ := r.Issue("task-a", "run-2")
	defer revoke2()
	if again == tok {
		t.Fatal("two runs of one task got the same token")
	}
	for _, c := range []struct{ task, tok, want string }{
		{"task-b", tok, "does not belong to task task-b"},
		{"", tok, "no STAYPOINT_TASK_ID"},
		{"task-a", "", "no " + RunTokenEnv},
		{"task-a", strings.ToUpper(tok), "not a live run's token"},
		{"task-a", tok[:63], "not a live run's token"},
	} {
		if _, err := r.Check(c.task, c.tok); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%q, %q) = %v, want %q", c.task, c.tok, err, c.want)
		}
	}
	revoke()
	revoke() // idempotent
	if _, err := r.Check("task-a", tok); err == nil {
		t.Fatal("token checked out after its run ended")
	}
	if _, err := r.Check("task-a", again); err != nil {
		t.Fatalf("revoking run-1 ended run-2's token: %v", err)
	}
	if _, _, err := r.Issue("", "run"); err == nil {
		t.Fatal("issued a token with no task")
	}
	if _, _, err := r.Issue("task-a", " "); err == nil {
		t.Fatal("issued a token with no run")
	}
}

func TestRunTokensConcurrent(t *testing.T) {
	r := NewRunTokens()
	var wg sync.WaitGroup
	toks := make([]string, 32)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, revoke, err := r.Issue("task-a", "run")
			if err != nil {
				t.Error(err)
				return
			}
			toks[i] = tok
			if _, err := r.Check("task-a", tok); err != nil {
				t.Error(err)
			}
			revoke()
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, tok := range toks {
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
		if _, err := r.Check("task-a", tok); err == nil {
			t.Fatal("token live after revoke")
		}
	}
}

func TestRemoveLegacyOpsKey(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveLegacyOpsKey(dir); err != nil {
		t.Fatalf("no key: %v", err)
	}
	p := filepath.Join(dir, legacyOpsKeyFile)
	if err := os.WriteFile(p, []byte("00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLegacyOpsKey(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("ops_key still there: %v", err)
	}
}
