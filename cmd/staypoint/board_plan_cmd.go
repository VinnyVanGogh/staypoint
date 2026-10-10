package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/boardplan"
	"github.com/spf13/cobra"
)

// Board action plans (task-e1b24d66): the Board's terminal session proposes
// a batch of Board actions; the Board signs it on /board/plans/<id> with one
// Touch ID. Proposing changes nothing and is refused inside a daemon run.

var boardPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Propose and inspect Board action plans (batch of Board actions signed with one Touch ID)",
}

var boardPlanProposeCmd = &cobra.Command{
	Use:   "propose --file plan.json",
	Short: "Propose a Board action plan for the Board to review and sign",
	Long: `Propose a batch of Board actions. Nothing runs until the Board opens the
printed page, keeps or unticks each row, and signs the selection with Touch ID.

plan.json is {"actions":[...]} (or a bare array). Each action:
  {"task_id":"task-…","action":"approve_merge","expected_head_sha":"<card head>","reason":"…"}
  {"task_id":"task-…","action":"send_back","text":"<comment for the agent>","reason":"…"}
  {"task_id":"task-…","action":"mark_done","text":"<optional note>"}
  {"task_id":"task-…","action":"run_now"} | "cancel" | "unblock"
  {"gate_id":"<gate request id>","action":"gate_approve"} | "gate_deny"

Refused inside a daemon agent run (STAYPOINT_TASK_ID set).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := boardplan.RefuseInAgentRun(os.Getenv); err != nil {
			return err
		}
		file, _ := cmd.Flags().GetString("file")
		if file == "" {
			return fmt.Errorf("--file is required")
		}
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(cmd.InOrStdin())
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return fmt.Errorf("read plan: %w", err)
		}
		actions, err := boardplan.ParseActions(data)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("config not loaded")
		}
		daemonURL, _ := cmd.Flags().GetString("daemon-url")
		creds, err := boardplan.LoadCredentials(cfg.DataDir, daemonURL)
		if err != nil {
			return err
		}
		explicit, _ := cmd.Flags().GetString("proposer")
		out, err := boardplan.Propose(context.Background(), creds, boardplan.Proposer(os.Getenv, explicit), actions)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Proposed %s (%d actions, content hash %s)\nReview and sign: %s\n",
			out.Plan.ID, len(out.Plan.Actions), out.Plan.ContentHash[:12], out.ReviewURL(creds.DaemonURL))
		return nil
	},
}

var boardPlanListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Board action plans",
	RunE: func(cmd *cobra.Command, args []string) error {
		status, _ := cmd.Flags().GetString("status")
		var out struct {
			Plans []boardplan.Plan `json:"plans"`
		}
		if err := boardPlanGet(cmd, "/api/board/plans?status="+url.QueryEscape(status), &out); err != nil {
			return err
		}
		if len(out.Plans) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No plans.")
			return nil
		}
		for _, p := range out.Plans {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %-9s  %2d actions  %s  by %s\n",
				p.ID, p.Status, len(p.Actions), p.CreatedAt.Local().Format("2006-01-02 15:04"), p.Proposer)
		}
		return nil
	},
}

var boardPlanShowCmd = &cobra.Command{
	Use:   "show <plan-id>",
	Short: "Show a Board action plan and its per-row results",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var out struct {
			Plan boardplan.Plan `json:"plan"`
		}
		if err := boardPlanGet(cmd, "/api/board/plans/"+url.PathEscape(args[0]), &out); err != nil {
			return err
		}
		p := out.Plan
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "%s  %s  by %s\n", p.ID, p.Status, p.Proposer)
		results := map[int]boardplan.Result{}
		for _, r := range p.Results {
			results[r.Index] = r
		}
		for i, a := range p.Actions {
			target := a.TaskID
			if a.GateID != "" {
				target = "gate " + a.GateID
			}
			line := fmt.Sprintf("  %2d. %-13s %s", i+1, a.Action, target)
			if r, ok := results[i]; ok {
				line += "  -> " + r.Status
				if r.Error != "" {
					line += ": " + r.Error
				}
			}
			fmt.Fprintln(w, line)
		}
		return nil
	},
}

// boardPlanGet reads a plans endpoint with the session token.
func boardPlanGet(cmd *cobra.Command, path string, v any) error {
	daemonURL, _ := cmd.Flags().GetString("daemon-url")
	_, token := resolveDaemonConn()
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(daemonURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, v)
}

func init() {
	boardPlanProposeCmd.Flags().String("file", "", "Plan JSON file ('-' for stdin)")
	boardPlanProposeCmd.Flags().String("proposer", "", "Proposer label shown on the review page (default: the Claude session id)")
	boardPlanListCmd.Flags().String("status", "", "Filter: pending, executing, executed, discarded")
	for _, c := range []*cobra.Command{boardPlanProposeCmd, boardPlanListCmd, boardPlanShowCmd} {
		c.Flags().String("daemon-url", boardplan.DefaultDaemonURL, "Daemon base URL")
		boardPlanCmd.AddCommand(c)
	}
	boardCmd.AddCommand(boardPlanCmd)
}
