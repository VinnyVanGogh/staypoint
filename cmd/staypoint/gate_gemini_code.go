package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/spf13/cobra"
)

// Board addition (2026-10-06): `staypoint gate gemini-code --session <id>`
// asks the Board to let one agy session write code in a personal repo. The
// command only files the request; the Board approves it with Touch ID
// (passkey) in the Board UI. Agents may run it (the agy PreToolUse hook tells
// them to); they can never approve it. Work repos are refused here, by the
// daemon endpoint, and again at use time.

var gateGeminiCodeCmd = &cobra.Command{
	Use:   "gemini-code",
	Short: "Ask the Board (Touch ID) to let one agy session write code in a personal repo",
	Long: `File a Board approval request that lets Gemini write code for one agy
session (conversationId) in a personal repo, for up to ` + fmt.Sprint(geminiapproval.MaxSessionHours) + ` hours after approval.

Work repos are always refused: Gemini never writes code there, approval or not.
Daemon task runs file their own per-run request (provider=gemini on a code task).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		session, _ := cmd.Flags().GetString("session")
		repo, _ := cmd.Flags().GetString("repo")
		noWait, _ := cmd.Flags().GetBool("no-wait")
		if repo == "" {
			repo, _ = os.Getwd()
		}
		daemonURL, token := resolveDaemonConn()
		return requestGeminiCode(cmd.OutOrStdout(), daemonURL, token, session, repoRootFor(repo), !noWait)
	},
}

func init() {
	gateGeminiCodeCmd.Flags().String("session", "", "agy conversationId the approval covers (required)")
	gateGeminiCodeCmd.Flags().String("repo", "", "Repo the approval covers (default: the git repo containing the working directory)")
	gateGeminiCodeCmd.Flags().Bool("no-wait", false, "File the request and exit without waiting for the Board decision")
	gateCmd.AddCommand(gateGeminiCodeCmd)
}

func requestGeminiCode(out io.Writer, daemonURL, token, session, repo string, wait bool) error {
	scope := geminiapproval.Scope{SessionID: strings.TrimSpace(session), Repo: repo}
	if err := geminiapproval.Validate(scope, cliRepoIsWork); err != nil {
		return fmt.Errorf("gate gemini-code: %w", err)
	}
	reasons := []string{scope.Title(), "Board rule: Gemini never writes code; personal repos may allow it per agy session with Touch ID. Work repos: never."}
	gr := createGateRequest(daemonURL, token, scope.Cmdline(), reasons, geminiapproval.RunID)
	if gr == nil {
		return fmt.Errorf("gate gemini-code: could not file the request with staypointd at %s (is it running?)", daemonURL)
	}
	fmt.Fprintf(out, "Request %s filed: %s. Approve it in the Board UI with Touch ID.\n", gr.ID, scope.Title())
	if !wait {
		return nil
	}
	fmt.Fprintln(out, "Waiting for the Board decision (Ctrl-C to stop waiting; the request stays pending)...")
	for {
		switch status := pollGateRequest(daemonURL, token, gr.ID); status {
		case "approved":
			fmt.Fprintf(out, "Approved. agy session %s may write code in %s for up to %dh.\n", scope.SessionID, repo, geminiapproval.MaxSessionHours)
			return nil
		case "denied":
			return fmt.Errorf("gate gemini-code: the Board denied request %s", gr.ID)
		case "":
			return fmt.Errorf("gate gemini-code: lost contact with staypointd while waiting; request %s may still be pending", gr.ID)
		}
	}
}

// repoRootFor returns the directory holding the nearest .git above p (p's
// own directory for a file), or p when there is none.
func repoRootFor(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	dir := abs
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		dir = filepath.Dir(abs)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if parent := filepath.Dir(d); parent == d {
			return dir
		}
	}
}
