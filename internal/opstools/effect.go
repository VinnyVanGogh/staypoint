// Package opstools holds the typed ops tools behind the staypoint MCP server
// (task-7d279c9d): each call declares its effect and validates its
// parameters, so the gate decides from the declaration, never from shell text.
package opstools

import "github.com/VinnyVanGogh/staypoint/internal/security"

// Effect is what a tool call does to the world.
type Effect string

const (
	// Read changes nothing.
	Read Effect = "read"
	// DevWrite changes a dev host, a dev branch or StayPoint's own records.
	DevWrite Effect = "dev_write"
	// ProdWrite changes production or a production branch (main, prod).
	ProdWrite Effect = "prod_write"
	// ExternalWrite changes a third-party service.
	ExternalWrite Effect = "external_write"
)

// Decision is the gate's answer for an effect.
type Decision string

const (
	// Allow runs the call now.
	Allow Decision = "allow"
	// Board holds the call for a Board decision.
	Board Decision = "board"
)

// Decide maps an effect to the gate decision: reads auto-allow, dev writes
// run under the unattended-run policy, prod and external writes need the
// Board. An unknown effect needs the Board (fail closed).
func Decide(e Effect) Decision {
	switch e {
	case Read, DevWrite:
		return Allow
	default:
		return Board
	}
}

// Call is one validated tool call: its effect, a one-line summary for the
// run timeline and the Board, and the canonical form a Board approval binds to.
type Call struct {
	Tool    string
	Effect  Effect
	Summary string
}

// Canonical is the gate-request command line for c: an approval for it
// covers exactly this tool and these parameters.
func (c Call) Canonical() string {
	return security.OpsToolPrefix + c.Tool + " " + c.Summary
}
