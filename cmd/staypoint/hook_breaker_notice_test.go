package main

import (
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
)

func TestCircuitBreakerNotice_NoEmptyPlaceholders(t *testing.T) {
	cases := []struct {
		tool, cmd, want string
	}{
		{"tool", "", "(repeated tool failures)"},
		{"", "", "(repeated tool failures)"},
		{"Bash", "", "(repeated failures of Bash)"},
		{"Bash", "go test ./...", "(repeated failures of Bash on go test ./...)"},
		{"tool", "make", "(repeated failures of make)"},
	}
	for _, c := range cases {
		got := circuitBreakerNotice(&telemetry.CircuitBreaker{SessionID: "s1", FailingTool: c.tool, FailingCommand: c.cmd, LastError: "boom", IsTripped: true})
		if !strings.Contains(got, c.want) {
			t.Errorf("tool=%q cmd=%q: want %q in %q", c.tool, c.cmd, c.want, got)
		}
		if strings.Contains(got, " on )") || strings.Contains(got, "( on") {
			t.Errorf("empty placeholder in %q", got)
		}
	}
}
