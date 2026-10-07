package main

import (
	"slices"
	"testing"
)

func TestResolveClaudeAccount(t *testing.T) {
	cases := []struct {
		name                   string
		work, personal, isWork bool
		want                   claudeAccount
	}{
		{"personal repo defaults to personal", false, false, false, claudeAccountPersonal},
		{"work repo defaults to work", false, false, true, claudeAccountWork},
		{"--work in personal repo", true, false, false, claudeAccountWork},
		{"--personal in work repo", false, true, true, claudeAccountPersonal},
		{"--work wins over --personal", true, true, false, claudeAccountWork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveClaudeAccount(c.work, c.personal, c.isWork); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestClaudeAccountEnv_WorkSetsIsolatedDir(t *testing.T) {
	env := claudeAccountEnv([]string{"PATH=/bin"}, claudeAccountWork, "/Users/me")
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/Users/me/.claude-work") {
		t.Fatalf("work env missing absolute CLAUDE_CONFIG_DIR: %v", env)
	}
	if !slices.Contains(env, "PATH=/bin") {
		t.Fatalf("dropped unrelated vars: %v", env)
	}
}

func TestClaudeAccountEnv_PersonalUsesSharedDefault(t *testing.T) {
	env := claudeAccountEnv([]string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/Users/me/.claude-work"}, claudeAccountPersonal, "/Users/me")
	for _, kv := range env {
		if len(kv) >= 18 && kv[:18] == "CLAUDE_CONFIG_DIR=" {
			t.Fatalf("personal must unset inherited CLAUDE_CONFIG_DIR, got %q", kv)
		}
	}
}

func TestClaudeAccountEnv_WorkReplacesInheritedDir(t *testing.T) {
	env := claudeAccountEnv([]string{"CLAUDE_CONFIG_DIR=/elsewhere"}, claudeAccountWork, "/Users/me")
	n := 0
	for _, kv := range env {
		if len(kv) >= 18 && kv[:18] == "CLAUDE_CONFIG_DIR=" {
			n++
			if kv != "CLAUDE_CONFIG_DIR=/Users/me/.claude-work" {
				t.Fatalf("got %q", kv)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one CLAUDE_CONFIG_DIR, got %d", n)
	}
}

func TestExtractPassthroughArgs_StripsAccountFlags(t *testing.T) {
	got := extractPassthroughArgs([]string{"--claude", "--work", "--personal", "-p", "hi"})
	want := []string{"-p", "hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
