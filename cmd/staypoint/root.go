package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var (
	version = "0.3.0"
	commit  = "none"
	date    = "unknown"
	cfg     *config.Config
	rootCmd = &cobra.Command{
		Use:     "staypoint [command|args...]",
		Version: version,
		Short:   "Staypoint: Autonomous AI Agent Ops, Quota Pacing & Context Platform",
		Long: `Staypoint: Autonomous AI Agent Ops, Quota Pacing & Context Platform

With no command, staypoint is a smart launcher: it routes this directory to a
Claude seat (or agy with --gemini), prints the route, and execs that CLI with
any remaining arguments. Use --dry-run (-n) to see the route without launching.

The interactive TUI is "staypoint board".`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			var err error
			cfg, err = config.LoadConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			return nil
		},
		FParseErrWhitelist: cobra.FParseErrWhitelist{
			UnknownFlags: true,
		},
		Run: runSmartLaunch,
	}
)

func runSmartLaunch(cmd *cobra.Command, args []string) {
	statusFlag, _ := cmd.Flags().GetBool("status")
	if statusFlag {
		statusCmd.Run(cmd, args)
		return
	}

	continueFlag, _ := cmd.Flags().GetBool("continue")
	resumeFlag, _ := cmd.Flags().GetBool("resume")
	handoffFlag, _ := cmd.Flags().GetBool("handoff")

	if continueFlag {
		handleContinueFlow(cmd, handoffFlag)
		return
	}
	if resumeFlag {
		handleResumeFlow(cmd, args, handoffFlag)
		return
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	forceClaude, _ := cmd.Flags().GetBool("claude")
	forceGemini, _ := cmd.Flags().GetBool("gemini")
	force, _ := cmd.Flags().GetBool("force")
	noSSH, _ := cmd.Flags().GetBool("no-ssh")
	forceWork, _ := cmd.Flags().GetBool("work")
	forcePersonal, _ := cmd.Flags().GetBool("personal")
	if forceWork || forcePersonal {
		// Picking an account only makes sense for Claude.
		forceClaude = true
		forceGemini = false
	}

	if force && !forceClaude && !forceGemini {
		if strings.Contains(os.Args[0], "claude") {
			forceClaude = true
		} else if strings.Contains(os.Args[0], "agy") {
			forceGemini = true
		}
	}

	var srcArgs []string
	if len(os.Args) > 1 && !strings.HasSuffix(os.Args[0], ".test") {
		srcArgs = os.Args[1:]
	} else {
		srcArgs = args
	}
	passthroughArgs := extractPassthroughArgs(srcArgs)
	headless := isHeadlessStream(passthroughArgs)

	// Say something before any work so a bare `staypoint` never looks hung.
	if !headless {
		fmt.Fprintln(os.Stderr, "staypoint: routing... (the TUI is `staypoint board`)")
	}

	pacerState, _ := router.LoadPacerState()
	cwd, _ := os.Getwd()
	routeCtx, routeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer routeCancel()

	remoteHost := "company-mbp"
	if cfg != nil && cfg.RemoteHost != "" {
		remoteHost = cfg.RemoteHost
	}

	var decision *router.RouteDecision
	if forceClaude {
		decision = &router.RouteDecision{Tool: "claude", Model: "claude-sonnet-4-6", Reason: "forced: --claude"}
	} else if forceGemini {
		decision = &router.RouteDecision{Tool: "agy", Model: "gemini-3.8-flash-high", Reason: "forced: --gemini"}
	} else {
		prefTool := "auto"
		if cfg != nil && cfg.PreferredPersonalTool != "" {
			prefTool = cfg.PreferredPersonalTool
		}

		// No LastUsedTool: Route never reads it (GeminiCodeForbidden), and
		// finding it parsed every Claude transcript on disk, seconds of CPU
		// before the first byte of output.
		var err error
		decision, err = router.Route(routeCtx, cwd, pacerState, router.RouteOptions{
			CheckSSH:              !noSSH,
			RemoteHost:            remoteHost,
			PreferredPersonalTool: prefTool,
			UIOLI:                 router.UIOLIFromConfig(cfg),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Routing error: %v\n", err)
			os.Exit(1)
		}
		if decision.Waiting {
			fmt.Fprintf(os.Stderr, "[staypoint] %s\n", decision.Reason)
		}
	}
	targetTool := decision.Tool
	targetModel := decision.Model
	isRemoteWork := decision.Target == router.TargetRemoteClaude

	// The seat is the route's too: a work repo whose work seat is locked
	// routes to the personal seat, and the launch must use that one.
	home, _ := os.UserHomeDir()
	isWorkRepo, _, _ := router.IsWorkRepo(cwd)
	seatIsWork := isWorkRepo
	if decision.AccountRole != "" {
		seatIsWork = decision.AccountRole == "work"
	}
	account := resolveClaudeAccount(forceWork, forcePersonal, seatIsWork)
	if targetTool == "claude" {
		decision.AccountRole = string(account)
	}

	// Render the Tokyo Night statusline before launch (to stderr if non-tty pipe or headless stream, stdout if interactive terminal).
	// Its plan line is this decision, not a second routing pass.
	isTerminal := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	if !isTerminal || headless {
		_ = router.RenderStatuslineFor(os.Stderr, nil, decision)
	} else {
		_ = router.RenderStatuslineFor(os.Stdout, nil, decision)
	}

	if dryRun {
		fmt.Printf("\n\033[1;36m[Staypoint :: Dry Run]\033[0m\n")
		if targetTool == "agy" && isWorkRepo {
			fmt.Printf("  • REFUSED:     agy is not allowed in work repos (use claude --work)\n")
		}
		fmt.Printf("  • Tool:        %s\n", targetTool)
		fmt.Printf("  • Model:       %s\n", targetModel)
		fmt.Printf("  • Remote Work: %t\n", isRemoteWork)
		if targetTool == "claude" {
			fmt.Printf("  • Account:     %s %s\n", account, describeClaudeConfigDir(account, home))
		}
		fmt.Printf("  • Arguments:   %v\n", passthroughArgs)
		return
	}

	// If remote work session on remote host:
	if isRemoteWork {
		err := bridge.Launch(context.Background(), bridge.LaunchOptions{
			Host:       remoteHost,
			TargetDir:  cwd,
			Args:       passthroughArgs,
			ForceLocal: false,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Bridge launch error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Local session execution. Board rule (router.GeminiCodeForbidden): agy is
	// launched only when chosen explicitly (--gemini, the agy wrapper, agy
	// --force); a missing claude binary is an error, never a switch to agy.
	binName := smartLaunchBinary(targetTool)
	binPath, err := exec.LookPath(binName)
	if err != nil {
		if binName != "agy" {
			fmt.Fprintf(os.Stderr, "Error: '%s' not found in PATH (staypoint never falls back to agy; use --gemini to launch it explicitly).\n", binName)
			os.Exit(1)
		}
		binPath, err = exec.LookPath("claude")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: neither 'agy' nor 'claude' found in PATH.\n")
			os.Exit(1)
		}
		binName = "claude"
	}

	exitIfAgyInWorkRepo(binName, cwd) // STA-854: never agy in a work repo

	env := os.Environ()
	if binName == "claude" {
		env = claudeAccountEnv(env, account, home)
		if isTerminal && !headless {
			fmt.Fprintf(os.Stderr, "Claude account: %s %s\n", account, describeClaudeConfigDir(account, home))
		}
	}

	execArgs := append([]string{binName}, passthroughArgs...)
	if err := syscall.Exec(binPath, execArgs, env); err != nil {
		// Fallback to exec.Command if syscall.Exec fails (e.g. on non-Unix)
		subCmd := exec.Command(binPath, passthroughArgs...)
		subCmd.Stdin = os.Stdin
		subCmd.Stdout = os.Stdout
		subCmd.Stderr = os.Stderr
		subCmd.Env = env

		if err := subCmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			os.Exit(1)
		}
	}
}

// smartLaunchBinary is the CLI the smart launch execs for a routed tool. An
// empty or unrecognised tool is Claude: agy only runs when the route (an
// explicit --gemini) named it.
func smartLaunchBinary(tool string) string {
	if tool == "agy" {
		return "agy"
	}
	return "claude"
}

func extractPassthroughArgs(rawArgs []string) []string {
	var forwarded []string
	staypointFlags := map[string]bool{
		"--claude": true, "-C": true,
		"--gemini": true, "-G": true,
		"--force": true, "-f": true,
		"--continue": true, "-c": true,
		"--resume": true, "-r": true,
		"--handoff": true, "-H": true,
		"--status": true, "-s": true,
		"--dry-run": true, "-n": true,
		"--no-ssh": true,
		"--work":   true, "--personal": true,
	}
	for _, arg := range rawArgs {
		if staypointFlags[arg] {
			continue
		}
		forwarded = append(forwarded, arg)
	}
	return forwarded
}

func isHeadlessStream(args []string) bool {
	for i, arg := range args {
		if arg == "--print" || arg == "-p" {
			return true
		}
		if strings.HasPrefix(arg, "--output-format=") {
			val := strings.TrimPrefix(arg, "--output-format=")
			if val == "stream-json" || val == "json" {
				return true
			}
		}
		if arg == "--output-format" && i+1 < len(args) {
			if args[i+1] == "stream-json" || args[i+1] == "json" {
				return true
			}
		}
	}
	return false
}

func init() {
	cfg = config.DefaultConfig()
	rootCmd.Flags().BoolP("claude", "C", false, "Force route to Claude Code")
	rootCmd.Flags().BoolP("gemini", "G", false, "Force route to Antigravity Gemini")
	rootCmd.Flags().BoolP("force", "f", false, "Force direct execution of target tool bypassing router")
	rootCmd.Flags().BoolP("continue", "c", false, "Continue the most recent agent session in this repository")
	rootCmd.Flags().BoolP("resume", "r", false, "Show recent sessions across Claude and Antigravity to resume or hand off")
	rootCmd.Flags().BoolP("handoff", "H", false, "Synthesize zero-effort handoff prompt to clipboard instead of launching")
	rootCmd.Flags().BoolP("status", "s", false, "Display fleet status & quota table")
	rootCmd.Flags().BoolP("dry-run", "n", false, "Preview routed target without executing")
	rootCmd.Flags().Bool("no-ssh", false, "Bypass remote SSH probe")
	rootCmd.Flags().Bool("work", false, "Launch Claude on the work account (CLAUDE_CONFIG_DIR=~/.claude-work)")
	rootCmd.Flags().Bool("personal", false, "Launch Claude on the personal account (shared default profile)")
}
