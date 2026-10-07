package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
	"github.com/spf13/cobra"
)

// STA-854: `staypoint task attach <task-id>` records an interactive agent
// session against a task so the PreToolUse tracking gate lets it write in
// work repos. `staypoint task create --org <company> "title"` creates a local
// task (no AI inference, no Paperclip) and attaches the session in one step.

var taskAttachCmd = &cobra.Command{
	Use:   "attach <task-id>",
	Short: "Attach this interactive agent session to a task (unlocks the work-repo tracking gate)",
	Long: `Attach an interactive Claude Code or agy session to a StayPoint task.

Work repos are gated: Edit/Write and state-changing shell commands are blocked
until the session is attached to an active task. The gate's block message
prints the exact command, including the session id.

The session id defaults to $CLAUDE_CODE_SESSION_ID (set by Claude Code in its
Bash tool). Pass --session for agy (its conversationId) or when the variable is
not set. The attachment is shown on the task timeline as
"Interactive session attached".`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		session, client, repo, err := attachFlags(cmd)
		if err != nil {
			return err
		}
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		return attachSession(cmd.OutOrStdout(), store.DB(), args[0], session, client, repo)
	},
}

func init() {
	addAttachFlags(taskAttachCmd)
	taskCmd.AddCommand(taskAttachCmd)

	taskCreateCmd.Flags().String("org", "", "Create a local task for this company (e.g. 'Managed Solution') and attach this session; skips AI inference and Paperclip")
	addAttachFlags(taskCreateCmd)
}

func addAttachFlags(cmd *cobra.Command) {
	cmd.Flags().String("session", "", "Agent session id (default $CLAUDE_CODE_SESSION_ID; agy: the conversationId from the gate message)")
	cmd.Flags().String("client", "", "Agent client: claude or gemini (agy); default claude")
	cmd.Flags().String("repo", "", "Repository path recorded with the attachment / task (default: current directory)")
}

// resolveSessionID picks the session id from the flag or the environment.
func resolveSessionID(flag string, getenv func(string) string) string {
	if s := strings.TrimSpace(flag); s != "" {
		return s
	}
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"} {
		if s := strings.TrimSpace(getenv(k)); s != "" {
			return s
		}
	}
	return ""
}

func attachFlags(cmd *cobra.Command) (session string, client trackgate.Client, repo string, err error) {
	sFlag, _ := cmd.Flags().GetString("session")
	cFlag, _ := cmd.Flags().GetString("client")
	repo, _ = cmd.Flags().GetString("repo")
	session = resolveSessionID(sFlag, os.Getenv)
	switch strings.ToLower(strings.TrimSpace(cFlag)) {
	case "", "claude":
		client = trackgate.ClientClaude
	case "gemini", "agy", "antigravity":
		client = trackgate.ClientGemini
	default:
		return "", "", "", fmt.Errorf("--client must be claude or gemini, got %q", cFlag)
	}
	if repo == "" {
		repo, _ = os.Getwd()
	}
	return session, client, repo, nil
}

func attachSession(out io.Writer, conn *sql.DB, taskID, session string, client trackgate.Client, repo string) error {
	if session == "" {
		return fmt.Errorf("no session id: pass --session <id> (the tracking-gate block message prints it; $CLAUDE_CODE_SESSION_ID is not set)")
	}
	if err := trackgate.Attach(conn, session, taskID, client, repo); err != nil {
		return err
	}
	fmt.Fprintf(out, "Attached %s session %s to task %s. Writes in gated work repos are now allowed for this session.\n", client, session, taskID)
	return nil
}

// localOrgCreateRequested reports whether runTaskCreate should take the
// local --org path.
func localOrgCreateRequested(cmd *cobra.Command) bool {
	org, _ := cmd.Flags().GetString("org")
	return strings.TrimSpace(org) != ""
}

func runTaskCreateLocalOrg(cmd *cobra.Command, args []string) error {
	org, _ := cmd.Flags().GetString("org")
	project, _ := cmd.Flags().GetString("project")
	budget, _ := cmd.Flags().GetFloat64("budget")
	maxTurns, _ := cmd.Flags().GetInt("max-turns")
	title := strings.TrimSpace(strings.Join(args, " "))
	if title == "" {
		return fmt.Errorf("a title is required: staypoint task create --org %q \"title\"", strings.TrimSpace(org))
	}
	session, client, repo, err := attachFlags(cmd)
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	return createLocalOrgTask(cmd.OutOrStdout(), store.DB(), strings.TrimSpace(org), project, title, repo, session, client, budget, maxTurns)
}

func createLocalOrgTask(out io.Writer, conn *sql.DB, org, project, title, repo, session string, client trackgate.Client, budget float64, maxTurns int) error {
	task, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{
		Name:         title,
		RepoPath:     repo,
		Organization: org,
		Project:      project,
		MaxBudgetUSD: budget,
		MaxTurns:     maxTurns,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Created task %s (%s): %s\n", task.ID, org, task.Name)
	if session == "" {
		fmt.Fprintf(out, "No session id found; attach with: staypoint task attach %s --session <id>\n", task.ID)
		return nil
	}
	return attachSession(out, conn, task.ID, session, client, task.RepoPath)
}
