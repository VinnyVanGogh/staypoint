// Package reflection builds the Board's "reflect" view: computed facts about a
// period (30/90/180/365 days) from the StayPoint DB, the telemetry DB and the
// transcript archive, then an optional LLM summary whose every claim cites a
// source. All inputs are opened read-only.
package reflection

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ref points at the evidence behind a fact or claim.
type Ref struct {
	Kind  string `json:"kind"` // task, comment, ship_review, run, gate, decision, transcript
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Count is one row of a grouped fact.
type Count struct {
	Key    string  `json:"key"`
	Count  int     `json:"count"`
	Value  float64 `json:"value,omitempty"`
	Tokens int64   `json:"tokens,omitempty"`
	Refs   []Ref   `json:"refs,omitempty"`
}

// Usage is tokens and cost for one seat and model.
type Usage struct {
	Seat     string  `json:"seat"`
	Model    string  `json:"model"`
	Requests int     `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
}

// Facts is everything computed (no LLM) for one period.
type Facts struct {
	Since          time.Time `json:"since"`
	Until          time.Time `json:"until"`
	Days           int       `json:"days"`
	TasksShipped   []Count   `json:"tasks_shipped_by_org"`
	TasksShippedN  int       `json:"tasks_shipped"`
	TasksCreatedN  int       `json:"tasks_created"`
	MergedReviews  []Count   `json:"merged_ship_reviews_by_org"`
	MergedReviewsN int       `json:"merged_ship_reviews"`
	PRLinks        int       `json:"pull_requests_recorded"`
	Runs           int       `json:"runs"`
	RunHours       float64   `json:"run_hours"`
	Usage          []Usage   `json:"usage_by_seat_model"`
	TotalTokens    int64     `json:"total_tokens"`
	TotalCostUSD   float64   `json:"total_cost_usd"`
	GateRequests   []Count   `json:"gate_requests_by_status"`
	BoardDecisions []Count   `json:"board_decisions"`
	BoardEvents    []Count   `json:"board_audit_events"`
	RunFailures    []Count   `json:"run_failures"`
	BlockedReasons []Count   `json:"blocked_reasons"`
	Repos          []Count   `json:"most_touched_repos"`
	Sessions       []Count   `json:"archived_sessions_by_profile"`
	// Warnings lists sections that could not be computed (missing table in an
	// older DB, absent telemetry or archive); the rest of the facts still stand.
	Warnings []string `json:"warnings,omitempty"`
}

// Sources are the read-only inputs; any may be nil.
type Sources struct {
	Mesh      *sql.DB
	Telemetry *sql.DB
	Archive   *sql.DB
	// Seats maps account email to a seat label (work, personal).
	Seats map[string]string
}

// ParsePeriod accepts 30d, 90d, 180d, 365d (any Nd), Nw, Nm (30-day months), 1y.
func ParsePeriod(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 30, nil
	}
	mult := map[byte]int{'d': 1, 'w': 7, 'm': 30, 'y': 365}
	m, ok := mult[s[len(s)-1]]
	if !ok {
		return 0, fmt.Errorf("period %q: use e.g. 30d, 90d, 180d, 365d", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 || n*m > 3650 {
		return 0, fmt.Errorf("period %q: use e.g. 30d, 90d, 180d, 365d", s)
	}
	return n * m, nil
}

// TaskURL is the web UI link for a task.
func TaskURL(id string) string { return "/tasks/" + id }

func taskRef(id, label string) Ref {
	return Ref{Kind: "task", ID: id, Label: label, URL: TaskURL(id)}
}

// sqliteTime is the format StayPoint stores timestamps in; it sorts as text.
func sqliteTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Compute builds the facts for [now-days, now].
func Compute(src Sources, days int, now time.Time) *Facts {
	f := &Facts{Until: now.UTC(), Since: now.UTC().AddDate(0, 0, -days), Days: days}
	since := sqliteTime(f.Since)
	warn := func(section string, err error) {
		if err != nil {
			f.Warnings = append(f.Warnings, section+": "+err.Error())
		}
	}
	if src.Mesh != nil {
		warn("tasks shipped", f.tasks(src.Mesh, since))
		warn("ship reviews", f.reviews(src.Mesh, since))
		warn("runs", f.runs(src.Mesh, since))
		warn("gates", f.gates(src.Mesh, since))
		warn("board decisions", f.decisions(src.Mesh, since))
		warn("failures", f.failures(src.Mesh, since))
		warn("blocked", f.blocked(src.Mesh, since))
	} else {
		f.Warnings = append(f.Warnings, "staypoint db: not available")
	}
	if src.Telemetry != nil {
		warn("usage", f.usage(src.Telemetry, since, src.Seats))
	} else {
		f.Warnings = append(f.Warnings, "usage: telemetry db not available")
	}
	repos := map[string]*Count{}
	if src.Mesh != nil {
		warn("repos", collectRepos(src.Mesh, since, repos, `SELECT repo_path, id, name FROM tasks
			WHERE repo_path != '' AND (created_at >= ? OR updated_at >= ?) AND status != 'soft_deleted'`, true))
	}
	if src.Archive != nil {
		archSince := f.Since.Format(time.RFC3339)
		warn("archive repos", collectRepos(src.Archive, archSince, repos, `SELECT repo, session_id, source FROM transcripts
			WHERE repo != '' AND (started_at >= ? OR ended_at >= ?)`, false))
		warn("archive sessions", f.sessions(src.Archive, archSince))
	} else {
		f.Warnings = append(f.Warnings, "transcripts: archive not built yet")
	}
	f.Repos = topN(repos, 10)
	return f
}

func (f *Facts) tasks(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT COALESCE(NULLIF(organization,''),'(none)'), id, name FROM tasks
		WHERE execution_stage = 'done' AND status != 'soft_deleted' AND updated_at >= ?
		ORDER BY updated_at DESC`, since)
	if err != nil {
		return err
	}
	byOrg := map[string]*Count{}
	for rows.Next() {
		var org, id, name string
		if rows.Scan(&org, &id, &name) != nil {
			continue
		}
		addRef(byOrg, org, taskRef(id, name))
		f.TasksShippedN++
	}
	rows.Close()
	f.TasksShipped = topN(byOrg, 0)
	return db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE created_at >= ? AND status != 'soft_deleted'`, since).Scan(&f.TasksCreatedN)
}

func (f *Facts) reviews(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT COALESCE(NULLIF(t.organization,''),'(none)'), c.id, c.task_id, t.name, COALESCE(c.main_sha,'')
		FROM ship_review_cards c JOIN tasks t ON t.id = c.task_id
		WHERE c.status = 'approved' AND COALESCE(c.main_sha,'') != '' AND c.updated_at >= ?
		ORDER BY c.updated_at DESC`, since)
	if err != nil {
		return err
	}
	byOrg := map[string]*Count{}
	for rows.Next() {
		var org, id, taskID, name, sha string
		if rows.Scan(&org, &id, &taskID, &name, &sha) != nil {
			continue
		}
		label := name
		if len(sha) >= 7 {
			label += " @" + sha[:7]
		}
		addRef(byOrg, org, Ref{Kind: "ship_review", ID: id, Label: label, URL: TaskURL(taskID)})
		f.MergedReviewsN++
	}
	rows.Close()
	f.MergedReviews = topN(byOrg, 0)
	return db.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE product_type = 'pull_request' AND created_at >= ?`, since).Scan(&f.PRLinks)
}

func (f *Facts) runs(db *sql.DB, since string) error {
	// A run's wall time is its first step start to its last step end.
	return db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(hours), 0) FROM (
			SELECT run_id, (julianday(MAX(COALESCE(ended_at, started_at, created_at))) -
			                julianday(MIN(COALESCE(started_at, created_at)))) * 24 AS hours
			FROM run_steps WHERE created_at >= ? GROUP BY run_id)`, since).Scan(&f.Runs, &f.RunHours)
}

func (f *Facts) usage(db *sql.DB, since string, seats map[string]string) error {
	// Telemetry stores ts as RFC3339; compare on the same shape.
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", since)
	rows, err := db.Query(`SELECT COALESCE(account_email,''), COALESCE(model,''), COUNT(*),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? GROUP BY 1, 2 ORDER BY 5 DESC`, t.Format(time.RFC3339))
	if err != nil {
		return err
	}
	defer rows.Close()
	merged := map[[2]string]*Usage{}
	for rows.Next() {
		var email, model string
		var u Usage
		if err := rows.Scan(&email, &model, &u.Requests, &u.Tokens, &u.CostUSD); err != nil {
			return err
		}
		seat := seats[strings.ToLower(email)]
		if seat == "" {
			seat = "other"
			if email == "" {
				seat = "unknown"
			}
		}
		k := [2]string{seat, model}
		if merged[k] == nil {
			merged[k] = &Usage{Seat: seat, Model: model}
		}
		merged[k].Requests += u.Requests
		merged[k].Tokens += u.Tokens
		merged[k].CostUSD += u.CostUSD
		f.TotalTokens += u.Tokens
		f.TotalCostUSD += u.CostUSD
	}
	for _, u := range merged {
		f.Usage = append(f.Usage, *u)
	}
	sort.Slice(f.Usage, func(i, j int) bool {
		if f.Usage[i].CostUSD != f.Usage[j].CostUSD {
			return f.Usage[i].CostUSD > f.Usage[j].CostUSD
		}
		return f.Usage[i].Seat+f.Usage[i].Model < f.Usage[j].Seat+f.Usage[j].Model
	})
	return rows.Err()
}

func (f *Facts) gates(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT status, id FROM security_gate_requests WHERE created_at >= ? ORDER BY created_at DESC`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string]*Count{}
	for rows.Next() {
		var status, id string
		if rows.Scan(&status, &id) != nil {
			continue
		}
		addRef(by, status, Ref{Kind: "gate", ID: id, URL: "/gates"})
	}
	f.GateRequests = topN(by, 0)
	return rows.Err()
}

func (f *Facts) decisions(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT final_decision, subject_kind, subject_id, id FROM decision_log
		WHERE decided_by != '' AND final_decision != '' AND COALESCE(decided_at, created_at) >= ?
		ORDER BY id DESC`, since)
	if err != nil {
		return err
	}
	by := map[string]*Count{}
	for rows.Next() {
		var dec, kind, subject string
		var id int64
		if rows.Scan(&dec, &kind, &subject, &id) != nil {
			continue
		}
		ref := Ref{Kind: "decision", ID: strconv.FormatInt(id, 10), Label: kind + " " + subject}
		if strings.HasPrefix(subject, "task-") {
			ref.URL = TaskURL(subject)
		}
		addRef(by, kind+": "+dec, ref)
	}
	rows.Close()
	f.BoardDecisions = topN(by, 0)

	rows, err = db.Query(`SELECT event_type, id FROM board_audit_log WHERE created_at >= ?`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	ev := map[string]*Count{}
	for rows.Next() {
		var et string
		var id int64
		if rows.Scan(&et, &id) != nil {
			continue
		}
		addRef(ev, et, Ref{Kind: "board_audit", ID: strconv.FormatInt(id, 10)})
	}
	f.BoardEvents = topN(ev, 0)
	return rows.Err()
}

func (f *Facts) failures(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT COALESCE(NULLIF(adapter,''),'?'), stderr_tail, COALESCE(task_id,''), run_id FROM run_errors
		WHERE created_at >= ? ORDER BY created_at DESC`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string]*Count{}
	for rows.Next() {
		var ad, tail, taskID, runID string
		if rows.Scan(&ad, &tail, &taskID, &runID) != nil {
			continue
		}
		ref := Ref{Kind: "run", ID: runID}
		if taskID != "" {
			ref.URL = TaskURL(taskID)
			ref.Label = taskID
		}
		addRef(by, ad+": "+errorSignature(tail), ref)
	}
	f.RunFailures = topN(by, 10)
	return rows.Err()
}

func (f *Facts) blocked(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT block_reason, id, name FROM tasks
		WHERE COALESCE(block_reason,'') != '' AND updated_at >= ? AND status != 'soft_deleted'`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	by := map[string]*Count{}
	for rows.Next() {
		var reason, id, name string
		if rows.Scan(&reason, &id, &name) != nil {
			continue
		}
		addRef(by, errorSignature(reason), taskRef(id, name))
	}
	f.BlockedReasons = topN(by, 10)
	return rows.Err()
}

func (f *Facts) sessions(db *sql.DB, since string) error {
	rows, err := db.Query(`SELECT profile, COUNT(*), COALESCE(SUM(user_msgs),0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_create_tokens),0)
		FROM transcripts WHERE started_at >= ? OR ended_at >= ? GROUP BY profile ORDER BY 2 DESC`, since, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Count
		var msgs int
		if err := rows.Scan(&c.Key, &c.Count, &msgs, &c.Tokens); err != nil {
			return err
		}
		c.Value = float64(msgs)
		f.Sessions = append(f.Sessions, c)
	}
	return rows.Err()
}

func collectRepos(db *sql.DB, since string, into map[string]*Count, q string, isTask bool) error {
	rows, err := db.Query(q, since, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var repo, id, label string
		if rows.Scan(&repo, &id, &label) != nil {
			continue
		}
		repo = strings.TrimRight(repo, "/")
		if i := strings.Index(repo, "/.worktrees/"); i >= 0 {
			repo = repo[:i]
		}
		ref := Ref{Kind: "transcript", ID: label + ":" + id}
		if isTask {
			ref = taskRef(id, label)
		}
		addRef(into, repo, ref)
	}
	return rows.Err()
}

// maxRefs bounds the evidence kept per row.
const maxRefs = 5

func addRef(m map[string]*Count, key string, r Ref) {
	c := m[key]
	if c == nil {
		c = &Count{Key: key}
		m[key] = c
	}
	c.Count++
	if len(c.Refs) < maxRefs {
		c.Refs = append(c.Refs, r)
	}
}

// topN sorts by count (then key) and keeps n rows (0 = all).
func topN(m map[string]*Count, n int) []Count {
	out := make([]Count, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// errorSignature collapses an error tail to a short, groupable line.
func errorSignature(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	pick := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			pick = l
			if strings.Contains(strings.ToLower(l), "error") {
				break
			}
		}
	}
	if pick == "" {
		return "(empty)"
	}
	if r := []rune(pick); len(r) > 100 {
		pick = string(r[:100]) + "…"
	}
	return pick
}
