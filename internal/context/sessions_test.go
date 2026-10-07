package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromptFiltering(t *testing.T) {
	dropCases := []string{
		"yes", "Yes", "OK", "okay", "yep", "sure", "proceed",
		"continue", "lgtm", "looks good", "thanks", "do it",
		"<USER_REQUEST>\nyes\n</USER_REQUEST>",
	}
	for _, c := range dropCases {
		if IsSubstantiveUserPrompt(c) {
			t.Errorf("expected %q to be dropped as non-substantive", c)
		}
	}

	keepCases := []string{
		"Implement circuit breaker and wire broadcast",
		"Make sure to keep zero external dependencies",
		"That broke the test, fix the import in breaker.go",
		"<USER_REQUEST>\nShip phases 3 through 5 please\n</USER_REQUEST>",
	}
	for _, c := range keepCases {
		if !IsSubstantiveUserPrompt(c) {
			t.Errorf("expected %q to be kept as substantive directive", c)
		}
	}
}

func TestClaudeSessionParsing(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "claude-session-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	jsonlPath := filepath.Join(tempDir, "sess-claude-test-123.jsonl")
	content := `{"type":"attachment","snapshot":{"workingDirectory":"/Users/dev/myrepo"}}
{"type":"user","message":{"content":"Initial goal: build authentication API"}}
{"type":"assistant","message":{"content":[{"text":"I will inspect the models"}]}}
{"type":"user","message":{"content":"yes"}}
{"type":"user","message":{"content":"Use bcrypt for password hashing and avoid plain text"}}
{"type":"assistant","message":{"content":[{"text":"Password hashing updated successfully."}]}}
`
	if err := os.WriteFile(jsonlPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test jsonl: %v", err)
	}

	sess := parseClaudeSession(jsonlPath, time.Now())
	if sess == nil {
		t.Fatalf("expected parsed session, got nil")
	}

	if sess.ID != "sess-claude-test-123" {
		t.Errorf("unexpected ID: %s", sess.ID)
	}
	if sess.RepoPath != "/Users/dev/myrepo" {
		t.Errorf("unexpected RepoPath: %s", sess.RepoPath)
	}
	if !strings.Contains(sess.RootGoal, "Initial goal: build authentication API") {
		t.Errorf("unexpected RootGoal: %s", sess.RootGoal)
	}
	if len(sess.UserDirectives) != 2 {
		t.Fatalf("expected 2 substantive directives (excluding 'yes'), got %d: %v", len(sess.UserDirectives), sess.UserDirectives)
	}
	if !strings.Contains(sess.UserDirectives[1], "Use bcrypt for password hashing") {
		t.Errorf("missing directive: %v", sess.UserDirectives)
	}
	if !strings.Contains(sess.LastAssistant, "Password hashing updated successfully") {
		t.Errorf("unexpected LastAssistant: %s", sess.LastAssistant)
	}
}

func TestGenerateSessionHandoff(t *testing.T) {
	// GenerateSessionHandoff saves its manifest under ~/.staypoint/handoffs (STA-741).
	t.Setenv("HOME", t.TempDir())
	sess := &SessionInfo{
		ID:             "sess-test-handoff",
		AgentType:      "claude",
		RepoPath:       ".",
		UpdatedAt:      time.Now().Add(-15 * time.Minute),
		RootGoal:       "Create a high-speed circuit breaker",
		UserDirectives: []string{"Use pure Go and standard library", "Add macOS notification chime"},
		LastUserPrompt: "Hook up prompt hook to inject breaker warning",
		LastAssistant:  "All tests passing with 100% coverage.",
	}

	handoff, err := GenerateSessionHandoff(sess, nil)
	if err != nil {
		t.Fatalf("GenerateSessionHandoff error: %v", err)
	}

	// Verify the 5 anchors
	if !strings.Contains(handoff, "STAYPOINT CONTEXT HANDOFF") {
		t.Errorf("missing header")
	}
	if !strings.Contains(handoff, "Primary Goal") || !strings.Contains(handoff, "Create a high-speed circuit breaker") {
		t.Errorf("missing root goal anchor")
	}
	if !strings.Contains(handoff, "User Directives & Constraints Trail") {
		t.Errorf("missing user directives trail")
	}
	if !strings.Contains(handoff, "Use pure Go and standard library") {
		t.Errorf("missing directive 1")
	}
	if !strings.Contains(handoff, "Cutoff Point (Latest Turn)") {
		t.Errorf("missing cutoff anchor")
	}
	if !strings.Contains(handoff, "Working Tree Status") {
		t.Errorf("missing git tree status")
	}
	if !strings.Contains(handoff, "Immediate Next Action Directive") {
		t.Errorf("missing immediate next step directive")
	}

	// Verify /tmp/ai-handoff.md was written
	data, err := os.ReadFile("/tmp/ai-handoff.md")
	if err != nil || len(data) == 0 {
		t.Errorf("expected /tmp/ai-handoff.md to be written")
	}
}

func TestFindGitRoot(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "git-root-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	gitDir := filepath.Join(tempDir, ".git")
	_ = os.MkdirAll(gitDir, 0755)

	subDir := filepath.Join(tempDir, "pkg", "subpkg")
	_ = os.MkdirAll(subDir, 0755)

	filePath := filepath.Join(subDir, "main.go")
	_ = os.WriteFile(filePath, []byte("package main"), 0644)

	root := findGitRoot(filePath)
	if root != tempDir {
		t.Errorf("expected git root %s, got %s", tempDir, root)
	}

	rootFromDir := findGitRoot(subDir)
	if rootFromDir != tempDir {
		t.Errorf("expected git root %s from dir, got %s", tempDir, rootFromDir)
	}

	noRoot := findGitRoot("/tmp")
	if noRoot == "/tmp" {
		t.Errorf("expected empty string or non-git root for /tmp")
	}
}
