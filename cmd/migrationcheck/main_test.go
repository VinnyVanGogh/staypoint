package main

import (
	"strings"
	"testing"
)

func src(entries string) []byte {
	return []byte("package db\n\nvar Migrations = []Migration{\n" + entries + ",\n}\n")
}

func mustParse(t *testing.T, b []byte) []migration {
	t.Helper()
	ms, err := parseMigrations(b)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// #247's first push: its own 40 on top of main's 40 (main's entry replaced).
func TestCheck_RejectsVersionAlreadyOnBaseUnderOtherName(t *testing.T) {
	base := mustParse(t, src(`{Version: 39, Name: "a"}, {Version: 40, Name: "task_target_branch"}`))
	head := mustParse(t, src(`{Version: 39, Name: "a"}, {Version: 40, Name: "decision_log_shadow_columns"}`))
	p := check(base, head)
	if len(p) != 1 || !strings.Contains(p[0], "version 40") || !strings.Contains(p[0], "renumber to 41") {
		t.Fatalf("problems = %q", p)
	}
}

// The same PR on a merge ref: both 40s present.
func TestCheck_RejectsDuplicateWithinHead(t *testing.T) {
	base := mustParse(t, src(`{Version: 40, Name: "task_target_branch"}`))
	head := mustParse(t, src(`{Version: 40, Name: "task_target_branch"}, {Version: 40, Name: "decision_log_shadow_columns"}`))
	p := check(base, head)
	if len(p) != 1 || !strings.Contains(p[0], "version 40 used twice") {
		t.Fatalf("problems = %q", p)
	}
}

func TestCheck_AcceptsNewVersionsAndGaps(t *testing.T) {
	base := mustParse(t, src(`{Version: 25, Name: "a"}, {Version: 27, Name: "b"}`))
	head := mustParse(t, src(`{Version: 25, Name: "a"}, {Version: 26, Name: "gap_filler"}, {Version: 27, Name: "b"}, {Version: 28, Name: "c"}`))
	if p := check(base, head); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}

func TestCheck_EmptyBase(t *testing.T) {
	head := mustParse(t, src(`{Version: 1, Name: "baseline"}`))
	if p := check(nil, head); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}

func TestParse_RejectsNonLiteralVersion(t *testing.T) {
	_, err := parseMigrations(src(`{Version: next(), Name: "x"}`))
	if err == nil || !strings.Contains(err.Error(), "integer literal") {
		t.Fatalf("err = %v", err)
	}
}

func TestParse_RejectsMissingVersion(t *testing.T) {
	_, err := parseMigrations(src(`{Name: "x"}`))
	if err == nil || !strings.Contains(err.Error(), "no Version") {
		t.Fatalf("err = %v", err)
	}
}

func TestParse_RejectsMissingList(t *testing.T) {
	_, err := parseMigrations([]byte("package db\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

// The real file still has the shape the parser expects.
func TestParse_RealMigrationsFile(t *testing.T) {
	b, err := readSource("", "../../"+defaultFile)
	if err != nil {
		t.Fatal(err)
	}
	ms := mustParse(t, b)
	if len(ms) < 40 {
		t.Fatalf("parsed only %d migrations from %s", len(ms), defaultFile)
	}
	if p := check(ms, ms); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}
