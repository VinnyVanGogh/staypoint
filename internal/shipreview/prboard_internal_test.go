package shipreview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// combineRepo makes a bare origin and a clone with main plus two PR heads
// (refs/pull/N/head on origin). With conflict, both PRs edit the same line.
func combineRepo(t *testing.T, conflict bool) (clone, origin, head1, head2 string) {
	t.Helper()
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	runGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	runGit(t, root, "clone", "-q", origin, clone)
	runGit(t, clone, "checkout", "-q", "-b", "main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(clone, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("f.txt", "base\n")
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-qm", "base")
	runGit(t, clone, "push", "-q", "origin", "main")
	mk := func(n, file, body string) string {
		runGit(t, clone, "checkout", "-q", "-b", "pr"+n, "main")
		write(file, body)
		runGit(t, clone, "add", ".")
		runGit(t, clone, "commit", "-qm", "pr "+n)
		sha := runGit(t, clone, "rev-parse", "HEAD")
		runGit(t, clone, "push", "-q", "origin", "HEAD:refs/pull/"+n+"/head")
		runGit(t, clone, "checkout", "-q", "main")
		return sha
	}
	head1 = mk("1", "f.txt", "one\n")
	if conflict {
		head2 = mk("2", "f.txt", "two\n")
	} else {
		head2 = mk("2", "g.txt", "two\n")
	}
	return clone, origin, head1, head2
}

func fakeGHOnPath(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func worktreeCount(t *testing.T, clone string) int {
	return len(strings.Split(runGit(t, clone, "worktree", "list", "--porcelain"), "\nworktree "))
}

func TestCombinePRs_ConflictLeavesNothing(t *testing.T) {
	clone, origin, h1, h2 := combineRepo(t, true)
	fakeGHOnPath(t, `echo "gh must not be called on conflict" >&2; exit 9`)
	_, err := GHAuth{RepoDir: clone}.CombinePRs(context.Background(), CombineRequest{
		Base: "main", Branch: "staypoint/combine-test",
		Sources: []CombineSource{{Number: 1, HeadSHA: h1}, {Number: 2, HeadSHA: h2}},
	})
	var ce *CombineError
	if !errors.As(err, &ce) || ce.Number != 2 || !errors.Is(err, ErrCombineConflict) {
		t.Fatalf("want conflict on #2, got %v", err)
	}
	if out := runGit(t, origin, "branch", "--list", "staypoint/combine-test"); out != "" {
		t.Fatalf("integration branch left on origin: %q", out)
	}
	if n := worktreeCount(t, clone); n != 1 {
		t.Fatalf("temp worktree not removed: %d worktrees", n)
	}
}

func TestCombinePRs_HeadMovedRefused(t *testing.T) {
	clone, origin, h1, _ := combineRepo(t, false)
	_, err := GHAuth{RepoDir: clone}.CombinePRs(context.Background(), CombineRequest{
		Base: "main", Branch: "staypoint/combine-test",
		Sources: []CombineSource{{Number: 1, HeadSHA: h1}, {Number: 2, HeadSHA: "deadbeef"}},
	})
	if !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("want head moved, got %v", err)
	}
	if out := runGit(t, origin, "branch", "--list", "staypoint/combine-test"); out != "" {
		t.Fatalf("branch pushed despite moved head: %q", out)
	}
}

func TestCombinePRs_OpensOnePR(t *testing.T) {
	clone, origin, h1, h2 := combineRepo(t, false)
	argsFile := filepath.Join(t.TempDir(), "args")
	fakeGHOnPath(t, `printf '%s\n' "$@" > "`+argsFile+`"; echo https://github.com/o/r/pull/42`)
	res, err := GHAuth{RepoDir: clone}.CombinePRs(context.Background(), CombineRequest{
		Base: "main", Branch: "staypoint/combine-ok",
		Sources: []CombineSource{{Number: 1, HeadSHA: h1, Title: "first"}, {Number: 2, HeadSHA: h2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 42 || res.Branch != "staypoint/combine-ok" {
		t.Fatalf("result: %+v", res)
	}
	tip := runGit(t, origin, "rev-parse", "refs/heads/staypoint/combine-ok")
	for _, h := range []string{h1, h2} {
		cmd := exec.Command("git", "merge-base", "--is-ancestor", h, tip)
		cmd.Dir = origin
		if err := cmd.Run(); err != nil {
			t.Errorf("%s not merged into integration branch", h)
		}
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "- #1 first") || !strings.Contains(string(args), "- #2") {
		t.Fatalf("PR body does not list sources: %s", args)
	}
}

func TestCombinePRs_PRCreateFailureDeletesBranch(t *testing.T) {
	clone, origin, h1, h2 := combineRepo(t, false)
	fakeGHOnPath(t, `echo "GraphQL: nope" >&2; exit 1`)
	_, err := GHAuth{RepoDir: clone}.CombinePRs(context.Background(), CombineRequest{
		Base: "main", Branch: "staypoint/combine-fail",
		Sources: []CombineSource{{Number: 1, HeadSHA: h1}, {Number: 2, HeadSHA: h2}},
	})
	if err == nil {
		t.Fatal("want error")
	}
	if out := runGit(t, origin, "branch", "--list", "staypoint/combine-fail"); out != "" {
		t.Fatalf("pushed branch not deleted after PR create failed: %q", out)
	}
}

func TestCountChecks_PlaywrightSeparate(t *testing.T) {
	c := countChecks([]ghRollupItem{
		{Typename: "CheckRun", Name: "go test", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Typename: "CheckRun", Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Typename: "CheckRun", Name: "build", Status: "IN_PROGRESS"},
		{Typename: "StatusContext", Context: "ci/legacy", State: "SUCCESS"},
		{Typename: "CheckRun", Name: "Playwright UI Specs", Status: "COMPLETED", Conclusion: "FAILURE"},
	})
	if c.Pass != 2 || c.Fail != 1 || c.Pending != 1 || c.Playwright != "fail" {
		t.Fatalf("counts: %+v", c)
	}
}
