package orchestrator

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// DefaultMaxConcurrentRuns is the global parallel-run cap used when
// config.toml does not set max_concurrent_runs (STA-773).
const DefaultMaxConcurrentRuns = 3

// capError is a refusal that callers should treat as "queue and retry later".
// errors.Is(err, ErrConcurrencyCap) is true for every capError so callers that
// only know the global cap keep working.
type capError struct{ msg string }

func (e *capError) Error() string        { return e.msg }
func (e *capError) Is(target error) bool { return target == ErrConcurrencyCap }

// ErrRepoBusy is returned by Claim when another run already holds the task's
// repo. Each repo runs one agent at a time so two runs never fight over the
// same repo's worktrees, index lock or branches.
var ErrRepoBusy error = &capError{msg: "Can't start run: another run is already working in this repo."}

// Queue wait reasons, shown on the task page.
const (
	WaitSlots = "slots" // every parallel-run slot is taken
	WaitRepo  = "repo"  // another run holds this task's repo
	WaitQuota = "quota" // every provider for this task's pool is quota-locked
)

// WaitFor maps a capacity refusal from Claim to its queue wait reason.
func WaitFor(err error) string {
	if errors.Is(err, ErrRepoBusy) {
		return WaitRepo
	}
	return WaitSlots
}

// QueuedRun is a run refused for capacity that will be re-dispatched when a
// slot (or its repo, or its quota pool) frees up.
type QueuedRun struct {
	TaskID   string    `json:"task_id"`
	RepoKey  string    `json:"repo"`
	Reason   string    `json:"reason"`
	Wait     string    `json:"wait"` // WaitSlots | WaitRepo | WaitQuota
	QueuedAt time.Time `json:"queued_at"`
}

// QueuePosition describes where a task sits in the run queue.
type QueuePosition struct {
	Queued bool   `json:"queued"`
	Ahead  int    `json:"ahead"`
	Wait   string `json:"wait,omitempty"`
}

// RunSlots enforces the global parallel-run cap and the per-repo cap of one,
// and holds the FIFO queue of runs refused by either cap.
type RunSlots struct {
	mu     sync.Mutex
	max    int
	active map[string]string // taskID -> repoKey
	repos  map[string]string // repoKey -> taskID
	queue  []QueuedRun
	// dispatched holds queued tasks Pump has woken whose dispatch has not yet
	// reached Acquire, Enqueue or Dequeue, so back-to-back pumps wake them once.
	dispatched map[string]bool

	// Wake re-dispatches a queued run. Nil uses GlobalDispatcher.Wake.
	Wake func(taskID, reason string)
	// OnChange, when set, is called (outside the lock) after the queue
	// changes, with the current queue. Used to publish SSE updates.
	OnChange func(queue []QueuedRun)
}

// GlobalRunSlots is the process-wide run limiter used by Harness when
// Harness.Slots is nil.
var GlobalRunSlots = NewRunSlots(DefaultMaxConcurrentRuns)

// NewRunSlots returns a limiter allowing max parallel runs (<=0 = default).
func NewRunSlots(max int) *RunSlots {
	if max <= 0 {
		max = DefaultMaxConcurrentRuns
	}
	return &RunSlots{
		max:        max,
		active:     make(map[string]string),
		repos:      make(map[string]string),
		dispatched: make(map[string]bool),
	}
}

// SetMax changes the global cap (<=0 = default). Raising it pumps the queue.
func (s *RunSlots) SetMax(max int) {
	if max <= 0 {
		max = DefaultMaxConcurrentRuns
	}
	s.mu.Lock()
	s.max = max
	s.mu.Unlock()
	s.Pump()
}

// Max returns the current global cap.
func (s *RunSlots) Max() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

// Active returns the number of runs currently holding a slot.
func (s *RunSlots) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// RepoKey normalises a repo path so the same repo always maps to one slot.
func RepoKey(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	p := filepath.Clean(repoPath)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// Acquire takes a slot for taskID in repoKey, or returns ErrRepoBusy /
// ErrConcurrencyCap. A run that is not queued may not jump ahead of queued
// runs: neither one queued for its repo, nor ones waiting for a global slot.
// On success the task leaves the queue.
func (s *RunSlots) Acquire(taskID, repoKey string) error {
	s.mu.Lock()
	delete(s.dispatched, taskID)
	if _, ok := s.active[taskID]; ok {
		s.mu.Unlock()
		return ErrAlreadyClaimed
	}
	queued := s.queuedLocked(taskID)
	if repoKey != "" {
		if _, busy := s.repos[repoKey]; busy {
			s.mu.Unlock()
			return ErrRepoBusy
		}
		if !queued {
			for _, q := range s.queue {
				if q.RepoKey == repoKey {
					s.mu.Unlock()
					return ErrRepoBusy
				}
			}
		}
	}
	limit := s.max
	if !queued {
		// Queued runs that could start now hold their slots against newcomers.
		limit -= s.startableLocked()
	}
	if len(s.active) >= limit {
		s.mu.Unlock()
		return ErrConcurrencyCap
	}
	s.active[taskID] = repoKey
	if repoKey != "" {
		s.repos[repoKey] = taskID
	}
	changed := s.removeLocked(taskID)
	q := s.snapshotLocked()
	s.mu.Unlock()
	if changed {
		s.notify(q)
	}
	return nil
}

// Release frees taskID's slot (no-op when it holds none) and pumps the queue.
func (s *RunSlots) Release(taskID string) {
	s.mu.Lock()
	repoKey, ok := s.active[taskID]
	if ok {
		delete(s.active, taskID)
		if repoKey != "" && s.repos[repoKey] == taskID {
			delete(s.repos, repoKey)
		}
	}
	s.mu.Unlock()
	if ok {
		s.Pump()
	}
}

// Enqueue adds a refused run to the back of the queue. Re-enqueueing a task
// already queued keeps its place (and refreshes its wait reason).
func (s *RunSlots) Enqueue(taskID, repoKey, reason, wait string) QueuePosition {
	s.mu.Lock()
	delete(s.dispatched, taskID)
	found := false
	for i := range s.queue {
		if s.queue[i].TaskID == taskID {
			s.queue[i].Wait = wait
			if repoKey != "" {
				s.queue[i].RepoKey = repoKey
			}
			found = true
			break
		}
	}
	if !found {
		s.queue = append(s.queue, QueuedRun{
			TaskID: taskID, RepoKey: repoKey, Reason: reason, Wait: wait, QueuedAt: time.Now().UTC(),
		})
	}
	pos := s.positionLocked(taskID)
	q := s.snapshotLocked()
	s.mu.Unlock()
	s.notify(q)
	return pos
}

// Dequeue drops taskID from the queue (task deleted, done, or unrunnable).
func (s *RunSlots) Dequeue(taskID string) {
	s.mu.Lock()
	delete(s.dispatched, taskID)
	changed := s.removeLocked(taskID)
	q := s.snapshotLocked()
	s.mu.Unlock()
	if changed {
		s.notify(q)
	}
}

// Position reports whether taskID is queued and how many runs are ahead of it.
func (s *RunSlots) Position(taskID string) QueuePosition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.positionLocked(taskID)
}

// Queue returns a copy of the current queue, oldest first.
func (s *RunSlots) Queue() []QueuedRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Pump re-dispatches every queued run that could start now: its repo is free
// and a global slot is available. Entries stay queued (keeping their place)
// until their Acquire succeeds, so a dispatch that is refused again (for
// example its quota pool is still locked) does not lose its position.
func (s *RunSlots) Pump() {
	s.mu.Lock()
	free := s.max - len(s.active)
	var toWake []QueuedRun
	claimedRepos := make(map[string]bool)
	for _, q := range s.queue {
		if free <= 0 {
			break
		}
		if q.RepoKey != "" {
			if _, busy := s.repos[q.RepoKey]; busy || claimedRepos[q.RepoKey] {
				continue
			}
			claimedRepos[q.RepoKey] = true
		}
		if q.Wait != WaitQuota {
			// A quota-waiting run is only probed: it usually re-queues, so it
			// must not keep a later run from being dispatched in this pump.
			free--
		}
		if s.dispatched[q.TaskID] {
			continue // already woken; its slot is counted, not re-dispatched
		}
		s.dispatched[q.TaskID] = true
		toWake = append(toWake, q)
	}
	wake := s.Wake
	s.mu.Unlock()

	if wake == nil {
		wake = func(taskID, reason string) { GlobalDispatcher.Wake(taskID, reason, "") }
	}
	for _, r := range toWake {
		slog.Info("run queue: dispatching queued run", slog.String("task", r.TaskID), slog.String("repo", r.RepoKey))
		wake(r.TaskID, r.Reason)
	}
}

// startableLocked counts queued runs that could take a slot right now (their
// repo is free; one per repo). Runs waiting on a quota-locked pool are not
// counted: they must not hold slots from runs on other pools.
func (s *RunSlots) startableLocked() int {
	n := 0
	seen := make(map[string]bool)
	for _, q := range s.queue {
		if q.Wait == WaitQuota {
			continue
		}
		if q.RepoKey != "" {
			if _, busy := s.repos[q.RepoKey]; busy || seen[q.RepoKey] {
				continue
			}
			seen[q.RepoKey] = true
		}
		n++
	}
	return n
}

func (s *RunSlots) queuedLocked(taskID string) bool {
	for _, q := range s.queue {
		if q.TaskID == taskID {
			return true
		}
	}
	return false
}

func (s *RunSlots) removeLocked(taskID string) bool {
	for i, q := range s.queue {
		if q.TaskID == taskID {
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			return true
		}
	}
	return false
}

func (s *RunSlots) positionLocked(taskID string) QueuePosition {
	for i, q := range s.queue {
		if q.TaskID == taskID {
			return QueuePosition{Queued: true, Ahead: i, Wait: q.Wait}
		}
	}
	return QueuePosition{}
}

func (s *RunSlots) snapshotLocked() []QueuedRun {
	out := make([]QueuedRun, len(s.queue))
	copy(out, s.queue)
	return out
}

func (s *RunSlots) notify(q []QueuedRun) {
	if s.OnChange != nil {
		s.OnChange(q)
	}
}
