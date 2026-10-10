package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// task-7d279c9d: typed ops tools. Each declares its effect (read, dev_write,
// prod_write, external_write) and validates its parameters; the server gates
// on the declaration, so no route to a tool (agent CLI, piped stdin, the
// daemon socket) skips the gate.

// ApprovalRequest asks the Board about one prod or external write.
type ApprovalRequest struct {
	Call   opstools.Call
	Reason string
	TaskID string
	CWD    string
	// GateID resumes a request held past the last wait.
	GateID string
}

// Approver asks the Board and reports whether the call may run; msg says why
// not when it may not.
type Approver func(ctx context.Context, req ApprovalRequest) (approved bool, msg string)

func opsTools() []Tool {
	str := func(d string) Property { return Property{Type: "string", Description: d} }
	num := func(d string) Property { return Property{Type: "integer", Description: d} }
	return []Tool{
		{
			Name: "dev_host_run",
			Description: "Run one fixed action on a configured dev host over ssh (BatchMode) and return redacted output. " +
				"Use this instead of `ssh <dev host> ...`. Read actions (effect read): git_status, git_log(lines), ls(path), " +
				"cat_file(path under the app dir; never env/secret files), journal_tail(service, lines), http_get_local(port, path), " +
				"systemctl_status(service). Deploy actions (effect dev_write, allowed unattended): git_ff_pull(branch), " +
				"collectstatic(app), restart(service), pip_sync(app). Hosts, services, branches and apps come from [gates.hosts] and [gates.ops] in config.toml.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"host":    str("Dev host alias from [gates.hosts] dev"),
					"action":  {Type: "string", Description: "Action to run", Enum: opstools.DevActions()},
					"app":     str("Configured app (collectstatic, pip_sync; optional for git/ls/cat_file to use that app's dir)"),
					"path":    str("ls/cat_file: path relative to (or under) the app dir; http_get_local: URL path starting with /"),
					"service": str("journal_tail/systemctl_status/restart: a configured systemd unit"),
					"branch":  str("git_ff_pull: branch to fast-forward (the checkout must already be on it)"),
					"lines":   num("git_log/journal_tail: how many lines"),
					"port":    num("http_get_local: localhost port on the dev host"),
				},
				Required: []string{"host", "action"},
			},
		},
		{
			Name: "dev_deploy_verify",
			Description: "Effect read. Run the repo's scripts/verify_dev_deploy.sh (from origin/dev-server, only when it matches origin/main " +
				"or a Board-trusted blob) and return the PASS/FAIL lines plus the final DEV DEPLOY VERIFIED / NOT ON DEV line. " +
				"Use this instead of `git show ...:scripts/verify_dev_deploy.sh | bash -s`.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"repo":        str("Absolute path of the local repo checkout (defaults to the working directory)"),
					"sha":         str("Commit that must be on dev"),
					"page_checks": {Type: "array", Description: "Page checks of the form /path=marker", Items: &Property{Type: "string"}},
				},
				Required: []string{"sha", "page_checks"},
			},
		},
		{
			Name: "staypoint_query",
			Description: "Effect read. Read your own task's StayPoint records instead of reading ~/.staypoint from a shell. " +
				"Queries: task, comments, documents (keys and versions), document(key), handoffs (file list), handoff(name), " +
				"run_steps(limit), run_errors(limit), gate_requests(limit). Output is redacted.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query":   {Type: "string", Description: "Named query", Enum: queryNames()},
					"task_id": str("Task ID (defaults to STAYPOINT_TASK_ID; a run may only read its own task)"),
					"key":     str("document: the document key"),
					"name":    str("handoff: the file name from handoffs"),
					"limit":   num("run_steps/run_errors/gate_requests: newest N rows (default 50, max 500)"),
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "task_comment",
			Description: "Effect dev_write. Post a comment on your task, passing the text directly (no temp file). It never wakes the task.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"text":    str("Comment text (markdown)"),
					"task_id": str("Task ID (defaults to STAYPOINT_TASK_ID; a run may only comment on its own task)"),
				},
				Required: []string{"text"},
			},
		},
		{
			Name:        "task_doc",
			Description: "Effect dev_write. Save a new version of a task document (e.g. plan, notes), passing the text directly.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"key":     str("Document key: lowercase letters, digits, - and _"),
					"text":    str("Document content (markdown)"),
					"task_id": str("Task ID (defaults to STAYPOINT_TASK_ID; a run may only write its own task)"),
				},
				Required: []string{"key", "text"},
			},
		},
		{
			Name:        "pr_body",
			Description: "Effect dev_write. Replace a GitHub PR's body with text passed directly (gh pr edit --body-file -), instead of staging it in /tmp.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"repo": str("Absolute path of the local repo checkout (defaults to the working directory)"),
					"pr":   num("PR number"),
					"text": str("New PR body (markdown)"),
				},
				Required: []string{"pr", "text"},
			},
		},
		{
			Name: "pr_merge",
			Description: "Merge a GitHub PR, pinned to its current head commit. Effect dev_write when the PR's base is dev-server or dev " +
				"(runs unattended); prod_write for main, prod or any other base (held for the Board; prod merges follow the night rule). " +
				"base must be the PR's real base. If a call was held past the wait, call again with approval_gate_id.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"repo":             str("Absolute path of the local repo checkout (defaults to the working directory)"),
					"pr":               num("PR number"),
					"base":             str("The PR's base branch, e.g. dev-server or main"),
					"method":           {Type: "string", Description: "Merge method (default merge)", Enum: []string{"merge", "squash", "rebase"}},
					"approval_gate_id": str("Gate request ID from an earlier held call with the same parameters"),
				},
				Required: []string{"pr", "base"},
			},
		},
	}
}

// decodeArgs unmarshals rawArgs into v, refusing parameters the tool does
// not declare.
func decodeArgs(rawArgs json.RawMessage, v any) error {
	if len(rawArgs) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(rawArgs))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (s *Server) opsRunner() opstools.Runner {
	if s.runner != nil {
		return s.runner
	}
	return opstools.ExecRunner
}

// ownTask resolves a task_id parameter: a daemon run (STAYPOINT_TASK_ID set)
// may only name its own task.
func ownTask(arg string) (string, error) {
	env := strings.TrimSpace(os.Getenv("STAYPOINT_TASK_ID"))
	arg = strings.TrimSpace(arg)
	switch {
	case env != "" && arg != "" && arg != env:
		return "", fmt.Errorf("a run may only use its own task (%s), not %s", env, arg)
	case env != "":
		return env, nil
	case arg == "":
		return "", fmt.Errorf("task_id is required (or set STAYPOINT_TASK_ID)")
	}
	return arg, nil
}

// gate applies the effect decision to c: nil lets it run, a result refuses it.
func (s *Server) gate(ctx context.Context, c opstools.Call, reason, gateID string) *ToolCallResult {
	if opstools.Decide(c.Effect) == opstools.Allow {
		return nil
	}
	if s.approver == nil {
		return toolError(fmt.Sprintf("%s is %s and needs the Board, but this MCP connection cannot ask the Board; not run", c.Tool, c.Effect))
	}
	ok, msg := s.approver(ctx, ApprovalRequest{
		Call: c, Reason: reason, TaskID: strings.TrimSpace(os.Getenv("STAYPOINT_TASK_ID")),
		CWD: s.getWorkDir(), GateID: strings.TrimSpace(gateID),
	})
	if !ok {
		return toolError(msg)
	}
	return nil
}

// logOps records the call and its declared effect on the task's activity
// log, so calls that never pass through an agent CLI are still on record.
func (s *Server) logOps(c opstools.Call) {
	taskID := strings.TrimSpace(os.Getenv("STAYPOINT_TASK_ID"))
	if taskID == "" {
		return
	}
	if dbConn, err := s.getDB(); err == nil {
		_ = meshContext.LogActivity(dbConn, taskID, "ops_tool", fmt.Sprintf("%s effect=%s %s", c.Tool, c.Effect, c.Summary))
	}
}

func opsResult(c opstools.Call, body string) *ToolCallResult {
	return toolSuccess(fmt.Sprintf("%s effect=%s %s\n%s", c.Tool, c.Effect, c.Summary, body))
}

func (s *Server) handleDevHostRun(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var req opstools.DevHostRequest
	if err := decodeArgs(rawArgs, &req); err != nil {
		return toolError(err.Error())
	}
	plan, err := opstools.PlanDevHost(s.getConfig().Gates, req)
	if err != nil {
		return toolError("dev_host_run refused: " + err.Error())
	}
	if res := s.gate(ctx, plan.Call, "", ""); res != nil {
		return res
	}
	s.logOps(plan.Call)
	return opsResult(plan.Call, opstools.RunDevHost(ctx, s.opsRunner(), plan))
}

func (s *Server) handleDevDeployVerify(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var req opstools.VerifyRequest
	if err := decodeArgs(rawArgs, &req); err != nil {
		return toolError(err.Error())
	}
	if err := opstools.ValidateVerify(&req, s.getWorkDir()); err != nil {
		return toolError("dev_deploy_verify refused: " + err.Error())
	}
	c := opstools.Call{Tool: "dev_deploy_verify", Effect: opstools.Read,
		Summary: fmt.Sprintf("repo=%s sha=%s checks=%d", req.Repo, req.SHA, len(req.PageChecks))}
	s.logOps(c)
	return opsResult(c, opstools.RunVerify(ctx, s.opsRunner(), req, s.getConfig().Gates.Ops.VerifyScriptBlobs))
}

var (
	docKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	gateIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

func (s *Server) handleTaskComment(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Text   string `json:"text"`
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(rawArgs, &args); err != nil {
		return toolError(err.Error())
	}
	taskID, err := ownTask(args.TaskID)
	if err != nil {
		return toolError(err.Error())
	}
	if err := opstools.ValidateText(args.Text); err != nil {
		return toolError(err.Error())
	}
	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}
	author := "agent"
	if os.Getenv("STAYPOINT_TASK_ID") == "" {
		if u := os.Getenv("USER"); u != "" {
			author = u
		}
	}
	if err := meshContext.AddAgentComment(dbConn, taskID, author, args.Text); err != nil {
		return toolError(fmt.Sprintf("add comment: %v", err))
	}
	c := opstools.Call{Tool: "task_comment", Effect: opstools.DevWrite, Summary: fmt.Sprintf("task=%s bytes=%d", taskID, len(args.Text))}
	s.logOps(c)
	return opsResult(c, "comment posted")
}

func (s *Server) handleTaskDoc(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Key    string `json:"key"`
		Text   string `json:"text"`
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(rawArgs, &args); err != nil {
		return toolError(err.Error())
	}
	taskID, err := ownTask(args.TaskID)
	if err != nil {
		return toolError(err.Error())
	}
	if !docKeyRe.MatchString(args.Key) {
		return toolError("key must be 1-64 lowercase letters, digits, - or _")
	}
	if err := opstools.ValidateText(args.Text); err != nil {
		return toolError(err.Error())
	}
	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}
	if _, err := meshContext.GetTask(dbConn, taskID); err != nil {
		return toolError(fmt.Sprintf("task not found: %v", err))
	}
	if err := meshContext.AddTaskDocument(dbConn, taskID, args.Key, args.Text); err != nil {
		return toolError(fmt.Sprintf("save document: %v", err))
	}
	c := opstools.Call{Tool: "task_doc", Effect: opstools.DevWrite, Summary: fmt.Sprintf("task=%s key=%s bytes=%d", taskID, args.Key, len(args.Text))}
	s.logOps(c)
	return opsResult(c, "document saved")
}

func (s *Server) handlePRBody(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Repo string `json:"repo"`
		PR   int    `json:"pr"`
		Text string `json:"text"`
	}
	if err := decodeArgs(rawArgs, &args); err != nil {
		return toolError(err.Error())
	}
	repo, err := opstools.RepoDir(args.Repo, s.getWorkDir())
	if err != nil {
		return toolError(err.Error())
	}
	if args.PR <= 0 {
		return toolError("pr must be a positive PR number")
	}
	if err := opstools.ValidateText(args.Text); err != nil {
		return toolError(err.Error())
	}
	c := opstools.Call{Tool: "pr_body", Effect: opstools.DevWrite, Summary: fmt.Sprintf("repo=%s pr=%d bytes=%d", repo, args.PR, len(args.Text))}
	s.logOps(c)
	res := s.opsRunner()(ctx, opstools.Cmd{Name: "gh", Args: []string{"pr", "edit", strconv.Itoa(args.PR), "--body-file", "-"}, Dir: repo, Stdin: []byte(args.Text)})
	return opsResult(c, res.Format())
}

func (s *Server) handlePRMerge(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		opstools.PRMergeRequest
		ApprovalGateID string `json:"approval_gate_id"`
	}
	if err := decodeArgs(rawArgs, &args); err != nil {
		return toolError(err.Error())
	}
	req := args.PRMergeRequest
	if args.ApprovalGateID != "" && !gateIDRe.MatchString(args.ApprovalGateID) {
		return toolError("approval_gate_id is not a gate request ID")
	}
	if err := opstools.ValidatePRMerge(&req, s.getWorkDir()); err != nil {
		return toolError("pr_merge refused: " + err.Error())
	}
	run := s.opsRunner()
	view := func() (*opstools.PRMergePlan, *ToolCallResult) {
		res := run(ctx, opstools.Cmd{Name: "gh", Args: opstools.PRViewArgs(req.PR), Dir: req.Repo})
		if res.ExitCode != 0 || res.Err != nil {
			return nil, toolError("pr_merge: gh pr view failed\n" + res.Format())
		}
		plan, err := opstools.PlanPRMerge(req, []byte(res.Stdout))
		if err != nil {
			return nil, toolError("pr_merge refused: " + err.Error())
		}
		return plan, nil
	}
	plan, bad := view()
	if bad != nil {
		return bad
	}
	reason := fmt.Sprintf("%s: merge PR #%d into %s of %s (head %s)", plan.Effect, plan.PR, req.Base, plan.Repo, plan.HeadSHA[:12])
	if plan.Effect == opstools.ProdWrite {
		reason += "; night rule: prod merges only at night unless urgent"
	}
	if res := s.gate(ctx, plan.Call, reason, args.ApprovalGateID); res != nil {
		return res
	}
	// The Board may have taken a while: merge only what it saw.
	again, bad := view()
	if bad != nil {
		return bad
	}
	if again.Summary != plan.Summary {
		return toolError(fmt.Sprintf("pr_merge refused: the PR changed while held (was %s, now %s); not merged", plan.Summary, again.Summary))
	}
	s.logOps(plan.Call)
	return opsResult(plan.Call, run(ctx, opstools.Cmd{Name: "gh", Args: plan.MergeArgs(), Dir: plan.Repo}).Format())
}
