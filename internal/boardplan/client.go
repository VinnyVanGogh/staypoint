package boardplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDaemonURL is where staypointd listens.
const DefaultDaemonURL = "http://127.0.0.1:41421"

// ErrAgentRun is returned when a daemon agent run tries to propose a plan.
var ErrAgentRun = errors.New("refused: a daemon agent run (STAYPOINT_TASK_ID is set) cannot propose Board action plans; only the Board's own terminal or the web UI can")

// RefuseInAgentRun refuses inside a daemon run. The daemon sets
// STAYPOINT_TASK_ID on every run it starts (orchestrator/harness.go); the
// Board's interactive session never has it.
func RefuseInAgentRun(getenv func(string) string) error {
	if strings.TrimSpace(getenv("STAYPOINT_TASK_ID")) != "" {
		return ErrAgentRun
	}
	return nil
}

// ParseActions reads a plan file: {"actions":[...]} or a bare array.
func ParseActions(data []byte) ([]Action, error) {
	trimmed := bytes.TrimSpace(data)
	var actions []Action
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &actions); err != nil {
			return nil, fmt.Errorf("parse plan: %w", err)
		}
	} else {
		var wrapped struct {
			Actions []Action `json:"actions"`
		}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wrapped); err != nil {
			return nil, fmt.Errorf("parse plan: %w", err)
		}
		actions = wrapped.Actions
	}
	if err := Validate(actions); err != nil {
		return nil, err
	}
	return actions, nil
}

// Proposer names the proposing session for the review page: the Claude Code
// session id when there is one, else the terminal process.
func Proposer(getenv func(string) string, explicit string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if id := strings.TrimSpace(getenv("CLAUDE_CODE_SESSION_ID")); id != "" {
		return "claude-session:" + id
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("terminal:%s:pid-%d", host, os.Getppid())
}

// Credentials are what a Board terminal sends to propose.
type Credentials struct {
	DaemonURL  string
	AuthToken  string
	BoardToken string
}

// LoadCredentials reads the auth and board tokens staypointd persists in
// dataDir.
func LoadCredentials(dataDir, daemonURL string) (Credentials, error) {
	c := Credentials{DaemonURL: daemonURL}
	if c.DaemonURL == "" {
		c.DaemonURL = DefaultDaemonURL
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"auth_token", &c.AuthToken}, {"board_token", &c.BoardToken}} {
		b, err := os.ReadFile(filepath.Join(dataDir, f.name))
		if err != nil {
			return c, fmt.Errorf("read %s: %w", f.name, err)
		}
		*f.dst = strings.TrimSpace(string(b))
		if len(*f.dst) < 16 {
			return c, fmt.Errorf("%s in %s is too short or invalid", f.name, dataDir)
		}
	}
	return c, nil
}

// Proposed is the daemon's reply to a proposal.
type Proposed struct {
	Plan *Plan  `json:"plan"`
	URL  string `json:"url"`
}

// ReviewURL is the page the Board signs on. localhost, not 127.0.0.1: the
// passkey's RP ID is "localhost".
func (p *Proposed) ReviewURL(daemonURL string) string {
	u, err := url.Parse(daemonURL)
	if err != nil || u.Port() == "" {
		return "http://localhost" + p.URL
	}
	return "http://localhost:" + u.Port() + p.URL
}

// Propose sends a plan to the daemon. Callers run RefuseInAgentRun first.
func Propose(ctx context.Context, c Credentials, proposer string, actions []Action) (*Proposed, error) {
	body, err := json.Marshal(map[string]any{"proposer": proposer, "actions": actions})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.DaemonURL, "/")+"/api/board/plans", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	req.Header.Set("X-Board-Token", c.BoardToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon request failed: %w (is staypointd running?)", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out Proposed
	if err := json.Unmarshal(raw, &out); err != nil || out.Plan == nil {
		return nil, fmt.Errorf("unexpected daemon reply: %s", raw)
	}
	return &out, nil
}
