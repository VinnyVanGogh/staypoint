package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installHooks installs optional Antigravity and Claude Code lifecycle hooks
// for bidirectional code review and context injection.
func installHooks() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error determining user home directory: %v\n", err)
		return
	}

	staypointBin := filepath.Join(homeDir, ".local", "bin", "staypoint")
	if _, err := os.Stat(staypointBin); err != nil {
		if exe, err := os.Executable(); err == nil {
			staypointBin = exe
		}
	}

	// 1. Antigravity PreInvocation Hook (~/.gemini/config/hooks.json)
	geminiConfigDir := filepath.Join(homeDir, ".gemini", "config")
	_ = os.MkdirAll(geminiConfigDir, 0755)
	geminiHooksPath := filepath.Join(geminiConfigDir, "hooks.json")

	hooksMap := make(map[string]interface{})
	if data, err := os.ReadFile(geminiHooksPath); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &hooksMap)
	}

	hooksMap["staypoint-lifecycle"] = map[string]interface{}{
		"PreInvocation": []map[string]interface{}{
			{
				"type":    "command",
				"command": staypointBin + " hook prompt",
				"timeout": 10,
			},
		},
		// STA-854: tracking gate for agy tool calls.
		"PreToolUse": agyPreToolHookGroups(staypointBin),
	}

	if out, err := json.MarshalIndent(hooksMap, "", "  "); err == nil {
		if err := os.WriteFile(geminiHooksPath, out, 0644); err == nil {
			fmt.Printf("\033[1;32m✔ Antigravity PreInvocation hook configured:\033[0m %s\n", geminiHooksPath)
		}
	}

	// 2. Claude Code UserPromptSubmit Hook (~/.claude/settings.json)
	claudeDir := filepath.Join(homeDir, ".claude")
	_ = os.MkdirAll(claudeDir, 0755)
	claudeSettingsPath := filepath.Join(claudeDir, "settings.json")

	claudeSettings := make(map[string]interface{})
	if data, err := os.ReadFile(claudeSettingsPath); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &claudeSettings)
	}

	hooksObj, ok := claudeSettings["hooks"].(map[string]interface{})
	if !ok || hooksObj == nil {
		hooksObj = make(map[string]interface{})
	}

	var promptHookList []interface{}
	if existing, ok := hooksObj["UserPromptSubmit"].([]interface{}); ok {
		promptHookList = existing
	}

	hookAlreadyPresent := false
	for _, entry := range promptHookList {
		if m, ok := entry.(map[string]interface{}); ok {
			if subHooks, ok := m["hooks"].([]interface{}); ok {
				for _, sh := range subHooks {
					if shMap, ok := sh.(map[string]interface{}); ok {
						cmdStr, _ := shMap["command"].(string)
						if strings.Contains(cmdStr, "hook prompt") {
							hookAlreadyPresent = true
							break
						}
					}
				}
			}
		}
	}

	if !hookAlreadyPresent {
		newEntry := map[string]interface{}{
			"hooks": []map[string]interface{}{
				{
					"type":    "command",
					"command": staypointBin + " hook prompt",
				},
			},
		}
		promptHookList = append(promptHookList, newEntry)
		hooksObj["UserPromptSubmit"] = promptHookList
		claudeSettings["hooks"] = hooksObj

		if out, err := json.MarshalIndent(claudeSettings, "", "  "); err == nil {
			if err := os.WriteFile(claudeSettingsPath, out, 0644); err == nil {
				fmt.Printf("\033[1;32m✔ Claude Code UserPromptSubmit hook configured:\033[0m %s\n", claudeSettingsPath)
			}
		}
	} else {
		fmt.Printf("\033[1;32m✔ Claude Code UserPromptSubmit hook already active:\033[0m %s\n", claudeSettingsPath)
	}

	// 2b. Claude Code PreToolUse tracking gate (STA-854)
	if changed, err := installClaudePreToolHook(claudeSettingsPath, staypointBin); err != nil {
		fmt.Fprintf(os.Stderr, "Error installing Claude Code PreToolUse hook: %v\n", err)
	} else if changed {
		fmt.Printf("\033[1;32m✔ Claude Code PreToolUse tracking gate configured:\033[0m %s\n", claudeSettingsPath)
	} else {
		fmt.Printf("\033[1;32m✔ Claude Code PreToolUse tracking gate already active:\033[0m %s\n", claudeSettingsPath)
	}

	// 3. Global Git Post-Commit Hook (~/.config/git/hooks/post-commit)
	gitHooksDir := filepath.Join(homeDir, ".config", "git", "hooks")
	_ = os.MkdirAll(gitHooksDir, 0755)
	postCommitPath := filepath.Join(gitHooksDir, "post-commit")

	postCommitScript := `#!/bin/bash
# Global Git post-commit hook for Staypoint bidirectional cross-agent code review.
# If commit was made by Antigravity/Gemini: dispatches reviewer-claude.sh in background.
# If commit was made by Claude Code: dispatches reviewer-antigravity.sh in background.
# Never blocks, never delays, exits 0 immediately.

set -uo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
[ -n "$REPO_ROOT" ] && [ -d "$REPO_ROOT" ] || exit 0

# 1. Forward to local repo post-commit hook if one exists
GIT_DIR=$(git rev-parse --git-dir 2>/dev/null)
if [ -n "$GIT_DIR" ] && [ -x "$GIT_DIR/hooks/post-commit" ]; then
  "$GIT_DIR/hooks/post-commit" "$@" || true
fi

CLAUDE_REVIEWER="$HOME/.claude/hooks/reviewer-claude.sh"
ANTIGRAVITY_REVIEWER="$HOME/.claude/hooks/reviewer-antigravity.sh"
REVIEW_HOME="$HOME/.claude/reviews"
mkdir -p "$REVIEW_HOME" 2>/dev/null || true

# 2. Check origin of commit
if [ -n "${ANTIGRAVITY_AGENT:-}" ] || [ -n "${ANTIGRAVITY_CONVERSATION_ID:-}" ]; then
  # Triggered by Google Antigravity / Gemini: send to Claude Code reviewer
  if [ -x "$CLAUDE_REVIEWER" ]; then
    nohup "$CLAUDE_REVIEWER" "$REPO_ROOT" >/dev/null 2>>"$REVIEW_HOME/worker-claude.err" &
    disown 2>/dev/null || true
  fi
elif [ -n "${CLAUDE_SESSION_ID:-}" ] || [ -n "${CLAUDE_PROJECT_DIR:-}" ]; then
  # Triggered by Claude Code: send to Antigravity reviewer
  if [ -x "$ANTIGRAVITY_REVIEWER" ]; then
    nohup "$ANTIGRAVITY_REVIEWER" "$REPO_ROOT" >/dev/null 2>>"$REVIEW_HOME/worker-antigravity.err" &
    disown 2>/dev/null || true
  fi
fi

exit 0
`
	if err := os.WriteFile(postCommitPath, []byte(postCommitScript), 0755); err == nil {
		_ = exec.Command("git", "config", "--global", "core.hooksPath", gitHooksDir).Run()
		fmt.Printf("\033[1;32m✔ Global Git post-commit hook active:\033[0m %s\n", postCommitPath)
	}

	// 4. PowerShell profile hook (Windows; best-effort on non-Windows)
	installPowerShellHook(homeDir, staypointBin)

	installAdapterBridges(homeDir, staypointBin)
	fmt.Println("\n\033[1;32m✔ Bidirectional review setup complete!\033[0m")
	fmt.Println("  • Commits by Antigravity are reviewed in the background by Claude Code.")
	fmt.Println("  • Commits by Claude Code are reviewed in the background by Antigravity.")
	fmt.Println("  • Warnings or defects inject automatically into the active agent on the next turn.")
}

// installPowerShellHook appends a staypoint UserPromptSubmit hook to the current
// user's PowerShell $PROFILE (Documents\PowerShell\Microsoft.PowerShell_profile.ps1).
// On non-Windows this writes the profile to a predictable path so Windows users who
// copy the home directory can benefit without re-running the installer.
func installPowerShellHook(homeDir, staypointBin string) {
	profileDir := filepath.Join(homeDir, "Documents", "PowerShell")
	profilePath := filepath.Join(profileDir, "Microsoft.PowerShell_profile.ps1")

	_ = os.MkdirAll(profileDir, 0755)

	// The hook snippet to inject: idempotent check prevents double-install.
	snippet := `
# --- Staypoint hook (auto-installed by staypoint hook install) ---
if (-not (Get-Variable -Name _StaypointHookLoaded -Scope Global -ErrorAction SilentlyContinue)) {
    Set-Variable -Name _StaypointHookLoaded -Value $true -Scope Global
    . (staypoint init --powershell | Out-String | Invoke-Expression)
}
# --- end Staypoint hook ---
`
	_ = snippet // snippet written below

	existing := ""
	if data, err := os.ReadFile(profilePath); err == nil {
		existing = string(data)
	}
	if strings.Contains(existing, "_StaypointHookLoaded") {
		fmt.Printf("\033[1;32m✔ PowerShell profile hook already active:\033[0m %s\n", profilePath)
		return
	}

	hookBlock := fmt.Sprintf(`
# --- Staypoint hook (auto-installed by staypoint hook install) ---
if (-not (Get-Variable -Name _StaypointHookLoaded -Scope Global -ErrorAction SilentlyContinue)) {
    Set-Variable -Name _StaypointHookLoaded -Value $true -Scope Global
    $staypointBin = "%s"
    if (Test-Path $staypointBin) {
        & $staypointBin init --powershell | Out-String | Invoke-Expression
    } elseif (Get-Command staypoint -ErrorAction SilentlyContinue) {
        staypoint init --powershell | Out-String | Invoke-Expression
    }
}
# --- end Staypoint hook ---
`, staypointBin)

	f, err := os.OpenFile(profilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write PowerShell profile %s: %v\n", profilePath, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprint(f, hookBlock); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not append to PowerShell profile: %v\n", err)
		return
	}
	fmt.Printf("\033[1;32m✔ PowerShell profile hook appended:\033[0m %s\n", profilePath)
}

func installAdapterBridges(homeDir string, staypointBin string) {
	binDir := filepath.Join(homeDir, ".local", "bin")
	os.MkdirAll(binDir, 0755)

	geminiPath := filepath.Join(binDir, "gemini-paperclip-bridge")
	claudePath := filepath.Join(binDir, "claude-paperclip-bridge")

	scriptTemplate := `#!/usr/bin/env bash
# Auto-generated by Staypoint
exec "%s" adapter %s "$@"
`
	_ = os.WriteFile(geminiPath, []byte(fmt.Sprintf(scriptTemplate, staypointBin, "gemini")), 0755)
	_ = os.WriteFile(claudePath, []byte(fmt.Sprintf(scriptTemplate, staypointBin, "claude")), 0755)
}
