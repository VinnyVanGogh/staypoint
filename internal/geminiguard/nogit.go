package geminiguard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// noGitEntry is everything recorded for one non-directory path in a non-git
// folder. Equal entries prove the path was not written during the turn.
type noGitEntry struct {
	sig   fileSig
	nlink uint64
	// Symlinks: the link text and the followed target, so a write through
	// the link (to a file inside or outside the folder) changes the entry.
	link      string
	tgt       fileSig
	tgtErr    bool // target missing or unreadable
	tgtDir    bool // target is a directory (walked separately when inside)
	tgtOutDir bool // target is a directory outside the folder
	isSymlink bool
}

func (a noGitEntry) unchanged(b noGitEntry) bool {
	if !a.sig.same(b.sig) || a.nlink != b.nlink || a.isSymlink != b.isSymlink {
		return false
	}
	if !a.isSymlink {
		return true
	}
	if a.link != b.link || a.tgtErr != b.tgtErr || a.tgtDir != b.tgtDir || a.tgtOutDir != b.tgtOutDir {
		return false
	}
	// A directory target's own times move with every doc added under it;
	// one inside the folder is walked anyway, one outside fails the check.
	return a.tgtErr || a.tgtDir || a.tgt.same(b.tgt)
}

// docOK reports whether a changed entry is a document Gemini may write: a
// doc path that is a plain, non-executable, singly linked regular file.
// Symlinks, executables, hard links and special files are never docs: each
// can carry code or write somewhere the walk does not see.
func (e noGitEntry) docOK(p string) bool {
	return IsDocPath(p) && !e.isSymlink && e.sig.mode.IsRegular() &&
		e.sig.mode.Perm()&0o111 == 0 && e.nlink <= 1
}

type noGitState struct {
	origRoot string // the path the caller passed (may be a symlink)
	rootIno  uint64
	rootDev  uint64
	entries  map[string]noGitEntry
}

// TakeNoGit records a non-git directory before a Gemini turn. Unlike Take it
// never runs git, so a parent repository above dir is never consulted. The
// folder is resolved through symlinks first and walked whole: nothing is
// skipped (not .git, not dependency dirs), so nothing goes unchecked.
func TakeNoGit(dir string) (*Snapshot, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve folder: %w", err)
	}
	ino, dev, err := rootID(root)
	if err != nil {
		return nil, err
	}
	entries, err := walkNoGit(root)
	if err != nil {
		return nil, err
	}
	files := make(map[string]fileSig, len(entries))
	for p, e := range entries {
		files[p] = e.sig
	}
	return &Snapshot{root: root, files: files, nogit: &noGitState{
		origRoot: abs, rootIno: ino, rootDev: dev, entries: entries,
	}}, nil
}

// CheckNoGit lists the non-doc paths a turn changed since TakeNoGit. A file
// whose record (size, mtime, ctime, inode, mode, link count, symlink target)
// differs counts as changed: without a checkpoint there is no content to
// compare with, so the check fails closed. Nothing on disk is modified.
func CheckNoGit(_ context.Context, pre *Snapshot) NoGitResult {
	if pre == nil || pre.nogit == nil {
		return NoGitResult{Err: errors.New("no pre-turn snapshot")}
	}
	st := pre.nogit
	res := NoGitResult{Dir: pre.root}
	changed := map[string]bool{}
	var errs []error

	// The folder itself must be the one recorded: a re-pointed link or a
	// replaced directory means the turn's writes went somewhere unrecorded.
	if now, err := filepath.EvalSymlinks(st.origRoot); err != nil || now != pre.root {
		changed["."] = true
	}
	ino, dev, err := rootID(pre.root)
	if err != nil {
		res.Err = err
		return res
	}
	if ino != st.rootIno || dev != st.rootDev {
		changed["."] = true
	}

	post, err := walkNoGit(pre.root)
	if err != nil {
		res.Err = err
		return res
	}
	var outDirs []string
	for p, e := range post {
		if e.tgtOutDir {
			outDirs = append(outDirs, p)
		}
		if old, ok := st.entries[p]; ok && old.unchanged(e) {
			continue
		}
		if e.docOK(p) {
			continue
		}
		changed[p] = true
	}
	for p := range st.entries {
		if _, ok := post[p]; !ok && !IsDocPath(p) {
			changed[p] = true
		}
	}
	res.Changed = sortedKeys(changed)
	if len(outDirs) > 0 {
		errs = append(errs, fmt.Errorf("edits through symlinked directories outside the folder cannot be checked: %s", listPaths(sortedKeys(toSet(outDirs)))))
	}
	if len(res.Changed) > 0 && len(post) > 0 && !anyStrongEntry(post) {
		errs = append(errs, fmt.Errorf("file change times are not available on this platform"))
	}
	res.Err = errors.Join(errs...)
	return res
}

func rootID(root string) (ino, dev uint64, err error) {
	info, err := os.Lstat(root)
	if err != nil {
		return 0, 0, fmt.Errorf("scan folder: %w", err)
	}
	if !info.IsDir() {
		return 0, 0, fmt.Errorf("scan folder: %s is not a directory", root)
	}
	_, ino, _ = statExtra(info.Sys())
	_, dev = statLinks(info.Sys())
	return ino, dev, nil
}

// walkNoGit records every non-directory entry under root by Lstat. Unlike
// walk it skips nothing: in a folder that is not a git repository a .git
// directory is just files the turn could write. Any error fails the walk.
func walkNoGit(root string) (map[string]noGitEntry, error) {
	out := make(map[string]noGitEntry)
	prefix := root + string(filepath.Separator)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := os.Lstat(p)
		if ierr != nil {
			return ierr
		}
		e := noGitEntry{sig: sigOf(info)}
		e.nlink, _ = statLinks(info.Sys())
		if info.Mode()&fs.ModeSymlink != 0 {
			e.isSymlink = true
			if e.link, ierr = os.Readlink(p); ierr != nil {
				return ierr
			}
			if ti, terr := os.Stat(p); terr != nil {
				e.tgtErr = true
			} else {
				e.tgt = sigOf(ti)
				if ti.IsDir() {
					e.tgtDir = true
					real, rerr := filepath.EvalSymlinks(p)
					e.tgtOutDir = rerr != nil || (real != root && !strings.HasPrefix(real, prefix))
				}
			}
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan folder: %w", err)
	}
	return out, nil
}

func sigOf(info fs.FileInfo) fileSig {
	sig := fileSig{mode: info.Mode(), size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
	sig.ctimeNs, sig.ino, sig.strong = statExtra(info.Sys())
	return sig
}

func toSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}

func anyStrongEntry(files map[string]noGitEntry) bool {
	for _, e := range files {
		if e.sig.strong {
			return true
		}
	}
	return false
}
