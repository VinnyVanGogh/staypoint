package router

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchRepoPath(t *testing.T) {
	tests := []struct {
		name        string
		dir         string
		recRepoPath string
		recRepo     string
		want        bool
	}{
		{
			name:        "exact repo_path match",
			dir:         "/home/user/dev/project",
			recRepoPath: "/home/user/dev/project",
			recRepo:     "project",
			want:        true,
		},
		{
			name:        "subdirectory in repo_path",
			dir:         "/home/user/dev/project/sub/pkg",
			recRepoPath: "/home/user/dev/project",
			recRepo:     "project",
			want:        true,
		},
		{
			name:        "false prefix does not match",
			dir:         "/home/user/dev/project-fork",
			recRepoPath: "/home/user/dev/project",
			recRepo:     "project",
			want:        false,
		},
		{
			name:        "mismatched repo_path does not fall through to basename",
			dir:         "/work/project",
			recRepoPath: "/personal/project",
			recRepo:     "project",
			want:        false,
		},
		{
			name:        "empty repo_path falls back to repo basename",
			dir:         "/home/user/dev/project",
			recRepoPath: "",
			recRepo:     "project",
			want:        true,
		},
		{
			name:        "empty repo_path and empty repo never matches",
			dir:         "/home/user/dev/project",
			recRepoPath: "",
			recRepo:     "",
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchRepoPath(tc.dir, tc.recRepoPath, tc.recRepo)
			if got != tc.want {
				t.Fatalf("matchRepoPath(%q, %q, %q) = %v; want %v", tc.dir, tc.recRepoPath, tc.recRepo, got, tc.want)
			}
		})
	}
}

func TestGetPendingReviewFromDir(t *testing.T) {
	tempDir := t.TempDir()

	// Write an older review
	oldRev := map[string]interface{}{
		"repo":      "bassline",
		"repo_path": "/Users/vincevasile/Documents/dev/bassline",
		"sha":       "1111111aaaa",
		"severity":  1,
	}
	oldData, _ := json.Marshal(oldRev)
	oldPath := filepath.Join(tempDir, "review-old.json")
	if err := os.WriteFile(oldPath, oldData, 0644); err != nil {
		t.Fatal(err)
	}

	// Set older mtime
	olderTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(oldPath, olderTime, olderTime)

	// Write a newer review
	newRev := map[string]interface{}{
		"repo":      "bassline",
		"repo_path": "/Users/vincevasile/Documents/dev/bassline",
		"sha":       "2222222bbbb",
		"severity":  2,
	}
	newData, _ := json.Marshal(newRev)
	newPath := filepath.Join(tempDir, "review-new.json")
	if err := os.WriteFile(newPath, newData, 0644); err != nil {
		t.Fatal(err)
	}
	newerTime := time.Now()
	_ = os.Chtimes(newPath, newerTime, newerTime)

	// Write an unrelated repo review
	otherRev := map[string]interface{}{
		"repo":      "other-project",
		"repo_path": "/Users/vincevasile/Documents/dev/other-project",
		"sha":       "3333333cccc",
		"verdict":   "FAIL",
	}
	otherData, _ := json.Marshal(otherRev)
	otherPath := filepath.Join(tempDir, "review-other.json")
	if err := os.WriteFile(otherPath, otherData, 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(otherPath, newerTime, newerTime)

	// Query for bassline should pick newest bassline review
	rev := getPendingReviewFromDir("/Users/vincevasile/Documents/dev/bassline", tempDir)
	if rev == nil {
		t.Fatal("expected pending review, got nil")
	}

	if rev.SHA != "2222222" {
		t.Fatalf("expected SHA 2222222, got %s", rev.SHA)
	}
	if rev.Verdict != "WARN" {
		t.Fatalf("expected verdict WARN, got %s", rev.Verdict)
	}

	// Query for unreviewed repo should return nil
	revUnrelated := getPendingReviewFromDir("/Users/vincevasile/Documents/dev/not-here", tempDir)
	if revUnrelated != nil {
		t.Fatalf("expected nil for unrelated repo, got %+v", revUnrelated)
	}
}

func TestRenderStatuslineWithReviewBadge(t *testing.T) {
	// RenderStatusline opens config.DefaultConfig().DBPath; without a temp
	// HOME that is the live ~/.staypoint/staypoint.db, which it migrates.
	home := t.TempDir()
	t.Setenv("HOME", home)
	pendingDir := filepath.Join(home, ".claude", "reviews", "pending")
	_ = os.MkdirAll(pendingDir, 0755)

	testPath := filepath.Join(pendingDir, "test-render-badge.json")
	revData := []byte(`{"repo":"agent-mesh","sha":"abc1234","verdict":"WARN"}`)
	if err := os.WriteFile(testPath, revData, 0644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testPath)

	var buf bytes.Buffer
	input := `{"cwd": "/Users/vincevasile/Documents/dev/agent-mesh", "vim_mode": "NORMAL"}`
	if err := RenderStatusline(&buf, strings.NewReader(input)); err != nil {
		t.Fatalf("RenderStatusline failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "agent-mesh") {
		t.Fatalf("expected agent-mesh in output, got:\n%s", out)
	}
	if !strings.Contains(out, "REVIEW: WARN (abc1234)") {
		t.Fatalf("expected review badge in statusline output, got:\n%s", out)
	}
}

// The launch banner and the launch are one routing answer: the plan line and
// model badge show the decision's tool, never a hard-coded Gemini route.
func TestRenderStatuslineFor_PlanLineIsTheDecision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	cases := []struct {
		name     string
		decision *RouteDecision
		want     []string
	}{
		{"routed claude", &RouteDecision{Tool: "claude", Model: "claude-opus-5", AccountRole: "personal"},
			[]string{"claude-opus-5 (claude)", "seat: personal"}},
		{"forced agy", &RouteDecision{Tool: "agy", Model: "gemini-3.8-flash-high"},
			[]string{"gemini-3.8-flash-high (agy)"}},
		{"waiting", &RouteDecision{Tool: "claude", Model: "claude-opus-5", AccountRole: "personal", Waiting: true},
			[]string{"claude-opus-5 (claude)", "waiting for a Claude seat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := RenderStatuslineFor(&buf, nil, tc.decision); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("banner missing %q:\n%s", w, out)
				}
			}
			if tc.decision.Tool == "claude" && strings.Contains(strings.ToLower(out), "gemini") {
				t.Errorf("claude route but banner mentions gemini:\n%s", out)
			}
		})
	}
}

// With no decision the statusline routes the directory itself, so the plan
// line still matches what Route would launch (Claude, GeminiCodeForbidden).
func TestRenderStatusline_PlanLineRoutesWhenNoDecision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	var buf bytes.Buffer
	if err := RenderStatusline(&buf, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "(claude)") || strings.Contains(out, "(agy)") {
		t.Fatalf("plan line should be the routed claude launch:\n%s", out)
	}
}

func TestPlanLine_NilOrEmptyDecision(t *testing.T) {
	if got := planLine(nil); got != "" {
		t.Errorf("planLine(nil) = %q", got)
	}
	if got := planLine(&RouteDecision{}); got != "" {
		t.Errorf("planLine(empty) = %q", got)
	}
}
