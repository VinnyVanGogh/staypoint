// Package workorgs is the set of organizations whose tasks are work (Board
// 2026-10-09): config.toml work_orgs, with Managed Solution always included.
// Work orgs share the work-repo tracking gate (a work repo accepts a task from
// any work org), one work-seat run cap, and work routing (Claude work seat;
// Gemini never writes code).
//
// Membership compares lower(trim(name)) plus the old "MAN" prefix. It is
// deliberately not names.Normalize: the tracking gate uses it as an allow
// list (a work repo accepts this org's task), and folding lookalikes there
// would widen what is allowed.
package workorgs

import (
	"strings"
	"sync"
)

// Default is the work org that is always present.
const Default = "Managed Solution"

var (
	mu   sync.RWMutex
	orgs = []string{Default}
)

// aliases maps Paperclip issue prefixes still found on older tasks.
var aliases = map[string]string{"man": "managed solution"}

func key(org string) string {
	k := strings.ToLower(strings.TrimSpace(org))
	if a, ok := aliases[k]; ok {
		return a
	}
	return k
}

// Normalize returns list trimmed and de-duplicated (case-insensitively), with
// Default first. Blank entries are dropped.
func Normalize(list []string) []string {
	out := []string{Default}
	seen := map[string]bool{key(Default): true}
	for _, o := range list {
		o = strings.TrimSpace(o)
		if o == "" || seen[key(o)] {
			continue
		}
		seen[key(o)] = true
		out = append(out, o)
	}
	return out
}

// Set replaces the process-wide work org list (config.LoadConfig calls it).
func Set(list []string) {
	n := Normalize(list)
	mu.Lock()
	orgs = n
	mu.Unlock()
}

// List returns the work orgs, Default first.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	return append([]string(nil), orgs...)
}

// IsWork reports whether org is a work org. An empty org is not.
func IsWork(org string) bool {
	k := key(org)
	if k == "" {
		return false
	}
	mu.RLock()
	defer mu.RUnlock()
	for _, o := range orgs {
		if key(o) == k {
			return true
		}
	}
	return false
}
