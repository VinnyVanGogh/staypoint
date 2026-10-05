package bridge

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// RepoInfo encapsulates git metadata for a local directory.
type RepoInfo struct {
	IsRepo    bool
	RepoRoot  string
	RemoteURL string
	Branch    string
}

// GetRepoInfo discovers git metadata for the given directory.
func GetRepoInfo(dir string) RepoInfo {
	if dir == "" {
		return RepoInfo{}
	}

	// Check if inside git work tree
	checkCmd := gitexec.Command(context.Background(), "-C", dir, "rev-parse", "--is-inside-work-tree")
	if out, err := checkCmd.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return RepoInfo{}
	}

	info := RepoInfo{IsRepo: true}

	// Top level directory
	if rootOut, err := gitexec.Command(context.Background(), "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		info.RepoRoot = strings.TrimSpace(string(rootOut))
	} else {
		info.RepoRoot = dir
	}

	// Remote origin URL
	if remOut, err := gitexec.Command(context.Background(), "-C", dir, "remote", "get-url", "origin").Output(); err == nil {
		info.RemoteURL = strings.TrimSpace(string(remOut))
	}

	// Current branch
	if brOut, err := gitexec.Command(context.Background(), "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		info.Branch = strings.TrimSpace(string(brOut))
	}

	return info
}

var (
	// RemoteDirExistsFunc allows mocking remote directory checks in unit tests.
	RemoteDirExistsFunc = defaultRemoteDirExists

	// PromptYesNoFunc allows mocking user prompt answers in unit tests.
	PromptYesNoFunc = defaultPromptYesNo
)

func defaultRemoteDirExists(ctx context.Context, host, remoteDir string) bool {
	script := fmt.Sprintf(`[ -d %s ]`, ShellPathForDir(remoteDir))
	cmd := exec.CommandContext(ctx, "ssh", host, script)
	return cmd.Run() == nil
}

func defaultPromptYesNo(prompt string, defaultVal bool) bool {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) == 0 {
		return defaultVal
	}

	suffix := " [y/N]: "
	if defaultVal {
		suffix = " [Y/n]: "
	}
	fmt.Print(prompt + suffix)

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println()
		return defaultVal
	}

	trimmed := strings.ToLower(strings.TrimSpace(line))
	if trimmed == "" {
		return defaultVal
	}
	return trimmed == "y" || trimmed == "yes"
}

// CloneRepoOnRemote clones a git repository onto the remote host via SSH.
func CloneRepoOnRemote(ctx context.Context, host, remoteURL, branch, remoteDir string) error {
	remoteCdPath := ShellPathForDir(remoteDir)
	mkdirScript := fmt.Sprintf(`mkdir -p $(dirname %s)`, remoteCdPath)
	mkdirCmd := exec.CommandContext(ctx, "ssh", host, mkdirScript)
	if err := mkdirCmd.Run(); err != nil {
		return fmt.Errorf("failed to create remote parent directory: %w", err)
	}

	var cloneScript string
	if branch != "" && branch != "HEAD" {
		cloneScript = fmt.Sprintf(`git clone -b %s %s %s || git clone %s %s`,
			quoteForShell(branch), quoteForShell(remoteURL), remoteCdPath,
			quoteForShell(remoteURL), remoteCdPath)
	} else {
		cloneScript = fmt.Sprintf(`git clone %s %s`, quoteForShell(remoteURL), remoteCdPath)
	}

	cmd := exec.CommandContext(ctx, "ssh", host, cloneScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// FormatRsyncRemotePath converts a path for rsync, converting ~/ into relative path for the remote home.
func FormatRsyncRemotePath(host, remoteDir string) string {
	clean := filepath.Clean(remoteDir)
	if strings.HasPrefix(clean, "~/") {
		return fmt.Sprintf("%s:%s", host, strings.TrimPrefix(clean, "~/"))
	}
	if clean == "~" {
		return fmt.Sprintf("%s:", host)
	}
	return fmt.Sprintf("%s:%s", host, clean)
}

// SyncDirectoryToRemote syncs a local directory to the remote host via rsync.
func SyncDirectoryToRemote(ctx context.Context, host, localDir, remoteDir string) error {
	remoteCdPath := ShellPathForDir(remoteDir)
	mkdirScript := fmt.Sprintf(`mkdir -p %s`, remoteCdPath)
	mkdirCmd := exec.CommandContext(ctx, "ssh", host, mkdirScript)
	if err := mkdirCmd.Run(); err != nil {
		return fmt.Errorf("failed to create remote destination directory: %w", err)
	}

	cleanLocal := filepath.Clean(localDir) + "/"
	remoteTarget := FormatRsyncRemotePath(host, remoteDir) + "/"

	cmd := exec.CommandContext(ctx, "rsync", "-az", "--progress", "--exclude=.DS_Store", cleanLocal, remoteTarget)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
