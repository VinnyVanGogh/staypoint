package context

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// HandoffOptions holds configuration for generating a handoff prompt.
type HandoffOptions struct {
	TargetModel       string // "gemini" or "claude"
	ImmediateNextStep string
	Directory         string
	DB                *sql.DB
	SessionID         string
	Trigger           string // "manual", "auto_daemon", "crash", "exit", "breaker", "quota_warning"
	DataDir           string
	MaxKeepPerRepo    int
}

// HandoffRecord represents the structured metadata saved to handoff.json.
type HandoffRecord struct {
	Timestamp         string   `json:"timestamp"`
	SourceModel       string   `json:"source_model"`
	TargetModel       string   `json:"target_model"`
	RepoPath          string   `json:"repo_path"`
	RepoName          string   `json:"repo_name"`
	GitBranch         string   `json:"git_branch"`
	ActiveTaskID      string   `json:"active_task_id,omitempty"`
	ActiveTaskName    string   `json:"active_task_name,omitempty"`
	ImmediateNextStep string   `json:"immediate_next_step"`
	ModifiedFiles     []string `json:"modified_files"`
	DiffStat          string   `json:"diff_stat"`
	RecentCommits     []string `json:"recent_commits"`
	HandoffPrompt     string   `json:"handoff_prompt"`
}

// GitContext holds captured git state for the working directory.
type GitContext struct {
	RepoRoot      string
	RepoName      string
	Branch        string
	ModifiedFiles []string
	DiffStat      string
	RecentCommits []string
}

// GatherGitContext extracts branch, status, diff stat, and recent commits.
func GatherGitContext(dir string) GitContext {
	gc := GitContext{
		Branch:        "main",
		RepoRoot:      dir,
		RepoName:      filepath.Base(dir),
		ModifiedFiles: []string{},
		DiffStat:      "Clean working tree (no uncommitted diff)",
		RecentCommits: []string{},
	}

	// Root path
	if rootOut, err := execGit(dir, "rev-parse", "--show-toplevel"); err == nil && len(rootOut) > 0 {
		gc.RepoRoot = strings.TrimSpace(rootOut)
		gc.RepoName = filepath.Base(gc.RepoRoot)
	}

	// Active branch
	if branchOut, err := execGit(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && len(branchOut) > 0 {
		gc.Branch = strings.TrimSpace(branchOut)
	}

	// Modified files: git status --short
	if statusOut, err := execGit(dir, "status", "--short"); err == nil && len(strings.TrimSpace(statusOut)) > 0 {
		lines := strings.Split(strings.TrimSpace(statusOut), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				gc.ModifiedFiles = append(gc.ModifiedFiles, trimmed)
			}
		}
	}

	// Diff stat: git diff --stat + git diff --cached --stat
	var diffParts []string
	if unstagedDiff, err := execGit(dir, "diff", "--stat"); err == nil && len(strings.TrimSpace(unstagedDiff)) > 0 {
		diffParts = append(diffParts, "Unstaged changes:\n"+strings.TrimSpace(unstagedDiff))
	}
	if stagedDiff, err := execGit(dir, "diff", "--cached", "--stat"); err == nil && len(strings.TrimSpace(stagedDiff)) > 0 {
		diffParts = append(diffParts, "Staged changes:\n"+strings.TrimSpace(stagedDiff))
	}
	if len(diffParts) > 0 {
		gc.DiffStat = strings.Join(diffParts, "\n\n")
	}

	// Recent commits: git log -n 3 --oneline
	if logOut, err := execGit(dir, "log", "-n", "3", "--oneline"); err == nil && len(strings.TrimSpace(logOut)) > 0 {
		lines := strings.Split(strings.TrimSpace(logOut), "\n")
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				gc.RecentCommits = append(gc.RecentCommits, trimmed)
			}
		}
	}

	return gc
}

func execGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := gitexec.Command(ctx, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// GenerateHandoff builds a zero-clarification handoff prompt, saves handoff.json and /tmp/ai-handoff.md,
// and copies the prompt to the clipboard via pbcopy.
func GenerateHandoff(opts HandoffOptions) (*HandoffRecord, error) {
	if opts.Directory == "" {
		var err error
		opts.Directory, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	absDir, err := filepath.Abs(opts.Directory)
	if err == nil {
		opts.Directory = absDir
	}

	// Normalize target and source models
	target := strings.ToLower(strings.TrimSpace(opts.TargetModel))
	if target == "" || (target != "gemini" && target != "claude") {
		target = "gemini"
	}
	source := "Claude"
	targetDisplay := "Gemini"
	if target == "claude" {
		source = "Gemini"
		targetDisplay = "Claude"
	}

	// Gather Git Context
	gitCtx := GatherGitContext(opts.Directory)

	// Fetch active task from SQLite if available
	var activeTask *Task
	if opts.DB != nil {
		activeTask, _ = GetActiveTaskForRepo(opts.DB, opts.Directory)
	}

	taskID := ""
	taskName := "Continuous Engineering / Active Task"
	if activeTask != nil {
		taskID = activeTask.ID
		taskName = activeTask.Name
	}

	nextStep := strings.TrimSpace(opts.ImmediateNextStep)
	if nextStep == "" {
		if activeTask != nil {
			nextStep = fmt.Sprintf("Continue implementation and verification of [%s]: %s", activeTask.ID, activeTask.Name)
		} else {
			nextStep = "Inspect modified files, run test suite, and proceed with pending implementation without clarification."
		}
	}

	// Construct zero-clarification prompt
	now := time.Now().Format("2006-01-02 15:04:05 MST")
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# ⚡ ZERO-EFFORT CONTEXT HANDOFF :: %s (%s)\n\n", gitCtx.RepoName, gitCtx.Branch))
	sb.WriteString(fmt.Sprintf("- **Switch Route:** `%s` ➔ **`%s`**\n", source, targetDisplay))
	sb.WriteString(fmt.Sprintf("- **Timestamp:** %s\n", now))
	sb.WriteString(fmt.Sprintf("- **Repository Path:** `%s`\n", gitCtx.RepoRoot))
	sb.WriteString(fmt.Sprintf("- **Active Branch:** `%s`\n", gitCtx.Branch))
	if taskID != "" {
		sb.WriteString(fmt.Sprintf("- **Active Task ID:** `%s`\n", taskID))
	}
	sb.WriteString("\n---\n\n")

	sb.WriteString("## 🎯 Active Task\n")
	sb.WriteString(fmt.Sprintf("> %s\n\n", taskName))

	sb.WriteString("## 🚀 Immediate Next Step (Zero-Clarification Mode)\n")
	sb.WriteString(fmt.Sprintf("```text\n%s\n```\n\n", nextStep))

	sb.WriteString("## 📂 Working Tree Status (`git status --short`)\n")
	if len(gitCtx.ModifiedFiles) > 0 {
		sb.WriteString("```bash\n")
		for _, f := range gitCtx.ModifiedFiles {
			sb.WriteString(fmt.Sprintf("%s\n", f))
		}
		sb.WriteString("```\n\n")
	} else {
		sb.WriteString("*(clean working tree - no uncommitted changes)*\n\n")
	}

	sb.WriteString("## 📊 Diff Summary (`git diff --stat`)\n")
	sb.WriteString("```text\n")
	sb.WriteString(gitCtx.DiffStat)
	sb.WriteString("\n```\n\n")

	if len(gitCtx.RecentCommits) > 0 {
		sb.WriteString("## 📜 Recent Commits\n")
		sb.WriteString("```text\n")
		for _, c := range gitCtx.RecentCommits {
			sb.WriteString(fmt.Sprintf("%s\n", c))
		}
		sb.WriteString("```\n\n")
	}

	sb.WriteString("## 📋 Execution Protocol\n")
	sb.WriteString("1. **Do NOT ask for clarification or confirmation.** Resume immediately from the Immediate Next Step.\n")
	sb.WriteString("2. Strictly adhere to existing architectural standards, test conventions, and coding patterns in this repository.\n")
	sb.WriteString("3. Run the local build or test suite after applying changes to verify integrity before reporting completion.\n")

	promptText := sb.String()

	record := &HandoffRecord{
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
		SourceModel:       source,
		TargetModel:       targetDisplay,
		RepoPath:          gitCtx.RepoRoot,
		RepoName:          gitCtx.RepoName,
		GitBranch:         gitCtx.Branch,
		ActiveTaskID:      taskID,
		ActiveTaskName:    taskName,
		ImmediateNextStep: nextStep,
		ModifiedFiles:     gitCtx.ModifiedFiles,
		DiffStat:          gitCtx.DiffStat,
		RecentCommits:     gitCtx.RecentCommits,
		HandoffPrompt:     promptText,
	}

	// 1. Resolve Session ID and Agent Type
	sessionID := opts.SessionID
	agentType := strings.ToLower(source)
	if sessionID == "" {
		if opts.DB != nil {
			if latest, err := GetLatestSession(opts.Directory, opts.DB); err == nil && latest != nil {
				sessionID = latest.ID
				agentType = latest.AgentType
			}
		}
	}
	if sessionID == "" {
		sessionID = fmt.Sprintf("session-%d", time.Now().Unix())
	}

	trigger := opts.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	maxKeep := opts.MaxKeepPerRepo
	if maxKeep <= 0 {
		maxKeep = 3
	}

	// 2. Serialize to ~/.staypoint/handoff.json and session manifest
	dataDir := opts.DataDir
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		newDir := filepath.Join(home, ".staypoint")
		oldDir := filepath.Join(home, ".agent-mesh")
		if _, err := os.Stat(newDir); err == nil {
			dataDir = newDir
		} else if _, err := os.Stat(oldDir); err == nil {
			dataDir = oldDir
		} else {
			dataDir = newDir
		}
	}
	_ = os.MkdirAll(dataDir, 0755)

	jsonPath := filepath.Join(dataDir, "handoff.json")
	if jsonData, err := json.MarshalIndent(record, "", "  "); err == nil {
		_ = os.WriteFile(jsonPath, jsonData, 0644)
	}

	manifest := HandoffManifest{
		SessionID:       sessionID,
		Title:           taskName,
		Goal:            nextStep,
		RepoPath:        gitCtx.RepoRoot,
		RepoName:        gitCtx.RepoName,
		GitBranch:       gitCtx.Branch,
		AgentType:       agentType,
		CreatedAt:       time.Now().UTC(),
		TotalUserTurns:  1,
		DirectivesCount: 1,
		ActiveTaskID:    taskID,
		ActiveTaskName:  taskName,
		ModifiedFiles:   gitCtx.ModifiedFiles,
		Trigger:         trigger,
	}
	_, _ = SaveHandoffWithManifest(dataDir, manifest, promptText, maxKeep)

	// 3. Write /tmp/ai-handoff.md
	handoffMdPath := "/tmp/ai-handoff.md"
	if err := os.WriteFile(handoffMdPath, []byte(promptText), 0644); err != nil {
		return nil, fmt.Errorf("failed to write %s: %w", handoffMdPath, err)
	}

	// 4. Copy to system clipboard using pbcopy on macOS
	_ = CopyToClipboard(promptText)

	return record, nil
}

// CopyToClipboard writes the string to macOS pbcopy (or xclip on Linux if available).
func CopyToClipboard(text string) error {
	if _, err := exec.LookPath("pbcopy"); err == nil {
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command("xclip", "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard utility available")
}
