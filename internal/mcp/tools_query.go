package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// queryNames are staypoint_query's named queries. There is no free-form SQL.
func queryNames() []string {
	return []string{"task", "comments", "documents", "document", "handoffs", "handoff", "run_steps", "run_errors", "gate_requests"}
}

var handoffNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

const (
	defaultQueryLimit = 50
	maxQueryLimit     = 500
	maxHandoffBytes   = 512 << 10
)

func (s *Server) handleStaypointQuery(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Query  string `json:"query"`
		TaskID string `json:"task_id"`
		Key    string `json:"key"`
		Name   string `json:"name"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(rawArgs, &args); err != nil {
		return toolError(err.Error())
	}
	if !slices.Contains(queryNames(), args.Query) {
		return toolError(fmt.Sprintf("query must be one of %s", strings.Join(queryNames(), ", ")))
	}
	taskID, err := ownTask(args.TaskID)
	if err != nil {
		return toolError(err.Error())
	}
	limit := args.Limit
	switch {
	case limit == 0:
		limit = defaultQueryLimit
	case limit < 0 || limit > maxQueryLimit:
		return toolError(fmt.Sprintf("limit must be 1-%d", maxQueryLimit))
	}

	var out any
	if args.Query == "handoffs" || args.Query == "handoff" {
		out, err = s.queryHandoffs(taskID, args.Query, args.Name)
	} else {
		out, err = s.queryDB(taskID, args.Query, args.Key, limit)
	}
	if err != nil {
		return toolError(fmt.Sprintf("staypoint_query %s: %v", args.Query, err))
	}
	text, ok := out.(string)
	if !ok {
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return toolError(fmt.Sprintf("json marshal error: %v", err))
		}
		text = string(data)
	}
	return toolSuccess(opstools.Redact(text))
}

func (s *Server) queryDB(taskID, query, key string, limit int) (any, error) {
	dbConn, err := s.getDB()
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	task, err := meshContext.GetTask(dbConn, taskID)
	if err != nil {
		return nil, fmt.Errorf("task not found: %w", err)
	}
	switch query {
	case "task":
		return map[string]any{
			"id": task.ID, "name": task.Name, "org": task.Organization, "project": task.Project,
			"repo_path": task.RepoPath, "git_branch": task.GitBranch, "execution_stage": task.ExecutionStage,
			"description": task.Description,
		}, nil
	case "comments":
		return meshContext.GetTaskComments(dbConn, task.ID)
	case "documents":
		docs, err := meshContext.ListTaskDocuments(dbConn, task.ID)
		if err != nil {
			return nil, err
		}
		type docRef struct {
			Key       string `json:"key"`
			Version   int    `json:"version"`
			CreatedAt string `json:"created_at"`
			Bytes     int    `json:"bytes"`
		}
		refs := make([]docRef, 0, len(docs))
		for _, d := range docs {
			refs = append(refs, docRef{d.DocKey, d.Version, d.CreatedAt, len(d.Content)})
		}
		return refs, nil
	case "document":
		if !docKeyRe.MatchString(key) {
			return nil, fmt.Errorf("key must be a document key")
		}
		d, err := meshContext.GetLatestTaskDocument(dbConn, task.ID, key)
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, fmt.Errorf("no document %q", key)
		}
		return d, nil
	case "run_steps":
		steps, err := meshContext.ListRunStepsByTask(dbConn, task.ID)
		if err != nil {
			return nil, err
		}
		if len(steps) > limit {
			steps = steps[len(steps)-limit:]
		}
		return steps, nil
	case "run_errors":
		return meshContext.ListRunErrorsByTask(dbConn, task.ID, limit)
	case "gate_requests":
		rows, err := dbConn.Query(`SELECT id, cmdline, reasons_json, status, created_at, COALESCE(decided_at, ''), decided_by
			FROM security_gate_requests WHERE task_id = ? ORDER BY created_at DESC LIMIT ?`, task.ID, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		type gateRow struct {
			ID        string          `json:"id"`
			Cmdline   string          `json:"cmdline"`
			Reasons   json.RawMessage `json:"reasons"`
			Status    string          `json:"status"`
			CreatedAt string          `json:"created_at"`
			DecidedAt string          `json:"decided_at,omitempty"`
			DecidedBy string          `json:"decided_by,omitempty"`
		}
		out := []gateRow{}
		for rows.Next() {
			var g gateRow
			var reasons string
			if err := rows.Scan(&g.ID, &g.Cmdline, &reasons, &g.Status, &g.CreatedAt, &g.DecidedAt, &g.DecidedBy); err != nil {
				return nil, err
			}
			g.Reasons = json.RawMessage(reasons)
			if !json.Valid(g.Reasons) {
				g.Reasons = json.RawMessage(`[]`)
			}
			out = append(out, g)
		}
		return out, rows.Err()
	}
	return nil, fmt.Errorf("unknown query")
}

// queryHandoffs lists or reads the task's own handoff files. A file is read
// only when it is a regular file directly in the task's handoff dir, never
// through a symlink.
func (s *Server) queryHandoffs(taskID, query, name string) (any, error) {
	cfg := s.getConfig()
	if cfg == nil || cfg.DataDir == "" {
		return nil, fmt.Errorf("no data dir configured")
	}
	dir := filepath.Join(cfg.DataDir, "handoffs", taskID)
	if query == "handoffs" {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, e := range entries {
			if e.Type().IsRegular() {
				names = append(names, e.Name())
			}
		}
		return names, nil
	}
	if !handoffNameRe.MatchString(name) {
		return nil, fmt.Errorf("name must be a file name from the handoffs query")
	}
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if fi.Size() > maxHandoffBytes {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", name, fi.Size(), maxHandoffBytes)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}
