package shipreview_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// STA-801: one configured repo path whose stat blocks (a pending macOS TCC
// prompt on a ~/Documents folder blocks open() indefinitely, STA-685) must not
// stall dev config lookups for every other repo. A lookup waits at most the
// stat deadline. A live row it could not compare in time gates the start, the
// same as a stat error; a non-live row is skipped.
func TestLookupDevConfig_HungStatIsBounded(t *testing.T) {
	const deadline = 200 * time.Millisecond
	const budget = 2 * time.Second

	db := openTestDB(t)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	hungPlain := filepath.Join(base, "hung-plain")
	hungLive := filepath.Join(base, "hung-live")

	var mu sync.Mutex
	hungCalls := map[string]int{}
	release := make(chan struct{})
	shipreview.SetDevConfigStatForTest(t, func(p string) (os.FileInfo, error) {
		if p == hungPlain || p == hungLive {
			mu.Lock()
			hungCalls[p]++
			mu.Unlock()
			<-release
			return nil, os.ErrPermission
		}
		return os.Stat(p)
	}, deadline)
	t.Cleanup(func() { close(release) })

	type result struct {
		cfg   *shipreview.ProjectDevConfig
		gated bool
		err   error
	}
	liveGate := func(p string) result {
		t.Helper()
		ch := make(chan result, 1)
		go func() {
			cfg, gated, err := shipreview.LiveGateConfig(db, p)
			ch <- result{cfg, gated, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("LiveGateConfig(%s): %v", p, r.err)
			}
			return r
		case <-time.After(budget):
			t.Fatalf("LiveGateConfig(%s) still blocked after %v: one hung repo path stalls every lookup", p, budget)
		}
		return result{}
	}
	getConfig := func(p string) *shipreview.ProjectDevConfig {
		t.Helper()
		ch := make(chan result, 1)
		go func() {
			cfg, err := shipreview.GetProjectDevConfig(db, p)
			ch <- result{cfg: cfg, err: err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("GetProjectDevConfig(%s): %v", p, r.err)
			}
			return r.cfg
		case <-time.After(budget):
			t.Fatalf("GetProjectDevConfig(%s) still blocked after %v: one hung repo path stalls every lookup", p, budget)
		}
		return nil
	}
	mustUpsert := func(cfg *shipreview.ProjectDevConfig) {
		t.Helper()
		if err := shipreview.UpsertProjectDevConfig(db, cfg); err != nil {
			t.Fatalf("UpsertProjectDevConfig(%s): %v", cfg.RepoPath, err)
		}
	}
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: repo, DevCommand: "own"})
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: hungPlain, DevCommand: "plain"})

	// A non-live row whose stat hangs is skipped: the unrelated repo loads its
	// own row and is not gated.
	if cfg := getConfig(repo); cfg.DevCommand != "own" {
		t.Errorf("GetProjectDevConfig(repo).DevCommand = %q, want its own row", cfg.DevCommand)
	}
	if r := liveGate(repo); r.gated || r.cfg.DevCommand != "own" {
		t.Errorf("hung non-live row: LiveGateConfig(repo) gated=%v dev_command=%q, want false, own", r.gated, r.cfg.DevCommand)
	}

	// A live row whose stat hangs could be repo itself, so the start gates.
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: hungLive, LiveCredentials: true})
	if r := liveGate(repo); !r.gated || r.cfg.DevCommand != "own" || r.cfg.LiveCredentials {
		t.Errorf("hung live row: LiveGateConfig(repo) gated=%v dev_command=%q live=%v, want true, own, false",
			r.gated, r.cfg.DevCommand, r.cfg.LiveCredentials)
	}
	if cfg := getConfig(repo); cfg.DevCommand != "own" || cfg.LiveCredentials {
		t.Errorf("hung live row: GetProjectDevConfig(repo) dev_command=%q live=%v, want own, false", cfg.DevCommand, cfg.LiveCredentials)
	}

	// When the looked-up path's own stat hangs, it cannot be ruled out as an
	// alias of the live repo: its own row loads, gated.
	if r := liveGate(hungPlain); !r.gated || r.cfg.DevCommand != "plain" {
		t.Errorf("hung repo path: LiveGateConfig gated=%v dev_command=%q, want true, plain", r.gated, r.cfg.DevCommand)
	}

	// Lookups while a stat is still hung join it instead of starting another,
	// so a stuck path costs one blocked thread, not one per lookup.
	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{hungPlain, hungLive} {
		if n := hungCalls[p]; n != 1 {
			t.Errorf("stat(%s) called %d times across lookups while hung, want 1", filepath.Base(p), n)
		}
	}
}

// BenchmarkLookupDevConfig measures a lookup with ten configured repos, none
// hung, which is the normal case STA-801 must not slow down.
func BenchmarkLookupDevConfig(b *testing.B) {
	db := openTestDB(b)
	base := b.TempDir()
	var repos []string
	for i := 0; i < 10; i++ {
		p := filepath.Join(base, fmt.Sprintf("repo%d", i))
		if err := os.MkdirAll(p, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: p, LiveCredentials: i == 0}); err != nil {
			b.Fatal(err)
		}
		repos = append(repos, p)
	}
	want := repos[5]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := shipreview.LiveGateConfig(db, want); err != nil {
			b.Fatal(err)
		}
	}
}
