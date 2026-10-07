package router

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
)

type RouteTarget string

const (
	TargetRemoteClaude    RouteTarget = "remote-claude"
	TargetLocalClaudeWork RouteTarget = "local-claude-work"
	TargetGeminiNative    RouteTarget = "gemini-native"
	TargetClaude3P        RouteTarget = "claude-3p"
	TargetClaudePersonal  RouteTarget = "claude-personal"
)

// ContinuityQuotaMarginPct is the weekly headroom margin below which repo continuity
// yields to balanced headroom pacing to prevent sticking with an exhausted tool.
const ContinuityQuotaMarginPct = 20.0

type RouteOptions struct {
	CheckSSH              bool
	RemoteHost            string
	PreferredPersonalTool string // "auto", "claude", or "agy"
	LastUsedTool          string // "claude", "agy", "gemini"
	PreferredModel        string // e.g. "opus", "sonnet", "claude-opus-5", "claude-sonnet-4-6"
	PreferredEffort       string // e.g. "high", "medium", "low"
	HighPriority          bool   // task is high priority: UIOLI routing selects the top-tier Claude model
	UIOLI                 UIOLIConfig
	Now                   time.Time // clock override for tests; zero means time.Now()
}

type RouteDecision struct {
	Target         RouteTarget `json:"target"`
	Tool           string      `json:"tool"`    // "claude", "agy", "ssh"
	Model          string      `json:"model"`   // "gemini-3.8-flash-high", "claude-opus-5", "claude-sonnet-4-6"
	Command        string      `json:"command"` // shell invocation command
	Workspace      string      `json:"workspace"`
	IsWorkRepo     bool        `json:"is_work_repo"`
	WorkRepoSource string      `json:"work_repo_source,omitempty"`
	SSHReachable   bool        `json:"ssh_reachable"`
	RemoteHost     string      `json:"remote_host,omitempty"`
	AccountRole    string      `json:"account_role"` // "work", "personal"
	AccountEmail   string      `json:"account_email"`
	Reason         string      `json:"reason"`
	Warnings       []string    `json:"warnings,omitempty"`
	PacerState     *PacerState `json:"pacer_state,omitempty"`
}

type scanReposFile struct {
	Repos []struct {
		Name     string   `json:"name"`
		Path     string   `json:"path"`
		Projects []string `json:"projects"`
	} `json:"repos"`
}

// IsWorkRepo determines whether the given directory is part of an enterprise work repo
// by matching configured paths, directory patterns, git remotes, or ~/.agents/skills/ticket-notes/scan-repos.json.
func IsWorkRepo(cwd string) (bool, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	if cwd == "" || cwd == "." {
		if cur, err := os.Getwd(); err == nil {
			cwd = cur
		}
	}

	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		absCwd = cwd
	}
	cleanCwd := filepath.Clean(absCwd)
	evalCwd, _ := filepath.EvalSymlinks(cleanCwd)
	if evalCwd == "" {
		evalCwd = cleanCwd
	}

	// 1. Check patterns: ~/Documents/dev/work[-_]*, ~/Documents/dev/mansol*
	// The work pattern matches a whole directory name, so the shared
	// ~/Documents/dev/worktrees folder (personal and work worktrees side by
	// side) is classified by its git remote below instead.
	devDir := filepath.Join(home, "Documents", "dev")
	if isWorkDirName(firstComponentUnder(devDir, cleanCwd)) || isWorkDirName(firstComponentUnder(devDir, evalCwd)) {
		return true, "prefix: ~/Documents/dev/work*", nil
	}
	mansolPrefix := filepath.Join(home, "Documents", "dev", "mansol")
	if strings.HasPrefix(cleanCwd, mansolPrefix) || strings.HasPrefix(evalCwd, mansolPrefix) {
		return true, "prefix: ~/Documents/dev/mansol*", nil
	}

	// 2. Check path keywords: mansol, managed-solution, managed_solution
	lowerCwd := strings.ToLower(cleanCwd)
	lowerEval := strings.ToLower(evalCwd)
	for _, kw := range []string{"mansol", "managed-solution", "managed_solution"} {
		if strings.Contains(lowerCwd, kw) || strings.Contains(lowerEval, kw) {
			return true, fmt.Sprintf("keyword: %s in path", kw), nil
		}
	}

	// 3. Check git remote origin URL in repository config (supports standard git and worktrees)
	checkDir := cleanCwd
	for {
		gitPath := filepath.Join(checkDir, ".git")
		var gitCfgPath string
		foundGit := false
		if fi, err := os.Stat(gitPath); err == nil {
			foundGit = true
			if fi.IsDir() {
				gitCfgPath = filepath.Join(gitPath, "config")
			} else {
				if content, err := os.ReadFile(gitPath); err == nil {
					line := strings.TrimSpace(string(content))
					if strings.HasPrefix(line, "gitdir:") {
						gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
						if !filepath.IsAbs(gitdir) {
							gitdir = filepath.Join(checkDir, gitdir)
						}
						// Check gitdir commondir pointer first, then local worktree config
						commondirFile := filepath.Join(gitdir, "commondir")
						if cdata, err := os.ReadFile(commondirFile); err == nil {
							cd := strings.TrimSpace(string(cdata))
							if !filepath.IsAbs(cd) {
								cd = filepath.Join(gitdir, cd)
							}
							gitCfgPath = filepath.Join(cd, "config")
						} else {
							candidate := filepath.Join(gitdir, "config")
							if _, err := os.Stat(candidate); err == nil {
								gitCfgPath = candidate
							}
						}
					}
				}
			}
		}

		if gitCfgPath != "" {
			if data, err := os.ReadFile(gitCfgPath); err == nil {
				lines := strings.Split(string(data), "\n")
				inRemote := false
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "[remote ") {
						inRemote = true
						continue
					} else if strings.HasPrefix(trimmed, "[") {
						inRemote = false
						continue
					}
					if inRemote && (strings.HasPrefix(trimmed, "url =") || strings.HasPrefix(trimmed, "url=")) {
						urlLower := strings.ToLower(trimmed)
						if strings.Contains(urlLower, "managedsolution") ||
							strings.Contains(urlLower, "managed-solution") ||
							strings.Contains(urlLower, "mansol") {
							return true, "git remote: Managed Solution", nil
						}
					}
				}
			}
		}

		if foundGit {
			break
		}

		parent := filepath.Dir(checkDir)
		if parent == checkDir || parent == "" || parent == "/" {
			break
		}
		checkDir = parent
	}

	// 4. Check scan-repos.json: ~/.agents/skills/ticket-notes/scan-repos.json
	scanPath := filepath.Join(home, ".agents", "skills", "ticket-notes", "scan-repos.json")
	if data, err := os.ReadFile(scanPath); err == nil {
		var srf scanReposFile
		if json.Unmarshal(data, &srf) == nil {
			for _, r := range srf.Repos {
				if r.Path == "" {
					continue
				}
				cleanRepoPath := filepath.Clean(r.Path)
				evalRepoPath, _ := filepath.EvalSymlinks(cleanRepoPath)
				if evalRepoPath == "" {
					evalRepoPath = cleanRepoPath
				}

				if cleanCwd == cleanRepoPath || evalCwd == evalRepoPath ||
					strings.HasPrefix(cleanCwd, cleanRepoPath+string(filepath.Separator)) ||
					strings.HasPrefix(evalCwd, evalRepoPath+string(filepath.Separator)) {
					return true, fmt.Sprintf("scan-repos: %s", r.Name), nil
				}
			}
		}
	}

	return false, "", nil
}

// CheckSSHConnectivity tests whether the remote host is reachable via SSH.
// Uses a fast cached result (/tmp/staypoint-ssh-<host>.cache) if within 20s.
func CheckSSHConnectivity(ctx context.Context, host string) bool {
	if host == "" {
		host = "company-mbp"
	}

	cachePath := filepath.Join(os.TempDir(), fmt.Sprintf("staypoint-ssh-%s.cache", host))
	if info, err := os.Stat(cachePath); err == nil {
		if time.Since(info.ModTime()) < 20*time.Second {
			content, _ := os.ReadFile(cachePath)
			return strings.TrimSpace(string(content)) == "ok"
		}
	}

	// Run quick SSH probe with 800ms timeout
	probeCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "ssh",
		"-o", "ConnectTimeout=1",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		host, "true",
	)
	err := cmd.Run()
	reachable := (err == nil)

	// Write cache
	status := "fail"
	if reachable {
		status = "ok"
	}
	_ = os.WriteFile(cachePath, []byte(status), 0644)

	return reachable
}

// FallbackPairingMatrix maps Claude models/tiers to their respective Gemini fallback counterparts:
// - Opus tier automatically routes to Gemini 3.1 Pro (effort: high).
// - Sonnet tier automatically routes to Gemini 3.8 Flash (High or Medium based on role effort).
func FallbackPairingMatrix(claudeModel, effort string) (geminiModel, geminiEffort, agyCommand string) {
	claudeLower := strings.ToLower(claudeModel)
	effLower := strings.ToLower(effort)

	if strings.Contains(claudeLower, "opus") {
		geminiEffort = "high"
		if effLower == "low" || effLower == "high" {
			geminiEffort = effLower
		}
		return "gemini-3.1-pro", geminiEffort, fmt.Sprintf("agy --model gemini-3.1-pro --effort %s", geminiEffort)
	}

	// Sonnet tier (or default) routes to Gemini 3.8 Flash
	geminiEffort = "medium"
	if effLower == "high" || effLower == "max" {
		geminiEffort = "high"
	} else if effLower == "low" || effLower == "medium" {
		geminiEffort = effLower
	}
	return "gemini-3.8-flash", geminiEffort, fmt.Sprintf("agy --model gemini-3.8-flash --effort %s", geminiEffort)
}

// Route executes the dynamic waterfall routing engine:
//  1. Check if cwd is an enterprise work repo.
//  2. If work repo: check if Claude Work is locked (falls back to Gemini 3.1 Pro per STA-12);
//     otherwise check SSH connectivity to remote node -> route to remote Claude, or fallback to local Claude work seat.
//  3. If personal repo: balanced peer pacing between Claude Code and Antigravity,
//     with dynamic quota-aware fallback to Gemini 3.1 Pro (Opus) / Gemini 3.8 Flash (Sonnet) when Claude is locked out.
func Route(ctx context.Context, cwd string, pacerState *PacerState, opts RouteOptions) (*RouteDecision, error) {
	if cwd == "" || cwd == "." {
		if cur, err := os.Getwd(); err == nil {
			cwd = cur
		}
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		absCwd = cwd
	}

	if opts.RemoteHost == "" {
		opts.RemoteHost = "company-mbp"
	}

	if pacerState == nil {
		var err error
		pacerState, err = LoadPacerState()
		if err != nil {
			return nil, fmt.Errorf("failed to load pacer state: %w", err)
		}
	}

	decision := &RouteDecision{
		Workspace:  absCwd,
		RemoteHost: opts.RemoteHost,
		PacerState: pacerState,
		Warnings:   make([]string, 0),
	}

	// 1. Work Repo Check
	isWork, workSrc, _ := IsWorkRepo(absCwd)
	decision.IsWorkRepo = isWork
	decision.WorkRepoSource = workSrc

	if isWork {
		decision.AccountRole = "work"
		poolWork := pacerState.Pools[PoolWorkClaude]
		if poolWork != nil && poolWork.IsLocked {
			// Dynamic Fallback Pairing Matrix (STA-12): Opus tier routes to Gemini 3.1 Pro
			decision.Target = TargetGeminiNative
			decision.Tool = "agy"
			decision.Model = "gemini-3.1-pro"
			decision.Command = "agy --model gemini-3.1-pro --effort high"
			decision.Reason = fmt.Sprintf("Enterprise work repo (%s); Claude Work Opus locked until %s, dynamically falling back to Gemini 3.1 Pro (STA-12)",
				workSrc, poolWork.LockoutUntil.Format("03:04pm"))
			decision.Warnings = append(decision.Warnings, fmt.Sprintf("Claude Work locked out until %s; routed to Gemini 3.1 Pro", poolWork.LockoutUntil.Format("03:04pm")))
			return decision, nil
		}

		sshOk := false
		if opts.CheckSSH {
			sshOk = CheckSSHConnectivity(ctx, opts.RemoteHost)
		}
		decision.SSHReachable = sshOk

		if sshOk {
			remoteCwd := bridge.ToRemotePath(absCwd)
			decision.Target = TargetRemoteClaude
			decision.Tool = "ssh"
			decision.Model = "claude-opus-5"
			decision.Command = fmt.Sprintf("ssh -t %s \"export PATH=\\\"$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:\\$PATH\\\"; cd %s && claude\"", opts.RemoteHost, bridge.ShellPathForDir(remoteCwd))
			decision.Reason = fmt.Sprintf("Enterprise work repo (%s); remote node %s reachable via SSH (Highest Priority)", workSrc, opts.RemoteHost)
			return decision, nil
		}

		// Work repo but remote not reachable
		decision.Target = TargetLocalClaudeWork
		decision.Tool = "claude"
		decision.Model = "claude-opus-5"
		decision.Command = "CLAUDE_CONFIG_DIR=~/.claude-work claude"
		decision.Reason = fmt.Sprintf("Enterprise work repo (%s); remote node %s unreachable via SSH, routing to local Claude Code with Managed Solution work seat", workSrc, opts.RemoteHost)
		if opts.CheckSSH {
			decision.Warnings = append(decision.Warnings, fmt.Sprintf("Remote node %s unreachable via SSH; falling back to local Claude", opts.RemoteHost))
		}
		return decision, nil
	}

	// 2. Personal Repo: Balanced Peer Pacing between Claude Code and Antigravity
	decision.AccountRole = "personal"

	poolGemini := pacerState.Pools[PoolGeminiNative]
	pool3P := pacerState.Pools[Pool3PClaude]
	poolPersonal := pacerState.Pools[PoolPersonalClaude]

	geminiOk := poolGemini != nil && !poolGemini.IsLocked && poolGemini.Weekly.RemainingPct > 0.0 && poolGemini.FiveHour.RemainingPct > 0.0
	claudeOk := poolPersonal != nil && !poolPersonal.IsLocked && poolPersonal.Weekly.RemainingPct > 0.0 && poolPersonal.FiveHour.RemainingPct > 0.0

	routeToGemini := func(reason string) (*RouteDecision, error) {
		decision.Target = TargetGeminiNative
		decision.Tool = "agy"
		targetModel, _, cmd := FallbackPairingMatrix(opts.PreferredModel, opts.PreferredEffort)
		decision.Model = targetModel
		decision.Command = cmd
		decision.Reason = reason
		return decision, nil
	}

	routeToClaude := func(reason string) (*RouteDecision, error) {
		decision.Target = TargetClaudePersonal
		decision.Tool = "claude"
		if opts.PreferredModel != "" {
			decision.Model = opts.PreferredModel
		} else {
			decision.Model = "claude-opus-5"
		}
		decision.Command = "claude"
		decision.Reason = reason
		return decision, nil
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Priority 0 (use-it-or-lose-it): near the weekly reset with unspent personal
	// Claude quota, spend it rather than let it expire. Beats balance/continuity,
	// but not an explicit user preference for Antigravity.
	if claudeOk && opts.PreferredPersonalTool != "agy" && opts.PreferredPersonalTool != "gemini" {
		if u := poolPersonal.UIOLIPressure(now, opts.UIOLI); u.Active {
			if opts.PreferredModel == "" && opts.HighPriority {
				opts.PreferredModel = UIOLIHighPriorityModel
			}
			return routeToClaude("Personal repo: " + u.describe() + " - preferring Claude Code over Gemini")
		}
	}

	// Priority 1: User explicitly configured a preferred personal tool
	if opts.PreferredPersonalTool == "claude" {
		if claudeOk {
			return routeToClaude(fmt.Sprintf("Personal repo: Claude Code selected by user preference (%d turns runway | week: %.1f%% left)",
				poolPersonal.TurnsRunway, poolPersonal.Weekly.RemainingPct))
		}
		if geminiOk {
			decision.Warnings = append(decision.Warnings, "Preferred tool Claude Code is locked out; dynamically falling back to Gemini Native per STA-12 pairing.")
			return routeToGemini(fmt.Sprintf("Personal repo: Preferred tool Claude Code locked out, dynamically falling back to Gemini Native (%d turns runway | week: %.1f%% left)",
				poolGemini.TurnsRunway, poolGemini.Weekly.RemainingPct))
		}
	}
	if (opts.PreferredPersonalTool == "agy" || opts.PreferredPersonalTool == "gemini") && geminiOk {
		return routeToGemini(fmt.Sprintf("Personal repo: Antigravity selected by user preference (%d turns runway | week: %.1f%% left)",
			poolGemini.TurnsRunway, poolGemini.Weekly.RemainingPct))
	}

	// Priority 2: Both tools are available, balance between them
	if claudeOk && geminiOk {
		// Repo continuity: if this repository was recently used with one tool, stick to it
		// provided that tool has not fallen behind by > ContinuityQuotaMarginPct weekly quota margin
		if opts.LastUsedTool == "claude" && poolPersonal.Weekly.RemainingPct >= poolGemini.Weekly.RemainingPct-ContinuityQuotaMarginPct {
			return routeToClaude(fmt.Sprintf("Personal repo: continuing with Claude Code (last tool used in this repo: %d turns runway | week: %.1f%% left)",
				poolPersonal.TurnsRunway, poolPersonal.Weekly.RemainingPct))
		}
		if (opts.LastUsedTool == "agy" || opts.LastUsedTool == "gemini") && poolGemini.Weekly.RemainingPct >= poolPersonal.Weekly.RemainingPct-ContinuityQuotaMarginPct {
			return routeToGemini(fmt.Sprintf("Personal repo: continuing with Antigravity (last tool used in this repo: %d turns runway | week: %.1f%% left)",
				poolGemini.TurnsRunway, poolGemini.Weekly.RemainingPct))
		}

		// Weekly headroom balance: route to the tool with more weekly quota remaining
		diff := poolPersonal.Weekly.RemainingPct - poolGemini.Weekly.RemainingPct
		if diff >= 5.0 {
			return routeToClaude(fmt.Sprintf("Personal repo: balanced pacing favors Claude Code (%.1f%% week left vs Gemini %.1f%%)",
				poolPersonal.Weekly.RemainingPct, poolGemini.Weekly.RemainingPct))
		}
		if diff <= -5.0 {
			return routeToGemini(fmt.Sprintf("Personal repo: balanced pacing favors Antigravity (%.1f%% week left vs Claude %.1f%%)",
				poolGemini.Weekly.RemainingPct, poolPersonal.Weekly.RemainingPct))
		}

		// Within 5%: alternate by ISO week number
		_, weekNum := time.Now().ISOWeek()
		if weekNum%2 == 0 {
			return routeToClaude(fmt.Sprintf("Personal repo: balanced weekly pacing (week %d) favors Claude Code (%.1f%% left)",
				weekNum, poolPersonal.Weekly.RemainingPct))
		}
		return routeToGemini(fmt.Sprintf("Personal repo: balanced weekly pacing (week %d) favors Antigravity (%.1f%% left)",
			weekNum, poolGemini.Weekly.RemainingPct))
	}

	// Priority 3: Only one tool is available
	if claudeOk && !geminiOk {
		geminiReason := "Gemini quota exhausted"
		if poolGemini != nil && poolGemini.LockoutReason != "" {
			geminiReason = poolGemini.LockoutReason
		}
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("Gemini Native is locked: %s. Routing to standalone Claude Code.", geminiReason))
		return routeToClaude(fmt.Sprintf("Personal repo: Gemini Native locked (%s), routing to Claude Code (%d turns runway | week: %.1f%% left)",
			geminiReason, poolPersonal.TurnsRunway, poolPersonal.Weekly.RemainingPct))
	}

	if geminiOk && !claudeOk {
		claudeReason := "Claude quota exhausted or locked out"
		if poolPersonal != nil && poolPersonal.LockoutReason != "" {
			claudeReason = poolPersonal.LockoutReason
		}
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("Claude Code is locked (%s). Dynamically routed to Gemini Native per STA-12 fallback pairing.", claudeReason))
		return routeToGemini(fmt.Sprintf("Personal repo: Claude Code locked (%s), routing to Gemini Native (%d turns runway | week: %.1f%% left)",
			claudeReason, poolGemini.TurnsRunway, poolGemini.Weekly.RemainingPct))
	}

	// Priority 4: Both native tools exhausted, check 3P Claude in Antigravity
	if pool3P != nil && !pool3P.IsLocked && pool3P.Weekly.RemainingPct > 0.0 && pool3P.FiveHour.RemainingPct > 0.0 {
		decision.Target = TargetClaude3P
		decision.Tool = "agy"
		decision.Model = "claude-sonnet-4-6"
		decision.Command = "agy"
		decision.Reason = fmt.Sprintf("Gemini Native & Standalone Claude locked, falling back to 3P Claude in Antigravity (%d turns runway)",
			pool3P.TurnsRunway)
		decision.Warnings = append(decision.Warnings, "Gemini Native and Standalone Claude are locked. Switch model in agy using '/model claude-sonnet-4-6'.")
		return decision, nil
	}

	// Priority 5: All locked
	decision.Target = TargetGeminiNative
	decision.Tool = "agy"
	decision.Model = "gemini-3.8-flash-high"
	decision.Command = "agy"
	decision.Reason = "All personal quota pools are currently locked or exhausted; awaiting window reset"

	if poolGemini != nil && poolGemini.IsLocked {
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("Gemini Native locked until %s", poolGemini.LockoutUntil.Format("03:04pm")))
	}
	if poolPersonal != nil && poolPersonal.IsLocked {
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("Personal Claude locked until %s", poolPersonal.LockoutUntil.Format("03:04pm")))
	}
	if pool3P != nil && pool3P.IsLocked {
		decision.Warnings = append(decision.Warnings, fmt.Sprintf("3P Claude locked until %s", pool3P.LockoutUntil.Format("03:04pm")))
	}

	return decision, nil
}

// firstComponentUnder returns the first path element of p below root, or ""
// when p is not inside root.
func firstComponentUnder(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return strings.SplitN(rel, string(filepath.Separator), 2)[0]
}

// isWorkDirName reports whether a ~/Documents/dev entry is a work folder by
// name: "work" itself or "work-…"/"work_…". "worktrees" is not.
func isWorkDirName(name string) bool {
	n := strings.ToLower(name)
	return n == "work" || strings.HasPrefix(n, "work-") || strings.HasPrefix(n, "work_")
}
