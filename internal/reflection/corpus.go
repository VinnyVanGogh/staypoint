package reflection

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Item is one piece of evidence the summary may cite by its ID.
type Item struct {
	ID   string `json:"id"`
	At   string `json:"at"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Ref  Ref    `json:"ref"`
}

// CorpusOptions bounds the sample sent to the model.
type CorpusOptions struct {
	// MaxChars caps the whole corpus (default 60000).
	MaxChars int
	// MaxItemChars caps one item (default 600).
	MaxItemChars int
	// IncludeWork adds work-profile transcripts and Managed Solution comments.
	// Off by default: the summary runs on the personal seat, and client data
	// from work sessions must not leave the work seat without the Board's say.
	IncludeWork bool
	// WorkOrgs are organizations whose task text is work data (default "Managed Solution").
	WorkOrgs []string
	// IsWorkRepo reports whether a repo path holds work data (default bridge.IsWorkRepo).
	IsWorkRepo func(string) bool
}

func (o *CorpusOptions) defaults() {
	if o.MaxChars <= 0 {
		o.MaxChars = 60000
	}
	if o.MaxItemChars <= 0 {
		o.MaxItemChars = 600
	}
	if o.WorkOrgs == nil {
		o.WorkOrgs = []string{"Managed Solution"}
	}
	if o.IsWorkRepo == nil {
		o.IsWorkRepo = bridge.IsWorkRepo
	}
}

// workFilter decides what is work data when IncludeWork is off. It fails
// closed: text it cannot attribute to a non-work task is treated as work.
type workFilter struct {
	include bool
	orgs    map[string]bool
	isRepo  func(string) bool
	// taskWork caches task id -> is work (org or repo).
	taskWork map[string]bool
	mesh     *sql.DB
}

func newWorkFilter(mesh *sql.DB, opts CorpusOptions) *workFilter {
	w := &workFilter{include: opts.IncludeWork, orgs: map[string]bool{}, isRepo: opts.IsWorkRepo,
		taskWork: map[string]bool{}, mesh: mesh}
	for _, o := range opts.WorkOrgs {
		w.orgs[strings.ToLower(o)] = true
	}
	return w
}

func (w *workFilter) org(org string) bool { return w.orgs[strings.ToLower(org)] }

// task reports whether a task's text is work data; unknown tasks count as work.
func (w *workFilter) task(id string) bool {
	if id == "" {
		return true
	}
	if v, ok := w.taskWork[id]; ok {
		return v
	}
	v := true
	if w.mesh != nil {
		var org, repo string
		if w.mesh.QueryRow(`SELECT COALESCE(organization,''), repo_path FROM tasks WHERE id = ?`, id).Scan(&org, &repo) == nil {
			v = w.org(org) || (repo != "" && w.isRepo(repo))
		}
	}
	w.taskWork[id] = v
	return v
}

// skip reports whether an item must stay out of a personal-seat corpus.
func (w *workFilter) skip(taskID string) bool { return !w.include && w.task(taskID) }

// Corpus is the sampled, size-capped evidence for one period.
type Corpus struct {
	Items     []Item `json:"items"`
	Chars     int    `json:"chars"`
	Dropped   int    `json:"dropped"`
	Truncated bool   `json:"truncated"`
}

// ByID indexes the items.
func (c *Corpus) ByID() map[string]Item {
	m := make(map[string]Item, len(c.Items))
	for _, it := range c.Items {
		m[it.ID] = it
	}
	return m
}

type bucket struct {
	name  string
	items []Item
}

// BuildCorpus samples Board comments, block reasons, run failures, decisions
// and typed user messages from archived transcripts. Each bucket gets a fair
// share of the budget and is sampled evenly over time.
func BuildCorpus(src Sources, archiveDir string, f *Facts, opts CorpusOptions) (*Corpus, error) {
	opts.defaults()
	since := sqliteTime(f.Since)
	wf := newWorkFilter(src.Mesh, opts)
	var buckets []bucket
	if src.Mesh != nil {
		buckets = append(buckets,
			bucket{"board comments", meshItems(src.Mesh, `SELECT c.id, c.created_at, c.message, c.task_id, t.name
				FROM task_comments c JOIN tasks t ON t.id = c.task_id
				WHERE c.author IN ('board','user') AND c.created_at >= ? ORDER BY c.created_at`, since, "comment", wf)},
			bucket{"blocked reasons", meshItems(src.Mesh, `SELECT id, updated_at, block_reason, id, name
				FROM tasks WHERE COALESCE(block_reason,'') != '' AND updated_at >= ? AND status != 'soft_deleted' ORDER BY updated_at`, since, "blocked", wf)},
			bucket{"run failures", meshItems(src.Mesh, `SELECT e.id, e.created_at, e.stderr_tail, COALESCE(e.task_id,''), COALESCE(t.name,'')
				FROM run_errors e LEFT JOIN tasks t ON t.id = e.task_id WHERE e.created_at >= ? ORDER BY e.created_at`, since, "run_error", wf)},
			bucket{"decisions", meshItems(src.Mesh, `SELECT d.id, d.created_at,
					d.subject_kind || ' ' || d.subject_id || ': ' || d.final_decision || ' (advisor said ' || d.recommendation || ': ' || d.reason || ')',
					CASE WHEN d.subject_id LIKE 'task-%' THEN d.subject_id ELSE '' END, COALESCE(t.name,'')
				FROM decision_log d LEFT JOIN tasks t ON t.id = d.subject_id
				WHERE d.final_decision != '' AND d.created_at >= ? ORDER BY d.created_at`, since, "decision", wf)},
		)
	}
	if src.Archive != nil {
		buckets = append(buckets, bucket{"transcripts", transcriptItems(src.Archive, f.Since, opts, wf)})
	}
	return assemble(buckets, opts), nil
}

func meshItems(db *sql.DB, q, since, kind string, wf *workFilter) []Item {
	rows, err := db.Query(q, since)
	if err != nil {
		return nil
	}
	type row struct{ id, at, text, taskID, name string }
	var all []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.at, &r.text, &r.taskID, &r.name) == nil {
			all = append(all, r)
		}
	}
	rows.Close()
	// Filter after closing rows: the filter queries the same single-connection DB.
	var out []Item
	for _, r := range all {
		id, at, text, taskID, name := r.id, r.at, r.text, r.taskID, r.name
		if wf.skip(taskID) {
			continue
		}
		ref := Ref{Kind: kind, ID: id, Label: name}
		if taskID != "" {
			ref.URL = TaskURL(taskID)
			if kind == "comment" {
				ref.URL += "#comment-" + id
			}
		}
		out = append(out, Item{At: at, Kind: kind, Text: text, Ref: ref})
	}
	return out
}

// transcriptItems reads up to 3 typed user messages from each sampled session.
func transcriptItems(db *sql.DB, since time.Time, opts CorpusOptions, wf *workFilter) []Item {
	q := `SELECT source, rel_path, archive_path, started_at, task_id, repo, cwd FROM transcripts
		WHERE (started_at >= ? OR ended_at >= ?) AND user_msgs > 0`
	if !opts.IncludeWork {
		q += ` AND profile != 'work'`
	}
	q += ` ORDER BY started_at`
	s := since.UTC().Format(time.RFC3339)
	rows, err := db.Query(q, s, s)
	if err != nil {
		return nil
	}
	type sess struct{ source, rel, path, at, task, repo, cwd string }
	var all []sess
	for rows.Next() {
		var x sess
		if rows.Scan(&x.source, &x.rel, &x.path, &x.at, &x.task, &x.repo, &x.cwd) != nil {
			continue
		}
		// A personal-profile session can still be in a work repo or on a
		// work task; without a repo it cannot be attributed, so it stays out.
		if !opts.IncludeWork && (x.repo == "" || opts.IsWorkRepo(x.repo) || opts.IsWorkRepo(x.cwd) ||
			(x.task != "" && wf.task(x.task))) {
			continue
		}
		all = append(all, x)
	}
	rows.Close()
	// Opening every archived file is the slow part: sample at most 120 sessions evenly.
	var out []Item
	for _, x := range evenSample(len(all), 120) {
		s := all[x]
		msgs := userMessages(s.path, 3)
		for _, m := range msgs {
			ref := Ref{Kind: "transcript", ID: fmt.Sprintf("%s/%s#L%d", s.source, s.rel, m.line), Label: s.task}
			if s.task != "" {
				ref.URL = TaskURL(s.task)
			}
			out = append(out, Item{At: s.at, Kind: "user_message", Text: m.text, Ref: ref})
		}
	}
	return out
}

type userMsg struct {
	line int
	text string
}

func userMessages(path string, limit int) []userMsg {
	rc, err := archive.OpenTranscript(path)
	if err != nil {
		return nil
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 256<<10), 8<<20)
	var out []userMsg
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"type":"user"`) {
			continue
		}
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "user" {
			continue
		}
		text := contentText(l.Message.Content)
		// Skip harness boilerplate (system reminders, command wrappers).
		if text == "" || strings.HasPrefix(text, "<") || strings.HasPrefix(text, "Caveat:") {
			continue
		}
		out = append(out, userMsg{line: n, text: text})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// evenSample returns up to k indexes spread evenly over [0, n).
func evenSample(n, k int) []int {
	if n <= k {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, k)
	for i := range out {
		out[i] = i * n / k
	}
	return out
}

func assemble(buckets []bucket, opts CorpusOptions) *Corpus {
	c := &Corpus{}
	var live []bucket
	for _, b := range buckets {
		if len(b.items) > 0 {
			live = append(live, b)
		}
	}
	if len(live) == 0 {
		return c
	}
	share := opts.MaxChars / len(live)
	var picked []Item
	for _, b := range live {
		used := 0
		// Estimate how many fit, then sample that many evenly over time.
		per := min(opts.MaxItemChars, avgLen(b.items))
		k := max(1, share/max(per, 1))
		idx := evenSample(len(b.items), k)
		c.Dropped += len(b.items) - len(idx)
		for _, i := range idx {
			it := b.items[i]
			it.Text = clip(security.Redact(strings.TrimSpace(it.Text)), opts.MaxItemChars)
			if used+len(it.Text) > share {
				c.Dropped++
				c.Truncated = true
				continue
			}
			used += len(it.Text)
			picked = append(picked, it)
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].At < picked[j].At })
	for i := range picked {
		picked[i].ID = "e" + strconv.Itoa(i+1)
		c.Chars += len(picked[i].Text)
	}
	c.Items = picked
	if c.Dropped > 0 {
		c.Truncated = true
	}
	return c
}

func avgLen(items []Item) int {
	if len(items) == 0 {
		return 0
	}
	n := 0
	for _, it := range items {
		n += len(it.Text)
	}
	return n / len(items)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
