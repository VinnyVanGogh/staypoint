package shipreview

// task-2114d4aa: the CI state of the branch a card merges into. #244-#251
// merged onto a red main because nothing on the card said main was red;
// Approve and Merge PR now read it and refuse a red target unless the Board
// overrides.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Target CI states.
const (
	TargetCIGreen   = "green"
	TargetCIRed     = "red"
	TargetCIPending = "pending"
	TargetCIUnknown = "unknown"
)

// redConclusions are the run and job conclusions that make a branch red. A
// cancelled run (superseded by a newer push) says nothing about the code.
var redConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
}

// BranchRun is one row of `gh run list --json`.
type BranchRun struct {
	ID           int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	HeadSHA      string `json:"headSha"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	URL          string `json:"url"`
	CreatedAt    string `json:"createdAt"`
}

// TargetCIRun is a workflow whose last finished run on the target is red.
type TargetCIRun struct {
	RunID      int64    `json:"run_id"`
	Workflow   string   `json:"workflow"`
	Conclusion string   `json:"conclusion"`
	HeadSHA    string   `json:"head_sha"`
	URL        string   `json:"url"`
	FailedJobs []string `json:"failed_jobs,omitempty"`
}

// TargetCI is the target branch's CI as shown on the card.
type TargetCI struct {
	Branch string `json:"branch"`
	State  string `json:"state"`
	// HeadSHA is the newest commit with a run on the branch.
	HeadSHA string        `json:"head_sha,omitempty"`
	Failing []TargetCIRun `json:"failing,omitempty"`
	// Running names workflows with a run still in progress on the branch.
	Running   []string `json:"running,omitempty"`
	Note      string   `json:"note,omitempty"`
	CheckedAt string   `json:"checked_at"`
}

// Blocking reports whether the target is red. Unknown and pending do not
// block: GitHub branch protection's required checks are the hard gate, this
// is the card saying so before the Board clicks.
func (c TargetCI) Blocking() bool { return c.State == TargetCIRed }

// EvaluateTargetCI decides the branch's state from its push runs, newest
// first (gh's order). Each workflow counts by its last finished run: a red
// run stays red while a newer run is still going, and a cancelled run is
// skipped for the one before it.
func EvaluateTargetCI(branch string, runs []BranchRun) TargetCI {
	out := TargetCI{Branch: branch, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(runs) == 0 {
		out.State = TargetCIUnknown
		out.Note = "no CI runs on " + branch
		return out
	}
	out.HeadSHA = runs[0].HeadSHA
	decided := map[string]bool{}
	running := map[string]bool{}
	var order []string
	for _, r := range runs {
		wf := r.WorkflowName
		if _, seen := decided[wf]; !seen {
			decided[wf] = false
			order = append(order, wf)
		}
		if decided[wf] {
			continue
		}
		if r.Status != "completed" {
			if !running[wf] {
				running[wf] = true
				out.Running = append(out.Running, wf)
			}
			continue
		}
		if r.Conclusion == "cancelled" || r.Conclusion == "skipped" {
			continue
		}
		decided[wf] = true
		if redConclusions[r.Conclusion] {
			out.Failing = append(out.Failing, TargetCIRun{RunID: r.ID, Workflow: wf, Conclusion: r.Conclusion, HeadSHA: r.HeadSHA, URL: r.URL})
		}
	}
	finished := 0
	for _, wf := range order {
		if decided[wf] {
			finished++
		}
	}
	switch {
	case len(out.Failing) > 0:
		out.State = TargetCIRed
	case finished == 0 && len(out.Running) > 0:
		out.State = TargetCIPending
	case finished == 0:
		out.State = TargetCIUnknown
		out.Note = "no finished CI runs on " + branch
	default:
		out.State = TargetCIGreen
	}
	return out
}

// targetCIRunLimit is how many recent push runs are read. Enough to reach
// back past a few in-progress heads to each workflow's last finished run.
const targetCIRunLimit = 40

// TargetBranchCI reads branch's CI from GitHub. It never returns an error: a
// branch it cannot read is TargetCIUnknown with the reason in Note.
func (a GHAuth) TargetBranchCI(ctx context.Context, branch string) TargetCI {
	unknown := func(note string) TargetCI {
		return TargetCI{Branch: branch, State: TargetCIUnknown, Note: note, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	if branch == "" || strings.HasPrefix(branch, "-") {
		return unknown("no target branch")
	}
	out, err := a.gh(ctx, "run", "list", "--branch", branch, "--event", "push",
		"--json", "databaseId,workflowName,headSha,status,conclusion,url,createdAt", "--limit", strconv.Itoa(targetCIRunLimit))
	if err != nil {
		return unknown("could not list CI runs on " + branch + " (" + firstLine(err.Error()) + ")")
	}
	var runs []BranchRun
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &runs); err != nil {
		return unknown("could not read CI runs on " + branch + " (" + err.Error() + ")")
	}
	ci := EvaluateTargetCI(branch, runs)
	// Name the failing jobs, best effort: the workflow name is the fallback.
	for i := range ci.Failing {
		jout, err := a.gh(ctx, "run", "view", strconv.FormatInt(ci.Failing[i].RunID, 10), "--json", "jobs")
		if err != nil {
			continue
		}
		var resp struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if json.Unmarshal([]byte(jout), &resp) != nil {
			continue
		}
		for _, j := range resp.Jobs {
			if redConclusions[j.Conclusion] {
				ci.Failing[i].FailedJobs = append(ci.Failing[i].FailedJobs, j.Name)
			}
		}
	}
	return ci
}

// TargetCICacheTTL is how long a read is reused for display. Approve and
// Merge PR always read fresh (maxAge 0), and their read refreshes the cache.
var TargetCICacheTTL = time.Minute

var targetCICache = struct {
	sync.Mutex
	m map[string]cachedTargetCI
}{m: map[string]cachedTargetCI{}}

type cachedTargetCI struct {
	at time.Time
	ci TargetCI
}

// TargetCIWithin is TargetBranchCI, reusing a read of the same repo and
// branch made less than maxAge ago, so cards polling the state don't each
// spawn gh. maxAge 0 always reads GitHub.
func (a GHAuth) TargetCIWithin(ctx context.Context, branch string, maxAge time.Duration) TargetCI {
	key := a.RepoDir + "\x00" + a.ConfigDir + "\x00" + branch
	if maxAge > 0 {
		targetCICache.Lock()
		c, ok := targetCICache.m[key]
		targetCICache.Unlock()
		if ok && time.Since(c.at) < maxAge {
			return c.ci
		}
	}
	ci := a.TargetBranchCI(ctx, branch)
	if ctx.Err() != nil {
		return ci // a cancelled read says nothing about the branch
	}
	targetCICache.Lock()
	targetCICache.m[key] = cachedTargetCI{at: time.Now(), ci: ci}
	targetCICache.Unlock()
	return ci
}
