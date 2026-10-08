package shipreview_test

import (
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func branchRun(id int64, wf, status, conclusion string) shipreview.BranchRun {
	return shipreview.BranchRun{ID: id, WorkflowName: wf, HeadSHA: "sha" + wf, Status: status, Conclusion: conclusion}
}

func TestEvaluateTargetCI(t *testing.T) {
	cases := []struct {
		name    string
		runs    []shipreview.BranchRun
		state   string
		failing int
		running int
	}{
		{"no runs", nil, shipreview.TargetCIUnknown, 0, 0},
		{"green", []shipreview.BranchRun{branchRun(2, "CI", "completed", "success")}, shipreview.TargetCIGreen, 0, 0},
		{"red", []shipreview.BranchRun{branchRun(2, "CI", "completed", "failure"), branchRun(1, "CI", "completed", "success")}, shipreview.TargetCIRed, 1, 0},
		// Fixed since: the newest finished run counts, not an older red one.
		{"fixed", []shipreview.BranchRun{branchRun(2, "CI", "completed", "success"), branchRun(1, "CI", "completed", "failure")}, shipreview.TargetCIGreen, 0, 0},
		// Red stays red while the run that may fix it is still going.
		{"red, newer in progress", []shipreview.BranchRun{branchRun(3, "CI", "in_progress", ""), branchRun(2, "CI", "completed", "failure")}, shipreview.TargetCIRed, 1, 1},
		// A cancelled (superseded) run is skipped for the one before it.
		{"cancelled skipped", []shipreview.BranchRun{branchRun(3, "CI", "completed", "cancelled"), branchRun(2, "CI", "completed", "failure")}, shipreview.TargetCIRed, 1, 0},
		{"timed out", []shipreview.BranchRun{branchRun(2, "CI", "completed", "timed_out")}, shipreview.TargetCIRed, 1, 0},
		// One red workflow is enough, whatever the others say.
		{"one of two red", []shipreview.BranchRun{branchRun(3, "Bottles", "completed", "success"), branchRun(2, "CI", "completed", "startup_failure")}, shipreview.TargetCIRed, 1, 0},
		{"only in progress", []shipreview.BranchRun{branchRun(1, "CI", "queued", "")}, shipreview.TargetCIPending, 0, 1},
		{"only cancelled", []shipreview.BranchRun{branchRun(1, "CI", "completed", "cancelled")}, shipreview.TargetCIUnknown, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ci := shipreview.EvaluateTargetCI("main", c.runs)
			if ci.State != c.state || len(ci.Failing) != c.failing || len(ci.Running) != c.running {
				t.Fatalf("got %+v, want state %s failing %d running %d", ci, c.state, c.failing, c.running)
			}
			if ci.Blocking() != (c.state == shipreview.TargetCIRed) {
				t.Errorf("Blocking() = %v for %s", ci.Blocking(), ci.State)
			}
			if c.failing > 0 && ci.Failing[0].RunID == 0 {
				t.Errorf("failing run lost its id: %+v", ci.Failing[0])
			}
		})
	}
}
