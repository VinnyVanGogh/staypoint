package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/boardplan"
	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// staypoint_board_plan_propose (task-e1b24d66): the Board's interactive
// session proposes a batch of Board actions for the Board to sign with one
// Touch ID. Proposing changes nothing. Refused inside a daemon run.
func boardPlanProposeTool() Tool {
	return Tool{
		Name: "staypoint_board_plan_propose",
		Description: "Board sessions only: propose a Board action plan (a batch of Approve & Merge / Send Back / Mark done / Run Now / Cancel / Unblock / gate approve-deny) " +
			"for the Board to review and sign with ONE Touch ID at the returned URL. Nothing runs until the Board signs. Refused inside a daemon agent run.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"actions": {
					Type: "string",
					Description: `JSON array of actions. Each: {"task_id","action","text","gate_id","expected_head_sha","reason"}. ` +
						`action is one of approve_merge (needs expected_head_sha = the Ship Review card's head_sha), send_back (needs text), ` +
						`mark_done (text = optional note), run_now, cancel, unblock, gate_approve / gate_deny (need gate_id).`,
				},
				"proposer": {
					Type:        "string",
					Description: "Optional label shown to the Board (defaults to the Claude Code session id)",
				},
			},
			Required: []string{"actions"},
		},
	}
}

func (s *Server) handleBoardPlanPropose(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	if err := boardplan.RefuseInAgentRun(os.Getenv); err != nil {
		return toolError(err.Error())
	}
	var args struct {
		Actions  string `json:"actions"`
		Proposer string `json:"proposer"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return toolError("invalid arguments: " + err.Error())
		}
	}
	if strings.TrimSpace(args.Actions) == "" {
		return toolError("actions is required (JSON array)")
	}
	actions, err := boardplan.ParseActions([]byte(args.Actions))
	if err != nil {
		return toolError(err.Error())
	}
	cfg := s.cfg
	if cfg == nil {
		if loaded, err := config.LoadConfig(); err == nil {
			cfg = loaded
		} else {
			cfg = config.DefaultConfig()
		}
	}
	creds, err := boardplan.LoadCredentials(cfg.DataDir, s.boardPlanDaemonURL)
	if err != nil {
		return toolError(err.Error())
	}
	out, err := boardplan.Propose(ctx, creds, boardplan.Proposer(os.Getenv, args.Proposer), actions)
	if err != nil {
		return toolError(err.Error())
	}
	return toolSuccess(fmt.Sprintf("Proposed %s with %d actions. Nothing has run. Ask the Board to review and sign it: %s",
		out.Plan.ID, len(out.Plan.Actions), out.ReviewURL(creds.DaemonURL)))
}
