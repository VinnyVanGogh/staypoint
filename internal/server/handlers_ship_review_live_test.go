//go:build !windows

package server_test

// STA-727: live_credentials project flag.
//
// A project whose previews run against production credentials is flagged
// live_credentials on project_dev_configs. The flag is set only through the
// Board-gated PUT /api/project-dev-configs and every change is audited. On a
// live project:
//   - POST start-dev needs the Board session, a passkey assertion and
//     {"confirm_live": true}; the confirmed start is audited as
//     live_dev_start_confirmed. Non-live projects keep today's behaviour.
//   - an agent's PUT ship-review never auto-starts the dev server.
//   - GET ship-review exposes live_credentials so the card can show the banner.
// Approve, Send back and Reject stop the running dev server for the card,
// including any processes the dev command spawned.
//
// The flag is driven through the HTTP API only (never the Go struct field), so
// this file compiles before the feature exists.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// liveShipEnv is a server with a stub passkey verifier, one task with a real
// repo and a pending ship review card.
type liveShipEnv struct {
	db                    *sql.DB
	baseURL, token, board string
	taskID, repoDir       string
	client                *http.Client
}

func newLiveShipEnv(t *testing.T) *liveShipEnv {
	t.Helper()
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	return &liveShipEnv{db: database, baseURL: baseURL, token: token, board: boardToken,
		taskID: taskID, repoDir: repoDir, client: client}
}

func (e *liveShipEnv) cardURL() string {
	return e.baseURL + "/api/tasks/" + e.taskID + "/ship-review"
}

// putDevConfig sends the Board-gated partial update. fields must not include
// repo_path; it is always the task's repo.
func (e *liveShipEnv) putDevConfig(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"repo_path": e.repoDir}
	for k, v := range fields {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	resp, rb := shipDoReq(t, e.client, e.token, "PUT", e.baseURL+"/api/project-dev-configs", b, e.board, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Board PUT project-dev-configs %s: %d %s", b, resp.StatusCode, rb)
	}
	var out map[string]any
	if err := json.Unmarshal(rb, &out); err != nil {
		t.Fatalf("PUT project-dev-configs response: %v: %s", err, rb)
	}
	return out
}

// listedLiveFlag returns the live_credentials value GET /api/project-dev-configs
// reports for the task's repo, and whether the key was present at all.
func (e *liveShipEnv) listedLiveFlag(t *testing.T) (val any, present bool) {
	t.Helper()
	resp, rb := shipDoReq(t, e.client, e.token, "GET", e.baseURL+"/api/project-dev-configs", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET project-dev-configs: %d %s", resp.StatusCode, rb)
	}
	var out struct {
		Configs []map[string]any `json:"configs"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		t.Fatalf("GET project-dev-configs: %v: %s", err, rb)
	}
	for _, c := range out.Configs {
		if c["repo_path"] == e.repoDir {
			val, present = c["live_credentials"]
			return val, present
		}
	}
	t.Fatalf("no config for %s in %s", e.repoDir, rb)
	return nil, false
}

// stopDevOnCleanup stops whatever dev server the test started before the repo
// temp dir is removed (cleanups run last-registered first).
func (e *liveShipEnv) stopDevOnCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		card, err := shipreview.GetCard(e.db, e.taskID)
		if err == nil {
			shipreview.StopDevServer(e.db, card)
		}
	})
}

func (e *liveShipEnv) card(t *testing.T) *shipreview.Card {
	t.Helper()
	card, err := shipreview.GetCard(e.db, e.taskID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	return card
}

// assertDevNotStarted checks a refused start-dev left no trace of a dev server.
func (e *liveShipEnv) assertDevNotStarted(t *testing.T, step string) {
	t.Helper()
	card := e.card(t)
	if card.DevState != shipreview.DevStateIdle || card.DevPID != 0 {
		t.Errorf("%s: dev server started anyway (dev_state=%q dev_pid=%d)", step, card.DevState, card.DevPID)
	}
	devDir := filepath.Join(e.repoDir, ".worktrees", "devserver-"+e.taskID)
	if _, err := os.Stat(devDir); !os.IsNotExist(err) {
		t.Errorf("%s: devserver worktree %s exists (stat err=%v)", step, devDir, err)
	}
}

// boardAuditRows returns the decoded payloads of board_audit_log rows whose
// event_type, or payload "action", equals name. Newest first.
func boardAuditRows(t *testing.T, database *sql.DB, name string) []map[string]any {
	t.Helper()
	rows, err := database.Query(`SELECT event_type, COALESCE(payload, '') FROM board_audit_log ORDER BY id DESC`)
	if err != nil {
		t.Fatalf("query board_audit_log: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var ev, payload string
		if err := rows.Scan(&ev, &payload); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		_ = json.Unmarshal([]byte(payload), &m)
		if ev == name || m["action"] == name {
			out = append(out, m)
		}
	}
	return out
}

func errorCode(rb []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rb, &e)
	return e.Error
}

// pidAlive reports whether pid still exists (zombies count as gone once reaped).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ── Board PUT sets the flag, audited with old/new ──────────────────────────

func TestShipReviewLive_BoardPutSetsFlagAndAudits(t *testing.T) {
	e := newLiveShipEnv(t)

	// Agent token alone can never set the flag.
	b, _ := json.Marshal(map[string]any{"repo_path": e.repoDir, "live_credentials": true})
	resp, rb := shipDoReq(t, e.client, e.token, "PUT", e.baseURL+"/api/project-dev-configs", b)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent PUT live_credentials: want 403, got %d %s", resp.StatusCode, rb)
	}

	out := e.putDevConfig(t, map[string]any{
		"dev_command":      "exec sleep 60",
		"dev_url":          "http://127.0.0.1:3999",
		"live_credentials": true,
	})
	if out["live_credentials"] != true {
		t.Errorf("PUT response live_credentials = %v, want true (body %v)", out["live_credentials"], out)
	}
	if v, ok := e.listedLiveFlag(t); v != true {
		t.Errorf("GET project-dev-configs live_credentials = %v (present=%v), want true", v, ok)
	}
	rows := boardAuditRows(t, e.db, "dev_config_change")
	if len(rows) == 0 {
		t.Fatal("no dev_config_change audit row")
	}
	if rows[0]["old_live_credentials"] != false || rows[0]["new_live_credentials"] != true {
		t.Errorf("dev_config_change audit = %v, want old_live_credentials=false new_live_credentials=true", rows[0])
	}

	// Partial PUT without the field keeps the flag.
	e.putDevConfig(t, map[string]any{"dev_url": "http://127.0.0.1:3998"})
	if v, _ := e.listedLiveFlag(t); v != true {
		t.Errorf("after partial PUT without live_credentials: flag = %v, want true (kept)", v)
	}
	rows = boardAuditRows(t, e.db, "dev_config_change")
	if rows[0]["old_live_credentials"] != true || rows[0]["new_live_credentials"] != true {
		t.Errorf("partial PUT audit = %v, want old_live_credentials=true new_live_credentials=true", rows[0])
	}

	// Clearing it is a Board action too, audited the same way.
	e.putDevConfig(t, map[string]any{"live_credentials": false})
	if v, ok := e.listedLiveFlag(t); v != false || !ok {
		t.Errorf("after clearing: live_credentials = %v (present=%v), want false", v, ok)
	}
	rows = boardAuditRows(t, e.db, "dev_config_change")
	if rows[0]["old_live_credentials"] != true || rows[0]["new_live_credentials"] != false {
		t.Errorf("clear audit = %v, want old_live_credentials=true new_live_credentials=false", rows[0])
	}
}

// ── start-dev on a live project needs Board + passkey + confirm_live ───────

func TestShipReviewLive_StartDevGate(t *testing.T) {
	e := newLiveShipEnv(t)
	e.stopDevOnCleanup(t)
	e.putDevConfig(t, map[string]any{
		"dev_command":      "exec sleep 60",
		"dev_url":          "http://127.0.0.1:3999",
		"live_credentials": true,
	})
	startURL := e.cardURL() + "/start-dev"
	confirm := []byte(`{"confirm_live":true}`)

	// 1. Agent token, no Board session: 403 even with confirm_live.
	resp, rb := shipDoReq(t, e.client, e.token, "POST", startURL, confirm)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("no Board session: want 403, got %d %s", resp.StatusCode, rb)
	} else if code := errorCode(rb); code != "board_session_required" {
		t.Errorf("no Board session: error = %q, want board_session_required", code)
	}
	e.assertDevNotStarted(t, "no Board session")

	// 2. Board session but no passkey assertion: 403.
	resp, rb = shipDoReq(t, e.client, e.token, "POST", startURL, confirm, e.board)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Board session without assertion: want 403, got %d %s", resp.StatusCode, rb)
	} else if code := errorCode(rb); code != "board_passkey_assertion_required" {
		t.Errorf("Board session without assertion: error = %q, want board_passkey_assertion_required", code)
	}
	e.assertDevNotStarted(t, "no assertion")

	// 3. Board session + assertion, but no confirm_live: 409.
	for _, body := range [][]byte{nil, []byte(`{}`), []byte(`{"confirm_live":false}`)} {
		resp, rb = shipDoReq(t, e.client, e.token, "POST", startURL, body, e.board, "", "mock-assertion")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("body %q without confirm_live: want 409, got %d %s", body, resp.StatusCode, rb)
		} else if code := errorCode(rb); code != "live_confirmation_required" {
			t.Errorf("body %q: error = %q, want live_confirmation_required", body, code)
		}
		e.assertDevNotStarted(t, fmt.Sprintf("body %q", body))
	}
	if n := len(boardAuditRows(t, e.db, "live_dev_start_confirmed")); n != 0 {
		t.Errorf("refused starts wrote %d live_dev_start_confirmed audit rows, want 0", n)
	}

	// 4. Everything present: 202, and the confirmation is audited.
	resp, rb = shipDoReq(t, e.client, e.token, "POST", startURL, confirm, e.board, "", "mock-assertion")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("confirmed live start: want 202, got %d %s", resp.StatusCode, rb)
	}
	rows := boardAuditRows(t, e.db, "live_dev_start_confirmed")
	if len(rows) != 1 {
		t.Fatalf("want 1 live_dev_start_confirmed audit row, got %d", len(rows))
	}
	if rows[0]["task_id"] != e.taskID || rows[0]["repo_path"] != e.repoDir {
		t.Errorf("live_dev_start_confirmed audit = %v, want task_id=%s repo_path=%s", rows[0], e.taskID, e.repoDir)
	}
}

// ── non-live start-dev is unchanged ────────────────────────────────────────

func TestShipReviewLive_NonLiveStartDevUnchanged(t *testing.T) {
	e := newLiveShipEnv(t)
	e.stopDevOnCleanup(t)
	e.putDevConfig(t, map[string]any{"dev_command": "exec sleep 60", "dev_url": "http://127.0.0.1:3999"})

	resp, rb := shipDoReq(t, e.client, e.token, "POST", e.cardURL()+"/start-dev", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("non-live start-dev with agent token: want 202, got %d %s", resp.StatusCode, rb)
	}
	if n := len(boardAuditRows(t, e.db, "live_dev_start_confirmed")); n != 0 {
		t.Errorf("non-live start wrote %d live_dev_start_confirmed rows, want 0", n)
	}
}

// ── agent PUT ship-review never auto-starts a live project ─────────────────

func TestShipReviewLive_AgentUpsertDoesNotAutoStart(t *testing.T) {
	e := newLiveShipEnv(t)
	e.stopDevOnCleanup(t)
	e.putDevConfig(t, map[string]any{
		"dev_command":      "exec sleep 60",
		"dev_url":          "http://127.0.0.1:3999",
		"live_credentials": true,
	})

	// Agent refreshes the card: with a saved dev_command this would auto-start.
	b, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, e.client, e.token, "PUT", e.cardURL(), b)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent PUT ship-review: %d %s", resp.StatusCode, rb)
	}
	e.assertDevNotStarted(t, "agent PUT ship-review on live project")
}

// ── GET card exposes live_credentials ─────────────────────────────────────

func TestShipReviewLive_GetCardExposesFlag(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run("live="+strconv.FormatBool(live), func(t *testing.T) {
			e := newLiveShipEnv(t)
			e.putDevConfig(t, map[string]any{"dev_url": "http://127.0.0.1:3999", "live_credentials": live})

			resp, rb := shipDoReq(t, e.client, e.token, "GET", e.cardURL(), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET card: %d %s", resp.StatusCode, rb)
			}
			var card map[string]any
			if err := json.Unmarshal(rb, &card); err != nil {
				t.Fatalf("GET card: %v", err)
			}
			v, ok := card["live_credentials"]
			if !ok || v != live {
				t.Errorf("card live_credentials = %v (present=%v), want %v", v, ok, live)
			}
		})
	}
}

// ── Approve / Send back / Reject stop the dev server and its children ─────

func TestShipReviewLive_DecisionKillsDevServerTree(t *testing.T) {
	cases := []struct {
		action string
		body   []byte
	}{
		{"approve", nil},
		{"send-back", []byte(`{"comment":"rework"}`)},
		{"reject", []byte(`{"comment":"no"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			e := newLiveShipEnv(t)
			pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
			// The shell stays as the dev server's direct child; the backgrounded
			// sleep is a grandchild that killing only the shell would orphan.
			e.putDevConfig(t, map[string]any{
				"dev_command": fmt.Sprintf("sleep 300 & echo $! > '%s'; wait", pidFile),
				"dev_url":     "http://127.0.0.1:3999",
			})
			e.stopDevOnCleanup(t)

			resp, rb := shipDoReq(t, e.client, e.token, "POST", e.cardURL()+"/start-dev", nil)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("start-dev: %d %s", resp.StatusCode, rb)
			}

			var childPID, grandPID int
			if !waitUntil(20*time.Second, func() bool {
				raw, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				grandPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
				childPID = e.card(t).DevPID
				return grandPID > 0 && childPID > 0
			}) {
				t.Fatalf("dev server never came up (dev_pid=%d grandchild=%d, card=%+v)", childPID, grandPID, e.card(t))
			}
			t.Cleanup(func() {
				if pidAlive(grandPID) {
					_ = syscall.Kill(grandPID, syscall.SIGKILL)
				}
			})
			if !pidAlive(childPID) || !pidAlive(grandPID) {
				t.Fatalf("dev server not running before %s (child alive=%v grandchild alive=%v)",
					tc.action, pidAlive(childPID), pidAlive(grandPID))
			}

			resp, rb = shipDoReq(t, e.client, e.token, "POST", e.cardURL()+"/"+tc.action, tc.body, e.board, "", "mock-assertion")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", tc.action, resp.StatusCode, rb)
			}

			if !waitUntil(5*time.Second, func() bool { return !pidAlive(childPID) }) {
				t.Errorf("%s: dev server process %d still running", tc.action, childPID)
			}
			if !waitUntil(5*time.Second, func() bool { return !pidAlive(grandPID) }) {
				t.Errorf("%s: dev server grandchild %d still running", tc.action, grandPID)
			}
			if card := e.card(t); card.DevPID != 0 || card.DevState != shipreview.DevStateIdle {
				t.Errorf("%s: card dev_pid=%d dev_state=%q, want 0 and idle", tc.action, card.DevPID, card.DevState)
			}
		})
	}
}
