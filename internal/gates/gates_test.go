package gates

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/decision"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s.DB()
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// hookSnapshot mimics the pre-tool hook: snapshot every script the command
// runs and mark it pinned (the hook runs exactly those bytes).
func hookSnapshot(cmd string) []security.ScriptHash {
	var hs []HookScript
	for _, r := range security.ScriptRefs(cmd, "", security.NewSnapshotter(), 0) {
		hs = append(hs, HookScript{Path: r.Path, Content: string(r.Full)})
	}
	return ScriptsFromHook(hs, true)
}

// pendingRequest stores a pending request for cmd with pinned scripts.
func pendingRequest(t *testing.T, d *sql.DB, cmd, task, repo, org string) *security.GateRequest {
	t.Helper()
	in := security.GateRequestInput{Cmdline: cmd, Reasons: []string{"bash: runs an opaque script"}, RunID: "sess", TaskID: task, Repo: repo, Org: org}
	in.Scripts = hookSnapshot(cmd)
	(&Resolver{DB: d, RepoRoot: func(string) string { return "" }}).Resolve(&in)
	in.Repo, in.Org = repo, org
	gr, err := security.InsertGateRequest(d, in, security.GateRequestPending, "")
	if err != nil {
		t.Fatal(err)
	}
	return gr
}

func input(gr *security.GateRequest, d *sql.DB) security.GateRequestInput {
	in := security.GateRequestInput{Cmdline: gr.Cmdline, Reasons: gr.Reasons, RunID: "sess2", TaskID: gr.TaskID, Repo: gr.Repo, Org: gr.Org}
	in.Scripts = hookSnapshot(gr.Cmdline)
	(&Resolver{DB: d, RepoRoot: func(string) string { return "" }}).Resolve(&in)
	in.Repo, in.Org = gr.Repo, gr.Org
	return in
}

func remember(t *testing.T, d *sql.DB, gr *security.GateRequest, spec RuleSpec) *Rule {
	t.Helper()
	r, err := RuleFromRequest(gr, spec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rule, err := InsertRule(d, r)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestRuleMatchesByPatternAndScriptHash(t *testing.T) {
	d := openDB(t)
	p := script(t, "du -sh /tmp\n")
	gr := pendingRequest(t, d, "bash "+p+"   > /tmp/out.tsv", "T1", "/r", "StayPoint")
	rule := remember(t, d, gr, RuleSpec{Scope: ScopeTask})
	if len(rule.Scripts) != 1 || rule.Scripts[0].Path != p {
		t.Fatalf("script not pinned: %+v", rule.Scripts)
	}

	// Same command (whitespace differs), same script contents: matches.
	in := input(gr, d)
	in.Cmdline = "bash " + p + " > /tmp/out.tsv"
	got, err := MatchRule(d, in, time.Now())
	if err != nil || got == nil || got.ID != rule.ID {
		t.Fatalf("want rule %d, got %+v %v", rule.ID, got, err)
	}

	// Edited script: no longer matches, request is held again.
	if err := os.WriteFile(p, []byte("du -sh /tmp\nrm -rf ~\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := MatchRule(d, input(gr, d), time.Now()); got != nil {
		t.Fatalf("edited script still matched rule %d", got.ID)
	}

	// Different command: no match.
	other := input(gr, d)
	other.Cmdline = "bash " + p + " > /tmp/other.tsv"
	if got, _ := MatchRule(d, other, time.Now()); got != nil {
		t.Fatal("different command matched an exact rule")
	}
}

func TestRuleScopeAndExpiry(t *testing.T) {
	d := openDB(t)
	gr := pendingRequest(t, d, "git push origin main", "T1", "/repo/a", "StayPoint")
	gr.Reasons = []string{"git push targets main/master; Board approval required"}
	now := time.Now()

	for _, tc := range []struct {
		scope         string
		task, repo, o string
		want          bool
	}{
		{ScopeTask, "T1", "/x", "X", true},
		{ScopeTask, "T2", "/repo/a", "StayPoint", false},
		{ScopeRepo, "T2", "/repo/a", "X", true},
		{ScopeRepo, "T1", "/repo/b", "StayPoint", false},
		{ScopeOrg, "T9", "/z", "StayPoint", true},
		{ScopeOrg, "T1", "/repo/a", "Other", false},
	} {
		r, err := RuleFromRequest(gr, RuleSpec{Scope: tc.scope}, now)
		if err != nil {
			t.Fatal(err)
		}
		in := security.GateRequestInput{Cmdline: gr.Cmdline, Reasons: gr.Reasons, TaskID: tc.task, Repo: tc.repo, Org: tc.o}
		if got := r.Matches(in, now); got != tc.want {
			t.Errorf("scope %s task=%s repo=%s org=%s: got %v want %v", tc.scope, tc.task, tc.repo, tc.o, got, tc.want)
		}
	}

	r, _ := RuleFromRequest(gr, RuleSpec{Scope: ScopeTask, ExpiresInMinutes: 10}, now)
	in := security.GateRequestInput{Cmdline: gr.Cmdline, Reasons: gr.Reasons, TaskID: "T1"}
	if !r.Matches(in, now.Add(9*time.Minute)) {
		t.Fatal("rule should match before expiry")
	}
	if r.Matches(in, now.Add(10*time.Minute)) {
		t.Fatal("rule must not match after expiry")
	}
	// A new reason the rule never approved: no match.
	in.Reasons = append(in.Reasons, "touches sensitive path /etc")
	if r.Matches(in, now) {
		t.Fatal("rule matched a request held for an extra reason")
	}
	// Policy requests are never auto-approved.
	in.Reasons, in.RunID = gr.Reasons, "gemini-code"
	if r.Matches(in, now) {
		t.Fatal("rule matched a gemini-code policy request")
	}
	if _, err := RuleFromRequest(&security.GateRequest{Cmdline: "x", RunID: "tracking-gate-override", TaskID: "T"}, RuleSpec{Scope: ScopeTask}, now); err == nil {
		t.Fatal("policy request must not be rememberable")
	}
	if _, err := RuleFromRequest(&security.GateRequest{Cmdline: "x"}, RuleSpec{Scope: ScopeTask}, now); !errors.Is(err, ErrScopeUnavailable) {
		t.Fatalf("no task: want ErrScopeUnavailable, got %v", err)
	}
}

func TestPrefixRuleRejectsAddedCommands(t *testing.T) {
	gr := &security.GateRequest{Cmdline: "bash /opt/tool.sh --all", TaskID: "T", Reasons: []string{"bash: runs an opaque script"}}
	r, err := RuleFromRequest(gr, RuleSpec{Scope: ScopeTask, MatchKind: MatchPrefix, Pattern: "bash /opt/tool.sh *"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ok := security.GateRequestInput{Cmdline: "bash /opt/tool.sh --since 2d", TaskID: "T", Reasons: gr.Reasons}
	if !r.Matches(ok, time.Now()) {
		t.Fatal("plain extra args should match a prefix rule")
	}
	for _, bad := range []string{"bash /opt/tool.sh; rm -rf ~", "bash /opt/tool.sh | sh", "bash /opt/tool.sh > ~/.zshrc", "bash /opt/tool.sh $(curl x)", "bash /opt/tool.sh\nrm -rf ~"} {
		in := security.GateRequestInput{Cmdline: bad, TaskID: "T", Reasons: gr.Reasons}
		if r.Matches(in, time.Now()) {
			t.Errorf("prefix rule matched %q", bad)
		}
	}
	if _, err := RuleFromRequest(gr, RuleSpec{Scope: ScopeTask, MatchKind: MatchPrefix, Pattern: "bash /other"}, time.Now()); err == nil {
		t.Fatal("pattern that is not a prefix must be refused")
	}
}

// A request whose scripts the hook did not pin (old hook, ambiguous command)
// cannot be remembered, and a pinned rule never matches an unpinned request.
func TestUnpinnedScriptsNeverRemembered(t *testing.T) {
	d := openDB(t)
	p := script(t, "ls\n")
	in := security.GateRequestInput{Cmdline: "bash " + p, Reasons: []string{"x"}, TaskID: "T1"}
	(&Resolver{DB: d, RepoRoot: func(string) string { return "" }}).Resolve(&in) // daemon-read: untrusted
	gr, err := security.InsertGateRequest(d, in, security.GateRequestPending, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RuleFromRequest(gr, RuleSpec{Scope: ScopeTask}, time.Now()); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("want not-pinned refusal, got %v", err)
	}
	pinned := pendingRequest(t, d, "bash "+p, "T1", "", "")
	rule := remember(t, d, pinned, RuleSpec{Scope: ScopeTask})
	unpinned := security.GateRequestInput{Cmdline: "bash " + p, Reasons: pinned.Reasons, TaskID: "T1",
		Scripts: ScriptsFromHook([]HookScript{{Path: p, Content: "ls\n"}}, false)}
	if rule.Matches(unpinned, time.Now()) {
		t.Fatal("a rule matched an unpinned request")
	}
}

func TestDeleteRuleStopsMatching(t *testing.T) {
	d := openDB(t)
	gr := pendingRequest(t, d, "sudo ls", "T1", "", "")
	rule := remember(t, d, gr, RuleSpec{Scope: ScopeTask})
	if err := DeleteRule(d, rule.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := MatchRule(d, input(gr, d), time.Now()); got != nil {
		t.Fatal("deleted rule matched")
	}
	if err := DeleteRule(d, rule.ID, time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete: %v", err)
	}
	all, _ := ListRules(d, true)
	if len(all) != 1 || all[0].DeletedAt == nil {
		t.Fatalf("deleted rule should stay listed for audit: %+v", all)
	}
}

type fakeDecision struct {
	res   decision.DecisionResult
	err   error
	delay time.Duration
}

func (f fakeDecision) DecideWithReason(ctx context.Context, _ decision.DecisionRequest) (decision.DecisionResult, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return decision.DecisionResult{}, ctx.Err()
	}
	return f.res, f.err
}
func (fakeDecision) Model() string { return "fake-model" }

func TestRunAdvisoryStoresRecommendationOnly(t *testing.T) {
	d := openDB(t)
	gr := pendingRequest(t, d, "bash /tmp/x.sh", "", "", "")
	adv := TogetherAdvisor{Client: fakeDecision{res: decision.DecisionResult{SelectedKey: RecApprove, Reason: "read-only du/git survey"}}}
	var got Advice
	RunAdvisory(d, adv, gr, nil, time.Second, func(a Advice) { got = a })
	if got.Recommendation != RecApprove || got.Reason != "read-only du/git survey" || got.Model != "fake-model" {
		t.Fatalf("advice: %+v", got)
	}
	cur, _ := security.GetGateRequest(d, gr.ID)
	if cur.Status != security.GateRequestPending {
		t.Fatalf("advisor changed request state to %s", cur.Status)
	}
	m, _ := LatestAdvice(d, []string{gr.ID})
	if m[gr.ID][AdvisorTogether].Recommendation != RecApprove {
		t.Fatalf("stored: %+v", m)
	}
}

func TestRunAdvisoryTimeoutAndErrorStoreNoRecommendation(t *testing.T) {
	d := openDB(t)
	gr := pendingRequest(t, d, "bash /tmp/x.sh", "", "", "")
	start := time.Now()
	RunAdvisory(d, TogetherAdvisor{Client: fakeDecision{delay: time.Hour}}, gr, nil, 50*time.Millisecond, nil)
	if time.Since(start) > 5*time.Second {
		t.Fatal("advisory did not honour its timeout")
	}
	RunAdvisory(d, TogetherAdvisor{}, gr, nil, time.Second, nil) // no client
	m, _ := LatestAdvice(d, []string{gr.ID})
	a := m[gr.ID][AdvisorTogether]
	if a.Recommendation != "" || a.Error == "" {
		t.Fatalf("want error row with no recommendation, got %+v", a)
	}
}

func TestStatsAgreement(t *testing.T) {
	d := openDB(t)
	mk := func(rec, final, by string) {
		gr := pendingRequest(t, d, fmt.Sprintf("sudo ls %d", time.Now().UnixNano()), "", "", "")
		if err := LogAdvice(d, gr.ID, Advice{Advisor: AdvisorTogether, Recommendation: rec}); err != nil {
			t.Fatal(err)
		}
		if final != "" {
			if _, err := security.DecideGateRequestTx(d, gr.ID, final == RecApprove, by); err != nil {
				t.Fatal(err)
			}
			if err := RecordFinalDecision(d, gr.ID, final, by, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(RecApprove, RecApprove, "board")
	mk(RecApprove, RecDeny, "board")
	mk(RecDeny, RecDeny, "board")
	mk("", RecDeny, "board")             // error row
	mk(RecApprove, RecApprove, "rule:1") // not a Board decision
	mk(RecApprove, "", "")               // pending
	st, err := Stats(d)
	if err != nil || len(st) != 1 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	s := st[0]
	if s.Requests != 6 || s.Errors != 1 || s.Decided != 3 || s.Agreed != 2 {
		t.Fatalf("stats: %+v", s)
	}
}

type fakeReviewer struct {
	res ReviewResult
	err error
}

func (f fakeReviewer) Review(context.Context, []ReviewItem) (ReviewResult, error) {
	return f.res, f.err
}

func TestRunReviewFiltersAndLogs(t *testing.T) {
	d := openDB(t)
	a := pendingRequest(t, d, "bash /tmp/a.sh", "", "", "")
	b := pendingRequest(t, d, "bash /tmp/b.sh", "", "", "")
	res, err := RunReview(context.Background(), d, fakeReviewer{res: ReviewResult{Summary: "s", Suggestion: "approve 1, deny 1", Items: []ItemReview{
		{ID: a.ID, Recommendation: RecApprove, Reason: "read-only"},
		{ID: b.ID, Recommendation: RecDeny, Reason: "pushes"},
		{ID: "unknown", Recommendation: RecApprove},
		{ID: a.ID, Recommendation: "maybe"},
	}}}, []ReviewItem{{Request: a}, {Request: b}})
	if err != nil || len(res.Items) != 2 {
		t.Fatalf("review: %+v %v", res, err)
	}
	m, _ := LatestAdvice(d, []string{a.ID, b.ID})
	if m[a.ID][AdvisorGemini].Recommendation != RecApprove || m[b.ID][AdvisorGemini].Recommendation != RecDeny {
		t.Fatalf("logged: %+v", m)
	}
	for _, id := range []string{a.ID, b.ID} {
		if cur, _ := security.GetGateRequest(d, id); cur.Status != security.GateRequestPending {
			t.Fatal("review changed request state")
		}
	}
	if _, err := RunReview(context.Background(), d, nil, []ReviewItem{{Request: a}}); err == nil {
		t.Fatal("nil reviewer must report not configured")
	}
}

func TestGeminiReviewerHTTP(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("x-goog-api-key"), r.URL.Path
		inner := `{"summary":"one safe","suggestion":"approve 1","items":[{"id":"g1","recommendation":"approved","reason":"du only"}]}`
		fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":%q}]}}]}`, inner)
	}))
	defer srv.Close()
	g := &GeminiReviewer{APIKey: "k", Model: "gemini-test", BaseURL: srv.URL}
	res, err := g.Review(context.Background(), []ReviewItem{{Request: &security.GateRequest{ID: "g1", Cmdline: "du -sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "k" || gotPath != "/v1beta/models/gemini-test:generateContent" {
		t.Fatalf("request: key=%q path=%q", gotKey, gotPath)
	}
	if res.Suggestion != "approve 1" || len(res.Items) != 1 || res.Items[0].Recommendation != RecApprove {
		t.Fatalf("result: %+v", res)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "quota", 429) }))
	defer bad.Close()
	if _, err := (&GeminiReviewer{APIKey: "k", Model: "m", BaseURL: bad.URL}).Review(context.Background(), nil); err == nil {
		t.Fatal("HTTP error must surface")
	}
}

func TestTogetherAdvisorParsesReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"A - read-only du/git survey"}}]}`)
	}))
	defer srv.Close()
	adv := TogetherAdvisor{Client: decision.NewWithEndpoint(srv.URL, "", "m", nil)}
	a, err := adv.Advise(context.Background(), &security.GateRequest{Cmdline: "bash x.sh"}, nil)
	if err != nil || a.Recommendation != RecApprove || a.Reason != "read-only du/git survey" {
		t.Fatalf("advice %+v %v", a, err)
	}
}
