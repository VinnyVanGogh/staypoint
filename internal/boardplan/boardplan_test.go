package boardplan

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidate_RefusesIncompleteActions(t *testing.T) {
	cases := []struct {
		name    string
		actions []Action
		want    string
	}{
		{"empty", nil, "no actions"},
		{"unknown kind", []Action{{TaskID: "t", Action: "deploy_prod"}}, "unknown action"},
		{"approve without head", []Action{{TaskID: "t", Action: ApproveMerge}}, "expected_head_sha"},
		{"send back without text", []Action{{TaskID: "t", Action: SendBack, Text: "  "}}, "text"},
		{"gate without id", []Action{{Action: GateApprove}}, "gate_id"},
		{"task action without task", []Action{{Action: RunNow}}, "task_id"},
		{"too many", make([]Action, MaxActions+1), "limit"},
	}
	for _, tc := range cases {
		err := Validate(tc.actions)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	ok := []Action{
		{TaskID: "t", Action: ApproveMerge, ExpectedHeadSHA: "abc"},
		{TaskID: "t", Action: SendBack, Text: "fix"},
		{TaskID: "t", Action: MarkDone}, {TaskID: "t", Action: RunNow}, {TaskID: "t", Action: Cancel},
		{TaskID: "t", Action: Unblock}, {GateID: "g", Action: GateApprove}, {GateID: "g", Action: GateDeny},
	}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid plan refused: %v", err)
	}
}

func TestSelectionHash_BindsPlanContentAndSelection(t *testing.T) {
	p := &Plan{ID: "plan-a", Actions: []Action{
		{TaskID: "t1", Action: SendBack, Text: "one"},
		{TaskID: "t2", Action: RunNow},
	}}
	p.ContentHash = ContentHash(p.Actions)
	base := SelectionHash(p, []int{0, 1})

	other := *p
	other.ID = "plan-b"
	edited := &Plan{ID: p.ID, Actions: []Action{{TaskID: "t1", Action: SendBack, Text: "two"}, p.Actions[1]}}
	edited.ContentHash = p.ContentHash // even with a stale content hash, the row bytes differ
	for name, h := range map[string][]byte{
		"other plan":     SelectionHash(&other, []int{0, 1}),
		"edited row":     SelectionHash(edited, []int{0, 1}),
		"fewer rows":     SelectionHash(p, []int{0}),
		"different rows": SelectionHash(p, []int{1}),
	} {
		if bytes.Equal(h, base) {
			t.Errorf("%s: selection hash did not change", name)
		}
	}
	if !bytes.Equal(SelectionHash(p, []int{0, 1}), base) {
		t.Fatal("selection hash is not deterministic")
	}
}

func TestNormalizeSelection(t *testing.T) {
	if _, err := NormalizeSelection(nil, 3); err == nil {
		t.Error("empty selection accepted")
	}
	if _, err := NormalizeSelection([]int{3}, 3); err == nil {
		t.Error("out-of-range index accepted")
	}
	if _, err := NormalizeSelection([]int{-1}, 3); err == nil {
		t.Error("negative index accepted")
	}
	got, err := NormalizeSelection([]int{2, 0, 2}, 3)
	if err != nil || len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("got %v, %v; want [0 2]", got, err)
	}
}

func TestRefuseInAgentRun(t *testing.T) {
	env := map[string]string{"STAYPOINT_TASK_ID": "task-x"}
	if err := RefuseInAgentRun(func(k string) string { return env[k] }); err == nil {
		t.Fatal("agent run allowed to propose")
	}
	if err := RefuseInAgentRun(func(string) string { return "" }); err != nil {
		t.Fatalf("Board terminal refused: %v", err)
	}
}

func TestParseActions(t *testing.T) {
	for _, in := range []string{
		`{"actions":[{"task_id":"t","action":"run_now"}]}`,
		`[{"task_id":"t","action":"run_now"}]`,
	} {
		a, err := ParseActions([]byte(in))
		if err != nil || len(a) != 1 {
			t.Errorf("%s: %v %v", in, a, err)
		}
	}
	if _, err := ParseActions([]byte(`{"actions":[],"auto_sign":true}`)); err == nil {
		t.Error("unknown top-level field accepted")
	}
}
