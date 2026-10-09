package archive

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
)

// Meta is what the index records about one transcript. It is read from the
// original lines; the archived copy is the redacted text.
type Meta struct {
	SessionID   string
	CWD         string
	Repo        string
	TaskID      string
	StartedAt   string
	EndedAt     string
	Model       string
	Lines       int
	UserMsgs    int
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64

	seenMsg map[string]bool
}

var taskIDRe = regexp.MustCompile(`\btask-[0-9a-f]{8}\b`)

type metaLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Message   *struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input       int64 `json:"input_tokens"`
			Output      int64 `json:"output_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (m *Meta) observe(line []byte) {
	if len(line) == 0 {
		return
	}
	m.Lines++
	if line[0] != '{' {
		return
	}
	var l metaLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	if l.SessionID != "" && m.SessionID == "" {
		m.SessionID = l.SessionID
	}
	if l.CWD != "" && m.CWD == "" {
		m.CWD = l.CWD
	}
	if ts := normTime(l.Timestamp); ts != "" {
		if m.StartedAt == "" || ts < m.StartedAt {
			m.StartedAt = ts
		}
		if ts > m.EndedAt {
			m.EndedAt = ts
		}
	}
	if l.Message == nil {
		return
	}
	if l.Type == "user" && isHumanText(l.Message.Content) {
		m.UserMsgs++
	}
	if l.Message.Model != "" && !strings.HasPrefix(l.Message.Model, "<") {
		m.Model = l.Message.Model
	}
	if u := l.Message.Usage; u != nil {
		// Claude Code writes one line per content block, each repeating the
		// message's usage: count a message once.
		key := l.Message.ID + "|" + l.RequestID
		if key != "|" {
			if m.seenMsg == nil {
				m.seenMsg = map[string]bool{}
			}
			if m.seenMsg[key] {
				return
			}
			m.seenMsg[key] = true
		}
		m.Input += u.Input
		m.Output += u.Output
		m.CacheRead += u.CacheRead
		m.CacheCreate += u.CacheCreate
	}
}

// isHumanText reports whether a user message's content is typed text rather
// than a tool result.
func isHumanText(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '"' {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "text" {
			return true
		}
	}
	return false
}

func normTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// finish fills what the lines did not say from the source layout.
func (m *Meta) finish(src Source, rel string) {
	if m.SessionID == "" {
		m.SessionID = sessionFromPath(src.Name, rel)
	}
	m.Repo = repoRoot(m.CWD)
	if id := taskIDRe.FindString(m.CWD); id != "" {
		m.TaskID = id
	}
}

func sessionFromPath(source, rel string) string {
	switch source {
	case "agy":
		first, _, _ := strings.Cut(rel, "/")
		return first
	}
	// claude: <project>/<session>.jsonl, subagents under <project>/<session>/subagents/.
	if i := strings.Index(rel, "/subagents/"); i >= 0 {
		return path.Base(rel[:i])
	}
	return strings.TrimSuffix(strings.TrimSuffix(path.Base(rel), ".jsonl"), ".json")
}

// repoRoot maps a worktree cwd back to its repository.
func repoRoot(cwd string) string {
	for _, marker := range []string{"/.worktrees/", "/.claude/worktrees/"} {
		if i := strings.Index(cwd, marker); i >= 0 {
			return cwd[:i]
		}
	}
	return cwd
}
