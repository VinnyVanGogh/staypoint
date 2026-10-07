package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePaperclip points the fleet aggregator at an empty local stand-in so
// tests never reach the live Paperclip control plane.
func fakePaperclip(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("PAPERCLIP_API_URL", ts.URL)
	t.Setenv("PAPERCLIP_API_KEY", "")
}

func getHiddenJSON(t *testing.T, url, token string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

func seedHiddenTasks(t *testing.T, exec func(string, ...any) error) {
	t.Helper()
	if err := exec(`INSERT INTO tasks (id, name, repo_path, git_branch, organization, project, status, account_role, execution_stage, spent_usd, origin) VALUES
		('n1', 'native task', '/repo/x', 'main', 'StayPoint', 'Core', 'active', 'personal', 'todo', 1.0, 'native'),
		('l1', 'legacy task', '/repo/x', 'main', 'StayPoint', 'Old', 'active', 'personal', 'todo', 10.0, 'legacy'),
		('a1', 'archived import', '/repo/x', 'main', 'StayPoint', 'Old', 'done', 'personal', 'done', 100.0, 'paperclip_import')`); err != nil {
		t.Fatal(err)
	}
}

func TestFleetOverview_HidesLegacyAndArchiveUnlessIncluded(t *testing.T) {
	fakePaperclip(t)
	database := setupTestDB(t)
	seedHiddenTasks(t, func(q string, a ...any) error { _, err := database.Exec(q, a...); return err })
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	type overview struct {
		Tasks []struct {
			ID     string `json:"id"`
			Origin string `json:"origin"`
		} `json:"tasks"`
		GlobalTasks struct {
			Total int `json:"total"`
		} `json:"global_tasks"`
	}
	ids := func(o overview) map[string]string {
		m := map[string]string{}
		for _, t := range o.Tasks {
			m[t.ID] = t.Origin
		}
		return m
	}

	var def overview
	getHiddenJSON(t, base+"/api/fleet/overview", token, &def)
	d := ids(def)
	if d["n1"] != "native" || d["l1"] != "" || d["a1"] != "" {
		t.Errorf("default overview tasks = %v, want only n1", d)
	}
	if _, ok := d["l1"]; ok {
		t.Error("legacy task in default overview")
	}
	if _, ok := d["a1"]; ok {
		t.Error("archived import in default overview")
	}

	// The include flag has its own cache: asking for it right after the
	// default must not be served the cached default body.
	for _, q := range []string{"?include_legacy=1", "?include_archive=true"} {
		var all overview
		getHiddenJSON(t, base+"/api/fleet/overview"+q, token, &all)
		a := ids(all)
		if a["l1"] != "legacy" || a["a1"] != "paperclip_import" || a["n1"] != "native" {
			t.Errorf("%s overview tasks = %v", q, a)
		}
		if all.GlobalTasks.Total != def.GlobalTasks.Total+2 {
			t.Errorf("%s total %d, default %d", q, all.GlobalTasks.Total, def.GlobalTasks.Total)
		}
	}
	// And the default is still the hidden body after the include request.
	var again overview
	getHiddenJSON(t, base+"/api/fleet/overview", token, &again)
	if _, ok := ids(again)["l1"]; ok {
		t.Error("default overview served the include_legacy cache")
	}
}

func TestTelemetry_TaskSpendHidesLegacyAndArchive(t *testing.T) {
	fakePaperclip(t)
	database := setupTestDB(t)
	seedHiddenTasks(t, func(q string, a ...any) error { _, err := database.Exec(q, a...); return err })
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	type telemetry struct {
		TaskSpend struct {
			TotalSpentUSD float64 `json:"total_spent_usd"`
			ActiveTasks   int     `json:"active_tasks"`
			DoneTasks     int     `json:"done_tasks"`
		} `json:"task_spend"`
	}
	var def, all telemetry
	getHiddenJSON(t, base+"/api/telemetry", token, &def)
	if def.TaskSpend.TotalSpentUSD != 1.0 || def.TaskSpend.ActiveTasks != 1 || def.TaskSpend.DoneTasks != 0 {
		t.Errorf("default task_spend = %+v, want native only", def.TaskSpend)
	}
	getHiddenJSON(t, base+"/api/telemetry?include_legacy=1", token, &all)
	if all.TaskSpend.TotalSpentUSD != 111.0 || all.TaskSpend.ActiveTasks != 2 || all.TaskSpend.DoneTasks != 1 {
		t.Errorf("include_legacy task_spend = %+v", all.TaskSpend)
	}
}
