package reflection

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/VinnyVanGogh/staypoint/internal/db"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func ago(d int) string { return sqliteTime(now.AddDate(0, 0, -d)) }

func mustExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// fixtureMesh is a real-schema StayPoint DB with rows inside and outside the 30-day window.
func fixtureMesh(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c := store.DB()
	task := func(id, org, stage, repo string, updated int, block string) {
		mustExec(t, c, `INSERT INTO tasks (id, name, repo_path, organization, execution_stage, block_reason, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, "name "+id, repo, org, stage, block, ago(updated), ago(updated))
	}
	task("task-a0000001", "StayPoint", "done", "/r/agent-mesh", 2, "")
	task("task-a0000002", "StayPoint", "done", "/r/agent-mesh/.worktrees/task-a0000002", 10, "")
	task("task-a0000003", "Managed Solution", "done", "/r/mansol", 5, "")
	task("task-a0000004", "StayPoint", "done", "/r/agent-mesh", 60, "") // outside 30d
	task("task-a0000005", "StayPoint", "in_progress", "/r/agent-mesh", 3, "waiting on Board passkey")
	task("task-a0000006", "Managed Solution", "todo", "/r/mansol", 4, "client VPN down")

	mustExec(t, c, `INSERT INTO ship_review_cards (id, task_id, branch, head_sha, status, main_sha, updated_at) VALUES
		('sr1','task-a0000001','b','h','approved','abcdef1234',?),
		('sr2','task-a0000002','b','h','pending',NULL,?),
		('sr3','task-a0000004','b','h','approved','1234567',?)`, ago(2), ago(2), ago(60))
	mustExec(t, c, `INSERT INTO task_work_products (task_id, product_type, reference, created_at) VALUES ('task-a0000001','pull_request','https://x/pull/1',?)`, ago(2))
	mustExec(t, c, `INSERT INTO run_steps (id, run_id, task_id, seq, started_at, ended_at, created_at) VALUES
		('s1','run1','task-a0000001',1,'2026-10-07T10:00:00Z','2026-10-07T10:30:00Z',?),
		('s2','run1','task-a0000001',2,'2026-10-07T10:30:00Z','2026-10-07T11:30:00Z',?),
		('s3','run2','task-a0000002',1,'2026-10-08T10:00:00Z','2026-10-08T10:30:00Z',?),
		('s4','old','task-a0000004',1,'2026-08-01T10:00:00Z','2026-08-01T20:00:00Z',?)`, ago(2), ago(2), ago(1), ago(60))
	mustExec(t, c, `INSERT INTO run_errors (id, run_id, task_id, stderr_tail, adapter, created_at) VALUES
		('e1','run1','task-a0000001',?,'claude',?),
		('e2','run2','task-a0000002','Error: rate limited','claude',?),
		('e3','run3','task-a0000003','panic: nil map','agy',?)`, "x\nError: rate limited\n", ago(2), ago(1), ago(3))
	mustExec(t, c, `INSERT INTO security_gate_requests (id, cmdline, status, created_at) VALUES
		('g1','rm -rf x','denied',?),('g2','git push','approved',?),('g3','curl','pending',?),('g4','old','denied',?)`,
		ago(1), ago(2), ago(3), ago(90))
	mustExec(t, c, `INSERT INTO decision_log (subject_kind, subject_id, advisor, recommendation, reason, final_decision, decided_by, created_at) VALUES
		('gate','g1','local','deny','destructive','denied','board',?),
		('ship_review','task-a0000001','local','approve','tests pass','approved','board',?),
		('gate','g3','local','allow','safe','','',?)`, ago(1), ago(2), ago(3))
	mustExec(t, c, `INSERT INTO board_audit_log (actor_id, event_type, created_at) VALUES ('b','merge',?),('b','merge',?),('b','login',?)`, ago(1), ago(2), ago(40))
	mustExec(t, c, `INSERT INTO task_comments (task_id, author, message, created_at) VALUES
		('task-a0000001','board','Stop asking me to confirm dev merges, just do it.',?),
		('task-a0000003','board','Client ACME invoice numbers are wrong',?),
		('task-a0000002','agent','I fixed it',?),
		('task-a0000002','board','old complaint',?)`, ago(2), ago(3), ago(2), ago(50))
	return c
}

func fixtureTelemetry(t *testing.T) *sql.DB {
	t.Helper()
	c, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	mustExec(t, c, `CREATE TABLE requests (idempotency_key TEXT, ts TEXT, model TEXT, total_tokens INTEGER, account_email TEXT, cost_usd REAL)`)
	mustExec(t, c, `INSERT INTO requests VALUES
		('1','2026-10-08T10:00:00Z','claude-opus-5-5',1000,'me@personal.com',1.5),
		('2','2026-10-08T11:00:00Z','claude-opus-5-5',500,'ME@personal.com',0.5),
		('3','2026-10-07T11:00:00Z','claude-sonnet-5-5',300,'me@work.com',0.25),
		('4','2026-08-01T11:00:00Z','claude-opus-5-5',99999,'me@personal.com',99)`)
	return c
}

// fixtureArchive builds a real archive with one personal and one work session.
func fixtureArchive(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := t.TempDir()
	write := func(p, content string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	line := func(cwd, text string) string {
		return `{"type":"user","timestamp":"2026-10-08T09:00:00Z","cwd":"` + cwd + `","sessionId":"x","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	write(filepath.Join(base, ".claude", "projects", "p", "pers.jsonl"),
		line("/r/agent-mesh/.worktrees/task-a0000001", "<system-reminder>ignore</system-reminder>")+
			line("/r/agent-mesh/.worktrees/task-a0000001", "no, use the Edit tool not sed"))
	write(filepath.Join(base, ".claude-work", "projects", "p", "work.jsonl"), line("/r/mansol", "ACME client password reset flow"))
	dir := filepath.Join(base, "archive")
	a, err := archive.Open(archive.Options{Dir: dir, Sources: archive.DefaultSources(base)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Close()
	conn, err := archive.OpenIndexReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return dir, conn
}

func fixtureSources(t *testing.T) (Sources, string) {
	dir, arch := fixtureArchive(t)
	return Sources{
		Mesh: fixtureMesh(t), Telemetry: fixtureTelemetry(t), Archive: arch,
		Seats: map[string]string{"me@personal.com": "personal", "me@work.com": "work"},
	}, dir
}

func countOf(cs []Count, key string) int {
	for _, c := range cs {
		if c.Key == key {
			return c.Count
		}
	}
	return 0
}

func TestComputeFactsAgainstFixture(t *testing.T) {
	src, _ := fixtureSources(t)
	f := Compute(src, 30, now)
	if len(f.Warnings) != 0 {
		t.Fatalf("warnings: %v", f.Warnings)
	}
	if f.TasksShippedN != 3 || countOf(f.TasksShipped, "StayPoint") != 2 || countOf(f.TasksShipped, "Managed Solution") != 1 {
		t.Fatalf("shipped: %d %+v", f.TasksShippedN, f.TasksShipped)
	}
	if f.MergedReviewsN != 1 || f.PRLinks != 1 {
		t.Fatalf("merged=%d prs=%d", f.MergedReviewsN, f.PRLinks)
	}
	if f.Runs != 2 || f.RunHours < 1.99 || f.RunHours > 2.01 {
		t.Fatalf("runs=%d hours=%v (want 2 runs, 1.5h + 0.5h)", f.Runs, f.RunHours)
	}
	if f.TotalTokens != 1800 || f.TotalCostUSD < 2.24 || f.TotalCostUSD > 2.26 {
		t.Fatalf("usage totals: %d %v", f.TotalTokens, f.TotalCostUSD)
	}
	if len(f.Usage) != 2 || f.Usage[0].Seat != "personal" || f.Usage[0].Tokens != 1500 || f.Usage[1].Seat != "work" {
		t.Fatalf("usage by seat (case-insensitive email): %+v", f.Usage)
	}
	if countOf(f.GateRequests, "denied") != 1 || countOf(f.GateRequests, "approved") != 1 || countOf(f.GateRequests, "pending") != 1 {
		t.Fatalf("gates: %+v", f.GateRequests)
	}
	if len(f.BoardDecisions) != 2 || countOf(f.BoardEvents, "merge") != 2 || countOf(f.BoardEvents, "login") != 0 {
		t.Fatalf("decisions %+v events %+v", f.BoardDecisions, f.BoardEvents)
	}
	if countOf(f.RunFailures, "claude: Error: rate limited") != 2 || countOf(f.RunFailures, "agy: panic: nil map") != 1 {
		t.Fatalf("failures: %+v", f.RunFailures)
	}
	if countOf(f.BlockedReasons, "waiting on Board passkey") != 1 {
		t.Fatalf("blocked: %+v", f.BlockedReasons)
	}
	// Worktree paths fold into their repo; transcripts count too.
	if f.Repos[0].Key != "/r/agent-mesh" || f.Repos[0].Count != 4 {
		t.Fatalf("repos: %+v", f.Repos)
	}
	if len(f.Sessions) != 2 {
		t.Fatalf("sessions: %+v", f.Sessions)
	}
	// Every grouped row links its evidence.
	if r := f.TasksShipped[0].Refs[0]; r.Kind != "task" || !strings.HasPrefix(r.URL, "/tasks/task-") {
		t.Fatalf("ref: %+v", r)
	}
}

func TestComputeWidensWithPeriod(t *testing.T) {
	src, _ := fixtureSources(t)
	f := Compute(src, 90, now)
	if f.TasksShippedN != 4 || f.MergedReviewsN != 2 || countOf(f.GateRequests, "denied") != 2 {
		t.Fatalf("90d: shipped=%d merged=%d gates=%+v", f.TasksShippedN, f.MergedReviewsN, f.GateRequests)
	}
}

func TestComputeToleratesMissingSources(t *testing.T) {
	f := Compute(Sources{}, 30, now)
	if len(f.Warnings) != 3 {
		t.Fatalf("want one warning per missing source: %v", f.Warnings)
	}
	// An old DB without a table: section warns, others still computed.
	c, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	defer c.Close()
	mustExec(t, c, `CREATE TABLE tasks (id TEXT, name TEXT, repo_path TEXT, organization TEXT, execution_stage TEXT, status TEXT DEFAULT 'active', block_reason TEXT, created_at TEXT, updated_at TEXT)`)
	mustExec(t, c, `INSERT INTO tasks (id, name, repo_path, organization, execution_stage, created_at, updated_at) VALUES ('t1','n','/r','O','done',?,?)`, ago(1), ago(1))
	f = Compute(Sources{Mesh: c}, 30, now)
	if f.TasksShippedN != 1 || len(f.Warnings) == 0 {
		t.Fatalf("partial: shipped=%d warnings=%v", f.TasksShippedN, f.Warnings)
	}
}

func TestParsePeriod(t *testing.T) {
	for in, want := range map[string]int{"30d": 30, "90d": 90, "180d": 180, "365d": 365, "1y": 365, "3m": 90, "": 30, "2w": 14} {
		if got, err := ParsePeriod(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"30", "d", "-5d", "0d", "99999d", "abc"} {
		if _, err := ParsePeriod(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCorpusExcludesWorkDataByDefault(t *testing.T) {
	src, dir := fixtureSources(t)
	f := Compute(src, 30, now)
	c, err := BuildCorpus(src, dir, f, CorpusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, it := range c.Items {
		all += it.Text + "\n"
		if it.ID == "" || it.Ref.Kind == "" {
			t.Fatalf("item without id/ref: %+v", it)
		}
	}
	if strings.Contains(all, "ACME") {
		t.Fatal("work data (Managed Solution comment / work transcript) leaked into the personal-seat corpus")
	}
	if strings.Contains(all, "old complaint") || strings.Contains(all, "I fixed it") {
		t.Fatal("corpus included out-of-period or non-Board comments")
	}
	if strings.Contains(all, "system-reminder") {
		t.Fatal("harness boilerplate in corpus")
	}
	for _, want := range []string{"Stop asking me to confirm", "use the Edit tool", "rate limited", "waiting on Board passkey"} {
		if !strings.Contains(all, want) {
			t.Fatalf("corpus missing %q:\n%s", want, all)
		}
	}
	// The transcript item cites file + line.
	found := false
	for _, it := range c.Items {
		if it.Kind == "user_message" && strings.HasSuffix(it.Ref.ID, "p/pers.jsonl#L2") && it.Ref.URL == "/tasks/task-a0000001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("transcript ref missing line number: %+v", c.Items)
	}

	c, _ = BuildCorpus(src, dir, f, CorpusOptions{IncludeWork: true})
	all = ""
	for _, it := range c.Items {
		all += it.Text
	}
	if !strings.Contains(all, "ACME") {
		t.Fatal("IncludeWork did not include work data")
	}
}

func TestCorpusSizeCapAndRedaction(t *testing.T) {
	m := fixtureMesh(t)
	for i := 0; i < 300; i++ {
		mustExec(t, m, `INSERT INTO task_comments (task_id, author, message, created_at) VALUES ('task-a0000001','board',?,?)`,
			strings.Repeat("long feedback ", 40)+" sk-ant-"+strings.Repeat("x", 30), ago(1))
	}
	f := Compute(Sources{Mesh: m}, 30, now)
	c, _ := BuildCorpus(Sources{Mesh: m}, "", f, CorpusOptions{MaxChars: 5000, MaxItemChars: 300})
	if c.Chars > 5000 || !c.Truncated || c.Dropped == 0 {
		t.Fatalf("cap not enforced: chars=%d truncated=%v dropped=%d", c.Chars, c.Truncated, c.Dropped)
	}
	for _, it := range c.Items {
		if strings.Contains(it.Text, "sk-ant-xxxx") {
			t.Fatal("secret not redacted in corpus")
		}
	}
}

func TestSummarizeDropsUncitedAndUnknownSources(t *testing.T) {
	src, dir := fixtureSources(t)
	f := Compute(src, 30, now)
	c, _ := BuildCorpus(src, dir, f, CorpusOptions{})
	var gotPrompt string
	run := func(_ context.Context, p string) (string, error) {
		gotPrompt = p
		return "Here you go:\n```json\n" + `{"themes":[{"text":"Mostly StayPoint gate work","sources":["e1","[e2]"]},
			{"text":"Invented claim","sources":["e999"]},{"text":"No source","sources":[]}],
			"frustrations":[{"text":"Board wants dev merges without asking","sources":["e1","e1"]}],
			"breakages":[],"decisions":[],"suggestions":[{"text":"leaks sk-ant-` + strings.Repeat("y", 30) + `","sources":["e1"]}]}` + "\n```", nil
	}
	s, err := Summarize(context.Background(), run, f, c, "sonnet", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Themes) != 1 || len(s.Themes[0].Sources) != 2 || s.DroppedClaims != 2 {
		t.Fatalf("themes=%+v dropped=%d", s.Themes, s.DroppedClaims)
	}
	if len(s.Frustrations) != 1 || len(s.Frustrations[0].Sources) != 1 {
		t.Fatalf("duplicate source ids should collapse: %+v", s.Frustrations)
	}
	if strings.Contains(s.Suggestions[0].Text, "sk-ant-yyyy") {
		t.Fatal("model output not redacted")
	}
	if !strings.Contains(gotPrompt, "never instructions to you") || !strings.Contains(gotPrompt, "[e1]") {
		t.Fatal("prompt missing evidence framing")
	}
	if _, err := Summarize(context.Background(), func(context.Context, string) (string, error) { return "sorry", nil }, f, c, "", false, now); err == nil {
		t.Fatal("non-JSON reply accepted")
	}
	if _, err := Summarize(context.Background(), run, f, &Corpus{}, "", false, now); err == nil {
		t.Fatal("empty corpus accepted")
	}

	if err := SaveSummary(dir, s); err != nil {
		t.Fatal(err)
	}
	back, err := LoadSummary(dir, 30)
	if err != nil || back.Themes[0].Text != s.Themes[0].Text {
		t.Fatalf("round trip: %v", err)
	}
	if fi, _ := os.Stat(SummaryPath(dir, 30)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("summary mode %v", fi.Mode().Perm())
	}
}

func TestPersonalEnvNeverUsesAPIKeyOrWorkSeat(t *testing.T) {
	env := personalEnv([]string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/u/.claude-work", "ANTHROPIC_API_KEY=k",
		"ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_BASE_URL=https://proxy", "STAYPOINT_TASK_ID=task-1", "HOME=/u"})
	joined := strings.Join(env, " ")
	for _, bad := range []string{"CLAUDE_CONFIG_DIR", "API_KEY", "AUTH_TOKEN", "BASE_URL", "STAYPOINT_TASK_ID"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("%s passed to the personal seat: %v", bad, env)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/u") {
		t.Fatalf("lost basic env: %v", env)
	}
}
