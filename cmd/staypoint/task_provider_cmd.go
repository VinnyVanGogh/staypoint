package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/spf13/cobra"
)

// STA-838: the Board's explicit per-task provider/model choice.
//
//	staypoint task create --org X --kind planning --provider gemini "title"
//	staypoint task set-provider <id> --provider gemini --model gemini-3.8-flash-high
//	staypoint task set-provider <id> --provider ""        # back to the default
//
// provider=gemini is refused for code kinds (coding, qa): Gemini never writes
// code (router.GeminiCodeForbidden).

const providerFlagHelp = "Provider for this task: claude or gemini (default: by kind). gemini on coding/qa: refused in work repos; in personal repos every run waits for Board Touch ID"
const modelFlagHelp = "Model for --provider: opus, sonnet, gemini-3.1-pro-high, gemini-3.8-flash-high"

func init() {
	taskCreateCmd.Flags().String("provider", "", providerFlagHelp)
	taskCreateCmd.Flags().String("model", "", modelFlagHelp)

	setProviderCmd.Flags().String("provider", "", providerFlagHelp+"; empty restores the default")
	setProviderCmd.Flags().String("model", "", modelFlagHelp)
	taskCmd.AddCommand(setProviderCmd)
}

// taskChoiceFromFlags validates --provider/--model for a task of kind in repo
// ("" = the working directory).
func taskChoiceFromFlags(cmd *cobra.Command, kind, repo string) (router.RouteChoice, error) {
	provider, _ := cmd.Flags().GetString("provider")
	model, _ := cmd.Flags().GetString("model")
	return router.ValidateTaskChoice(kind, cliRepoIsWork(repo), provider, model)
}

// cliRepoIsWork classifies repo ("" = cwd) for the Gemini rules; an
// unreadable classification counts as work (fail closed).
func cliRepoIsWork(repo string) bool {
	if repo == "" {
		repo, _ = os.Getwd()
	}
	ok, _, err := router.IsWorkRepo(repo)
	return err != nil || ok
}

var setProviderCmd = &cobra.Command{
	Use:   "set-provider <id>",
	Short: "Set the Board's provider/model choice for a task (gemini on code kinds: personal repos only, Touch ID per run)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider, _ := cmd.Flags().GetString("provider")
		model, _ := cmd.Flags().GetString("model")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		return setTaskProvider(cmd.OutOrStdout(), store.DB(), args[0], provider, model)
	},
}

func setTaskProvider(out io.Writer, conn *sql.DB, id, provider, model string) error {
	task, err := meshContext.GetTask(conn, id)
	if err != nil {
		return err
	}
	choice, err := router.ValidateTaskChoice(task.WorkKind, cliRepoIsWork(task.RepoPath), provider, model)
	if err != nil {
		return err
	}
	updated, err := meshContext.SetTaskProvider(conn, task.ID, choice.Provider, choice.Model)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Task %s (%s): provider %s\n", updated.ID, updated.WorkKind, describeChoice(choice))
	if router.NeedsGeminiCodeApproval(updated.WorkKind, cliRepoIsWork(updated.RepoPath), choice) {
		fmt.Fprintln(out, "Gemini on a code task: every run waits for a Board Touch ID approval (one approval = one run).")
	}
	return nil
}

func describeChoice(c router.RouteChoice) string {
	p := c.Provider
	if p == "" {
		p = "default"
	}
	if c.Model != "" {
		p += " / " + c.Model
	}
	return p
}
