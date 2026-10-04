package main

import (
	"bytes"
	"os"
	"testing"
)

// TestDaemon_NeverCallsSetTestMode proves the production daemon binary does not
// enable TestMode. TestMode is a security bypass (WrapBoardSession accepts any
// staypoint_board cookie without value validation) and must only ever appear in
// test harnesses. A compile-time grep on this file is the cheapest enforcement
// that survives refactors.
func TestDaemon_NeverCallsSetTestMode(t *testing.T) {
	for _, name := range []string{"main.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if bytes.Contains(data, []byte("TestMode: true")) {
			t.Errorf("cmd/staypointd/%s contains TestMode: true — must never appear in the production daemon", name)
		}
		if bytes.Contains(data, []byte("SetTestMode(true)")) {
			t.Errorf("cmd/staypointd/%s calls SetTestMode(true) — must never appear in the production daemon", name)
		}
	}
}
