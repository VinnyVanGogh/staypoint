//go:build !windows

package server_test

import (
	gocontext "context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// STA-767: the live_credentials gate looked up project_dev_configs by the
// exact task repo_path string. A task whose repo_path reaches the live repo
// through an alias (symlink, trailing "/", "/./", different case on a
// case-insensitive volume) missed the live row, skipped the Board gate, and on
// a Supabase repo the auto-propose path wrote a non-live row for the alias and
// started a dev server in a worktree of the live repo.
func TestShipReviewLive_AliasedRepoPathStillGated(t *testing.T) {
	cases := []struct {
		name  string
		alias func(t *testing.T, repoDir string) string
	}{
		{"symlink", func(t *testing.T, repoDir string) string {
			link := filepath.Join(t.TempDir(), "live-link")
			if err := os.Symlink(repoDir, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"trailing_slash", func(_ *testing.T, repoDir string) string { return repoDir + "/" }},
		{"dot_segment", func(_ *testing.T, repoDir string) string {
			return filepath.Dir(repoDir) + "/./" + filepath.Base(repoDir)
		}},
		{"case_variant", func(t *testing.T, repoDir string) string {
			variant := filepath.Join(filepath.Dir(repoDir), strings.ToUpper(filepath.Base(repoDir)))
			a, errA := os.Stat(repoDir)
			b, errB := os.Stat(variant)
			if errA != nil || errB != nil || !os.SameFile(a, b) {
				t.Skip("case-sensitive volume: a case variant is a different path")
			}
			return variant
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDockerOnPath(t)
			e := newLiveShipEnv(t)
			e.stopDevOnCleanup(t)
			e.putDevConfig(t, map[string]any{"live_credentials": true})
			writeSupabaseFiles(t, e.repoDir)

			alias := tc.alias(t, e.repoDir)
			aliasTaskID := createAliasShipTask(t, e, alias)
			t.Cleanup(func() {
				if card, err := shipreview.GetCard(e.db, aliasTaskID); err == nil {
					shipreview.StopDevServer(e.db, card)
				}
			})

			startURL := e.baseURL + "/api/tasks/" + aliasTaskID + "/ship-review/start-dev"
			resp, rb := shipDoReq(t, e.client, e.token, "POST", startURL, []byte(`{"confirm_live":true}`))
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("agent start-dev via %s alias %q: want 403, got %d %s", tc.name, alias, resp.StatusCode, rb)
			} else if code := errorCode(rb); code != "board_session_required" {
				t.Errorf("agent start-dev via %s alias: error = %q, want board_session_required", tc.name, code)
			}

			if dirs, _ := filepath.Glob(filepath.Join(e.repoDir, ".worktrees", "devserver-*")); len(dirs) != 0 {
				t.Errorf("devserver worktree created through the alias: %v", dirs)
			}
			card, err := shipreview.GetCard(e.db, aliasTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if card.DevState != shipreview.DevStateIdle || card.DevPID != 0 {
				t.Errorf("dev server started through the alias (dev_state=%q dev_pid=%d)", card.DevState, card.DevPID)
			}
			cfgs, err := shipreview.ListProjectDevConfigs(e.db)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cfgs {
				if !c.LiveCredentials {
					t.Errorf("non-live dev config row written for %q", c.RepoPath)
				}
			}
		})
	}
}

// stubDockerOnPath puts a docker that reports "running" with no containers
// first on PATH. If a test regresses into Supabase setup, setup then stops at
// the missing .env.local instead of opening Docker Desktop or stopping the
// host's real Supabase containers.
func stubDockerOnPath(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeSupabaseFiles makes repoDir look like a Supabase project with a
// package.json, so a config proposal would get dev_command "bun run dev".
func writeSupabaseFiles(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, "supabase"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "supabase", "config.toml"), []byte("project_id = \"live\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "package.json"), []byte(`{"scripts":{"dev":"exit 0"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// createAliasShipTask creates an agent task whose repo_path is alias, gives it
// a staypoint/<id> branch in the live repo and a pending ship review card.
func createAliasShipTask(t *testing.T, e *liveShipEnv, alias string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":         "alias-task",
		"repo_path":    alias,
		"git_branch":   "main",
		"organization": "STA",
		"project":      "ship-review-test",
	})
	resp, rb := shipDoReq(t, e.client, e.token, "POST", e.baseURL+"/api/tasks", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create alias task: %d %s", resp.StatusCode, rb)
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rb, &task); err != nil || task.ID == "" {
		t.Fatalf("create alias task: no id in %s", rb)
	}
	// The task branch carries one commit on top of main, its recorded base
	// (STA-774: a card needs a verified base and something to review).
	gitE := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", e.repoDir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mainTip := gitE("rev-parse", "main")
	if err := workspace.RecordTaskBase(gocontext.Background(), e.db, e.repoDir, task.ID, mainTip); err != nil {
		t.Fatalf("RecordTaskBase: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "alias-wt")
	gitE("worktree", "add", "-q", "-b", "staypoint/"+task.ID, wt, mainTip)
	if err := os.WriteFile(filepath.Join(wt, "alias-task.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitE("-C", wt, "add", "alias-task.txt")
	gitE("-C", wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "alias task work")
	gitE("worktree", "remove", "--force", wt)
	upsert, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb = shipDoReq(t, e.client, e.token, "PUT", e.baseURL+"/api/tasks/"+task.ID+"/ship-review", upsert)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("alias UpsertCard: %d %s", resp.StatusCode, rb)
	}
	return task.ID
}

// STA-798: a Board-confirmed start-dev for a task whose repo_path is a symlink
// to a live Supabase repo with no dev_command auto-proposes a config. The
// proposal must update the live row it matched, keyed by the live repo's own
// path, not add a second row keyed by the alias.
func TestShipReviewLive_AliasSupabaseAutoProposeKeepsLiveRowKey(t *testing.T) {
	e := newLiveShipEnv(t)
	e.putDevConfig(t, map[string]any{"live_credentials": true})
	// supabase/config.toml but no package.json: the proposal has an empty
	// dev_command, so start-dev ends in 409 after the upsert and never starts
	// a dev server.
	if err := os.MkdirAll(filepath.Join(e.repoDir, "supabase"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repoDir, "supabase", "config.toml"), []byte("project_id = \"live\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(t.TempDir(), "live-link")
	if err := os.Symlink(e.repoDir, link); err != nil {
		t.Fatal(err)
	}
	aliasTaskID := createAliasShipTask(t, e, link)

	startURL := e.baseURL + "/api/tasks/" + aliasTaskID + "/ship-review/start-dev"
	resp, rb := shipDoReq(t, e.client, e.token, "POST", startURL, []byte(`{"confirm_live":true}`), e.board, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("confirmed Board start-dev via symlink: want 409 (proposal has no dev_command), got %d %s", resp.StatusCode, rb)
	}
	if n := len(boardAuditRows(t, e.db, "live_dev_start_confirmed")); n != 1 {
		t.Fatalf("want 1 live_dev_start_confirmed audit row (the Board gate ran), got %d", n)
	}

	cfgs, err := shipreview.ListProjectDevConfigs(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 1 || cfgs[0].RepoPath != e.repoDir {
		keys := make([]string, len(cfgs))
		for i, c := range cfgs {
			keys[i] = c.RepoPath
		}
		t.Fatalf("dev config rows after auto-propose via symlink = %q, want exactly [%q]", keys, e.repoDir)
	}
	if !cfgs[0].SupabaseEnabled || !cfgs[0].LiveCredentials {
		t.Errorf("live row after auto-propose: supabase_enabled=%v live_credentials=%v, want the proposal written with the live flag kept", cfgs[0].SupabaseEnabled, cfgs[0].LiveCredentials)
	}
}
