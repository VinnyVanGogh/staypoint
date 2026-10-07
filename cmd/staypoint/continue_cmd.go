package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/spf13/cobra"
)

var continueCmd = &cobra.Command{
	Use:     "continue",
	Aliases: []string{"cont"},
	Short:   "Continue the most recent agent session in this repository",
	Run: func(cmd *cobra.Command, args []string) {
		handoffFlag, _ := cmd.Flags().GetBool("handoff")
		handleContinueFlow(cmd, handoffFlag)
	},
}

var resumeCmd = &cobra.Command{
	Use:   "resume [session-id]",
	Short: "Interactive session picker across Claude and Antigravity",
	Run: func(cmd *cobra.Command, args []string) {
		handoffFlag, _ := cmd.Flags().GetBool("handoff")
		handleResumeFlow(cmd, args, handoffFlag)
	},
}

func handleContinueFlow(cmd *cobra.Command, handoffOnly bool) {
	cwd, _ := os.Getwd()
	var storeDB *sql.DB
	if store, err := db.Open(cfg.DBPath); err == nil {
		storeDB = store.DB()
		defer store.Close()
	}

	sess, err := meshContext.GetLatestSession(cwd, storeDB)
	if err != nil || sess == nil {
		fmt.Println("No previous session found in this repository. Starting new session...")
		_ = cmd.Flags().Set("continue", "false")
		runSmartLaunch(cmd, nil)
		return
	}

	if handoffOnly {
		_, manifest, err := meshContext.GenerateSessionHandoffWithOptions(sess, storeDB, meshContext.SessionHandoffOptions{
			Trigger:        "manual",
			DataDir:        cfg.DataDir,
			MaxKeepPerRepo: cfg.MaxHandoffsPerRepo,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating handoff: %v\n", err)
			return
		}
		origin := "Claude Code"
		if sess.AgentType == "gemini" {
			origin = "Antigravity (Gemini)"
		}
		idShort := sess.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		fmt.Printf("\n\033[1;32m✔ Handoff synthesized from %s session\033[0m [%s]\n", origin, idShort)
		fmt.Printf("  • Staged in system clipboard (`pbcopy`) and `/tmp/ai-handoff.md`\n")
		if manifest != nil && manifest.HandoffFile != "" {
			fmt.Printf("  • Saved to: %s\n", manifest.HandoffFile)
		}
		if sess.RootGoal != "" {
			fmt.Printf("  • Goal: %s\n", sess.RootGoal)
		}
		if len(sess.UserDirectives) > 0 {
			fmt.Printf("  • Directives captured: %d user instructions\n", len(sess.UserDirectives))
		}
		fmt.Println("  • Ready to paste into any agent (Claude, Gemini, ChatGPT, etc.).")
		return
	}

	// Dispatch to latest tool's continue flag
	if sess.AgentType == "claude" {
		binPath, err := exec.LookPath("claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: claude not found on PATH: %v\n", err)
			return
		}
		_ = syscall.Exec(binPath, []string{"claude", "-c"}, os.Environ())
	} else {
		exitIfAgyInWorkRepo("agy", "") // STA-854
		binPath, err := exec.LookPath("agy")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: agy not found on PATH: %v\n", err)
			return
		}
		_ = syscall.Exec(binPath, []string{"agy", "-c"}, os.Environ())
	}
}

func handleResumeFlow(cmd *cobra.Command, args []string, handoffOnly bool) {
	cwd, _ := os.Getwd()
	var storeDB *sql.DB
	if store, err := db.Open(cfg.DBPath); err == nil {
		storeDB = store.DB()
		defer store.Close()
	}

	sessions, err := meshContext.DiscoverSessions(cwd, 8, storeDB)
	if err != nil || len(sessions) == 0 {
		fmt.Println("No previous agent sessions found in this repository.")
		return
	}

	var selectedSess *meshContext.SessionInfo
	if len(args) > 0 {
		targetID := args[0]
		for _, s := range sessions {
			if strings.HasPrefix(s.ID, targetID) {
				selectedSess = &s
				break
			}
		}
		if selectedSess == nil {
			fmt.Fprintf(os.Stderr, "Session %q not found among recent sessions.\n", targetID)
			return
		}
	} else {
		repoName := filepath.Base(cwd)
		fmt.Printf("\n\033[1;36m[Staypoint :: Recent Sessions in %s]\033[0m\n", repoName)
		for i, s := range sessions {
			toolBadge := "\033[1;35m🟣 Claude\033[0m"
			if s.AgentType == "gemini" {
				toolBadge = "\033[1;34m🔵 Antigravity\033[0m"
			}
			age := time.Since(s.UpdatedAt).Round(time.Minute)
			ageStr := fmt.Sprintf("%v ago", age)
			if age < time.Minute {
				ageStr = "just now"
			}

			summary := s.LastUserPrompt
			if summary == "" {
				summary = s.RootGoal
			}
			if summary == "" {
				summary = "Ongoing session"
			}
			if len(summary) > 60 {
				summary = summary[:60] + "..."
			}

			branchInfo := ""
			if s.GitBranch != "" {
				branchInfo = fmt.Sprintf(" [%s]", s.GitBranch)
			}

			fmt.Printf("  %d. %s  (%s)%s  %q\n", i+1, toolBadge, ageStr, branchInfo, summary)
		}

		actionLabel := "resume"
		if handoffOnly {
			actionLabel = "hand off"
		}
		fmt.Printf("\nSelect session [1-%d] to %s (or press Enter for #1): ", len(sessions), actionLabel)

		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		pickIdx := 0
		if input != "" {
			var choice int
			if _, err := fmt.Sscanf(input, "%d", &choice); err == nil && choice >= 1 && choice <= len(sessions) {
				pickIdx = choice - 1
			} else {
				found := false
				for i, s := range sessions {
					if strings.HasPrefix(s.ID, input) {
						pickIdx = i
						found = true
						break
					}
				}
				if !found {
					fmt.Printf("Invalid choice, defaulting to #1.\n")
					pickIdx = 0
				}
			}
		}
		selectedSess = &sessions[pickIdx]
	}

	if handoffOnly {
		_, manifest, err := meshContext.GenerateSessionHandoffWithOptions(selectedSess, storeDB, meshContext.SessionHandoffOptions{
			Trigger:        "manual",
			DataDir:        cfg.DataDir,
			MaxKeepPerRepo: cfg.MaxHandoffsPerRepo,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating handoff: %v\n", err)
			return
		}
		origin := "Claude Code"
		if selectedSess.AgentType == "gemini" {
			origin = "Antigravity (Gemini)"
		}
		idShort := selectedSess.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		fmt.Printf("\n\033[1;32m✔ Handoff synthesized from %s session\033[0m [%s]\n", origin, idShort)
		fmt.Printf("  • Staged in system clipboard (`pbcopy`) and `/tmp/ai-handoff.md`\n")
		if manifest != nil && manifest.HandoffFile != "" {
			fmt.Printf("  • Saved to: %s\n", manifest.HandoffFile)
		}
		if selectedSess.RootGoal != "" {
			fmt.Printf("  • Goal: %s\n", selectedSess.RootGoal)
		}
		if len(selectedSess.UserDirectives) > 0 {
			fmt.Printf("  • Directives captured: %d user instructions\n", len(selectedSess.UserDirectives))
		}
		fmt.Println("  • Ready to paste into any agent (Claude, Gemini, ChatGPT, etc.).")
		return
	}

	// Launch session
	if selectedSess.AgentType == "claude" {
		binPath, err := exec.LookPath("claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: claude not found on PATH: %v\n", err)
			return
		}
		_ = syscall.Exec(binPath, []string{"claude", "--resume", selectedSess.ID}, os.Environ())
	} else {
		exitIfAgyInWorkRepo("agy", "") // STA-854
		binPath, err := exec.LookPath("agy")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: agy not found on PATH: %v\n", err)
			return
		}
		_ = syscall.Exec(binPath, []string{"agy", "--conversation", selectedSess.ID}, os.Environ())
	}
}

func init() {
	rootCmd.AddCommand(continueCmd)
	rootCmd.AddCommand(resumeCmd)
	continueCmd.Flags().BoolP("handoff", "H", false, "Synthesize handoff prompt to clipboard instead of launching")
	resumeCmd.Flags().BoolP("handoff", "H", false, "Synthesize handoff prompt to clipboard instead of launching")
}
