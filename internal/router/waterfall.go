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
	LastUsedTool          string // "claude", "agy", "gemini"; ignored: repo continuity never picks agy (GeminiCodeForbidden)
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
	// Waiting is set when the decision's seat is locked and there is no
	// permitted fallback: the caller should wait for it (work repos never fall
	// back to Gemini, GeminiCodeForbidden).
	Waiting    bool        `json:"waiting,omitempty"`
	PacerState *PacerState `json:"pacer_state,omitempty"`
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
//  2. If work repo: always Claude, never agy (GeminiCodeForbidden, STA-856). A
//     locked work seat falls back to the personal Claude seat; with both locked
//     the decision sets Waiting instead of falling back to Gemini. Otherwise check SSH connectivity to the remote node
//     -> route to remote Claude, or fall back to the local Claude work seat.
//  3. If personal repo: Claude Code on the personal seat; with it locked the
//     decision sets Waiting. The router never picks agy (GeminiCodeForbidden):
//     Gemini is launched only explicitly (--gemini, agy, agy --force).
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
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		poolWork := pacerState.Pools[PoolWorkClaude]
		if locked, why := PoolLockReason(poolWork, now); locked && GeminiCodeForbidden(true) {
			// Board rule (STA-856): Gemini never writes code in a work repo.
			// Fall back to the personal Claude seat; if it is locked too, wait.
			until := ""
			if poolWork != nil && !poolWork.LockoutUntil.IsZero() {
				until = " until " + poolWork.LockoutUntil.Format("03:04pm")
			}
			decision.Tool = "claude"
			decision.Model = "claude-opus-5"
			if pLocked, _ := PoolLockReason(pacerState.Pools[PoolPersonalClaude], now); !pLocked {
				decision.Target = TargetClaudePersonal
				decision.AccountRole = "personal"
				decision.Command = "claude"
				decision.Reason = fmt.Sprintf("Enterprise work repo (%s); fell back to personal Claude: work seat locked (%s%s). %s",
					workSrc, why, until, GeminiCodeRule)
				decision.Warnings = append(decision.Warnings, fmt.Sprintf("Work Claude seat locked (%s%s); using the personal Claude seat. Gemini is not used in work repos.", why, until))
				return decision, nil
			}
			_, pWhy := PoolLockReason(pacerState.Pools[PoolPersonalClaude], now)
			decision.Target = TargetLocalClaudeWork
			decision.Command = "CLAUDE_CONFIG_DIR=~/.claude-work claude"
			decision.Waiting = true
			decision.Reason = fmt.Sprintf("Enterprise work repo (%s); waiting for a Claude seat: work seat locked (%s%s), personal seat locked (%s). %s, so there is no Gemini fallback",
				workSrc, why, until, pWhy, GeminiCodeRule)
			decision.Warnings = append(decision.Warnings, "Both Claude seats are locked; waiting. Gemini is not used in work repos.")
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

	// 2. Personal repo: Claude Code on the personal seat. Board rule
	// (GeminiCodeForbidden): an interactive session may write code, so the
	// router never picks agy (no pacing balance, repo continuity, remembered
	// preference or quota fallback). Gemini runs only when launched
	// explicitly: --gemini, the agy wrapper, agy --force, or resuming an agy
	// session. With the personal seat locked the decision waits.
	decision.AccountRole = "personal"
	poolPersonal := pacerState.Pools[PoolPersonalClaude]

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
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

	if opts.PreferredPersonalTool == "agy" || opts.PreferredPersonalTool == "gemini" {
		decision.Warnings = append(decision.Warnings, "preferred_personal_tool = "+opts.PreferredPersonalTool+" is ignored: Gemini never writes code, so it is only launched explicitly (staypoint --gemini or agy).")
	}

	if locked, why := PoolLockReason(poolPersonal, now); locked {
		until := ""
		if poolPersonal != nil && !poolPersonal.LockoutUntil.IsZero() {
			until = " until " + poolPersonal.LockoutUntil.Format("03:04pm")
		}
		d, _ := routeToClaude(fmt.Sprintf("Personal repo: waiting for the personal Claude seat (%s%s). %s, so there is no automatic Gemini fallback; launch agy explicitly for non-code work",
			why, until, GeminiCodeRule))
		d.Waiting = true
		d.Warnings = append(d.Warnings, fmt.Sprintf("Personal Claude seat locked (%s%s); waiting. Gemini is not picked automatically.", why, until))
		return d, nil
	}

	// Use-it-or-lose-it: near the weekly reset with unspent personal Claude
	// quota, high-priority work gets the top-tier model.
	if poolPersonal != nil {
		if u := poolPersonal.UIOLIPressure(now, opts.UIOLI); u.Active {
			if opts.PreferredModel == "" && opts.HighPriority {
				opts.PreferredModel = UIOLIHighPriorityModel
			}
			return routeToClaude("Personal repo: " + u.describe() + " - Claude Code")
		}
		return routeToClaude(fmt.Sprintf("Personal repo: Claude Code on the personal seat (%d turns runway | week: %s left)",
			poolPersonal.TurnsRunway, poolPersonal.Weekly.FormatPct(true, 1)))
	}
	return routeToClaude("Personal repo: Claude Code on the personal seat")
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
