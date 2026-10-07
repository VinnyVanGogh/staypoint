package main

import (
	"fmt"
	"os"

	"github.com/VinnyVanGogh/staypoint/internal/ui/chat"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Interactive full-screen Bubble Tea chat TUI with streaming and slash commands",
	Long: `Launch an interactive full-screen Bubble Tea chat session.
Features:
  - 30fps debounced token streaming without terminal flicker
  - Markdown and syntax highlighting via glamour
  - Slash commands: /model, /task, /checkpoint, /undo, /clear, /help
  - Graceful subprocess cancellation on Ctrl+C without orphan processes
  - Automatic connection to staypointd over IPC with seamless in-process fallback`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID, _ := cmd.Flags().GetString("session")
		modelName, _ := cmd.Flags().GetString("model")
		taskID, _ := cmd.Flags().GetString("task")
		socketPath, _ := cmd.Flags().GetString("socket")
		inProcess, _ := cmd.Flags().GetBool("in-process")

		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}

		chatCfg := chat.Config{
			SessionID:  sessionID,
			RepoPath:   cwd,
			Model:      modelName,
			TaskID:     taskID,
			SocketPath: socketPath,
			InProcess:  inProcess,
		}

		if cfg != nil {
			chatCfg.DBPath = cfg.DBPath
		}

		m, err := chat.NewModel(chatCfg)
		if err != nil {
			return fmt.Errorf("failed to initialize chat: %w", err)
		}

		p := tea.NewProgram(
			m,
			tea.WithAltScreen(),
			tea.WithMouseCellMotion(),
		)

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("chat session error: %w", err)
		}
		return nil
	},
}

func init() {
	chatCmd.Flags().StringP("session", "s", "", "Resume or specify a conversation session ID")
	chatCmd.Flags().StringP("model", "m", "", "Initial model to chat with (defaults to opus; Gemini only when named)")
	chatCmd.Flags().StringP("task", "t", "", "Bind chat session to a specific task ID")
	chatCmd.Flags().String("socket", "", "Override staypointd IPC socket path")
	chatCmd.Flags().Bool("in-process", false, "Force in-process mode without connecting to staypointd")

	rootCmd.AddCommand(chatCmd)
}
