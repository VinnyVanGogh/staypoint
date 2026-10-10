package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
)

// Board 2026-10-09: a task in a configured work org routes as work (work
// seat, no Gemini code) even when its repo path is not recognised as a work
// repo. Unconfigured, the same task routes as personal.
func TestResolveTaskRoute_WorkOrgRoutesAsWork(t *testing.T) {
	t.Cleanup(func() { workorgs.Set(nil) })
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	repo := personalRepo(t)
	if _, err := store.DB().Exec(`INSERT INTO tasks (id, name, repo_path, organization, status, work_kind, provider)
		VALUES ('pp-1', 'mail router', ?, 'Power Platform', 'active', 'coding', 'gemini')`, repo); err != nil {
		t.Fatal(err)
	}

	route := resolveTaskRoute(store.DB(), "pp-1", repo, openPacer(), time.Now())
	if route.IsWork {
		t.Fatal("unconfigured Power Platform routed as work")
	}

	workorgs.Set([]string{"Power Platform"})
	route = resolveTaskRoute(store.DB(), "pp-1", repo, openPacer(), time.Now())
	if !route.IsWork {
		t.Fatal("work-org task did not route as work")
	}
	if len(route.Candidates) == 0 || route.Candidates[0].Seat != router.SeatWork {
		t.Fatalf("work-org route does not start on the work seat: %+v", route.Candidates)
	}
	for _, c := range route.Candidates {
		if c.Family == router.FamilyGemini {
			t.Fatalf("work-org coding route includes Gemini: %+v", route.Candidates)
		}
	}
}
