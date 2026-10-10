package governance

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The task page's stage control (task-40f0a2f0) mirrors the server's stage
// lists in webui/lib/taskactions.js. If they drift, the page offers moves the
// server refuses, or hides ones it accepts.
func TestTaskActionsMirrorsStageLists(t *testing.T) {
	src, err := os.ReadFile("../server/webui/lib/taskactions.js")
	if err != nil {
		t.Fatal(err)
	}
	jsList := func(re string) []string {
		m := regexp.MustCompile(re).FindSubmatch(src)
		if m == nil {
			t.Fatalf("taskactions.js: %s not found", re)
		}
		var out []string
		for _, q := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
		return out
	}
	if got, want := strings.Join(jsList(`BOARD_STAGES = \[([^\]]*)\]`), ","), strings.Join(BoardSettableStages, ","); got != want {
		t.Errorf("BOARD_STAGES = %s, want governance.BoardSettableStages %s", got, want)
	}
	if got, want := strings.Join(jsList(`PARKED_STAGES = new Set\(\[([^\]]*)\]\)`), ","), strings.Join(nonRunnableStages, ","); got != want {
		t.Errorf("PARKED_STAGES = %s, want governance.nonRunnableStages %s", got, want)
	}
}
