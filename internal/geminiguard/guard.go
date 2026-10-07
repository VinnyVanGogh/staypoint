package geminiguard

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git object ids, not a security hash
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// fileSig is what a turn must not change for a file to count as untouched.
// ctime and inode cannot be set back by an unprivileged process, so equal
// signatures prove the file was not written; differing ones are content-checked.
type fileSig struct {
	mode    fs.FileMode
	size    int64
	mtimeNs int64
	ctimeNs int64
	ino     uint64
	strong  bool // ctime/inode available
}

func (a fileSig) same(b fileSig) bool {
	return a.strong && b.strong && a == b
}

// Snapshot is the daemon-held state of a worktree just before an agent turn.
// Nothing in it is re-read from agent-writable storage when the turn is
// checked: the manifest, HEAD, branch and checkpoint SHA live in daemon memory.
type Snapshot struct {
	root       string
	files      map[string]fileSig
	gitIsFile  bool
	gitFile    []byte // contents of <root>/.git when it is a worktree pointer file
	isGit      bool
	head       string // HEAD commit ("" when unborn or not a git repo)
	branch     string // ref HEAD pointed at ("" when detached)
	checkpoint string // pre-turn checkpoint commit created by the daemon
	// allowCode is set for a run the Board approved with Touch ID (personal
	// repo, one run): code changes are allowed, but .git tampering and paths
	// escaping the repo are still blocked and reverted.
	allowCode bool
	// nogit is the full record TakeNoGit keeps for a folder that is not a
	// git repository (nil for Take).
	nogit *noGitState
}

// AllowCode switches the snapshot to the Board-approved code policy (see
// Snapshot.allowCode) and returns it.
func (s *Snapshot) AllowCode() *Snapshot {
	if s != nil {
		s.allowCode = true
	}
	return s
}

// allowed reports whether the turn may change repo-relative path p.
func (s *Snapshot) allowed(p string) bool {
	if s.allowCode {
		return IsRepoPath(p)
	}
	return IsDocPath(p)
}

// Take records the worktree state before a turn. checkpointCommit is the
// commit SHA returned by checkpoint.CreateCheckpoint for this turn (the
// in-memory value, never a ref the agent could move); "" when none was made.
func Take(ctx context.Context, root, checkpointCommit string) (*Snapshot, error) {
	s := &Snapshot{root: root, checkpoint: strings.TrimSpace(checkpointCommit)}
	if top, err := git(ctx, root, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		s.isGit = true
		s.root = top
		s.branch, _ = git(ctx, s.root, "symbolic-ref", "-q", "HEAD")
		s.head, _ = git(ctx, s.root, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	}
	files, gitFile, gitIsFile, err := walk(s.root)
	if err != nil {
		return nil, err
	}
	s.files, s.gitFile, s.gitIsFile = files, gitFile, gitIsFile
	return s, nil
}

// Result is the outcome of checking one Gemini turn.
type Result struct {
	// Blocked lists non-documentation paths the turn changed, in the worktree
	// or in commits on the task branch.
	Blocked []string
	// Unresolved lists blocked paths that could not be restored.
	Unresolved []string
	// Err is set when the turn could not be verified. It is treated as a
	// violation: the guard fails closed.
	Err error
}

// Violated reports whether the run must be stopped.
func (r Result) Violated() bool { return len(r.Blocked) > 0 || r.Err != nil }

// Title is the timeline row for a violation.
func (r Result) Title() string {
	if len(r.Blocked) == 0 {
		return "Blocked: could not verify Gemini's edits"
	}
	status := "reverted"
	if len(r.Unresolved) > 0 {
		status = "revert incomplete: " + listPaths(r.Unresolved)
	}
	return "Blocked: Gemini edited code: " + listPaths(r.Blocked) + " (" + status + ")"
}

// Body explains the rule and any verification error.
func (r Result) Body() string {
	parts := []string{"Board rule: Gemini never writes code; it may only edit non-code files (" + strings.Join(Allowlist(), ", ") + ")."}
	if r.Err != nil {
		parts = append(parts, "Guard error: "+r.Err.Error()+".")
	}
	if len(r.Blocked) > 0 && len(r.Unresolved) == 0 {
		parts = append(parts, "Files were restored to the pre-turn checkpoint.")
	}
	return strings.Join(parts, " ")
}

func listPaths(p []string) string {
	const max = 10
	if len(p) <= max {
		return strings.Join(p, ", ")
	}
	return strings.Join(p[:max], ", ") + fmt.Sprintf(" and %d more", len(p)-max)
}

// Enforce compares the worktree and task branch with the pre-turn snapshot.
// Any changed path outside the documentation allowlist is restored to the
// checkpoint (new files are deleted, commits on the branch are rolled back),
// and the result reports the violation. Doc-only turns return a zero Result.
func Enforce(ctx context.Context, pre *Snapshot) Result {
	if pre == nil {
		return Result{Err: errors.New("no pre-turn snapshot")}
	}
	var res Result
	var errs []error

	post, gitFile, gitIsFile, walkErr := walk(pre.root)
	if walkErr != nil {
		errs = append(errs, walkErr)
	}

	tree, treeErr := pre.checkpointTree(ctx)
	worktreeBad := map[string]bool{}
	gitPointerChanged := gitIsFile != pre.gitIsFile || !bytes.Equal(gitFile, pre.gitFile)
	if gitPointerChanged {
		worktreeBad[".git"] = true
	}
	if walkErr == nil {
		for p, sig := range post {
			if pre.allowed(p) {
				continue
			}
			old, existed := pre.files[p]
			if existed && old.same(sig) {
				continue
			}
			if existed && treeErr == nil && contentMatches(pre.root, p, tree[p]) {
				continue
			}
			worktreeBad[p] = true
		}
		for p := range pre.files {
			if _, still := post[p]; !still && !pre.allowed(p) {
				worktreeBad[p] = true
			}
		}
	}

	// Restore the worktree pointer first: every git call below depends on it.
	if gitPointerChanged && pre.gitIsFile {
		_ = os.RemoveAll(filepath.Join(pre.root, ".git"))
		if err := os.WriteFile(filepath.Join(pre.root, ".git"), pre.gitFile, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("restore .git pointer: %w", err))
		}
	}

	committedBad, refsMoved, commitErr := pre.committedChanges(ctx)
	if commitErr != nil {
		errs = append(errs, commitErr)
	}

	blocked := map[string]bool{}
	for p := range worktreeBad {
		blocked[p] = true
	}
	for _, p := range committedBad {
		blocked[p] = true
	}
	res.Blocked = sortedKeys(blocked)
	if len(errs) > 0 {
		res.Err = errors.Join(errs...)
	}
	if !res.Violated() {
		return res
	}

	// Fail closed from here: roll the branch back, then restore files.
	if pre.isGit && (refsMoved || len(res.Blocked) > 0) {
		if err := pre.resetRefs(ctx); err != nil {
			res.Err = errors.Join(res.Err, err)
			res.Unresolved = append(res.Unresolved, committedBad...)
		}
	}
	paths := sortedKeys(worktreeBad)
	sort.SliceStable(paths, func(i, j int) bool {
		return strings.Count(paths[i], "/") < strings.Count(paths[j], "/")
	})
	for _, p := range paths {
		if p == ".git" {
			if !pre.gitIsFile && gitIsFile {
				// A pointer file appeared where a git dir was; nothing safe to do.
				res.Unresolved = append(res.Unresolved, p)
			}
			continue
		}
		if err := pre.restore(ctx, p, tree, treeErr); err != nil {
			res.Unresolved = append(res.Unresolved, p)
			res.Err = errors.Join(res.Err, err)
		}
	}
	res.Unresolved = dedupSorted(res.Unresolved)
	return res
}

// walk lists every non-directory entry under root by Lstat, skipping .git
// directories. Ignore rules, attributes and git config play no part, so an
// agent cannot hide a file from it.
func walk(root string) (map[string]fileSig, []byte, bool, error) {
	files := make(map[string]fileSig)
	var gitFile []byte
	gitIsFile := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.Name() == ".git" {
			if rel == ".git" && !d.IsDir() {
				data, rerr := os.ReadFile(p)
				if rerr != nil {
					return rerr
				}
				gitFile, gitIsFile = data, true
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		sig := fileSig{mode: info.Mode(), size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
		sig.ctimeNs, sig.ino, sig.strong = statExtra(info.Sys())
		files[rel] = sig
		return nil
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("scan worktree: %w", err)
	}
	return files, gitFile, gitIsFile, nil
}

type treeEntry struct {
	mode string
	sha  string
}

// checkpointTree lists the pre-turn checkpoint commit. Objects are
// content-addressed, so an agent cannot alter what this SHA names.
func (s *Snapshot) checkpointTree(ctx context.Context) (map[string]treeEntry, error) {
	if !s.isGit || s.checkpoint == "" {
		return nil, errors.New("no pre-turn checkpoint")
	}
	out, err := gitRaw(ctx, s.root, "ls-tree", "-r", "-z", "--full-tree", s.checkpoint)
	if err != nil {
		return nil, err
	}
	tree := make(map[string]treeEntry)
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		tree[p] = treeEntry{mode: f[0], sha: f[2]}
	}
	return tree, nil
}

// contentMatches reports whether the file at rel has exactly the content and
// mode recorded in the checkpoint entry.
func contentMatches(root, rel string, e treeEntry) bool {
	if e.sha == "" {
		return false
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		return false
	}
	var data []byte
	mode := "100644"
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		t, err := os.Readlink(full)
		if err != nil {
			return false
		}
		data, mode = []byte(t), "120000"
	case info.Mode().IsRegular():
		if data, err = os.ReadFile(full); err != nil {
			return false
		}
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
	default:
		return false
	}
	return mode == e.mode && blobID(data, len(e.sha)) == e.sha
}

// blobID is git's object id for a blob (sha1, or sha256 repos by id length).
func blobID(data []byte, hexLen int) string {
	hdr := fmt.Sprintf("blob %d\x00", len(data))
	if hexLen == 64 {
		h := sha256.New()
		_, _ = io.WriteString(h, hdr)
		_, _ = h.Write(data)
		return hex.EncodeToString(h.Sum(nil))
	}
	h := sha1.New() //nolint:gosec // git object id
	_, _ = io.WriteString(h, hdr)
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// committedChanges lists non-doc paths changed by commits made during the
// turn on the task branch (or on HEAD, if the agent moved it elsewhere).
func (s *Snapshot) committedChanges(ctx context.Context) ([]string, bool, error) {
	if !s.isGit {
		return nil, false, nil
	}
	var tips []string
	moved := false
	if s.branch != "" {
		tip, _ := git(ctx, s.root, "rev-parse", "-q", "--verify", s.branch+"^{commit}")
		if tip != s.head {
			moved = true
			if tip != "" {
				tips = append(tips, tip)
			}
		}
	}
	sym, _ := git(ctx, s.root, "symbolic-ref", "-q", "HEAD")
	head, _ := git(ctx, s.root, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	if sym != s.branch || head != s.head {
		moved = true
		if head != "" && head != s.head {
			tips = append(tips, head)
		}
	}
	bad := map[string]bool{}
	var errs []error
	for _, tip := range tips {
		var out string
		var err error
		if s.head == "" {
			out, err = gitRaw(ctx, s.root, "ls-tree", "-r", "-z", "--name-only", "--full-tree", tip)
		} else {
			out, err = gitRaw(ctx, s.root, "diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--name-only", s.head, tip)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range strings.Split(out, "\x00") {
			if p != "" && !s.allowed(p) {
				bad[p] = true
			}
		}
	}
	return sortedKeys(bad), moved, errors.Join(errs...)
}

// resetRefs puts the task branch and HEAD back where the daemon recorded
// them, and resets the index to that commit. The worktree is left alone;
// restore handles files.
func (s *Snapshot) resetRefs(ctx context.Context) error {
	var errs []error
	if s.branch != "" {
		var err error
		if s.head != "" {
			_, err = git(ctx, s.root, "update-ref", s.branch, s.head)
		} else {
			_, err = git(ctx, s.root, "update-ref", "-d", s.branch)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if _, err := git(ctx, s.root, "symbolic-ref", "HEAD", s.branch); err != nil {
			errs = append(errs, err)
		}
	} else if s.head != "" {
		if _, err := git(ctx, s.root, "update-ref", "--no-deref", "HEAD", s.head); err != nil {
			errs = append(errs, err)
		}
	}
	if s.head != "" {
		if _, err := git(ctx, s.root, "read-tree", s.head); err != nil {
			errs = append(errs, err)
		}
	} else if _, err := git(ctx, s.root, "read-tree", "--empty"); err != nil {
		errs = append(errs, err)
	}
	if s.branch != "" {
		tip, _ := git(ctx, s.root, "rev-parse", "-q", "--verify", s.branch+"^{commit}")
		if tip != s.head {
			errs = append(errs, fmt.Errorf("task branch still at %q, want %q", tip, s.head))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("roll back commits: %w", err)
	}
	return nil
}

// restore puts one path back to its checkpoint content, or deletes it when the
// checkpoint has no such file (new in this turn, or an untracked ignored file
// whose old content the checkpoint never held). The result is re-verified.
func (s *Snapshot) restore(ctx context.Context, rel string, tree map[string]treeEntry, treeErr error) error {
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := s.realParents(rel); err != nil {
		return err
	}
	e, inTree := tree[rel]
	if treeErr != nil {
		inTree = false
	}
	if err := os.RemoveAll(full); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	if !inTree {
		if _, err := os.Lstat(full); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s still present after removal", rel)
		}
		return nil
	}
	data, err := gitRaw(ctx, s.root, "cat-file", "blob", e.sha)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	switch e.mode {
	case "120000":
		err = os.Symlink(data, full)
	case "100755":
		err = os.WriteFile(full, []byte(data), 0o755)
	default:
		err = os.WriteFile(full, []byte(data), 0o644)
	}
	if err != nil {
		return fmt.Errorf("restore %s: %w", rel, err)
	}
	if !contentMatches(s.root, rel, e) {
		return fmt.Errorf("%s does not match the checkpoint after restore", rel)
	}
	return nil
}

// realParents refuses to write through a parent that is now a symlink or file.
func (s *Snapshot) realParents(rel string) error {
	dir := filepath.Dir(filepath.FromSlash(rel))
	if dir == "." {
		return nil
	}
	cur := s.root
	for _, seg := range strings.Split(dir, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %s of %s is not a directory", cur, rel)
		}
	}
	return nil
}

// gitArgs are prepended to every call: an agent can write .git/config and
// hooks, so nothing it planted may run inside the daemon's guard.
var gitArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

func gitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := gitexec.Command(ctx, append(append([]string{}, gitArgs...), args...)...)
	cmd.Dir = dir
	cmd.Env = security.ChildEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupSorted(in []string) []string {
	m := map[string]bool{}
	for _, s := range in {
		m[s] = true
	}
	return sortedKeys(m)
}
