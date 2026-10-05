package repoaccess_test

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
)

// Failures a non-root test cannot produce for real go through the classifier
// with the error injected.

// EPERM is what a macOS privacy (TCC) denial returns. Non-root tests can't
// produce it without TCC.
func TestFSFailure_EPERMIsMacOSPrivacy(t *testing.T) {
	r := repoaccess.FSFailure(target("/Users/x/Documents/dev/repo"), repoaccess.StepOpen, syscall.EPERM, nil)
	assertCause(t, r, repoaccess.CausePrivacy, repoaccess.StepOpen,
		"open(", "EPERM", "operation not permitted", "macOS privacy setting is blocking staypointd")
	if strings.Contains(r.Message, "Unix file permissions") {
		t.Errorf("EPERM was described as Unix permissions: %s", r.Message)
	}
}

func TestFSFailure_EACCESNamesOwnerModeAndDaemonUID(t *testing.T) {
	owner := &repoaccess.Owner{UID: 0, Mode: os.ModeDir | 0o700}
	r := repoaccess.FSFailure(target("/srv/repo"), repoaccess.StepOpen, syscall.EACCES, owner)
	assertCause(t, r, repoaccess.CauseUnixPermissions, repoaccess.StepOpen,
		"EACCES", "permission denied", "Unix file permissions", "owner uid 0", "mode drwx------",
		fmt.Sprintf("staypointd runs as uid %d", os.Getuid()))
}

// An errno with no specific mapping still names the step and the raw errno;
// it is not folded into a generic message.
func TestFSFailure_OtherErrnoKeepsRawError(t *testing.T) {
	r := repoaccess.FSFailure(target("/mnt/repo"), repoaccess.StepOpen, syscall.EIO, nil)
	assertCause(t, r, repoaccess.CauseFSError, repoaccess.StepOpen, "open(", "EIO", "input/output error")
}

func TestGitFailure_IndexLock(t *testing.T) {
	stderr := "fatal: Unable to create '/srv/repo/.git/index.lock': File exists.\n\nAnother git process seems to be running in this repository"
	r := repoaccess.GitFailure(target("/srv/repo"), repoaccess.StepGitStatus, 128, stderr)
	assertCause(t, r, repoaccess.CauseIndexLock, repoaccess.StepGitStatus,
		"git status --porcelain", "exit 128", "index.lock': File exists", "index.lock exists")
}

func TestGitFailure_UnknownStderrIsShownVerbatim(t *testing.T) {
	stderr := "error: object file .git/objects/ab/cdef is empty\nfatal: loose object abcdef is corrupt"
	r := repoaccess.GitFailure(target("/srv/repo"), repoaccess.StepGitStatus, 128, stderr)
	assertCause(t, r, repoaccess.CauseGitError, repoaccess.StepGitStatus,
		"git status --porcelain", "exit 128", "loose object abcdef is corrupt")
	if r.RawError != stderr {
		t.Errorf("raw error = %q, want git's stderr verbatim", r.RawError)
	}
}

func TestGitFailure_NotARepoAndDubiousOwnershipMap(t *testing.T) {
	cases := []struct {
		stderr string
		cause  repoaccess.Cause
		phrase string
	}{
		{"fatal: not a git repository (or any of the parent directories): .git", repoaccess.CauseNotGitRepo, "not a git repository"},
		{"fatal: detected dubious ownership in repository at '/srv/repo'\nTo add an exception for this directory, call:\n\n\tgit config --global --add safe.directory /srv/repo", repoaccess.CauseDubiousOwnership, "safe.directory"},
	}
	for _, tc := range cases {
		r := repoaccess.GitFailure(target("/srv/repo"), repoaccess.StepGitRevParse, 128, tc.stderr)
		assertCause(t, r, tc.cause, repoaccess.StepGitRevParse, "git rev-parse --show-toplevel", tc.phrase)
	}
}
