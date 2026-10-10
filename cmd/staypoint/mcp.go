package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/mcp"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start Model Context Protocol (MCP) JSON-RPC 2.0 stdio server",
	Long:  "Run the pure Go Model Context Protocol (MCP) server over standard I/O for LLM client integration.",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		opts := []mcp.Option{mcp.WithApprover(boardApprover)}
		if pinned, err := pinMCPConfig(); err != nil {
			// Ops tools stay off (no ops data dir); the rest still serve.
			fmt.Fprintf(os.Stderr, "staypoint mcp: ops tools disabled: %v\n", err)
		} else {
			cfg = pinned
			opts = append(opts, mcp.WithOpsDataDir(pinned.DataDir), mcp.WithRunCheck(daemonRunCheck))
		}
		if cfg != nil {
			opts = append(opts, mcp.WithConfig(cfg))
		}

		server := mcp.NewServer(opts...)
		defer server.Close()

		if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server exited with error: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}

// mcpRealHome is config.RealHomeDir; tests replace it.
var mcpRealHome = config.RealHomeDir

// pinMCPConfig loads the config the ops tools trust: the real account's
// config.toml, whatever HOME the server was started with (Board review #2
// H1). HOME is reset too, so gh, git and ssh children read the real
// account's files and the Board gate token comes from the real data dir.
func pinMCPConfig() (*config.Config, error) {
	home, err := mcpRealHome()
	if err != nil {
		return nil, fmt.Errorf("resolve the real home directory: %w", err)
	}
	if os.Getenv("HOME") != home {
		if err := os.Setenv("HOME", home); err != nil {
			return nil, err
		}
	}
	c, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(c.DataDir) {
		return nil, fmt.Errorf("data dir %q is not absolute", c.DataDir)
	}
	return c, nil
}

// daemonRunCheck asks the daemon whether token is a live run's token for
// taskID (Board review #4: the daemon holds run tokens in memory only, so
// nothing on disk can mint one). Any failure to get a yes refuses.
func daemonRunCheck(ctx context.Context, taskID, token string) error {
	daemonURL, auth := gateDaemonConn()
	if daemonURL == "" || auth == "" {
		return fmt.Errorf("StayPoint daemon unreachable; cannot confirm this run's token")
	}
	body, err := json.Marshal(map[string]string{"task_id": taskID, "token": token})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonURL+"/api/ops/run-token/check", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("StayPoint daemon unreachable; cannot confirm this run's token")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
	if e.Error == "" {
		e.Error = fmt.Sprintf("daemon answered %d", resp.StatusCode)
	}
	return fmt.Errorf("run token not confirmed: %s", e.Error)
}

// boardApprover holds an ops tool's prod or external write for the Board,
// the way the PreToolUse hook holds a Red command: a gate request whose
// cmdline is the call's canonical form, then a bounded wait. It fails closed.
func boardApprover(ctx context.Context, req mcp.ApprovalRequest) (bool, string) {
	canon := req.Call.Canonical()
	daemonURL, token := gateDaemonConn()
	if daemonURL == "" || token == "" {
		return false, fmt.Sprintf("Board gate unreachable; %s not run", req.Call.Tool)
	}
	id := req.GateID
	if id != "" {
		gr := fetchGateRequest(daemonURL, token, id)
		if gr == nil {
			return false, fmt.Sprintf("gate %s not found; %s not run", id, req.Call.Tool)
		}
		if gr.Cmdline != canon || gr.TaskID != req.TaskID {
			return false, fmt.Sprintf("gate %s was for a different call; %s not run", id, req.Call.Tool)
		}
	} else {
		gr := createGateRequest(daemonURL, token, gateRequestBody{
			Cmdline: canon, Reasons: []string{req.Reason}, TaskID: req.TaskID, CWD: req.CWD,
		})
		if gr == nil {
			return false, fmt.Sprintf("could not register gate request; %s not run", req.Call.Tool)
		}
		if gr.Status == "approved" {
			return true, ""
		}
		id = gr.ID
	}
	status, decidedBy := waitGateDecision(daemonURL, token, id)
	switch status {
	case "approved":
		return true, ""
	case "denied":
		if strings.HasSuffix(decidedBy, ":tev1") {
			return false, fmt.Sprintf("tev1 denied %s, so it was not run, and this task is parked until the Board reviews it (gate %s). Stop here; do not retry or work around it.", req.Call.Tool, id)
		}
		return false, fmt.Sprintf("Board denied %s (gate %s); not run", req.Call.Tool, id)
	case "deferred":
		return false, deferredMessage(id)
	case "expired":
		return false, fmt.Sprintf("held: the Board has not decided yet, so %s was NOT run (gate %s stays pending). Do not work around it; once the Board approves, call %s again with the same parameters and approval_gate_id=%s.", req.Call.Tool, id, req.Call.Tool, id)
	default:
		return false, fmt.Sprintf("Board gate unreachable during poll; %s not run (gate %s)", req.Call.Tool, id)
	}
}

// fetchGateRequest reads a gate request without waiting.
func fetchGateRequest(daemonURL, token, id string) *struct {
	Cmdline string `json:"cmdline"`
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
} {
	req, err := http.NewRequest(http.MethodGet, daemonURL+"/api/security/gate-requests/"+id, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var gr struct {
		Cmdline string `json:"cmdline"`
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
	}
	if json.NewDecoder(resp.Body).Decode(&gr) != nil {
		return nil
	}
	return &gr
}
