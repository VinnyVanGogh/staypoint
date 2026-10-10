package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

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
