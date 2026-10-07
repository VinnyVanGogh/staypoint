package orchestrator

import (
	"os"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// TestMain keeps per-task scratch dirs (tasks with no repo_path, STA-864)
// out of the real ~/.staypoint.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "staypoint-scratch-")
	if err != nil {
		panic(err)
	}
	os.Setenv(workspace.ScratchRootEnv, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
