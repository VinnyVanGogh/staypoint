package fleet

import (
	"context"
	"testing"
	"time"
)

// Legacy tasks and archived Paperclip imports are left out of the fleet
// overview by default: tasks, org task lists, counts, project lists and spend.
// GatherWith(IncludeHidden) keeps them.
func TestGather_HidesLegacyAndArchiveByDefault(t *testing.T) {
	testDB := setupTestDB(t)
	defer testDB.Close()

	if _, err := testDB.Exec(`
		INSERT INTO tasks (id, name, repo_path, organization, project, status, execution_stage, is_blocked, spent_usd, spent_tokens, origin)
		VALUES
		('legacy-1', 'Old legacy task', '/repo/sta', 'StayPoint', 'LegacyOnly', 'active', 'todo', 0, 100.0, 1000, 'legacy'),
		('arch-1', 'Archived import', '/repo/sta', 'StayPoint', 'ArchiveOnly', 'done', 'done', 0, 50.0, 500, 'paperclip_import'),
		('arch-2', 'Cancelled import', '/repo/sta', 'Archive Co', 'ArchiveOnly', 'active', 'cancelled', 0, 0, 0, 'paperclip_import'),
		('open-imp', 'Open import', '/repo/sta', 'StayPoint', 'Core', 'active', 'todo', 0, 0, 0, 'paperclip_import');
	`); err != nil {
		t.Fatal(err)
	}

	agg := &Aggregator{DB: testDB, Now: func() time.Time { return time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC) }}

	def, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all, err := agg.GatherWith(context.Background(), GatherOptions{IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}

	ids := func(o *FleetOverview) map[string]string {
		m := map[string]string{}
		for _, ti := range o.Tasks {
			m[ti.ID] = ti.Origin
		}
		return m
	}
	org := func(o *FleetOverview, name string) *OrgFleetSummary {
		for i := range o.Organizations {
			if o.Organizations[i].Name == name {
				return &o.Organizations[i]
			}
		}
		return nil
	}
	hasProject := func(s *OrgFleetSummary, p string) bool {
		for _, x := range s.Projects {
			if x == p {
				return true
			}
		}
		return false
	}

	d := ids(def)
	for _, hidden := range []string{"legacy-1", "arch-1", "arch-2"} {
		if _, ok := d[hidden]; ok {
			t.Errorf("default overview lists hidden task %s", hidden)
		}
	}
	if d["open-imp"] != "paperclip_import" {
		t.Errorf("open import missing or origin wrong: %q", d["open-imp"])
	}
	if d["task-1"] != "native" {
		t.Errorf("native task origin = %q, want native", d["task-1"])
	}
	a := ids(all)
	for _, hidden := range []string{"legacy-1", "arch-1", "arch-2"} {
		if _, ok := a[hidden]; !ok {
			t.Errorf("include-hidden overview missing %s", hidden)
		}
	}
	if a["legacy-1"] != "legacy" {
		t.Errorf("legacy origin = %q", a["legacy-1"])
	}

	if got, want := all.GlobalTasks.Total-def.GlobalTasks.Total, 3; got != want {
		t.Errorf("hidden tasks counted in default totals: diff %d, want %d", got, want)
	}
	sp := org(def, "StayPoint")
	spAll := org(all, "StayPoint")
	if sp == nil || spAll == nil {
		t.Fatal("StayPoint org missing")
	}
	if spAll.TaskCounts.Total-sp.TaskCounts.Total != 2 {
		t.Errorf("StayPoint counts: default %d, all %d", sp.TaskCounts.Total, spAll.TaskCounts.Total)
	}
	if hasProject(sp, "LegacyOnly") || hasProject(sp, "ArchiveOnly") {
		t.Errorf("default StayPoint projects include hidden-only projects: %v", sp.Projects)
	}
	if !hasProject(spAll, "LegacyOnly") || !hasProject(spAll, "ArchiveOnly") {
		t.Errorf("include-hidden StayPoint projects: %v", spAll.Projects)
	}
	if spAll.SpentUSD-sp.SpentUSD < 149.9 {
		t.Errorf("hidden spend counted by default: default %.2f, all %.2f", sp.SpentUSD, spAll.SpentUSD)
	}
	// An org whose only tasks are archived does not appear by default.
	if org(def, "Archive Co") != nil {
		t.Error("archive-only org shown in default overview")
	}
	if org(all, "Archive Co") == nil {
		t.Error("archive-only org missing with IncludeHidden")
	}
}
