package server_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_staypoint.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store.DB()
}

func startTestServer(t *testing.T, database *sql.DB) (*server.Server, string) {
	t.Helper()
	token := "test-secret-token-1234567890abcdef"
	opts := server.Options{
		BindHost:             "127.0.0.1",
		Port:                 0, // dynamic port
		AuthToken:            token,
		DB:                   database,
		TelemetryDBPath:      filepath.Join(t.TempDir(), "test_telemetry.db"),
		ReplayBufferSize:     100,
		SubscriberBufferSize: 16,
	}

	srv, err := server.New(opts)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return srv, token
}

func TestServer_LoopbackEnforcement(t *testing.T) {
	database := setupTestDB(t)

	// Attempting to bind to 0.0.0.0 must fail
	_, err := server.New(server.Options{
		BindHost: "0.0.0.0",
		DB:       database,
	})
	if err == nil {
		t.Fatalf("expected error binding to 0.0.0.0, got nil")
	}
	if !strings.Contains(err.Error(), "only binds to 127.0.0.1") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Attempting to bind to remote IP must fail
	_, err = server.New(server.Options{
		BindHost: "192.168.1.100",
		DB:       database,
	})
	if err == nil {
		t.Fatalf("expected error binding to 192.168.1.100, got nil")
	}

	// Valid 127.0.0.1 must succeed
	s, err := server.New(server.Options{
		BindHost: "127.0.0.1",
		DB:       database,
	})
	if err != nil {
		t.Fatalf("expected success with 127.0.0.1, got: %v", err)
	}
	if s == nil {
		t.Fatalf("server is nil")
	}
}

func TestServer_DNSRebinding_FailClosed(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	serverPort := srv.Port()

	tests := []struct {
		name       string
		hostHeader string
		wantStatus int
	}{
		{
			name:       "valid 127.0.0.1 with port",
			hostHeader: fmt.Sprintf("127.0.0.1:%d", serverPort),
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid localhost with port",
			hostHeader: fmt.Sprintf("localhost:%d", serverPort),
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid 127.0.0.1 without port",
			hostHeader: "127.0.0.1",
			wantStatus: http.StatusOK,
		},
		{
			name:       "valid localhost without port",
			hostHeader: "localhost",
			wantStatus: http.StatusOK,
		},
		{
			name:       "attacker evil.com - DNS rebinding attempt",
			hostHeader: "evil.com",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "attacker evil.com with port - DNS rebinding attempt",
			hostHeader: fmt.Sprintf("evil.com:%d", serverPort),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "attacker sub.evil.com",
			hostHeader: fmt.Sprintf("sub.evil.com:%d", serverPort),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "attacker local IP",
			hostHeader: fmt.Sprintf("192.168.1.1:%d", serverPort),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong port for 127.0.0.1",
			hostHeader: "127.0.0.1:9999",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Host = tt.hostHeader
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("host %q: expected status %d, got %d (body: %s)", tt.hostHeader, tt.wantStatus, resp.StatusCode, string(body))
			}
		})
	}
}

func TestServer_CrossOrigin_FailClosed(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	serverPort := srv.Port()

	tests := []struct {
		name       string
		origin     string
		wantStatus int
		wantCORS   bool
	}{
		{
			name:       "no origin (CLI/curl request)",
			origin:     "",
			wantStatus: http.StatusOK,
			wantCORS:   false,
		},
		{
			name:       "valid 127.0.0.1 origin",
			origin:     fmt.Sprintf("http://127.0.0.1:%d", serverPort),
			wantStatus: http.StatusOK,
			wantCORS:   true,
		},
		{
			name:       "valid localhost origin",
			origin:     fmt.Sprintf("http://localhost:%d", serverPort),
			wantStatus: http.StatusOK,
			wantCORS:   true,
		},
		{
			name:       "attacker evil.com origin",
			origin:     "http://evil.com",
			wantStatus: http.StatusForbidden,
			wantCORS:   false,
		},
		{
			name:       "attacker null origin (sandboxed iframe)",
			origin:     "null",
			wantStatus: http.StatusForbidden,
			wantCORS:   false,
		},
		{
			name:       "attacker HTTPS external origin",
			origin:     "https://attacker.org",
			wantStatus: http.StatusForbidden,
			wantCORS:   false,
		},
		{
			name:       "wrong port origin",
			origin:     "http://127.0.0.1:9999",
			wantStatus: http.StatusForbidden,
			wantCORS:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("origin %q: expected status %d, got %d (body: %s)", tt.origin, tt.wantStatus, resp.StatusCode, string(body))
			}

			// Validate NO wildcard CORS
			allowOrigin := resp.Header.Get("Access-Control-Allow-Origin")
			if allowOrigin == "*" {
				t.Errorf("critical security violation: wildcard CORS (*) was returned!")
			}

			if tt.wantCORS {
				if allowOrigin != tt.origin {
					t.Errorf("expected Access-Control-Allow-Origin: %q, got: %q", tt.origin, allowOrigin)
				}
			}
		})
	}
}

func TestServer_AuthToken_Methods(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	// 1. Authorization: Bearer <token>
	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer auth failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 2. X-StayPoint-Token: <token>
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
	req.Header.Set("X-StayPoint-Token", token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("X-StayPoint-Token auth failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 3. Query parameter ?token=<token>
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/health?token="+token, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Query param auth failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Missing token -> 401 Unauthorized
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized on missing token, got status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Wrong token -> 401 Unauthorized
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized on invalid token, got status: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServer_SSE_ReconnectReplaysMissedEvents(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	hub := srv.Hub()

	// Publish initial event before connection
	evt1 := hub.Publish("task_status", map[string]string{"task": "init", "status": "active"})
	if evt1.ID != 1 {
		t.Fatalf("expected event 1, got ID %d", evt1.ID)
	}

	// Publish event 2
	evt2 := hub.Publish("task_status", map[string]string{"task": "step1", "status": "done"})
	if evt2.ID != 2 {
		t.Fatalf("expected event 2, got ID %d", evt2.ID)
	}

	// Publish event 3 and 4 while client was "disconnected"
	evt3 := hub.Publish("thread_msg", map[string]string{"content": "hello"})
	evt4 := hub.Publish("thread_msg", map[string]string{"content": "world"})
	if evt3.ID != 3 || evt4.ID != 4 {
		t.Fatalf("unexpected IDs: %d, %d", evt3.ID, evt4.ID)
	}

	// Client reconnects with Last-Event-ID: 2 (requesting missed events 3 and 4)
	req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/events", nil)
	if err != nil {
		t.Fatalf("failed to create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Last-Event-ID", "2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for SSE, got: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got: %s", ct)
	}

	reader := bufio.NewReader(resp.Body)

	// Helper to read one SSE message. Events are unnamed (no "event:" line);
	// the full Event struct is JSON-encoded in the data field so we read
	// evt.type from JSON.parse(data).type to match the new writeSSEEvent format.
	readEvent := func() (id int64, eventType, data string) {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return 0, "", ""
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "id:") {
				fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "id:")), "%d", &id)
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			} else if line == "" && id > 0 {
				// Parse type from the JSON-encoded Event struct in data.
				var env struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal([]byte(data), &env); err == nil {
					eventType = env.Type
				}
				return id, eventType, data
			}
		}
	}

	// 1. Should receive replayed event 3
	id3, type3, _ := readEvent()
	if id3 != 3 || type3 != "thread_msg" {
		t.Fatalf("expected replayed event 3, got id=%d type=%s", id3, type3)
	}

	// 2. Should receive replayed event 4
	id4, type4, _ := readEvent()
	if id4 != 4 || type4 != "thread_msg" {
		t.Fatalf("expected replayed event 4, got id=%d type=%s", id4, type4)
	}

	// 3. Publish live event 5 while connected
	go func() {
		time.Sleep(50 * time.Millisecond)
		hub.Publish("live_event", map[string]string{"foo": "bar"})
	}()

	id5, type5, _ := readEvent()
	if id5 != 5 || type5 != "live_event" {
		t.Fatalf("expected live event 5, got id=%d type=%s", id5, type5)
	}
}

func TestServer_SSE_BoundedBuffers(t *testing.T) {
	// Replay buffer 50, subscriber channel buffer 4
	hub := server.NewEventHub(50, 4)
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	// Publish 20 events without reading from sub.Channel()
	for i := 1; i <= 20; i++ {
		hub.Publish("test_event", map[string]int{"num": i})
	}

	// Verify channel didn't block, and subscriber dropped overflow events
	if sub.Dropped() != 16 {
		t.Fatalf("expected 16 dropped events for bounded buffer of size 4, got: %d", sub.Dropped())
	}

	// Channel buffer has at most 4 items
	count := 0
drain:
	for {
		select {
		case <-sub.Channel():
			count++
		default:
			break drain
		}
	}

	if count != 4 {
		t.Fatalf("expected exactly 4 buffered events, got %d", count)
	}
}

func TestServer_REST_Tasks(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	client := &http.Client{}

	// 1. Create a Task (POST /api/tasks)
	createBody := []byte(`{
		"name": "Implement Local Server",
		"repo_path": "/tmp/test-repo",
		"git_branch": "feature/server",
		"max_budget_usd": 10.0,
		"max_turns": 20
	}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create task request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 Created, got %d: %s", resp.StatusCode, string(b))
	}

	var createdTask map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&createdTask)
	taskID, _ := createdTask["id"].(string)
	if taskID == "" {
		t.Fatalf("task id is empty: %v", createdTask)
	}

	// 2. List Tasks (GET /api/tasks)
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list tasks failed: %v, status: %d", err, resp.StatusCode)
	}
	var listResp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	tasks, _ := listResp["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// 3. Get Task (GET /api/tasks/{id})
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+taskID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get task failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Add Comment (POST /api/tasks/{id}/comments)
	commentBody := []byte(`{"author":"tester","message":"Checking implementation"}`)
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/comments", bytes.NewReader(commentBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("add comment failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Block Task (POST /api/tasks/{id}/block)
	blockBody := []byte(`{"reason":"Waiting on review"}`)
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/block", bytes.NewReader(blockBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("block task failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 6. Unblock Task (POST /api/tasks/{id}/unblock)
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/unblock", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unblock task failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServer_REST_BlockersAndDependencies(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// Create 2 tasks
	createBody1 := []byte(`{"name":"Backend Migration","repo_path":"/tmp/repo","git_branch":"main"}`)
	req1, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(createBody1))
	req1.Header.Set("Authorization", "Bearer "+token)
	resp1, err := client.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusCreated {
		t.Fatalf("create task 1 failed: %v", err)
	}
	var t1Resp map[string]any
	_ = json.NewDecoder(resp1.Body).Decode(&t1Resp)
	resp1.Body.Close()
	t1ID := t1Resp["id"].(string)

	createBody2 := []byte(`{"name":"Frontend Feature","repo_path":"/tmp/repo","git_branch":"main"}`)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(createBody2))
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusCreated {
		t.Fatalf("create task 2 failed: %v", err)
	}
	var t2Resp map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&t2Resp)
	resp2.Body.Close()
	t2ID := t2Resp["id"].(string)

	// Block task 2 on task 1 with explicit rationale
	blockPayload, _ := json.Marshal(map[string]any{
		"reason": "Blocked on migration",
		"blockers": []map[string]string{
			{"id": t1ID, "rationale": "Database schema migration must be applied first"},
		},
	})
	reqBlock, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+t2ID+"/block", bytes.NewReader(blockPayload))
	reqBlock.Header.Set("Authorization", "Bearer "+token)
	respBlock, err := client.Do(reqBlock)
	if err != nil || respBlock.StatusCode != http.StatusOK {
		t.Fatalf("block task 2 failed: %v", err)
	}
	respBlock.Body.Close()

	// GET /api/tasks/{id} should have blocked_by populated with rationale
	reqGet, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+t2ID, nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	respGet, err := client.Do(reqGet)
	if err != nil || respGet.StatusCode != http.StatusOK {
		t.Fatalf("get task 2 failed: %v", err)
	}
	var getResp map[string]any
	_ = json.NewDecoder(respGet.Body).Decode(&getResp)
	respGet.Body.Close()

	taskObj := getResp["task"].(map[string]any)
	if isBlocked, _ := taskObj["is_blocked"].(bool); !isBlocked {
		t.Errorf("expected task 2 to be blocked")
	}
	blockedBy, ok := taskObj["blocked_by"].([]any)
	if !ok || len(blockedBy) != 1 {
		t.Fatalf("expected 1 blocked_by entry, got: %+v", taskObj["blocked_by"])
	}
	b0 := blockedBy[0].(map[string]any)
	if b0["rationale"] != "Database schema migration must be applied first" {
		t.Errorf("expected explicit rationale, got: %v", b0["rationale"])
	}

	// GET /api/tasks/{id}/dependencies
	reqDep, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+t2ID+"/dependencies", nil)
	reqDep.Header.Set("Authorization", "Bearer "+token)
	respDep, err := client.Do(reqDep)
	if err != nil || respDep.StatusCode != http.StatusOK {
		t.Fatalf("get dependencies failed: %v", err)
	}
	var depResp map[string]any
	_ = json.NewDecoder(respDep.Body).Decode(&depResp)
	respDep.Body.Close()
	if depResp["task_id"] != t2ID {
		t.Errorf("expected task_id %s in dependencies, got: %v", t2ID, depResp["task_id"])
	}

	// DELETE /api/tasks/{id}/blockers/{bid}
	reqDelBlocker, _ := http.NewRequest(http.MethodDelete, srv.URL()+"/api/tasks/"+t2ID+"/blockers/"+t1ID, nil)
	reqDelBlocker.Header.Set("Authorization", "Bearer "+token)
	respDelBlocker, err := client.Do(reqDelBlocker)
	if err != nil || respDelBlocker.StatusCode != http.StatusOK {
		t.Fatalf("delete blocker failed: %v", err)
	}
	respDelBlocker.Body.Close()

	// Verify task 2 is now unblocked
	reqGetAfter, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+t2ID, nil)
	reqGetAfter.Header.Set("Authorization", "Bearer "+token)
	respGetAfter, err := client.Do(reqGetAfter)
	if err != nil || respGetAfter.StatusCode != http.StatusOK {
		t.Fatalf("get task 2 after unblock failed: %v", err)
	}
	var getAfterResp map[string]any
	_ = json.NewDecoder(respGetAfter.Body).Decode(&getAfterResp)
	respGetAfter.Body.Close()
	taskAfterObj := getAfterResp["task"].(map[string]any)
	if isBlocked, _ := taskAfterObj["is_blocked"].(bool); isBlocked {
		t.Errorf("expected task 2 to be unblocked")
	}
}

func TestServer_REST_Threads_Pagination(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// 1. Create a Thread (POST /api/threads)
	threadBody := []byte(`{"title":"Long Chat Thread","repo_path":"/tmp/repo","system_prompt":"You are a helpful assistant"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/threads", bytes.NewReader(threadBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create thread failed: %v, status: %d", err, resp.StatusCode)
	}
	var createdThread map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&createdThread)
	resp.Body.Close()
	threadID, _ := createdThread["id"].(string)

	// 2. Append 15 messages
	for i := 1; i <= 15; i++ {
		msgBody := fmt.Sprintf(`{"role":"user","content":"Message number %d","token_count":10}`, i)
		req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/threads/"+threadID+"/messages", strings.NewReader(msgBody))
		req.Header.Set("Authorization", "Bearer "+token)
		r, err := client.Do(req)
		if err != nil || r.StatusCode != http.StatusCreated {
			t.Fatalf("append message %d failed: %v", i, err)
		}
		r.Body.Close()
	}

	// 3. Paginate Page 1: limit 5, offset 0
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/threads/"+threadID+"/messages?limit=5&offset=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get page 1 failed: %v, status: %d", err, resp.StatusCode)
	}
	var page1 struct {
		Messages []map[string]any `json:"messages"`
		Total    int              `json:"total"`
		Limit    int              `json:"limit"`
		Offset   int              `json:"offset"`
		HasMore  bool             `json:"has_more"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&page1)
	resp.Body.Close()

	if page1.Total != 15 || len(page1.Messages) != 5 || !page1.HasMore {
		t.Fatalf("unexpected page 1: %+v", page1)
	}
	if page1.Messages[0]["content"] != "Message number 1" {
		t.Fatalf("expected Message number 1, got %v", page1.Messages[0]["content"])
	}

	// 4. Paginate Page 3: limit 5, offset 10 (last page)
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/threads/"+threadID+"/messages?limit=5&offset=10", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get page 3 failed: %v, status: %d", err, resp.StatusCode)
	}
	var page3 struct {
		Messages []map[string]any `json:"messages"`
		Total    int              `json:"total"`
		Limit    int              `json:"limit"`
		Offset   int              `json:"offset"`
		HasMore  bool             `json:"has_more"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&page3)
	resp.Body.Close()

	if page3.Total != 15 || len(page3.Messages) != 5 || page3.HasMore {
		t.Fatalf("expected has_more=false on last page, got: %+v", page3)
	}
	if page3.Messages[4]["content"] != "Message number 15" {
		t.Fatalf("expected Message number 15, got %v", page3.Messages[4]["content"])
	}
}

func TestServer_REST_Sessions(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// 1. Register Session (POST /api/sessions)
	sessBody := []byte(`{
		"id": "agent-sess-42",
		"agent_type": "claude",
		"repo_path": "/tmp/test-repo",
		"git_branch": "main",
		"pid": 12345
	}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/sessions", bytes.NewReader(sessBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register session failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Heartbeat (POST /api/sessions/{id}/heartbeat)
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/sessions/agent-sess-42/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	// 3. Get Session (GET /api/sessions/{id})
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/sessions/agent-sess-42", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get session failed: %v, status: %d", err, resp.StatusCode)
	}
	var sess map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess["id"] != "agent-sess-42" || sess["status"] != "active" {
		t.Fatalf("unexpected session details: %v", sess)
	}

	// 4. Close Session (POST /api/sessions/{id}/close)
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/sessions/agent-sess-42/close", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("close session failed: %v, status: %d", err, resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServer_REST_Telemetry(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// Insert mock quota row
	_, err := database.Exec(`
		INSERT INTO quota_windows (pool_key, window_type, used_percent, remaining_pct, is_locked)
		VALUES ('work_claude', 'rolling_5h', 45.0, 55.0, 0);
	`)
	if err != nil {
		t.Fatalf("insert quota failed: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/telemetry", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get telemetry failed: %v, status: %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()

	var tel struct {
		QuotaPools []map[string]any `json:"quota_pools"`
		TaskSpend  map[string]any   `json:"task_spend"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tel)

	if len(tel.QuotaPools) != 1 {
		t.Fatalf("expected 1 quota pool, got %d", len(tel.QuotaPools))
	}
	if tel.QuotaPools[0]["pool_key"] != "work_claude" {
		t.Fatalf("expected pool_key work_claude, got: %v", tel.QuotaPools[0]["pool_key"])
	}
}

func TestServer_REST_FleetOverview(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// Seed task across organization
	_, err := database.Exec(`
		INSERT INTO tasks (id, name, repo_path, organization, project, status, execution_stage, is_blocked, spent_usd, spent_tokens)
		VALUES
		('task-f1', 'Multi-Org Fleet Feature', '/repo/sta', 'StayPoint', 'Core', 'active', 'in_progress', 0, 15.0, 60000),
		('task-f2', 'Enterprise Sync', '/repo/man', 'Managed Solution', 'Cloud', 'active', 'in_progress', 0, 30.0, 120000);
	`)
	if err != nil {
		t.Fatalf("insert tasks failed: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/fleet/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get fleet overview failed: %v, status: %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()

	var overview map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		t.Fatalf("failed to decode fleet overview: %v", err)
	}

	if overview["global_tasks"] == nil {
		t.Errorf("expected global_tasks in fleet overview")
	}
	if overview["provider_quotas"] == nil {
		t.Errorf("expected provider_quotas in fleet overview")
	}
	if overview["organizations"] == nil {
		t.Errorf("expected organizations in fleet overview")
	}
}

func TestServer_REST_ReportHTMLPreview(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	types := []string{"work", "combined", "personal", "gemini"}
	for _, rType := range types {
		req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/report?type="+rType+"&format=html", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("get report html failed for %s: %v", rType, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for %s report html, got %d", rType, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("expected Content-Type text/html for %s, got %s", rType, ct)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if !strings.Contains(string(body), "<html") {
			t.Errorf("expected valid HTML body for %s report, got %s", rType, string(body))
		}
	}
}

func TestServer_REST_Interactions(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	// Create a task first
	createBody := []byte(`{"name":"interaction test task"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task failed: %v, status %d", err, resp.StatusCode)
	}
	var task map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&task)
	resp.Body.Close()
	taskID, _ := task["id"].(string)

	// GET /api/tasks/{id}/interactions with no interactions → 200 {"interactions":[]}
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+taskID+"/interactions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("list interactions request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var listResp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listResp)
	interactions, _ := listResp["interactions"].([]any)
	if interactions == nil {
		t.Fatalf("expected interactions key in response, got: %v", listResp)
	}
	if len(interactions) != 0 {
		t.Fatalf("expected 0 interactions, got %d", len(interactions))
	}

	// POST /api/tasks/{id}/interactions → create one
	payload := `{"questions":[{"question":"confirm?","options":["yes","no"]}]}`
	interBody, _ := json.Marshal(map[string]string{
		"kind":    "ask_user_questions",
		"payload": payload,
	})
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/interactions", bytes.NewReader(interBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("create interaction request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 201, got %d: %s", resp2.StatusCode, body)
	}
	var created map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&created)
	interactionID := int(created["id"].(float64))
	if interactionID == 0 {
		t.Fatalf("expected non-zero interaction id")
	}

	// POST /api/tasks/{id}/interactions/{iid}/resolve
	resolveBody, _ := json.Marshal(map[string]any{
		"status":   "accepted",
		"response": map[string]any{"answers": []any{}},
	})
	req, _ = http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/tasks/%s/interactions/%d/resolve", srv.URL(), taskID, interactionID),
		bytes.NewReader(resolveBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp3, err := client.Do(req)
	if err != nil {
		t.Fatalf("resolve interaction request failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("expected 200, got %d: %s", resp3.StatusCode, body)
	}
	var resolved map[string]any
	_ = json.NewDecoder(resp3.Body).Decode(&resolved)
	if resolved["status"] != "accepted" {
		t.Fatalf("expected status=accepted, got %v", resolved["status"])
	}
}

// TestServer_REST_AuditMergesActivity verifies STA-358: GET /api/tasks/{id}/audit
// returns both governance events and activity-log entries (comments, interactions).
func TestServer_REST_AuditMergesActivity(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}
	auth := func(r *http.Request) *http.Request { r.Header.Set("Authorization", "Bearer "+token); return r }

	// create task
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks",
		bytes.NewReader([]byte(`{"name":"audit-test","repo_path":"/tmp/audit-test"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(auth(req))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %v %d", err, resp.StatusCode)
	}
	var ct map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ct)
	resp.Body.Close()
	taskID := ct["id"].(string)

	// post a comment
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/comments",
		bytes.NewReader([]byte(`{"author":"planner","message":"plan: do the thing"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = client.Do(auth(req))
	resp.Body.Close()

	// create + resolve an interaction
	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/interactions",
		bytes.NewReader([]byte(`{"kind":"request_confirmation","payload":"{\"prompt\":\"Merge?\"}","idempotency_key":"audit-ix"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = client.Do(auth(req))
	var ix map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ix)
	resp.Body.Close()
	ixID := fmt.Sprintf("%v", ix["id"])

	req, _ = http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks/"+taskID+"/interactions/"+ixID+"/resolve",
		bytes.NewReader([]byte(`{"status":"accepted","response":{"confirmed":true}}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = client.Do(auth(req))
	resp.Body.Close()

	// fetch audit log
	req, _ = http.NewRequest(http.MethodGet, srv.URL()+"/api/tasks/"+taskID+"/audit", nil)
	resp, err = client.Do(auth(req))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("audit: %v %d", err, resp.StatusCode)
	}
	var ar map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ar)
	resp.Body.Close()

	entries, _ := ar["audit_log"].([]any)
	types := ""
	for _, e := range entries {
		if em, ok := e.(map[string]any); ok {
			types += em["event_type"].(string) + " "
		}
	}
	if !strings.Contains(types, "comment") {
		t.Errorf("audit_log missing comment event; got types: %s", types)
	}
	if !strings.Contains(types, "interaction") {
		t.Errorf("audit_log missing interaction event; got types: %s", types)
	}
}

func TestServer_REST_RunErrors(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}

	authGet := func(url string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		return resp
	}

	// Seed a task.
	createBody := []byte(`{"name":"err-task","repo_path":"/tmp/err-repo"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %v, status %d", err, resp.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	taskID, _ := created["id"].(string)

	// Seed a run_error row directly.
	_, err = database.Exec(
		`INSERT INTO run_errors (id, run_id, task_id, turn, exit_code, stderr_tail, duration_ms, model, adapter)
		 VALUES ('re-01', 'run-xyz', ?, 1, 2, 'oom error', 1200, 'claude-opus', 'claude_local')`,
		taskID,
	)
	if err != nil {
		t.Fatalf("seed run_error: %v", err)
	}

	// GET /api/tasks/{id}/run-errors
	resp = authGet(srv.URL() + "/api/tasks/" + taskID + "/run-errors")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET task run-errors: want 200, got %d: %s", resp.StatusCode, body)
	}
	var taskErrors map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&taskErrors)
	resp.Body.Close()
	errs, _ := taskErrors["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("expected 1 task run-error, got %d", len(errs))
	}
	first := errs[0].(map[string]any)
	if first["id"] != "re-01" {
		t.Errorf("expected id re-01, got %v", first["id"])
	}

	// GET /api/tasks/{id}/run-errors — non-existent task returns 404.
	resp = authGet(srv.URL() + "/api/tasks/no-such-task/run-errors")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing task: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET /api/run-errors — global endpoint.
	resp = authGet(srv.URL() + "/api/run-errors")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/run-errors: want 200, got %d: %s", resp.StatusCode, body)
	}
	var allErrors map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&allErrors)
	resp.Body.Close()
	allErrs, _ := allErrors["errors"].([]any)
	if len(allErrs) != 1 {
		t.Fatalf("expected 1 global run-error, got %d", len(allErrs))
	}

	// GET /api/run-errors?task_id=... — filtered.
	resp = authGet(srv.URL() + "/api/run-errors?task_id=" + taskID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/run-errors?task_id: want 200, got %d", resp.StatusCode)
	}
	var filtered map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&filtered)
	resp.Body.Close()
	filteredErrs, _ := filtered["errors"].([]any)
	if len(filteredErrs) != 1 {
		t.Fatalf("expected 1 filtered run-error, got %d", len(filteredErrs))
	}

	// GET /api/run-errors?run_id=run-xyz — filtered by run.
	resp = authGet(srv.URL() + "/api/run-errors?run_id=run-xyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/run-errors?run_id: want 200, got %d", resp.StatusCode)
	}
	var byRun map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&byRun)
	resp.Body.Close()
	byRunErrs, _ := byRun["errors"].([]any)
	if len(byRunErrs) != 1 {
		t.Fatalf("expected 1 run_id-filtered run-error, got %d", len(byRunErrs))
	}
}



func TestServer_REST_RunControl(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	authPost := func(url, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		return resp
	}

	// Create a task via the API so all required columns get defaults.
	createResp := authPost(srv.URL()+"/api/tasks", `{"name":"RunCtrl Test","repo_path":"/tmp","git_branch":"main"}`)
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		createResp.Body.Close()
		t.Fatalf("create task: want 201, got %d: %s", createResp.StatusCode, b)
	}
	var created map[string]any
	_ = json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()
	taskID, _ := created["id"].(string)
	if taskID == "" {
		t.Fatalf("create task: empty id")
	}
	// Set to in_progress.
	_, err := database.Exec(`UPDATE tasks SET execution_stage='in_progress' WHERE id=?`, taskID)
	if err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	base := srv.URL() + "/api/tasks/" + taskID + "/run-control"

	// Pause.
	resp := authPost(base, `{"action":"pause"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Resume (task now paused, still allowed).
	_, _ = database.Exec(`UPDATE tasks SET execution_stage='paused' WHERE id=?`, taskID)
	resp = authPost(base, `{"action":"resume"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Inject message.
	resp = authPost(base, `{"action":"message","text":"hello agent"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("message: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Stop.
	resp = authPost(base, `{"action":"stop"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid action.
	resp = authPost(base, `{"action":"bogus"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bogus action: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Not-found task.
	resp = authPost(srv.URL()+"/api/tasks/no-such-task/run-control", `{"action":"stop"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestServer_REST_RunControlState verifies the GET /api/tasks/{id}/run-control-state
// endpoint returns correct pause/stop flags (STA-505 step-boundary pause gate).
func TestServer_REST_RunControlState(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	authGet := func(url string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		return resp
	}
	authPost := func(url, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		return resp
	}

	// Wire GlobalRunControl to the test DB so IsPaused/SetPause hit the right store.
	orchestrator.GlobalRunControl.SetDB(database)
	t.Cleanup(func() { orchestrator.GlobalRunControl.SetDB(nil) })

	createResp := authPost(srv.URL()+"/api/tasks", `{"name":"RCState Test","repo_path":"/tmp","git_branch":"main"}`)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: want 201, got %d", createResp.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()
	taskID, _ := created["id"].(string)

	_, _ = database.Exec(`UPDATE tasks SET execution_stage='in_progress' WHERE id=?`, taskID)

	stateURL := srv.URL() + "/api/tasks/" + taskID + "/run-control-state"

	// Initially not paused, not stopped.
	resp := authGet(stateURL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state: want 200, got %d", resp.StatusCode)
	}
	var s struct {
		Paused        bool `json:"paused"`
		StopRequested bool `json:"stop_requested"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if s.Paused || s.StopRequested {
		t.Fatalf("want paused=false stop=false, got %+v", s)
	}

	// After pause, state reflects it.
	authPost(srv.URL()+"/api/tasks/"+taskID+"/run-control", `{"action":"pause"}`).Body.Close()
	resp = authGet(stateURL)
	_ = json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if !s.Paused {
		t.Fatal("want paused=true after pause action")
	}

	// Long-poll returns immediately when not paused; set up goroutine to resume
	// after a short delay and verify the long-poll unblocks.
	go func() {
		time.Sleep(100 * time.Millisecond)
		authPost(srv.URL()+"/api/tasks/"+taskID+"/run-control", `{"action":"resume"}`).Body.Close()
	}()
	pollStart := time.Now()
	resp = authGet(stateURL + "?wait=true")
	elapsed := time.Since(pollStart)
	_ = json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if s.Paused {
		t.Fatal("want paused=false after resume")
	}
	// Should have waited ~100ms (the goroutine delay) not 0ms.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("long-poll returned too fast (%v), expected ~100ms", elapsed)
	}
}

// TestServer_BoardToken_Required verifies that Board-only mutation endpoints
// reject requests that carry only the agent auth token (403 Forbidden) and
// accept requests that include the staypoint_board session cookie.
func TestServer_BoardToken_Required(t *testing.T) {
	database := setupTestDB(t)
	// Seed a passkey so fail-closed allows through with a valid assertion.
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	// Stub the WebAuthn verifier so tests don't need real Touch ID hardware.
	if s, ok := any(srv).(webAuthnVerifierSetter); ok {
		s.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	}
	boardToken := srv.BoardToken()
	if boardToken == "" {
		t.Fatal("BoardToken() returned empty string — board token was not generated")
	}
	if boardToken == token {
		t.Fatal("BoardToken() must differ from the agent AuthToken")
	}

	authOnly := func(method, url, body string) *http.Response {
		var b io.Reader
		if body != "" {
			b = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, url, b)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		return resp
	}

	withBoard := func(method, url, body string) *http.Response {
		var b io.Reader
		if body != "" {
			b = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, url, b)
		req.Header.Set("Authorization", "Bearer "+token)
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: boardToken})
		req.Header.Set("X-WebAuthn-Assertion", "stub-assertion")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		return resp
	}

	base := srv.URL()

	// Create a task so ship-review and gate endpoints have a valid ID to reject.
	taskID := "test-board-token-task"
	createBody := `{"id":"` + taskID + `","name":"Board token test task","repo_path":"/tmp"}`
	createResp := authOnly("POST", base+"/api/tasks", createBody)
	createResp.Body.Close()

	boardEndpoints := []struct {
		method string
		path   string
		body   string
	}{
		{"POST", "/api/tasks/" + taskID + "/ship-review/approve", ""},
		{"POST", "/api/tasks/" + taskID + "/ship-review/send-back", `{"comment":"test"}`},
		{"POST", "/api/tasks/" + taskID + "/ship-review/reject", `{"comment":"test"}`},
		{"POST", "/api/security/gate-requests/nonexistent-id/decide", `{"decision":"approved"}`},
		{"POST", "/api/settings/security-gate", `{"main_merge_approval":true}`},
		{"POST", "/api/settings/ship-review", `{"ship_review":true}`},
		// STA-520: dev-config write is Board-only (can inject dev_command/setup_steps).
		{"PUT", "/api/project-dev-configs", `{"repo_path":"/tmp/testrepo","dev_command":"npm run dev"}`},
	}

	for _, ep := range boardEndpoints {
		// Agent auth token alone → 403 board_session_required
		resp := authOnly(ep.method, base+ep.path, ep.body)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with agent token only: want 403, got %d", ep.method, ep.path, resp.StatusCode)
		} else {
			var e struct{ Error string `json:"error"` }
			if _ = json.Unmarshal(raw, &e); e.Error != "board_session_required" {
				t.Errorf("%s %s with agent token only: want error=board_session_required, got %s", ep.method, ep.path, raw)
			}
		}

		// Board cookie + assertion + seeded passkey → board auth gate passes (not 403).
		// The separate TestBoardAction_NoPasskeyRegistered_Returns403 covers the
		// enrollment_required case; here we verify the full happy-path gate clears.
		resp = withBoard(ep.method, base+ep.path, ep.body)
		raw, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("%s %s with board cookie+passkey+assertion: want not 403, got %d %s", ep.method, ep.path, resp.StatusCode, raw)
		}
	}
}

// TestServer_BoardBootstrap_RequiresBoardNonce verifies that the ?token= bootstrap
// redirect only sets the board cookie when a valid single-use board_nonce is present.
// The ?board_token= fallback has been removed (STA-583): a long-lived credential in a
// URL is replayable and can be captured in logs, history, and network traces.
func TestServer_BoardBootstrap_RequiresBoardNonce(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	boardToken := srv.BoardToken()
	boardNonce := srv.BoardNonce()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}

	hasBoardCookie := func(resp *http.Response) bool {
		for _, c := range resp.Cookies() {
			if c.Name == "staypoint_board" && c.Value == boardToken {
				return true
			}
		}
		return false
	}

	// Bootstrap with auth only → redirect should NOT set staypoint_board cookie.
	resp, err := client.Get(fmt.Sprintf("%s/?token=%s", srv.URL(), token))
	if err != nil {
		t.Fatalf("bootstrap (auth-only) request failed: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "staypoint_board" {
			t.Errorf("board cookie was set from auth token alone — want no board cookie, got %q", c.Value)
		}
	}

	// Bootstrap with auth + board_nonce → redirect SHOULD set staypoint_board cookie.
	// The nonce is single-use: this call consumes it.
	resp, err = client.Get(fmt.Sprintf("%s/?token=%s&board_nonce=%s", srv.URL(), token, boardNonce))
	if err != nil {
		t.Fatalf("bootstrap (nonce) request failed: %v", err)
	}
	resp.Body.Close()
	if !hasBoardCookie(resp) {
		t.Error("board cookie was not set when valid board_nonce was provided")
	}

	// Second request with the SAME nonce → nonce already consumed, no board cookie.
	resp, err = client.Get(fmt.Sprintf("%s/?token=%s&board_nonce=%s", srv.URL(), token, boardNonce))
	if err != nil {
		t.Fatalf("bootstrap (consumed-nonce) request failed: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "staypoint_board" {
			t.Errorf("board cookie was set with already-consumed nonce — want no board cookie, got %q", c.Value)
		}
	}

	// STA-583: ?board_token= fallback is REMOVED. A long-lived credential in a URL is
	// replayable. Presenting board_token in the query string must NOT grant the board cookie.
	resp, err = client.Get(fmt.Sprintf("%s/?token=%s&board_token=%s", srv.URL(), token, boardToken))
	if err != nil {
		t.Fatalf("bootstrap (board_token fallback) request failed: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "staypoint_board" {
			t.Errorf("STA-583: board cookie was set via ?board_token= URL fallback — this must be rejected, got %q", c.Value)
		}
	}
}

// TestServer_FreshNonce_RequiresBoardToken verifies that POST /api/board/fresh-nonce
// requires both the session auth token and the board token (X-Board-Token header).
// This ensures agents with only the session token cannot mint a new nonce.
func TestServer_FreshNonce_RequiresBoardToken(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	boardToken := srv.BoardToken()

	// Helper to call fresh-nonce with specified headers.
	doFreshNonce := func(authToken, boardTokenHeader string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL()+"/api/board/fresh-nonce", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}
		if boardTokenHeader != "" {
			req.Header.Set("X-Board-Token", boardTokenHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()
		return resp
	}

	// No auth at all → 401.
	if resp := doFreshNonce("", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth: want 401, got %d", resp.StatusCode)
	}

	// Auth only (agent token), no board token → 403.
	if resp := doFreshNonce(token, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("auth-only: want 403, got %d", resp.StatusCode)
	}

	// Wrong board token → 403.
	if resp := doFreshNonce(token, "wrongtoken"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong-board-token: want 403, got %d", resp.StatusCode)
	}

	// Correct auth + correct board token → 200 with a nonce.
	req, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/board/fresh-nonce", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Board-Token", boardToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("valid fresh-nonce request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("valid: want 200, got %d", resp.StatusCode)
	}
}

// TestUpsertDevConfig_BoardGate_AuditLog verifies STA-520:
//   - Agent token alone → 403 (no board cookie)
//   - Board session + passkey assertion succeeds and writes a board_audit_log row with old→new diff
func TestUpsertDevConfig_BoardGate_AuditLog(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	// Stub WebAuthn verifier so tests don't need real Touch ID hardware.
	if s, ok := any(srv).(webAuthnVerifierSetter); ok {
		s.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	}
	boardToken := srv.BoardToken()
	base := srv.URL()

	body := `{"repo_path":"/tmp/sta520repo","dev_command":"npm run dev","setup_steps":["npm ci"]}`

	// Agent-only → must be rejected with 403 (missing board cookie).
	req, _ := http.NewRequest("PUT", base+"/api/project-dev-configs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("agent-only request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("agent-only PUT /api/project-dev-configs: want 403, got %d", resp.StatusCode)
	}

	// Board session + passkey assertion → must succeed (200) and write an audit row.
	req, _ = http.NewRequest("PUT", base+"/api/project-dev-configs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: boardToken})
	req.Header.Set("X-WebAuthn-Assertion", "stub-assertion")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("board session request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("board session PUT: want 200, got %d body=%s", resp.StatusCode, b)
	}

	// Verify board_audit_log has exactly one dev_config_change row.
	var count int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM board_audit_log WHERE event_type = 'dev_config_change'`,
	).Scan(&count); err != nil {
		t.Fatalf("audit log query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("audit log: want 1 dev_config_change row, got %d", count)
	}

	// Verify the audit payload contains old and new dev_command.
	var payload string
	if err := database.QueryRow(
		`SELECT payload FROM board_audit_log WHERE event_type = 'dev_config_change' LIMIT 1`,
	).Scan(&payload); err != nil {
		t.Fatalf("audit payload query: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("audit payload not valid JSON: %v", err)
	}
	if p["new_dev_command"] != "npm run dev" {
		t.Errorf("audit payload new_dev_command: want 'npm run dev', got %v", p["new_dev_command"])
	}
	// First upsert has no prior config, so old_dev_command should be empty.
	if p["old_dev_command"] != "" {
		t.Errorf("audit payload old_dev_command: want '', got %v", p["old_dev_command"])
	}
}

// TestUpsertDevConfig_PartialUpdate verifies that a PUT carrying only dev_command
// does not blank dev_url, sql_editor_url, or supabase_enabled (fix for STA-520 blocker).
func TestUpsertDevConfig_PartialUpdate(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	// Stub WebAuthn verifier so tests don't need real Touch ID hardware.
	if s, ok := any(srv).(webAuthnVerifierSetter); ok {
		s.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	}
	boardToken := srv.BoardToken()
	base := srv.URL()

	putJSON := func(body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("PUT", base+"/api/project-dev-configs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: boardToken})
		req.Header.Set("X-WebAuthn-Assertion", "stub-assertion")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT /api/project-dev-configs: %v", err)
		}
		return resp
	}

	// Seed the config with all fields set.
	seed := `{"repo_path":"/tmp/partial-repo","dev_command":"make dev","dev_url":"http://127.0.0.1:3000","sql_editor_url":"http://localhost:54323","supabase_enabled":true,"setup_steps":["make install"]}`
	resp := putJSON(seed)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seed PUT: want 200, got %d body=%s", resp.StatusCode, b)
	}

	// Partial update: only dev_command is provided; all other fields are absent.
	partial := `{"repo_path":"/tmp/partial-repo","dev_command":"npm run dev"}`
	resp2 := putJSON(partial)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("partial PUT: want 200, got %d body=%s", resp2.StatusCode, b)
	}

	var got map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if got["dev_command"] != "npm run dev" {
		t.Errorf("dev_command: want 'npm run dev', got %v", got["dev_command"])
	}
	if got["dev_url"] != "http://127.0.0.1:3000" {
		t.Errorf("dev_url: want 'http://127.0.0.1:3000', got %v", got["dev_url"])
	}
	if got["sql_editor_url"] != "http://localhost:54323" {
		t.Errorf("sql_editor_url: want 'http://localhost:54323', got %v", got["sql_editor_url"])
	}
	if got["supabase_enabled"] != true {
		t.Errorf("supabase_enabled: want true, got %v", got["supabase_enabled"])
	}
}
