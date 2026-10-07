package main

import (
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// barGeminiOutsideGit removes Gemini from a route that may write code when
// the task runs in a directory that is not a git repository (or the scratch
// dir of a task with no repo). The Gemini guard needs git checkpoints to
// revert code edits; without them Gemini must not do code work at all.
// Docs-shaped kinds keep Gemini; the harness fails those runs closed if a
// Gemini turn changes a non-doc file.
func barGeminiOutsideGit(route router.KindRoute, repoPath, taskID string) router.KindRoute {
	if !router.KindMayWriteCode(route.Kind) {
		return route
	}
	if taskDirIsGit(repoPath, taskID) {
		return route
	}
	keep := func(in []router.SkippedSlot) []router.SkippedSlot {
		out := in[:0:0]
		for _, s := range in {
			if s.Slot.Family != router.FamilyGemini {
				out = append(out, s)
			}
		}
		return out
	}
	cands := route.Candidates[:0:0]
	for _, s := range route.Candidates {
		if s.Family != router.FamilyGemini {
			cands = append(cands, s)
		}
	}
	route.Candidates = cands
	route.Skipped = keep(route.Skipped)
	route.Locked = keep(route.Locked)
	route.GeminiCodeApprovalID = "" // an approval never applies outside git
	return route
}

// taskDirIsGit reports whether the task runs in a git repo (the harness uses
// the same workspace.DescribeTaskDir). An unresolvable dir counts as git: the
// harness then takes the git path and fails as before.
func taskDirIsGit(repoPath, taskID string) bool {
	td, err := workspace.DescribeTaskDir(repoPath, taskID)
	return err != nil || td.Git
}
