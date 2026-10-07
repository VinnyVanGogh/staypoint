package geminiguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// NoGitResult is the outcome of checking a Gemini turn in a directory that
// is not a git repository. There is no checkpoint to restore from, so
// nothing is reverted: any non-doc change fails the run, and says so.
type NoGitResult struct {
	Dir     string
	Changed []string // non-doc paths created, modified or deleted
	Err     error    // the directory could not be checked: fail closed
}

// Violated reports whether the run must be stopped.
func (r NoGitResult) Violated() bool { return len(r.Changed) > 0 || r.Err != nil }

// Title is the timeline row for a violation.
func (r NoGitResult) Title() string {
	if len(r.Changed) == 0 {
		return "Blocked: could not verify Gemini's edits in a folder that is not a git repository"
	}
	return "Blocked: Gemini changed non-doc files in a folder that is not a git repository: " + listPaths(r.Changed) + " (not reverted)"
}

// Body explains why nothing was reverted.
func (r NoGitResult) Body() string {
	parts := []string{
		"Board rule: Gemini may only edit documentation (" + strings.Join(Allowlist(), ", ") + ").",
		"This folder is not a git repository, so there is no checkpoint to restore: the changes were NOT reverted. Check these files by hand.",
	}
	if r.Err != nil {
		parts = append(parts, "Guard error: "+r.Err.Error()+".")
	}
	return strings.Join(parts, " ")
}

// TakeNoGit records a non-git directory before a Gemini turn. Unlike Take it
// never runs git, so a parent repository above dir is never consulted.
func TakeNoGit(dir string) (*Snapshot, error) {
	files, gitFile, gitIsFile, err := walk(dir)
	if err != nil {
		return nil, err
	}
	return &Snapshot{root: dir, files: files, gitFile: gitFile, gitIsFile: gitIsFile}, nil
}

// CheckNoGit lists the non-doc paths a turn changed since TakeNoGit. A file
// whose signature (size, mtime, ctime, inode) differs counts as changed:
// without a checkpoint there is no content to compare with, so the check
// fails closed. Nothing on disk is modified.
func CheckNoGit(_ context.Context, pre *Snapshot) NoGitResult {
	if pre == nil {
		return NoGitResult{Err: errors.New("no pre-turn snapshot")}
	}
	res := NoGitResult{Dir: pre.root}
	post, gitFile, gitIsFile, err := walk(pre.root)
	if err != nil {
		res.Err = err
		return res
	}
	changed := map[string]bool{}
	if gitIsFile != pre.gitIsFile || string(gitFile) != string(pre.gitFile) {
		changed[".git"] = true
	}
	for p, sig := range post {
		if IsDocPath(p) {
			continue
		}
		if old, ok := pre.files[p]; ok && old.same(sig) {
			continue
		}
		changed[p] = true
	}
	for p := range pre.files {
		if _, ok := post[p]; !ok && !IsDocPath(p) {
			changed[p] = true
		}
	}
	res.Changed = sortedKeys(changed)
	if len(res.Changed) > 0 && len(post) > 0 && !anyStrong(post) {
		res.Err = fmt.Errorf("file change times are not available on this platform")
	}
	return res
}

func anyStrong(files map[string]fileSig) bool {
	for _, s := range files {
		if s.strong {
			return true
		}
	}
	return false
}
