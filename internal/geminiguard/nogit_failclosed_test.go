package geminiguard

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// Fail-closed regressions for the non-git guard: every case is a turn that
// wrote code (or could have) and must fail the run.

func nogitWrite(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func mustTakeNoGit(t *testing.T, dir string) *Snapshot {
	t.Helper()
	pre, err := TakeNoGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := CheckNoGit(context.Background(), pre); r.Violated() {
		t.Fatalf("untouched folder flagged: %+v", r)
	}
	return pre
}

func wantViolated(t *testing.T, pre *Snapshot, why string) NoGitResult {
	t.Helper()
	r := CheckNoGit(context.Background(), pre)
	if !r.Violated() {
		t.Fatalf("%s: guard passed (fail open): %+v", why, r)
	}
	return r
}

// The task folder itself is a symlink (repo_path pointing at a link). WalkDir
// does not descend a symlinked root, so the snapshot was empty and nothing
// was ever flagged.
func TestCheckNoGit_SymlinkedRootFailsClosed(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	nogitWrite(t, real, "main.go", "package main\n", 0o644)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	pre := mustTakeNoGit(t, link)
	nogitWrite(t, real, "main.go", "package main\n// edit\n", 0o644)
	nogitWrite(t, real, "new.py", "print(1)\n", 0o644)
	wantViolated(t, pre, "code edit under a symlinked root")

	// Re-pointing the root link during the turn is a change too.
	pre = mustTakeNoGit(t, link)
	other := filepath.Join(base, "other")
	nogitWrite(t, other, "main.go", "package main\n", 0o644)
	_ = os.Remove(link)
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	wantViolated(t, pre, "root link re-pointed")
}

// walk skipped every directory named .git at any depth, so a turn could
// create .git/hooks/* or vendor/x/.git/payload.py unseen.
func TestCheckNoGit_DotGitDirsAreChecked(t *testing.T) {
	dir := t.TempDir()
	nogitWrite(t, dir, "README.md", "# x\n", 0o644)
	pre := mustTakeNoGit(t, dir)
	nogitWrite(t, dir, ".git/hooks/post-checkout", "#!/bin/sh\nrm -rf ~\n", 0o755)
	wantViolated(t, pre, "new root .git dir")

	dir = t.TempDir()
	nogitWrite(t, dir, "vendor/lib/.git/payload.py", "print(1)\n", 0o644)
	pre = mustTakeNoGit(t, dir)
	nogitWrite(t, dir, "vendor/lib/.git/payload.py", "import os\n", 0o644)
	nogitWrite(t, dir, "sub/.git", "gitdir: /tmp/evil\n", 0o644)
	r := wantViolated(t, pre, "nested .git contents")
	want := map[string]bool{"vendor/lib/.git/payload.py": true, "sub/.git": true}
	for _, p := range r.Changed {
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("changed = %q, missing %v", r.Changed, want)
	}
}

// A doc-named symlink is not a doc: it can point at code or outside the
// folder, and a write through it lands there.
func TestCheckNoGit_SymlinksAreNotDocs(t *testing.T) {
	dir := t.TempDir()
	nogitWrite(t, dir, "main.go", "package main\n", 0o644)
	pre := mustTakeNoGit(t, dir)
	if err := os.Symlink("main.go", filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	wantViolated(t, pre, "new README.md symlink to code")

	// A pre-existing doc-named link to a file outside the folder: writing
	// through it changes code the folder walk never sees.
	outside := t.TempDir()
	nogitWrite(t, outside, "tool.py", "print(1)\n", 0o644)
	dir = t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "tool.py"), filepath.Join(dir, "notes.md")); err != nil {
		t.Fatal(err)
	}
	pre = mustTakeNoGit(t, dir)
	nogitWrite(t, dir, "notes.md", "import os\n", 0o644) // follows the link
	wantViolated(t, pre, "write through doc-named symlink")
}

// A symlinked directory leads out of the folder: edits under it are invisible
// to the walk, so the turn cannot be verified.
func TestCheckNoGit_SymlinkedDirFailsClosed(t *testing.T) {
	outside := t.TempDir()
	nogitWrite(t, outside, "x.py", "print(1)\n", 0o644)
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}
	pre, err := TakeNoGit(dir)
	if err != nil {
		return // refusing to snapshot is fail-closed too
	}
	nogitWrite(t, outside, "x.py", "import os\n", 0o644)
	wantViolated(t, pre, "edit through symlinked dir")
}

// An executable "doc" is a script: a new run.md with the exec bit, or chmod
// +x on an existing doc, counts as a non-doc change.
func TestCheckNoGit_ExecutableDocIsCode(t *testing.T) {
	dir := t.TempDir()
	nogitWrite(t, dir, "README.md", "# x\n", 0o644)
	pre := mustTakeNoGit(t, dir)
	nogitWrite(t, dir, "run.md", "#!/bin/sh\necho pwned\n", 0o755)
	wantViolated(t, pre, "new executable .md")

	pre = mustTakeNoGit(t, dir)
	if err := os.Chmod(filepath.Join(dir, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantViolated(t, pre, "chmod +x on a doc")
}

// A doc-named hard link shares an inode with a file the walk may not see
// (outside the folder): writes to it are writes to that file.
func TestCheckNoGit_HardLinkedDocIsCode(t *testing.T) {
	outside := t.TempDir()
	nogitWrite(t, outside, "tool.py", "print(1)\n", 0o644)
	dir := t.TempDir()
	pre := mustTakeNoGit(t, dir)
	if err := os.Link(filepath.Join(outside, "tool.py"), filepath.Join(dir, "notes.md")); err != nil {
		t.Skip("hard links unsupported here:", err)
	}
	wantViolated(t, pre, "new hard-linked doc")
}

// A doc-named FIFO/socket/device is not a document.
func TestCheckNoGit_SpecialFileIsCode(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("mkfifo")
	}
	dir := t.TempDir()
	pre := mustTakeNoGit(t, dir)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	wantViolated(t, pre, "doc-named fifo")
}

// Already fail-closed; kept as regressions.
func TestCheckNoGit_ExistingFailClosedPaths(t *testing.T) {
	// Rename code to a doc name, edit, rename back: ctime moves.
	dir := t.TempDir()
	nogitWrite(t, dir, "a.py", "print(1)\n", 0o644)
	pre := mustTakeNoGit(t, dir)
	_ = os.Rename(filepath.Join(dir, "a.py"), filepath.Join(dir, "a.md"))
	nogitWrite(t, dir, "a.md", "import os\n", 0o644)
	_ = os.Rename(filepath.Join(dir, "a.md"), filepath.Join(dir, "a.py"))
	wantViolated(t, pre, "rename round trip")

	// Unreadable subtree after the turn: the walk errors, the check fails.
	if os.Geteuid() != 0 {
		dir = t.TempDir()
		nogitWrite(t, dir, "sub/x.md", "x\n", 0o644)
		pre = mustTakeNoGit(t, dir)
		sub := filepath.Join(dir, "sub")
		_ = os.Chmod(sub, 0)
		t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
		r := wantViolated(t, pre, "unreadable subtree")
		if r.Err == nil {
			t.Fatalf("unreadable subtree must report an error: %+v", r)
		}
	}
}

// Fail-closed must not turn into fail-always: doc edits next to an in-folder
// directory link, and untouched executable or linked files, still pass.
func TestCheckNoGit_DocEditsStillPass(t *testing.T) {
	dir := t.TempDir()
	nogitWrite(t, dir, "docs/v2/guide.md", "v2\n", 0o644)
	nogitWrite(t, dir, "bin/tool.sh", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("v2", filepath.Join(dir, "docs", "latest")); err != nil {
		t.Fatal(err)
	}
	pre := mustTakeNoGit(t, dir)
	nogitWrite(t, dir, "docs/v2/new.md", "new\n", 0o644)
	nogitWrite(t, dir, "docs/v2/guide.md", "v2 edited\n", 0o644)
	nogitWrite(t, dir, "README.md", "# readme\n", 0o644)
	if r := CheckNoGit(context.Background(), pre); r.Violated() {
		t.Fatalf("doc-only turn flagged: %+v", r)
	}
}
