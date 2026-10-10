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
	if _, err := checkToken(r, "task-a", "anything"); err == nil {
		t.Fatal("an unissued token checked out")
	}
	tok, revoke, err := r.Issue("task-a", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token %q is not 32 random bytes in hex", tok)
	}
	ref, err := checkToken(r, "task-a", tok)
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
		{"task-a", "", "not a live run's token"},
		{"task-a", strings.ToUpper(tok), "not a live run's token"},
		{"task-a", tok[:63], "not a live run's token"},
	} {
		if _, err := checkToken(r, c.task, c.tok); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%q, %q) = %v, want %q", c.task, c.tok, err, c.want)
		}
	}
	revoke()
	revoke() // idempotent
	if _, err := checkToken(r, "task-a", tok); err == nil {
		t.Fatal("token checked out after its run ended")
	}
	if _, err := checkToken(r, "task-a", again); err != nil {
		t.Fatalf("revoking run-1 ended run-2's token: %v", err)
	}
	if _, _, err := r.Issue("", "run"); err == nil {
		t.Fatal("issued a token with no task")
	}
	if _, _, err := r.Issue("task-a", " "); err == nil {
		t.Fatal("issued a token with no run")
	}
}

// The daemon's answer to a hash-only check proves it holds the token.
func TestRunTokensCheckHashProof(t *testing.T) {
	r := NewRunTokens()
	tok, revoke, _ := r.Issue("task-a", "run-1")
	defer revoke()
	nonce := strings.Repeat("n", 32)
	ref, proof, err := r.CheckHash("task-a", TokenHash(tok), nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRunCheckProof(tok, nonce, ref, proof) {
		t.Fatal("daemon proof does not verify")
	}
	for name, ok := range map[string]bool{
		"other nonce":       VerifyRunCheckProof(tok, strings.Repeat("m", 32), ref, proof),
		"other run":         VerifyRunCheckProof(tok, nonce, RunRef{TaskID: "task-a", RunID: "run-2"}, proof),
		"keyed by the hash": VerifyRunCheckProof(tok, nonce, ref, RunCheckProof(TokenHash(tok), nonce, ref)),
	} {
		if ok {
			t.Errorf("%s: proof verified", name)
		}
	}
	for _, bad := range []string{"", "zz", TokenHash(tok)[:62], tok} {
		if _, _, err := r.CheckHash("task-a", bad, nonce); err == nil {
			t.Errorf("CheckHash(%q) passed", bad)
		}
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
			if _, err := checkToken(r, "task-a", tok); err != nil {
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
		if _, err := checkToken(r, "task-a", tok); err == nil {
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

// checkToken checks tok the way the daemon's run-token endpoint does: by
// hash, with no proof wanted.
func checkToken(r *RunTokens, taskID, tok string) (RunRef, error) {
	ref, _, err := r.CheckHash(taskID, TokenHash(tok), "")
	return ref, err
}
