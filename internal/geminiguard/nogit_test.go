package geminiguard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckNoGit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("gone.sh", "echo hi\n")
	write("README.md", "# x\n")

	pre, err := TakeNoGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	write("README.md", "# x\nmore\n")
	write("docs/guide.md", "guide\n")
	if r := CheckNoGit(context.Background(), pre); r.Violated() {
		t.Fatalf("doc-only turn flagged: %+v", r)
	}

	write("main.go", "package main\n// edit\n")
	write("new.py", "print(1)\n")
	_ = os.Remove(filepath.Join(dir, "gone.sh"))
	r := CheckNoGit(context.Background(), pre)
	if want := []string{"gone.sh", "main.go", "new.py"}; !reflect.DeepEqual(r.Changed, want) {
		t.Fatalf("changed = %q, want %q", r.Changed, want)
	}
	if !strings.Contains(r.Title(), "(not reverted)") || !strings.Contains(r.Body(), "NOT reverted") {
		t.Fatalf("title/body must say nothing was reverted: %q / %q", r.Title(), r.Body())
	}
	if got := readBody(t, filepath.Join(dir, "main.go")); got != "package main\n// edit\n" {
		t.Fatalf("CheckNoGit modified files: main.go = %q", got)
	}
	if r := CheckNoGit(context.Background(), nil); !r.Violated() {
		t.Fatal("nil snapshot must fail closed")
	}
}

func readBody(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
