package config

import (
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func TestTurnLimitsDefaults(t *testing.T) {
	var c Config
	if d, err := c.TurnTimeoutOrDefault(); err != nil || d != 0 {
		t.Fatalf("turn_timeout default = %v, %v; want 0 (no limit)", d, err)
	}
	if d, err := c.StallTimeoutOrDefault(); err != nil || d != 20*time.Minute {
		t.Fatalf("stall_timeout default = %v, %v; want 20m", d, err)
	}
}

func TestTurnLimitsFromTOML(t *testing.T) {
	var c Config
	if err := toml.Unmarshal([]byte("turn_timeout = \"2h\"\nstall_timeout = \"0\"\n"), &c); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.TurnTimeoutOrDefault(); d != 2*time.Hour {
		t.Fatalf("turn_timeout = %v", d)
	}
	if d, _ := c.StallTimeoutOrDefault(); d != 0 {
		t.Fatalf("stall_timeout = %v, want 0 (off)", d)
	}
	c.StallTimeout = "soon"
	if d, err := c.StallTimeoutOrDefault(); err == nil || d != 20*time.Minute {
		t.Fatalf("bad stall_timeout = %v, %v; want error and the default", d, err)
	}
	c.TurnTimeout = "-5m"
	if d, err := c.TurnTimeoutOrDefault(); err == nil || d != 0 {
		t.Fatalf("negative turn_timeout = %v, %v", d, err)
	}
}
