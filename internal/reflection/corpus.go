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
}

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
	workOrg := map[string]bool{}
	for _, o := range opts.WorkOrgs {
		workOrg[strings.ToLower(o)] = true
	}
	var buckets []bucket
	if src.Mesh != nil {
		buckets = append(buckets,
			bucket{"board comments", meshItems(src.Mesh, `SELECT c.id, c.created_at, c.message, c.task_id, t.name, COALESCE(t.organization,'')
				FROM task_comments c JOIN tasks t ON t.id = c.task_id
				WHERE c.author IN ('board','user') AND c.created_at >= ? ORDER BY c.created_at`, since, "comment", workOrg, opts.IncludeWork)},
			bucket{"blocked reasons", meshItems(src.Mesh, `SELECT id, updated_at, block_reason, id, name, COALESCE(organization,'')
				FROM tasks WHERE COALESCE(block_reason,'') != '' AND updated_at >= ? AND status != 'soft_deleted' ORDER BY updated_at`, since, "blocked", workOrg, opts.IncludeWork)},
			bucket{"run failures", meshItems(src.Mesh, `SELECT e.id, e.created_at, e.stderr_tail, COALESCE(e.task_id,''), COALESCE(t.name,''), COALESCE(t.organization,'')
				FROM run_errors e LEFT JOIN tasks t ON t.id = e.task_id WHERE e.created_at >= ? ORDER BY e.created_at`, since, "run_error", workOrg, opts.IncludeWork)},
			bucket{"decisions", meshItems(src.Mesh, `SELECT d.id, d.created_at,
					d.subject_kind || ' ' || d.subject_id || ': ' || d.final_decision || ' (advisor said ' || d.recommendation || ': ' || d.reason || ')',
					CASE WHEN d.subject_id LIKE 'task-%' THEN d.subject_id ELSE '' END, COALESCE(t.name,''), COALESCE(t.organization,'')
				FROM decision_log d LEFT JOIN tasks t ON t.id = d.subject_id
				WHERE d.final_decision != '' AND d.created_at >= ? ORDER BY d.created_at`, since, "decision", workOrg, opts.IncludeWork)},
		)
	}
	if src.Archive != nil {
		buckets = append(buckets, bucket{"transcripts", transcriptItems(src.Archive, f.Since, opts)})
	}
	return assemble(buckets, opts), nil
}

func meshItems(db *sql.DB, q, since, kind string, workOrg map[string]bool, includeWork bool) []Item {
	rows, err := db.Query(q, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var id, at, text, taskID, name, org string
		if rows.Scan(&id, &at, &text, &taskID, &name, &org) != nil {
			continue
		}
		if !includeWork && workOrg[strings.ToLower(org)] {
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
func transcriptItems(db *sql.DB, since time.Time, opts CorpusOptions) []Item {
	q := `SELECT source, rel_path, archive_path, started_at, task_id FROM transcripts
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
	type sess struct{ source, rel, path, at, task string }
	var all []sess
	for rows.Next() {
		var x sess
		if rows.Scan(&x.source, &x.rel, &x.path, &x.at, &x.task) == nil {
			all = append(all, x)
		}
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
