package repoaccess_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
)

// TestMain lets this test binary act as the probe child DefaultCommand starts,
// and keeps git away from the machine's global and system config. CI runners
// set safe.directory=* globally, which disables git's ownership check and
// hides the dubious-ownership failure the tests need to see.
func TestMain(m *testing.M) {
	repoaccess.RunProbeChild()
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

func TestHelperNoop(t *testing.T) {}

// The probe env var alone must not turn a process into a probe child: an
// empty or stray STAYPOINT_REPO_PROBE_PATH would otherwise make the daemon
// exit at startup (STA-692).
func TestRunProbeChild_RequiresItsFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperNoop$")
	cmd.Env = append(os.Environ(), "STAYPOINT_REPO_PROBE_PATH=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("process without the probe flag did not run normally: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("process without the probe flag acted as a probe child:\n%s", out)
	}
}
