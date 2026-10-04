package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/ui/board"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var boardCmd = &cobra.Command{
	Use:   "board",
	Short: "Interactive Kanban board TUI with live daemon updates and thread view",
	Long: `Launch an interactive Kanban board TUI with columns for todo, in_progress,
in_review, and done. Supports live push updates over SSE from staypointd and
detailed task thread views.

Features:
  - 4 Kanban columns (todo, in_progress, in_review, done) matching SQLite state
  - Detail Thread View with comments, deliverables, and activity log
  - Real-time reactive updates from the daemon over SSE without polling
  - In-place task stage moving and comment submission`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("db")
		daemonURL, _ := cmd.Flags().GetString("daemon-url")
		token, _ := cmd.Flags().GetString("token")
		standalone, _ := cmd.Flags().GetBool("standalone")

		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}

		if dbPath == "" && cfg != nil {
			dbPath = cfg.DBPath
		}
		if token == "" && cfg != nil {
			tokenPath := filepath.Join(cfg.DataDir, "auth_token")
			if data, err := os.ReadFile(tokenPath); err == nil {
				token = strings.TrimSpace(string(data))
			}
		}

		boardCfg := board.Config{
			DBPath:     dbPath,
			DaemonURL:  daemonURL,
			Token:      token,
			Standalone: standalone,
			RepoPath:   cwd,
		}

		m, err := board.NewModel(boardCfg)
		if err != nil {
			return fmt.Errorf("failed to initialize board: %w", err)
		}

		p := tea.NewProgram(
			m,
			tea.WithAltScreen(),
			tea.WithMouseCellMotion(),
		)

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("board session error: %w", err)
		}
		return nil
	},
}

// boardURLCmd prints a Board bootstrap URL containing a single-use nonce.
// It is TTY-gated and refuses in agent context. The nonce is obtained from
// the daemon's /api/board/fresh-nonce endpoint (which requires both the session
// auth token and the board token), so the long-lived board_token never appears
// in a URL and the nonce cannot be replayed.
var boardURLCmd = &cobra.Command{
	Use:   "url",
	Short: "Print the Board bootstrap URL (TTY-only; refuses in agent context)",
	Long: `Print a single-use Board bootstrap URL containing a one-time nonce.
Open the URL in a browser to start a Board session with full approve/reject/decide rights.

The URL contains board_nonce (a single-use random value) instead of the long-lived
board_token, so it is safe to share exactly once and cannot be replayed.

Refused when:
  - stdout is not a terminal (prevents credential capture in logs/pipes)
  - STAYPOINT_TASK_ID is set (running inside an agent harness)

The board_token is read from DataDir/board_token, which staypointd persists on startup.
Run 'scripts/reinstall-daemon.sh' once after upgrading to create the file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Refuse in agent context.
		if os.Getenv("STAYPOINT_TASK_ID") != "" {
			return fmt.Errorf("board url: refused in agent context (STAYPOINT_TASK_ID is set)")
		}
		// Refuse when stdout is not a TTY.
		fi, err := os.Stdout.Stat()
		if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
			return fmt.Errorf("board url: stdout is not a terminal; refusing to print board credential")
		}

		if cfg == nil {
			return fmt.Errorf("board url: config not loaded")
		}

		authTokenPath := filepath.Join(cfg.DataDir, "auth_token")
		boardTokenPath := filepath.Join(cfg.DataDir, "board_token")

		authData, err := os.ReadFile(authTokenPath)
		if err != nil {
			return fmt.Errorf("board url: cannot read auth token from %s: %w", authTokenPath, err)
		}
		authToken := strings.TrimSpace(string(authData))
		if len(authToken) < 16 {
			return fmt.Errorf("board url: auth token at %s is too short or invalid", authTokenPath)
		}

		boardData, err := os.ReadFile(boardTokenPath)
		if err != nil {
			return fmt.Errorf("board url: cannot read board token from %s: %w\nRun 'scripts/reinstall-daemon.sh' to create it", boardTokenPath, err)
		}
		boardToken := strings.TrimSpace(string(boardData))
		if len(boardToken) < 16 {
			return fmt.Errorf("board url: board token at %s is too short or invalid", boardTokenPath)
		}

		daemonURL, _ := cmd.Flags().GetString("daemon-url")

		// Obtain a fresh single-use nonce from the daemon.
		// The endpoint requires both the session auth token (Bearer) and the board
		// token (X-Board-Token), so agents with only the session token cannot call it.
		req, err := http.NewRequest(http.MethodPost, daemonURL+"/api/board/fresh-nonce", nil)
		if err != nil {
			return fmt.Errorf("board url: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("X-Board-Token", boardToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("board url: daemon request failed: %w\nIs staypointd running?", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("board url: daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var result struct {
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal(body, &result); err != nil || result.Nonce == "" {
			return fmt.Errorf("board url: unexpected response from daemon: %s", string(body))
		}

		// Emit localhost (not 127.0.0.1) so the RPID "localhost" matches the WebAuthn origin.
		u, err := url.Parse(daemonURL)
		if err != nil {
			return fmt.Errorf("board url: parse daemon URL: %w", err)
		}
		localhostURL := "http://localhost"
		if port := u.Port(); port != "" {
			localhostURL = fmt.Sprintf("http://localhost:%s", port)
		}
		fmt.Printf("%s/?token=%s&board_nonce=%s\n", localhostURL, authToken, result.Nonce)
		return nil
	},
}

func init() {
	boardCmd.Flags().String("db", "", "Path to SQLite database")
	boardCmd.Flags().String("daemon-url", "http://127.0.0.1:41421", "Daemon HTTP/SSE server URL")
	boardCmd.Flags().String("token", "", "Daemon authentication token")
	boardCmd.Flags().Bool("standalone", false, "Force standalone mode without connecting to daemon SSE")

	boardURLCmd.Flags().String("daemon-url", "http://127.0.0.1:41421", "Daemon base URL")
	boardCmd.AddCommand(boardURLCmd)

	rootCmd.AddCommand(boardCmd)
}
