package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "Test Agent")
	runCmd(t, dir, "git", "config", "user.email", "agent@mesh.local")
	initFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(initFile, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}
	runCmd(t, dir, "git", "add", "README.md")
	runCmd(t, dir, "git", "commit", "-m", "initial commit")
	return dir
}

func runCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v (output: %s)", name, args, err, string(out))
	}
}

func setupTestDB(t *testing.T) (*db.Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mesh.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store, store.DB()
}

func sendRequest(t *testing.T, s *Server, req Request) Response {
	t.Helper()
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request error: %v", err)
	}

	respBytes, err := s.HandleMessage(context.Background(), reqBytes)
	if err != nil {
		t.Fatalf("handle message error: %v", err)
	}
	if len(respBytes) == 0 {
		return Response{}
	}

	var resp Response
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("unmarshal response error: %v (raw: %s)", err, string(respBytes))
	}
	return resp
}

func makeRawID(id any) *json.RawMessage {
	b, _ := json.Marshal(id)
	raw := json.RawMessage(b)
	return &raw
}

func TestInitialize(t *testing.T) {
	s := NewServer()
	defer s.Close()

	req := Request{
		JSONRPC: "2.0",
		ID:      makeRawID(1),
		Method:  "initialize",
		Params:  json.RawMessage(`{}`),
	}

	resp := sendRequest(t, s, req)
	if resp.Error != nil {
		t.Fatalf("expected no error, got: %v", resp.Error)
	}

	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result error: %v", err)
	}

	var initRes InitializeResult
	if err := json.Unmarshal(data, &initRes); err != nil {
		t.Fatalf("unmarshal init result error: %v", err)
	}

	if initRes.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocol version 2024-11-05, got: %s", initRes.ProtocolVersion)
	}
	if initRes.ServerInfo.Name != "staypoint" {
		t.Errorf("expected server name staypoint, got: %s", initRes.ServerInfo.Name)
	}
	if initRes.ServerInfo.Version != "0.1.0" {
		t.Errorf("expected server version 0.1.0, got: %s", initRes.ServerInfo.Version)
	}
	if initRes.Capabilities.Tools == nil {
		t.Errorf("expected tools capability present")
	}
}

func TestNotificationsInitialized(t *testing.T) {
	s := NewServer()
	defer s.Close()

	// Notification without ID should return empty response
	req := Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	reqBytes, _ := json.Marshal(req)
	respBytes, err := s.HandleMessage(context.Background(), reqBytes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respBytes) != 0 {
		t.Errorf("expected no response bytes for notification, got: %s", string(respBytes))
	}

	// Request with ID should acknowledge cleanly
	reqWithID := Request{
		JSONRPC: "2.0",
		ID:      makeRawID("note-1"),
		Method:  "notifications/initialized",
	}
	resp := sendRequest(t, s, reqWithID)
	if resp.Error != nil {
		t.Errorf("expected no error on initialized notification with ID: %v", resp.Error)
	}
}

func TestPing(t *testing.T) {
	s := NewServer()
	defer s.Close()

	req := Request{
		JSONRPC: "2.0",
		ID:      makeRawID("ping-test"),
		Method:  "ping",
	}

	resp := sendRequest(t, s, req)
	if resp.Error != nil {
		t.Fatalf("expected no error on ping, got: %v", resp.Error)
	}

	resMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result on ping, got: %T", resp.Result)
	}
	if len(resMap) != 0 {
		t.Errorf("expected empty object for ping result, got: %v", resMap)
	}
}

func TestToolsList(t *testing.T) {
	s := NewServer()
	defer s.Close()

	req := Request{
		JSONRPC: "2.0",
		ID:      makeRawID(10),
		Method:  "tools/list",
	}

	resp := sendRequest(t, s, req)
	if resp.Error != nil {
		t.Fatalf("expected no error on tools/list: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var listRes ListToolsResult
	if err := json.Unmarshal(data, &listRes); err != nil {
		t.Fatalf("failed to unmarshal tools list: %v", err)
	}

	expectedTools := map[string][]string{
		"staypoint_checkpoint":    {"message", "session_id"},
		"staypoint_undo":          {"checkpoint_id", "dry_run", "keep_untracked", "clean_ignored"},
		"staypoint_wire_post":     {"content", "channel", "ttl_seconds"},
		"staypoint_wire_list":     {"channel", "limit"},
		"staypoint_task_list":     {"all"},
		"staypoint_condense":      {"raw_text", "format", "max_lines"},
		"staypoint_status":        {},
		"staypoint_ship_review":   {"test_steps", "dev_url", "check_runs"},
	}

	foundTools := make(map[string]Tool)
	for _, tool := range listRes.Tools {
		foundTools[tool.Name] = tool
	}

	for toolName, expectedProps := range expectedTools {
		tool, exists := foundTools[toolName]
		if !exists {
			t.Errorf("missing tool in tools/list: %s", toolName)
			continue
		}
		if tool.Description == "" {
			t.Errorf("tool %s missing description", toolName)
		}
		for _, prop := range expectedProps {
			if _, propExists := tool.InputSchema.Properties[prop]; !propExists {
				t.Errorf("tool %s missing property %s", toolName, prop)
			}
		}
	}
}

func parseToolCallResult(t *testing.T, resp Response) ToolCallResult {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %v", resp.Error)
	}
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("failed to marshal result: %v", err)
	}
	var res ToolCallResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("failed to unmarshal tool call result: %v (raw: %s)", err, string(data))
	}
	return res
}

func TestToolCallCheckpointAndUndo(t *testing.T) {
	gitDir := setupTestGitRepo(t)
	s := NewServer(WithWorkDir(gitDir))
	defer s.Close()

	// 1. Modify a file
	modFile := filepath.Join(gitDir, "README.md")
	if err := os.WriteFile(modFile, []byte("# Test Repo\nModified line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Call staypoint_checkpoint
	cpArgs, _ := json.Marshal(map[string]any{
		"message":    "test micro-checkpoint",
		"session_id": "test-session-123",
	})
	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_checkpoint",
		Arguments: cpArgs,
	})

	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("call-cp"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("checkpoint returned error: %s", res.Content[0].Text)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "commit_sha") {
		t.Fatalf("expected commit_sha in checkpoint output: %v", res.Content)
	}

	// 3. Make another modification
	if err := os.WriteFile(modFile, []byte("# Test Repo\nSecond modification\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 4. Test staypoint_undo dry-run
	undoDryArgs, _ := json.Marshal(map[string]any{
		"dry_run":       true,
		"clean_ignored": false,
	})
	undoDryParams, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_undo",
		Arguments: undoDryArgs,
	})
	undoDryResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("call-undo-dry"),
		Method:  "tools/call",
		Params:  undoDryParams,
	})
	undoDryRes := parseToolCallResult(t, undoDryResp)
	if undoDryRes.IsError {
		t.Fatalf("undo dry-run returned error: %s", undoDryRes.Content[0].Text)
	}
	if !strings.Contains(undoDryRes.Content[0].Text, "files_reverted") {
		t.Errorf("expected files_reverted in dry run output: %s", undoDryRes.Content[0].Text)
	}

	// 5. Test staypoint_undo actual restore
	undoArgs, _ := json.Marshal(map[string]any{
		"dry_run":        false,
		"keep_untracked": true,
		"clean_ignored":  true,
	})
	undoParams, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_undo",
		Arguments: undoArgs,
	})
	undoResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("call-undo"),
		Method:  "tools/call",
		Params:  undoParams,
	})
	undoRes := parseToolCallResult(t, undoResp)
	if undoRes.IsError {
		t.Fatalf("undo actual restore returned error: %s", undoRes.Content[0].Text)
	}

	content, _ := os.ReadFile(modFile)
	if !strings.Contains(string(content), "Modified line") {
		t.Errorf("file content was not restored to checkpoint state: %s", string(content))
	}
}

func TestToolCallWirePostAndList(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	// 1. Post valid wire message
	postArgs, _ := json.Marshal(map[string]any{
		"content":     "Running database migration",
		"channel":     "dev-sync",
		"ttl_seconds": 3600,
	})
	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_wire_post",
		Arguments: postArgs,
	})
	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("post-1"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("expected wire_post success, got error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "Running database migration") {
		t.Errorf("expected content in wire_post output: %s", res.Content[0].Text)
	}

	// 2. Post empty content which should fail validation
	emptyPostArgs, _ := json.Marshal(map[string]any{
		"content": "   ",
	})
	emptyParams, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_wire_post",
		Arguments: emptyPostArgs,
	})
	emptyResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("post-empty"),
		Method:  "tools/call",
		Params:  emptyParams,
	})
	emptyRes := parseToolCallResult(t, emptyResp)
	if !emptyRes.IsError {
		t.Errorf("expected wire_post with empty content to return isError=true")
	}

	// 3. List messages on channel
	listArgs, _ := json.Marshal(map[string]any{
		"channel": "dev-sync",
		"limit":   10,
	})
	listParams, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_wire_list",
		Arguments: listArgs,
	})
	listResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("list-1"),
		Method:  "tools/call",
		Params:  listParams,
	})
	listRes := parseToolCallResult(t, listResp)
	if listRes.IsError {
		t.Fatalf("wire_list returned error: %s", listRes.Content[0].Text)
	}
	if !strings.Contains(listRes.Content[0].Text, "Running database migration") {
		t.Errorf("expected message in wire_list output: %s", listRes.Content[0].Text)
	}
}

func TestToolCallTaskList(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	// Seed task
	_, err := meshContext.CreateTask(database, "Refactor MCP Subsystem", "/tmp/repo", "main", "personal")
	if err != nil {
		t.Fatalf("failed to seed task: %v", err)
	}

	args, _ := json.Marshal(map[string]any{
		"all": true,
	})
	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_task_list",
		Arguments: args,
	})

	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("task-list-1"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("task_list returned error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "Refactor MCP Subsystem") {
		t.Errorf("expected task name in task_list output: %s", res.Content[0].Text)
	}
}

func TestToolCallCondense(t *testing.T) {
	s := NewServer()
	defer s.Close()

	rawLog := `main.go:14:2: undefined: nonExistentFunc
main.go:15:2: undefined: anotherVar
main.go:16:2: cannot use 42 as string
`
	args, _ := json.Marshal(map[string]any{
		"raw_text":  rawLog,
		"format":    "go",
		"max_lines": 50,
	})
	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_condense",
		Arguments: args,
	})

	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("condense-1"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("condense returned error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "undefined: nonExistentFunc") {
		t.Errorf("expected condensed error in output: %s", res.Content[0].Text)
	}

	// Missing raw_text should return error
	missingArgs, _ := json.Marshal(map[string]any{
		"raw_text": "",
	})
	missingParams, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_condense",
		Arguments: missingArgs,
	})
	missingResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("condense-missing"),
		Method:  "tools/call",
		Params:  missingParams,
	})
	missingRes := parseToolCallResult(t, missingResp)
	if !missingRes.IsError {
		t.Errorf("expected error for empty raw_text")
	}
}

func TestToolCallStatus(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_status",
		Arguments: json.RawMessage(`{}`),
	})

	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("status-1"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("status returned error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "pacer_state") {
		t.Errorf("expected pacer_state in status output: %s", res.Content[0].Text)
	}
}

func TestToolCallShipReview(t *testing.T) {
	_, database := setupTestDB(t)
	repoDir := setupTestGitRepo(t)

	// Insert a task.
	_, err := database.Exec(`
		INSERT INTO tasks (id, name, repo_path, git_branch, status, execution_stage,
		                   account_role, max_budget_usd, max_turns, work_kind)
		VALUES ('sr-mcp-task', 'Ship Review MCP test', ?, 'feature/mcp-test', 'active', 'in_progress',
		        'personal', 0, 0, 'coding')`, repoDir)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// Create the harness branch staypoint/<taskID> so BuildAndStartCard can resolve HEAD.
	// git_branch="feature/mcp-test" stays in the DB to confirm BuildAndStartCard ignores it.
	// The daemon records the task's base when it makes the worktree (STA-774).
	baseOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if err := workspace.RecordTaskBase(context.Background(), database, repoDir, "sr-mcp-task", strings.TrimSpace(string(baseOut))); err != nil {
		t.Fatalf("RecordTaskBase: %v", err)
	}
	runCmd(t, repoDir, "git", "checkout", "-b", "staypoint/sr-mcp-task")
	featureFile := filepath.Join(repoDir, "feature.txt")
	if err := os.WriteFile(featureFile, []byte("feature\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repoDir, "git", "add", "feature.txt")
	runCmd(t, repoDir, "git", "commit", "-m", "add feature")

	s := NewServer(WithDB(database), WithWorkDir(repoDir))
	defer s.Close()

	args := map[string]string{
		"task_id":    "sr-mcp-task",
		"test_steps": `["1. Run go test ./...","2. Verify the new endpoint returns 200"]`,
		"check_runs": `[{"command":"go test ./...","exit_code":0,"output_tail":"ok  ..."}]`,
	}
	argsJSON, _ := json.Marshal(args)

	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_ship_review",
		Arguments: json.RawMessage(argsJSON),
	})
	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("sr-mcp"),
		Method:  "tools/call",
		Params:  params,
	})

	res := parseToolCallResult(t, resp)
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "Ship Review card created") {
		t.Errorf("unexpected success message: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "staypoint/sr-mcp-task") {
		t.Errorf("expected harness branch in response, got: %s", res.Content[0].Text)
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	s := NewServer()
	defer s.Close()

	// Unknown tool
	params, _ := json.Marshal(CallToolParams{
		Name:      "staypoint_unknown_tool",
		Arguments: json.RawMessage(`{}`),
	})
	resp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("unknown-tool"),
		Method:  "tools/call",
		Params:  params,
	})
	res := parseToolCallResult(t, resp)
	if !res.IsError {
		t.Errorf("expected isError=true for unknown tool")
	}
	if !strings.Contains(res.Content[0].Text, "unknown tool: staypoint_unknown_tool") {
		t.Errorf("unexpected error message: %s", res.Content[0].Text)
	}

	// Unknown method
	unknownResp := sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("unknown-method"),
		Method:  "arbitrary/method",
	})
	if unknownResp.Error == nil {
		t.Fatalf("expected RPC error for unknown method")
	}
	if unknownResp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got: %d", unknownResp.Error.Code)
	}
}

func TestSimulatedStdioServe(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	var inBuf bytes.Buffer
	var outBuf bytes.Buffer

	// Write batch of newline-delimited requests
	inBuf.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	inBuf.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	inBuf.WriteString(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
	inBuf.WriteString(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}` + "\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Serve(ctx, &inBuf, &outBuf)
	if err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 response lines (excluding notification), got %d: %s", len(lines), outBuf.String())
	}

	// Check response 1 (initialize)
	var r1 Response
	if err := json.Unmarshal([]byte(lines[0]), &r1); err != nil {
		t.Fatalf("failed to parse r1: %v", err)
	}
	if string(*r1.ID) != "1" {
		t.Errorf("expected id 1, got %s", *r1.ID)
	}

	// Check response 2 (ping)
	var r2 Response
	if err := json.Unmarshal([]byte(lines[1]), &r2); err != nil {
		t.Fatalf("failed to parse r2: %v", err)
	}
	if string(*r2.ID) != "2" {
		t.Errorf("expected id 2, got %s", *r2.ID)
	}

	// Check response 3 (tools/list)
	var r3 Response
	if err := json.Unmarshal([]byte(lines[2]), &r3); err != nil {
		t.Fatalf("failed to parse r3: %v", err)
	}
	if string(*r3.ID) != "3" {
		t.Errorf("expected id 3, got %s", *r3.ID)
	}
}
