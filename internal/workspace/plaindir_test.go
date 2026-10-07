package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveTaskDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv(ScratchRootEnv, filepath.Join(root, "scratch"))

	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if InGitRepo(plain) {
		t.Skip("temp dir is inside a git repository on this machine")
	}
	td, err := ResolveTaskDir(plain, "task-1")
	if err != nil || td.Git || td.Scratch || td.Dir != plain {
		t.Fatalf("plain dir = %+v, %v", td, err)
	}

	td, err = ResolveTaskDir("", "task-2")
	if err != nil || td.Git || !td.Scratch || td.Dir != filepath.Join(root, "scratch", "task-2") {
		t.Fatalf("empty repo_path = %+v, %v", td, err)
	}
	if fi, err := os.Stat(td.Dir); err != nil || !fi.IsDir() {
		t.Fatalf("scratch dir not created: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(td.Dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("scratch dir must never be git-initialised")
	}
	if td, _ := DescribeTaskDir("", "task-3"); !td.Scratch {
		t.Fatalf("describe empty = %+v", td)
	} else if _, err := os.Stat(td.Dir); !os.IsNotExist(err) {
		t.Fatal("DescribeTaskDir created the scratch dir")
	}
	if _, err := ResolveTaskDir("", "../escape"); err == nil {
		t.Fatal("unsafe task id accepted for the scratch dir")
	}

	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	for _, p := range []string{repo, sub} {
		if td, err := ResolveTaskDir(p, "task-4"); err != nil || !td.Git || td.Dir != p {
			t.Fatalf("git path %s = %+v, %v", p, td, err)
		}
	}

	missing := filepath.Join(root, "missing")
	if td, err := ResolveTaskDir(missing, "task-5"); err != nil || !td.Git || td.Dir != missing {
		t.Fatalf("missing path keeps the git path: %+v, %v", td, err)
	}
}
