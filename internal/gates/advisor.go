package gates

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VinnyVanGogh/staypoint/internal/decision"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Decision log (STA-868). One row per advisor recommendation on a subject,
// with the human decision filled in when it is made, so agreement can be
// measured. subject_kind is "security_gate" here; STA-433's decision log can
// record other kinds (governance transitions, ...) in the same table.
const SubjectSecurityGate = "security_gate"

// Advisor names.
const (
	AdvisorTogether = "together"
	AdvisorGemini   = "gemini"
)

// Recommendation keys match gate decisions.
const (
	RecApprove = "approved"
	RecDeny    = "denied"
)

// Advice is one advisor's recommendation.
type Advice struct {
	Advisor        string    `json:"advisor"`
	Model          string    `json:"model,omitempty"`
	Recommendation string    `json:"recommendation"` // approved | denied | "" (none)
	Reason         string    `json:"reason,omitempty"`
	LatencyMS      int64     `json:"latency_ms"`
	Error          string    `json:"error,omitempty"`
	FinalDecision  string    `json:"final_decision,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// Probability of the recommendation (tev1 /v1/systemone only, else 0).
	// Not stored; the reason text carries it for the log.
	Probability float64 `json:"probability,omitempty"`
}

// LogAdvice inserts a decision_log row. When the subject is already decided
// (a late advisory), the final decision is copied in so it still counts.
func LogAdvice(db Execer, subjectID string, a Advice) error {
	var final, by string
	var at sql.NullString
	_ = db.QueryRow(`SELECT CASE WHEN status='pending' THEN '' ELSE status END, decided_by, decided_at
		FROM security_gate_requests WHERE id = ?`, subjectID).Scan(&final, &by, &at)
	_, err := db.Exec(`INSERT INTO decision_log
		(subject_kind, subject_id, advisor, model, recommendation, reason, latency_ms, error, final_decision, decided_by, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		SubjectSecurityGate, subjectID, a.Advisor, a.Model, a.Recommendation, a.Reason, a.LatencyMS, a.Error,
		final, by, at)
	return err
}

// RecordFinalDecision stamps the human (or rule) decision on every advisory
// row for the subject. Run inside the decision's transaction.
func RecordFinalDecision(db Execer, subjectID, decisionStatus, decidedBy string, at time.Time) error {
	_, err := db.Exec(`UPDATE decision_log SET final_decision = ?, decided_by = ?, decided_at = ?
		WHERE subject_kind = ? AND subject_id = ?`,
		decisionStatus, decidedBy, at.UTC().Format(time.RFC3339Nano), SubjectSecurityGate, subjectID)
	return err
}

// LatestAdvice returns the newest row per advisor for each subject id.
func LatestAdvice(db Execer, ids []string) (map[string]map[string]Advice, error) {
	out := map[string]map[string]Advice{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{SubjectSecurityGate}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`SELECT subject_id, advisor, model, recommendation, reason, latency_ms, error, final_decision, created_at
		FROM decision_log WHERE subject_kind = ? AND subject_id IN (`+ph+`) ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, created string
		var a Advice
		if err := rows.Scan(&id, &a.Advisor, &a.Model, &a.Recommendation, &a.Reason, &a.LatencyMS, &a.Error, &a.FinalDecision, &created); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse("2006-01-02T15:04:05.999Z", created)
		if out[id] == nil {
			out[id] = map[string]Advice{}
		}
		out[id][a.Advisor] = a // later rows win
	}
	return out, rows.Err()
}

// AdvisorStats is how often an advisor agreed with the Board.
type AdvisorStats struct {
	Advisor   string  `json:"advisor"`
	Requests  int     `json:"requests"`  // subjects with any row
	Errors    int     `json:"errors"`    // latest row is an error / no recommendation
	Decided   int     `json:"decided"`   // Board-decided with a recommendation
	Agreed    int     `json:"agreed"`    // ... where the recommendation matched
	Agreement float64 `json:"agreement"` // Agreed / Decided, 0 when Decided == 0
}

// Stats compares each advisor's latest recommendation per request with the
// Board's decision. Rule auto-approvals are not Board decisions and are left out.
func Stats(db Execer) ([]AdvisorStats, error) {
	rows, err := db.Query(`SELECT d.advisor, d.recommendation, d.final_decision, d.decided_by
		FROM decision_log d
		JOIN (SELECT MAX(id) AS id FROM decision_log WHERE subject_kind = ? GROUP BY subject_id, advisor) l ON l.id = d.id
		ORDER BY d.advisor`, SubjectSecurityGate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*AdvisorStats{}
	var order []string
	for rows.Next() {
		var adv, rec, final, decidedBy string
		if err := rows.Scan(&adv, &rec, &final, &decidedBy); err != nil {
			return nil, err
		}
		s := by[adv]
		if s == nil {
			s = &AdvisorStats{Advisor: adv}
			by[adv] = s
			order = append(order, adv)
		}
		s.Requests++
		if rec == "" {
			s.Errors++
			continue
		}
		if final != "" && decidedBy == "board" {
			s.Decided++
			if rec == final {
				s.Agreed++
			}
		}
	}
	out := []AdvisorStats{}
	for _, a := range order {
		s := by[a]
		if s.Decided > 0 {
			s.Agreement = float64(s.Agreed) / float64(s.Decided)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// RequestAdvisor gives one recommendation for one gate request.
type RequestAdvisor interface {
	Name() string
	Advise(ctx context.Context, gr *security.GateRequest, scripts []security.ScriptRef) (Advice, error)
}

// DecisionClient is satisfied by *decision.TogetherDecisionClient.
type DecisionClient interface {
	DecideWithReason(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error)
	Model() string
}

// TogetherAdvisor asks the Together decision model A=approve / B=deny.
type TogetherAdvisor struct{ Client DecisionClient }

func (TogetherAdvisor) Name() string { return AdvisorTogether }

// Advise implements RequestAdvisor.
func (t TogetherAdvisor) Advise(ctx context.Context, gr *security.GateRequest, scripts []security.ScriptRef) (Advice, error) {
	a := Advice{Advisor: AdvisorTogether}
	if t.Client == nil {
		return a, fmt.Errorf("no Together client (TOGETHER_API_KEY unset)")
	}
	a.Model = t.Client.Model()
	res, err := t.Client.DecideWithReason(ctx, decision.DecisionRequest{
		State:    DescribeRequest(gr, scripts),
		Question: "An autonomous coding agent wants to run this shell command; StayPoint's classifier held it as Red-tier (possibly destructive, secret-touching or exfiltrating). Should the Board approve it?",
		Options: []decision.Option{
			{Key: RecApprove, Letter: "A", Label: "Approve: safe for an agent to run"},
			{Key: RecDeny, Letter: "B", Label: "Deny: destructive, risky, or unclear"},
		},
	})
	if err != nil {
		return a, err
	}
	a.Recommendation, a.Reason, a.Probability = res.SelectedKey, res.Reason, res.Probability
	return a, nil
}

// DescribeLimit caps DescribeRequest in bytes. tev1's /v1/systemone rejects
// prompts over 2048 tokens; code runs ~2.6 bytes/token there plus ~180
// tokens of question and template, so 3500 bytes stays under ~1,800.
const DescribeLimit = 3500

// describeCmdLimit caps the command; scripts share what is left.
const describeCmdLimit = 2000

// DescribeRequest is the context an advisor sees: command, reasons,
// task/repo/org and the scripts it runs, head+tail cut to DescribeLimit.
// Binary files are named but never shown (a Mach-O pasted as a
// "script" became 17K tokens).
func DescribeRequest(gr *security.GateRequest, scripts []security.ScriptRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Command:\n%s\n\nHeld because: %s\n", headTail(printable(gr.Cmdline), describeCmdLimit), strings.Join(gr.Reasons, "; "))
	if gr.TaskID != "" {
		fmt.Fprintf(&b, "Task: %s\n", gr.TaskID)
	}
	if gr.Repo != "" {
		fmt.Fprintf(&b, "Repo: %s\n", gr.Repo)
	}
	if gr.Org != "" {
		fmt.Fprintf(&b, "Organization: %s\n", gr.Org)
	}
	if gr.CWD != "" {
		fmt.Fprintf(&b, "Working dir: %s\n", gr.CWD)
	}
	for i, s := range scripts {
		switch {
		case s.Content == "":
			fmt.Fprintf(&b, "\nScript %s: (unreadable)\n", s.Path)
		case looksBinary(s.Content):
			fmt.Fprintf(&b, "\nScript %s (sha256 %.12s): binary, not shown\n", s.Path, s.SHA256)
		default:
			head := fmt.Sprintf("\nScript %s (sha256 %.12s):\n", s.Path, s.SHA256)
			share := (DescribeLimit - b.Len()) / (len(scripts) - i)
			fmt.Fprintf(&b, "%s%s\n", head, headTail(printable(s.Content), share-len(head)-1))
		}
	}
	return headTail(printable(b.String()), DescribeLimit)
}

// headTail keeps the first and last parts of s within n bytes, marking the
// cut, on rune boundaries.
func headTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "\n…(truncated)…\n"
	keep := n - len(mark)
	if keep < 2 {
		return strings.ToValidUTF8(s[:max(n, 0)], "")
	}
	h, t := keep/2, len(s)-(keep-keep/2)
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h] + mark + s[t:]
}

// looksBinary mirrors bash's own check (check_binary_file): a NUL in the
// first line of the first 80 bytes, unless it starts with "#!". Mach-O, ELF
// and PE headers all qualify. Anything bash would run as a script is shown,
// so stray bytes cannot hide a script from the advisor.
func looksBinary(s string) bool {
	if strings.HasPrefix(s, "#!") {
		return false
	}
	if len(s) > 80 {
		s = s[:80]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.IndexByte(s, 0) >= 0
}

// printable replaces NULs and invalid UTF-8 with U+FFFD.
func printable(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

// RunAdvisory asks adv about gr and logs the result. It never touches the
// request's status: an advisor is advisory only. Errors and timeouts become
// a row with no recommendation. done (optional) runs after the row is written.
func RunAdvisory(db *sql.DB, adv RequestAdvisor, gr *security.GateRequest, scripts []security.ScriptRef, timeout time.Duration, done func(Advice)) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	a, err := adv.Advise(ctx, gr, scripts)
	a.Advisor = adv.Name()
	a.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		a.Recommendation, a.Error = "", truncate(err.Error(), 300)
	} else if a.Recommendation != RecApprove && a.Recommendation != RecDeny {
		a.Error = "unrecognised recommendation " + fmt.Sprintf("%q", a.Recommendation)
		a.Recommendation = ""
	}
	if err := LogAdvice(db, gr.ID, a); err != nil {
		slog.Warn("gate advisory: log failed", slog.String("gate_id", gr.ID), slog.String("error", err.Error()))
		return
	}
	if done != nil {
		done(a)
	}
}
