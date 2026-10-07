package main

import (
	"fmt"
	"os"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize staypoint directories and SQLite storage engine",
	Run: func(cmd *cobra.Command, args []string) {
		shellFlag, _ := cmd.Flags().GetBool("shell")
		powershellFlag, _ := cmd.Flags().GetBool("powershell")

		if powershellFlag {
			fmt.Print(`# Staypoint PowerShell Integration
# Add to your $PROFILE: . (staypoint init --powershell | Out-String | Invoke-Expression)
# Or save to a file: staypoint init --powershell | Out-File -Encoding UTF8 $PROFILE -Append

Set-Alias ai-status    { staypoint status }
Set-Alias agy-status   { staypoint statusline }

function ai-memo    { staypoint report --pdf --type work }
function ai-report  { staypoint report --pdf --type combined }
function ai-personal { staypoint report --pdf --type personal }
function ai-all     { staypoint report --pdf --type all }

function ai {
    $routeOutput = staypoint route $PWD --eval 2>$null
    if ($routeOutput) { Invoke-Expression $routeOutput }
    # Never defaults to agy: Gemini is launched only explicitly (agy / staypoint --gemini).
    $model   = if ($env:STAYPOINT_ROUTE_MODEL)   { $env:STAYPOINT_ROUTE_MODEL }   else { 'claude-opus-5' }
    $cmd     = if ($env:STAYPOINT_ROUTE_COMMAND) { $env:STAYPOINT_ROUTE_COMMAND } else { 'claude' }
    $reason  = if ($env:STAYPOINT_ROUTE_REASON)  { $env:STAYPOINT_ROUTE_REASON }  else { '' }
    Write-Host "[Staypoint] Target: $model ($cmd)" -ForegroundColor Cyan
    if ($reason) { Write-Host "[Context] $reason" -ForegroundColor Yellow }
    staypoint statusline
    if ($env:STAYPOINT_ROUTE_TARGET -eq 'remote-claude') {
        staypoint bridge launch $PWD @args
    } else {
        & claude @args
    }
}

function Invoke-Claude {
    param([switch]$Force)
    if ($Force) { & claude @args } else { staypoint --claude @args }
}
Set-Alias claude Invoke-Claude

function Invoke-Agy {
    param([switch]$Force)
    # Refused in Managed Solution work repos, -Force included (STA-854).
    staypoint agy-guard $PWD
    if ($LASTEXITCODE -ne 0) { return }
    if ($Force) { & agy @args } else { staypoint --gemini @args }
}
Set-Alias agy Invoke-Agy
`)
			return
		}

		if shellFlag {
			fmt.Print(`# Staypoint Shell Integration
# Add to ~/.zshrc or ~/.bashrc: eval "$(staypoint init --shell)"

alias ai-status="staypoint status"
alias ai-memo="staypoint report --pdf --type work"
alias ai-report="staypoint report --pdf --type combined"
alias ai-personal="staypoint report --pdf --type personal"
alias ai-gemini="staypoint report --pdf --type gemini"
alias ai-all="staypoint report --pdf --type all"
alias agy-status="staypoint statusline"
alias ai-shot="staypoint screenshot"
alias ai-snap="staypoint screenshot -i"
alias ai-pull-shot="staypoint screenshot --pull"
alias ai-scp="staypoint scp"

ai() {
  eval "$(staypoint route "$PWD" --eval 2>/dev/null)"
  # Never defaults to agy: Gemini is launched only explicitly (agy / staypoint --gemini).
  local TARGET_MODEL="${STAYPOINT_ROUTE_MODEL:-${MESH_ROUTE_MODEL:-claude-opus-5}}"
  local TARGET_CMD="${STAYPOINT_ROUTE_COMMAND:-${MESH_ROUTE_COMMAND:-claude}}"

  echo -e "\033[1;36m[Staypoint]\033[0m Target: \033[1;32m$TARGET_MODEL\033[0m ($TARGET_CMD)"
  echo -e "\033[0;33m[Context]\033[0m ${STAYPOINT_ROUTE_REASON:-$MESH_ROUTE_REASON}"

  staypoint statusline

  if [[ "${STAYPOINT_ROUTE_TARGET:-$MESH_ROUTE_TARGET}" == "remote-claude" ]]; then
    staypoint bridge launch "$PWD" "$@"
  elif [[ "${STAYPOINT_ROUTE_TARGET:-$MESH_ROUTE_TARGET}" == "local-claude-work" ]]; then
    CLAUDE_CONFIG_DIR="$HOME/.claude-work" command claude "$@"
  else
    env -u CLAUDE_CONFIG_DIR claude "$@"
  fi
}

# claude [--work|--personal] [--force] ...
#   --work      work seat, isolated in ~/.claude-work
#   --personal  personal seat, the shared default profile (~/.claude.json)
#   neither     staypoint picks by repo (work repos -> work); with --force, personal
#   --force     skip staypoint and run Claude Code directly
claude() {
  local force=false account=""
  local clean_args=()
  for arg in "$@"; do
    case "$arg" in
      --force) force=true ;;
      --work) account=work ;;
      --personal) account=personal ;;
      *) clean_args+=("$arg") ;;
    esac
  done

  if [[ "$force" == true ]]; then
    if [[ "$account" == work ]]; then
      CLAUDE_CONFIG_DIR="$HOME/.claude-work" command claude "${clean_args[@]}"
    else
      env -u CLAUDE_CONFIG_DIR claude "${clean_args[@]}"
    fi
  elif [[ -n "$account" ]]; then
    staypoint --claude "--$account" "${clean_args[@]}"
  else
    staypoint --claude "${clean_args[@]}"
  fi
}

# agy refuses in Managed Solution work repos, --force included (STA-854).
agy() {
  staypoint agy-guard "$PWD" || return 1
  local force=false
  local clean_args=()
  for arg in "$@"; do
    if [[ "$arg" == "--force" ]]; then
      force=true
    else
      clean_args+=("$arg")
    fi
  done

  if [[ "$force" == true ]]; then
    command agy "${clean_args[@]}"
  else
    staypoint --gemini "${clean_args[@]}"
  fi
}
`)
			return
		}

		if err := config.EnsureDataDir(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating data dir: %v\n", err)
			os.Exit(1)
		}

		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error initializing staypoint.db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		fmt.Printf("\033[1;32m✔ Staypoint initialized at: %s\033[0m\n", cfg.DataDir)
		fmt.Printf("✔ SQLite database active with WAL mode: %s\n", cfg.DBPath)

		hooksFlag, _ := cmd.Flags().GetBool("hooks")
		if hooksFlag {
			installHooks()
		} else {
			fmt.Println("\nTo install shell aliases & auto-router, add this to your ~/.zshrc or ~/.bashrc:")
			fmt.Println("  \033[1;36meval \"$(staypoint init --shell)\"\033[0m")
			fmt.Println("\nOn PowerShell (Windows), add this to your $PROFILE:")
			fmt.Println("  \033[1;36mstaypoint init --powershell | Out-File -Encoding UTF8 $PROFILE -Append\033[0m")
			fmt.Println("\nOptional: To install cross-agent review and prompt hooks for Antigravity & Claude Code, run:")
			fmt.Println("  \033[1;36mstaypoint init --hooks\033[0m")
		}
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().Bool("shell", false, "Print shell integration hook code for ~/.zshrc or ~/.bashrc")
	initCmd.Flags().Bool("powershell", false, "Print PowerShell profile integration code for $PROFILE")
	initCmd.Flags().Bool("hooks", false, "Install Antigravity and Claude Code lifecycle hooks for bidirectional review and context injection")
}
