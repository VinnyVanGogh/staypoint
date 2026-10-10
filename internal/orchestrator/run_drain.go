package orchestrator

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Drain mode (task-db71fba9, zero-kill deploys). A deploy puts the daemon in
// drain: it stops claiming runs, queues every wake that arrives (the queue is
// persisted, so the next daemon starts them), and waits for the live runs.
// Each mode includes the ones before it; a drain only ever escalates.
type DrainMode int

const (
	// DrainOff: normal operation.
	DrainOff DrainMode = iota
	// DrainFinish: no new runs; live runs finish on their own.
	DrainFinish
	// DrainBoundary: live runs are also suspended at their next turn
	// boundary (between two agent CLI turns, never mid-tool-call) and resume
	// from that turn on the next daemon.
	DrainBoundary
	// DrainNow: emergency. The current turn is cut short (its agent process
	// group is stopped) and the run is suspended to redo that turn on the
	// next daemon. Uncommitted work stays in the worktree.
	DrainNow
)

var drainModeNames = map[DrainMode]string{
	DrainOff:      "off",
	DrainFinish:   "finish",
	DrainBoundary: "boundary",
	DrainNow:      "now",
}

func (m DrainMode) String() string {
	if s, ok := drainModeNames[m]; ok {
		return s
	}
	return fmt.Sprintf("DrainMode(%d)", int(m))
}

// ParseDrainMode parses "finish", "boundary" or "now" ("" is finish).
func ParseDrainMode(s string) (DrainMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "finish":
		return DrainFinish, nil
	case "boundary", "suspend":
		return DrainBoundary, nil
	case "now":
		return DrainNow, nil
	}
	return DrainOff, fmt.Errorf("unknown drain mode %q (want finish, boundary or now)", s)
}

// ErrDraining is returned by Claim while the daemon drains for a deploy. It
// is a capacity refusal (errors.Is(err, ErrConcurrencyCap)), so callers queue
// the run; the queue is persisted and the next daemon starts it.
var ErrDraining error = &capError{msg: "Can't start run now: StayPoint is draining for a deploy. The run is queued and starts after the restart."}

type drainState struct {
	mode  DrainMode
	since time.Time
	// boundary is closed when the drain reaches DrainBoundary and now when
	// it reaches DrainNow, so waiting runs and live turns can react. They
	// are made on first use and kept across CancelDrain while still open,
	// so a run that took one before a cancel still hears a later drain.
	boundary chan struct{}
	now      chan struct{}
}

func lazyChan(c *chan struct{}) chan struct{} {
	if *c == nil {
		*c = make(chan struct{})
	}
	return *c
}

func closeOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func isClosed(c chan struct{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// DrainStatus is the drain state shown on the task page, the statusline and
// GET /api/daemon/drain.
type DrainStatus struct {
	Draining bool      `json:"draining"`
	Mode     string    `json:"mode"`
	Since    time.Time `json:"since,omitempty"`
	Live     int       `json:"live"`
	Queued   int       `json:"queued"`
	// LiveTasks names the tasks still running, so the deploy script can say
	// what it is waiting on.
	LiveTasks []string `json:"live_tasks"`
}

// Label is the one-line summary: "Draining for deploy: 2 runs left, 1 queued".
func (d DrainStatus) Label() string {
	if !d.Draining {
		return ""
	}
	runs := "runs"
	if d.Live == 1 {
		runs = "run"
	}
	s := fmt.Sprintf("Draining for deploy: %d %s left, %d queued", d.Live, runs, d.Queued)
	switch d.Mode {
	case DrainBoundary.String():
		s += " (suspending at the next turn)"
	case DrainNow.String():
		s += " (suspending now)"
	}
	return s
}

// StartDrain puts the limiter in drain mode, or escalates the current drain
// to mode. A lower mode than the current one is ignored: a drain never
// relaxes except through CancelDrain.
func (s *RunSlots) StartDrain(mode DrainMode) DrainStatus {
	if mode <= DrainOff {
		mode = DrainFinish
	}
	s.mu.Lock()
	if s.drain.mode == DrainOff {
		s.drain.since = time.Now().UTC()
	}
	if mode > s.drain.mode {
		s.drain.mode = mode
	}
	if s.drain.mode >= DrainBoundary {
		closeOnce(lazyChan(&s.drain.boundary))
	}
	if s.drain.mode >= DrainNow {
		closeOnce(lazyChan(&s.drain.now))
	}
	st := s.drainStatusLocked()
	q := s.snapshotLocked()
	s.mu.Unlock()
	s.notify(q)
	return st
}

// CancelDrain leaves drain mode and starts the queued runs that fit.
func (s *RunSlots) CancelDrain() DrainStatus {
	s.mu.Lock()
	old := s.drain
	s.drain = drainState{}
	if !isClosed(old.boundary) {
		s.drain.boundary = old.boundary
	}
	if !isClosed(old.now) {
		s.drain.now = old.now
	}
	st := s.drainStatusLocked()
	s.mu.Unlock()
	s.Pump()
	s.notify(s.Queue())
	return st
}

// Drain returns the current drain mode.
func (s *RunSlots) Drain() DrainMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drain.mode
}

// SuspendRequested reports whether live runs must suspend at their next
// turn boundary.
func (s *RunSlots) SuspendRequested() bool {
	return s.Drain() >= DrainBoundary
}

// DrainBoundaryChan is closed when the drain reaches DrainBoundary.
func (s *RunSlots) DrainBoundaryChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lazyChan(&s.drain.boundary)
}

// DrainNowChan is closed when the drain reaches DrainNow.
func (s *RunSlots) DrainNowChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lazyChan(&s.drain.now)
}

// DrainStatus reports the drain state with the live and queued counts.
func (s *RunSlots) DrainStatus() DrainStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drainStatusLocked()
}

func (s *RunSlots) drainStatusLocked() DrainStatus {
	st := DrainStatus{
		Draining:  s.drain.mode != DrainOff,
		Mode:      s.drain.mode.String(),
		Since:     s.drain.since,
		Live:      len(s.active),
		Queued:    len(s.queue),
		LiveTasks: make([]string, 0, len(s.active)),
	}
	for id := range s.active {
		st.LiveTasks = append(st.LiveTasks, id)
	}
	sort.Strings(st.LiveTasks)
	return st
}
