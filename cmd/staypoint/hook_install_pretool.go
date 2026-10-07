package main

import (
	"encoding/json"
	"os"
	"strings"
)

// STA-854: `staypoint hook install` registers the PreToolUse tracking gate
// for interactive Claude Code and agy sessions. Daemon runs already get the
// hook through a per-run --settings file (internal/adapter/claude.go).

// claudePreToolMatcher limits the hook to the tools the tracking gate checks.
const claudePreToolMatcher = "Edit|Write|MultiEdit|NotebookEdit|Bash"

func claudePreToolCommand(bin string) string { return bin + " hook pre-tool --tracking-only" }

func agyPreToolHookGroups(bin string) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"matcher": "*",
			"hooks": []map[string]interface{}{
				{"type": "command", "command": bin + " hook pre-tool --format gemini", "timeout": 30},
			},
		},
	}
}

// installClaudePreToolHook adds the tracking-gate PreToolUse entry to a Claude
// Code settings file, leaving every other key untouched. It reports whether
// the file changed. An existing `hook pre-tool` entry is left as is.
func installClaudePreToolHook(settingsPath, bin string) (bool, error) {
	settings := map[string]interface{}{}
	if data, err := os.ReadFile(settingsPath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return false, err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
	}
	groups, _ := hooks["PreToolUse"].([]interface{})
	for _, g := range groups {
		gm, _ := g.(map[string]interface{})
		subs, _ := gm["hooks"].([]interface{})
		for _, sh := range subs {
			shm, _ := sh.(map[string]interface{})
			if c, _ := shm["command"].(string); strings.Contains(c, "hook pre-tool") {
				return false, nil
			}
		}
	}
	groups = append(groups, map[string]interface{}{
		"matcher": claudePreToolMatcher,
		"hooks": []interface{}{
			map[string]interface{}{"type": "command", "command": claudePreToolCommand(bin)},
		},
	})
	hooks["PreToolUse"] = groups
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	// Write through symlinks (~/.claude-work/settings.json -> ~/.claude/settings.json).
	return true, os.WriteFile(settingsPath, out, 0644)
}
