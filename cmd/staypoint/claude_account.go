package main

import (
	"path/filepath"
	"strings"
)

// claudeAccount is the Claude Code login a local `claude` launch uses.
// Personal is the shared default profile (~/.claude.json + the default
// keychain item, CLAUDE_CONFIG_DIR unset). Work is fully isolated in
// ~/.claude-work so a /login in one profile can never flip the other.
type claudeAccount string

const (
	claudeAccountPersonal claudeAccount = "personal"
	claudeAccountWork     claudeAccount = "work"
)

// resolveClaudeAccount picks the account: an explicit --work/--personal wins,
// otherwise work repos get the work seat and everything else personal.
func resolveClaudeAccount(forceWork, forcePersonal, isWorkRepo bool) claudeAccount {
	switch {
	case forceWork:
		return claudeAccountWork
	case forcePersonal:
		return claudeAccountPersonal
	case isWorkRepo:
		return claudeAccountWork
	default:
		return claudeAccountPersonal
	}
}

// claudeConfigDir is the CLAUDE_CONFIG_DIR for account, or "" for the
// shared default profile.
func claudeConfigDir(account claudeAccount, home string) string {
	if account == claudeAccountWork {
		return filepath.Join(home, ".claude-work")
	}
	return ""
}

// claudeAccountEnv returns environ with CLAUDE_CONFIG_DIR set for account.
// Any inherited CLAUDE_CONFIG_DIR is dropped first, so a personal launch
// from a shell that exported the work dir still lands on personal.
func claudeAccountEnv(environ []string, account claudeAccount, home string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		out = append(out, kv)
	}
	if dir := claudeConfigDir(account, home); dir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+dir)
	}
	return out
}

// describeClaudeConfigDir is the human label for where account's login lives.
func describeClaudeConfigDir(account claudeAccount, home string) string {
	if dir := claudeConfigDir(account, home); dir != "" {
		return "(CLAUDE_CONFIG_DIR=" + dir + ")"
	}
	return "(default profile ~/.claude.json)"
}
