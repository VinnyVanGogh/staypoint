package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// STA-838 CLI: task create --kind/--provider and task set-provider.
func TestTaskProviderCLI(t *testing.T) {
	work, personal := trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	var out bytes.Buffer
	choice, err := router.ValidateTaskChoice("docs", true, "gemini", "flash")
	if err != nil {
		t.Fatal(err)
	}
	if err := createLocalOrgTaskWith(&out, conn, "Managed Solution", "", "Write docs", work, "", trackgate.ClientClaude, 0, 0, "docs", choice); err != nil {
		t.Fatal(err)
	}
	tasks, _ := meshContext.ListTasks(conn, true)
	if len(tasks) != 1 || tasks[0].Provider != "gemini" || tasks[0].ModelOverride != "gemini-3.8-flash-high" {
		t.Fatalf("created task = %+v", tasks)
	}
	docsID := tasks[0].ID
	if got, _ := meshContext.GetTask(conn, docsID); got.WorkKind != "docs" {
		t.Errorf("work_kind = %q", got.WorkKind)
	}

	// Work-repo coding task: gemini refused, claude accepted.
	code, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: "fix", RepoPath: work, WorkKind: "coding"})
	if err != nil {
		t.Fatal(err)
	}
	if err := setTaskProvider(&out, conn, code.ID, "gemini", ""); err == nil || !strings.Contains(err.Error(), "Gemini never writes code") {
		t.Fatalf("gemini on work coding: err = %v", err)
	}
	if err := setTaskProvider(&out, conn, code.ID, "claude", "opus"); err != nil {
		t.Fatal(err)
	}

	// Personal-repo coding task: gemini accepted, with the Touch ID note.
	pcode, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{Name: "hack", RepoPath: personal, WorkKind: "coding"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := setTaskProvider(&out, conn, pcode.ID, "gemini", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Touch ID") {
		t.Errorf("set-provider output should explain the approval: %s", out.String())
	}
	// Restore the default.
	if err := setTaskProvider(&out, conn, pcode.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := meshContext.GetTask(conn, pcode.ID); got.Provider != "" || got.ModelOverride != "" {
		t.Errorf("default not restored: %+v", got)
	}
}

// Interactive agy in a personal git repo: code writes are denied with the
// exact approval command; doc writes pass.
func TestAgyPersonalRepoCodeWriteNeedsApproval(t *testing.T) {
	_, personal := trackingEnv(t)
	if err := os.Mkdir(filepath.Join(personal, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := func(file string) []byte {
		args, _ := json.Marshal(map[string]string{"TargetFile": file, "Cwd": personal})
		raw, _ := json.Marshal(map[string]any{
			"toolCall":       map[string]any{"name": "write_to_file", "args": json.RawMessage(args)},
			"conversationId": "conv-42",
		})
		return raw
	}
	_, o, blocked := runTrackingGate(payload(filepath.Join(personal, "main.go")), "gemini", noEnv, productionTrackingGate())
	if !blocked || !strings.Contains(o, "staypoint gate gemini-code --session conv-42") {
		t.Fatalf("agy code write in personal repo: blocked=%v %s", blocked, o)
	}
	if _, o, blocked := runTrackingGate(payload(filepath.Join(personal, "NOTES.md")), "gemini", noEnv, productionTrackingGate()); blocked {
		t.Errorf("agy doc write blocked: %s", o)
	}
}

// Smart launch never execs agy unless the route named it (only --gemini,
// the agy wrapper or agy --force do), and the router never names it.
func TestSmartLaunchNeverDefaultsToAgy(t *testing.T) {
	for _, tool := range []string{"", "claude", "ssh", "gemini", "bogus"} {
		if got := smartLaunchBinary(tool); got != "claude" {
			t.Errorf("smartLaunchBinary(%q) = %q, want claude", tool, got)
		}
	}
	if smartLaunchBinary("agy") != "agy" {
		t.Error("an explicit agy route must launch agy")
	}
	_, personal := trackingEnv(t)
	gemRich := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolGeminiNative:   {FiveHour: router.QuotaWindow{RemainingPct: 100}, Weekly: router.QuotaWindow{RemainingPct: 100}},
		router.PoolPersonalClaude: {IsLocked: true, LockoutReason: "5h"},
	}}
	for _, pref := range []string{"auto", "agy", "gemini", "claude"} {
		d, err := router.Route(t.Context(), personal, gemRich, router.RouteOptions{PreferredPersonalTool: pref, LastUsedTool: "agy"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Tool == "agy" || smartLaunchBinary(d.Tool) != "claude" {
			t.Errorf("pref %s: routed %s/%s", pref, d.Tool, d.Target)
		}
	}
}
