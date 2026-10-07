package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// STA-854: Board-only, expiring override of the tracking gate.
//
// The CLI only *requests* the override: it files a security gate request
// (run_id trackgate.OverrideRunID) whose text names the company and minutes.
// The override exists only once the Board approves that request through
// POST /api/security/gate-requests/{id}/decide, which is wrapped by
// WrapBoardAction (Board session cookie + WebAuthn passkey assertion). The
// window starts at approval. An agent holding the daemon auth token can file
// a request but cannot approve it, and this command refuses to run inside an
// agent session at all.

var gateOverrideCmd = &cobra.Command{
	Use:   "override",
	Short: "Board-only: request a time-limited tracking-gate override (passkey approval in the Board UI)",
	Long: `Request a Board override of the work-repo tracking gate for N minutes.

The request appears in the Board UI's pending security gate requests and takes
effect only after the Board approves it with a passkey. The window starts at
approval and expires automatically (max ` + fmt.Sprint(trackgate.MaxOverrideMinutes) + ` minutes). Approval and the
decision are recorded in security_gate_audit_log and board_audit_log.

Refused inside agent sessions (Claude Code, agy, StayPoint daemon runs) and
when stdin/stdout is not a terminal.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		minutes, _ := cmd.Flags().GetInt("minutes")
		company, _ := cmd.Flags().GetString("company")
		reason, _ := cmd.Flags().GetString("reason")
		noWait, _ := cmd.Flags().GetBool("no-wait")
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		if err := refuseOverrideInAgentContext(os.Getenv, tty); err != nil {
			return err
		}
		daemonURL, token := resolveDaemonConn()
		return requestTrackingOverride(cmd.OutOrStdout(), daemonURL, token, company, minutes, reason, !noWait)
	},
}

var gateTrackingCmd = &cobra.Command{
	Use:   "tracking",
	Short: "Show the work-repo tracking gate: per-company state and active Board overrides",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		conn := store.DB()
		out := cmd.OutOrStdout()
		settings, err := trackgate.ListSettings(conn)
		if err != nil {
			return err
		}
		companies := []string{trackgate.CompanyManagedSolution, trackgate.CompanyPersonal}
		seen := map[string]bool{strings.ToLower(companies[0]): true, strings.ToLower(companies[1]): true}
		for k := range settings {
			if !seen[k] {
				companies = append(companies, k)
				seen[k] = true
			}
		}
		for _, c := range companies {
			on, err := trackgate.Enabled(conn, c)
			if err != nil {
				return err
			}
			src := "default"
			if _, ok := settings[strings.ToLower(c)]; ok {
				src = "Board setting"
			}
			state := "off"
			if on {
				state = "ON"
			}
			line := fmt.Sprintf("%-20s %-3s (%s)", c, state, src)
			if ov, err := trackgate.ActiveOverride(conn, c, time.Now()); err == nil && ov != nil {
				line += fmt.Sprintf("  override active until %s (request %s)", ov.ExpiresAt.Local().Format("15:04"), ov.GateRequestID)
			}
			fmt.Fprintln(out, line)
		}
		fmt.Fprintln(out, "Toggle per company: Board UI / POST /api/settings/tracking-gate (passkey). Override: staypoint gate override --minutes N")
		return nil
	},
}

func init() {
	gateOverrideCmd.Flags().Int("minutes", 30, fmt.Sprintf("Override length in minutes after Board approval (1-%d)", trackgate.MaxOverrideMinutes))
	gateOverrideCmd.Flags().String("company", trackgate.CompanyManagedSolution, "Company whose tracking gate to override")
	gateOverrideCmd.Flags().String("reason", "", "Why the override is needed (shown to the Board)")
	gateOverrideCmd.Flags().Bool("no-wait", false, "File the request and exit without waiting for the Board decision")
	gateCmd.AddCommand(gateOverrideCmd)
	gateCmd.AddCommand(gateTrackingCmd)
}

// agentContextEnv are variables set inside agent sessions. Their presence
// means the caller is (or was spawned by) an agent.
var agentContextEnv = []string{
	"STAYPOINT_TASK_ID",      // StayPoint daemon run
	"CLAUDECODE",             // Claude Code Bash tool
	"CLAUDE_CODE_SESSION_ID", // Claude Code Bash tool
	"CLAUDE_CODE_ENTRYPOINT", // Claude Code
	"AI_AGENT",               // Claude Code and other agent CLIs
	"GEMINI_CLI",             // Gemini CLI
	"ANTIGRAVITY_LS_ADDRESS", // agy
	"ANTIGRAVITY_CSRF_TOKEN", // agy
}

// refuseOverrideInAgentContext is the first, soft layer: an agent cannot
// even file an override request through this command. The hard layer is the
// passkey-gated approval.
func refuseOverrideInAgentContext(getenv func(string) string, tty bool) error {
	return refuseBoardOnlyInAgentContext("gate override", getenv, tty)
}

// refuseBoardOnlyInAgentContext refuses a Board-only CLI action (label names
// it in the error) inside an agent session or without a terminal.
func refuseBoardOnlyInAgentContext(label string, getenv func(string) string, tty bool) error {
	for _, k := range agentContextEnv {
		if strings.TrimSpace(getenv(k)) != "" {
			return fmt.Errorf("%s: refused inside an agent session (%s is set); the Board runs this from its own terminal", label, k)
		}
	}
	if !tty {
		return fmt.Errorf("%s: stdin/stdout is not a terminal; refusing (Board-only command)", label)
	}
	return nil
}

func requestTrackingOverride(out io.Writer, daemonURL, token, company string, minutes int, reason string, wait bool) error {
	company = strings.TrimSpace(company)
	if company == "" {
		return fmt.Errorf("--company is required")
	}
	if minutes < 1 || minutes > trackgate.MaxOverrideMinutes {
		return fmt.Errorf("--minutes must be between 1 and %d", trackgate.MaxOverrideMinutes)
	}
	if daemonURL == "" {
		return fmt.Errorf("gate override: daemon not configured")
	}
	reasons := []string{fmt.Sprintf("Tracking-gate override: allow untracked writes in %s work repos for %d minutes after approval", company, minutes)}
	if r := strings.TrimSpace(reason); r != "" {
		reasons = append(reasons, "Reason: "+r)
	}
	gr := createGateRequest(daemonURL, token, trackgate.OverrideCmdline(company, minutes), reasons, trackgate.OverrideRunID)
	if gr == nil {
		return fmt.Errorf("gate override: could not file the request with staypointd at %s (is it running?)", daemonURL)
	}
	fmt.Fprintf(out, "Override request %s filed. Approve it in the Board UI (pending security gate requests; passkey required).\n", gr.ID)
	if !wait {
		return nil
	}
	fmt.Fprintln(out, "Waiting for the Board decision (Ctrl-C to stop waiting; the request stays pending)...")
	for {
		switch status := pollGateRequest(daemonURL, token, gr.ID); status {
		case "approved":
			fmt.Fprintf(out, "Approved. Tracking gate for %s is overridden until %s.\n", company,
				time.Now().Add(time.Duration(minutes)*time.Minute).Format("15:04"))
			return nil
		case "denied":
			return fmt.Errorf("gate override: the Board denied request %s", gr.ID)
		case "":
			return fmt.Errorf("gate override: lost contact with staypointd while waiting; request %s may still be pending", gr.ID)
		}
	}
}
