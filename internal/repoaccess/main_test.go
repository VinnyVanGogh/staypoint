package repoaccess_test

import (
	"os"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
)

// TestMain lets this test binary act as the probe child DefaultCommand starts.
func TestMain(m *testing.M) {
	repoaccess.RunProbeChild()
	os.Exit(m.Run())
}
