package orchestrator

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Default parallel-run caps (STA-773, STA-867), used when config.toml does
// not set max_concurrent_runs, max_runs_per_repo or max_runs_per_org.
const (
	DefaultMaxConcurrentRuns = 9 // 3 organizations x 3 runs
	DefaultMaxRunsPerRepo    = 3
	DefaultMaxRunsPerOrg     = 3
)

// UnassignedOrg is the organization bucket for tasks with no organization.
const UnassignedOrg = "Unassigned"

// capError is a refusal that callers should treat as "queue and retry later".
// errors.Is(err, ErrConcurrencyCap) is true for every capError so callers that
// only know the global cap keep working.
type capError struct{ msg string }

func (e *capError) Error() string        { return e.msg }
func (e *capError) Is(target error) bool { return target == ErrConcurrencyCap }

// ErrRepoBusy is returned by Claim when the task's repo already has
// max_runs_per_repo runs, or its plain (non-git) folder already has its one
// run.
var ErrRepoBusy error = &capError{msg: "Can't start run: this repo already has the most parallel runs allowed (max_runs_per_repo)."}

// ErrPlainDirBusy is returned by Claim when another run is working in the
// same non-git folder. A plain folder has no worktrees, so runs in it share
// every file and run one at a time.
var ErrPlainDirBusy error = &capError{msg: "Can't start run: another run is already working in this folder (not a git repo, so one run at a time)."}

// ErrOrgBusy is returned by Claim when the task's organization already has
// max_runs_per_org runs.
var ErrOrgBusy error = &capError{msg: "Can't start run: this organization already has the most parallel runs allowed (max_runs_per_org)."}

// ErrParentBusy is returned by Claim when the task's parent already has
// tasks.max_running_children children running.
var ErrParentBusy error = &capError{msg: "Can't start run: this task's parent already has the most children running at once (tasks.max_running_children)."}

// Queue wait reasons, shown on the task page.
const (
	WaitSlots  = "slots"  // the global cap (max_concurrent_runs) is reached
	WaitRepo   = "repo"   // the task's repo is at max_runs_per_repo
	WaitDir    = "folder" // another run holds the task's plain (non-git) folder
	WaitOrg    = "org"    // the task's organization is at max_runs_per_org
	WaitQuota  = "quota"  // every provider for this task's pool is quota-locked
	WaitParent = "parent" // the task's parent is at tasks.max_running_children
)

// WaitFor maps a capacity refusal from Claim to its queue wait reason.
func WaitFor(err error) string {
	switch {
	case errors.Is(err, ErrPlainDirBusy):
		return WaitDir
	case errors.Is(err, ErrRepoBusy):
		return WaitRepo
	case errors.Is(err, ErrOrgBusy):
		return WaitOrg
	case errors.Is(err, ErrParentBusy):
		return WaitParent
	}
	return WaitSlots
}

// SlotKey says which caps a run counts against.
type SlotKey struct {
	// Dir is the RepoKey of the task's git repo, or of its plain folder when
	// Plain is set. Empty counts against no repo cap.
	Dir string
	// Plain marks a non-git folder: one run at a time in it.
	Plain bool
	// Org is the task's organization; empty is the "Unassigned" bucket.
	Org string
}

// OrgBucket returns the organization bucket org counts against.
func OrgBucket(org string) string {
	if o := strings.TrimSpace(org); o != "" {
		return o
	}
	return UnassignedOrg
}

// RunLimits are the parallel-run caps. Zero or negative values use the
// defaults.
type RunLimits struct {
	Global  int            // max_concurrent_runs
	PerRepo int            // max_runs_per_repo (git repos)
	PerOrg  int            // max_runs_per_org
	Orgs    map[string]int // [run_limits.orgs] per-organization overrides
}

func (l RunLimits) normalized() RunLimits {
	if l.Global <= 0 {
		l.Global = DefaultMaxConcurrentRuns
	}
	if l.PerRepo <= 0 {
		l.PerRepo = DefaultMaxRunsPerRepo
	}
	if l.PerOrg <= 0 {
		l.PerOrg = DefaultMaxRunsPerOrg
	}
	orgs := make(map[string]int, len(l.Orgs))
	for k, v := range l.Orgs {
		if v > 0 {
			orgs[strings.ToLower(OrgBucket(k))] = v
		}
	}
	l.Orgs = orgs
	return l
}

// orgCap returns the cap for an org bucket (overrides match case-insensitively).
func (l RunLimits) orgCap(bucket string) int {
	if v, ok := l.Orgs[strings.ToLower(bucket)]; ok {
		return v
	}
	return l.PerOrg
}

// QueuedRun is a run refused for capacity that will be re-dispatched when a
// slot (or its repo, org or quota pool) frees up.
type QueuedRun struct {
	TaskID   string    `json:"task_id"`
	RepoKey  string    `json:"repo"`
	Plain    bool      `json:"plain,omitempty"`
	Org      string    `json:"org"`
	Reason   string    `json:"reason"`
	Wait     string    `json:"wait"` // WaitSlots | WaitRepo | WaitDir | WaitOrg | WaitQuota
	QueuedAt time.Time `json:"queued_at"`
}

func (q QueuedRun) key() SlotKey { return SlotKey{Dir: q.RepoKey, Plain: q.Plain, Org: q.Org} }

// QueuePosition describes where a task sits in the run queue.
type QueuePosition struct {
	Queued bool   `json:"queued"`
	Ahead  int    `json:"ahead"`
	Wait   string `json:"wait,omitempty"`
}

// usage counts runs per cap.
type usage struct {
	total int
	dirs  map[string]int
	orgs  map[string]int
}

func (u *usage) add(k SlotKey) {
	u.total++
	if k.Dir != "" {
		u.dirs[k.Dir]++
	}
	u.orgs[OrgBucket(k.Org)]++
}

// RunSlots enforces the parallel-run caps (global, per repo, per plain
// folder, per organization) and holds the FIFO queue of runs they refused.
type RunSlots struct {
	mu     sync.Mutex
	limits RunLimits
	active map[string]SlotKey // taskID -> key
	// since is when each active run took its slot (task-3387cad2: the
	// lists' Running badge and its elapsed time).
	since map[string]time.Time
	queue []QueuedRun
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

// NewRunSlots returns a limiter allowing max parallel runs (<=0 = default)
// with the default per-repo and per-org caps.
func NewRunSlots(max int) *RunSlots {
	return NewRunSlotsWithLimits(RunLimits{Global: max})
}

// NewRunSlotsWithLimits returns a limiter with the given caps.
func NewRunSlotsWithLimits(l RunLimits) *RunSlots {
	return &RunSlots{
		limits:     l.normalized(),
		active:     make(map[string]SlotKey),
		since:      make(map[string]time.Time),
		dispatched: make(map[string]bool),
	}
}

// SetMax changes the global cap (<=0 = default). Raising it pumps the queue.
func (s *RunSlots) SetMax(max int) {
	s.mu.Lock()
	l := s.limits
	s.mu.Unlock()
	l.Global = max
	s.SetLimits(l)
}

// SetLimits replaces every cap and pumps the queue.
func (s *RunSlots) SetLimits(l RunLimits) {
	s.mu.Lock()
	s.limits = l.normalized()
	s.mu.Unlock()
	s.Pump()
}

// Max returns the current global cap.
func (s *RunSlots) Max() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits.Global
}

// Limits returns the current caps.
func (s *RunSlots) Limits() RunLimits {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limits
	l.Orgs = make(map[string]int, len(s.limits.Orgs))
	for k, v := range s.limits.Orgs {
		l.Orgs[k] = v
	}
	return l
}

// Active returns the number of runs currently holding a slot.
func (s *RunSlots) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// LiveRuns returns the tasks whose run holds a slot in this process, with
// when it took the slot. This is the daemon's own record of runs in flight:
// a task keeps checkout_run_id after a crash until RecoveryScan clears it,
// but never keeps a slot.
func (s *RunSlots) LiveRuns() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.active))
	for id := range s.active {
		out[id] = s.since[id]
	}
	return out
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

// Acquire takes a slot for taskID, or returns ErrPlainDirBusy, ErrRepoBusy,
// ErrOrgBusy or ErrConcurrencyCap (checked in that order). A run that is not
// queued may not take capacity a queued run could start with now; a queued
// run is checked only against running ones (Pump already dispatches queued
// runs oldest first). On success the task leaves the queue.
func (s *RunSlots) Acquire(taskID string, key SlotKey) error {
	s.mu.Lock()
	delete(s.dispatched, taskID)
	if _, ok := s.active[taskID]; ok {
		s.mu.Unlock()
		return ErrAlreadyClaimed
	}
	var u usage
	if s.queuedLocked(taskID) {
		u = s.activeUsageLocked()
	} else {
		u, _ = s.planLocked(false)
	}
	if err := s.refusalLocked(key, &u); err != nil {
		s.mu.Unlock()
		return err
	}
	s.active[taskID] = key
	s.since[taskID] = time.Now().UTC()
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
	_, ok := s.active[taskID]
	delete(s.active, taskID)
	delete(s.since, taskID)
	s.mu.Unlock()
	if ok {
		s.Pump()
	}
}

// Enqueue adds a refused run to the back of the queue. Re-enqueueing a task
// already queued keeps its place (and refreshes its wait reason and key).
func (s *RunSlots) Enqueue(taskID string, key SlotKey, reason, wait string) QueuePosition {
	s.mu.Lock()
	delete(s.dispatched, taskID)
	found := false
	for i := range s.queue {
		if s.queue[i].TaskID == taskID {
			s.queue[i].Wait = wait
			if key.Dir != "" {
				s.queue[i].RepoKey = key.Dir
				s.queue[i].Plain = key.Plain
			}
			s.queue[i].Org = OrgBucket(key.Org)
			found = true
			break
		}
	}
	if !found {
		s.queue = append(s.queue, QueuedRun{
			TaskID: taskID, RepoKey: key.Dir, Plain: key.Plain, Org: OrgBucket(key.Org),
			Reason: reason, Wait: wait, QueuedAt: time.Now().UTC(),
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

// Pump re-dispatches every queued run that could start now, oldest first.
// A run blocked by its repo, folder or organization cap is skipped, so later
// runs from other repos and organizations still start (one organization at
// its cap never starves the rest); the global cap stops the walk. Entries
// stay queued (keeping their place) until their Acquire succeeds, so a
// dispatch that is refused again (for example its quota pool is still
// locked) does not lose its position.
func (s *RunSlots) Pump() {
	s.mu.Lock()
	_, startable := s.planLocked(true)
	var toWake []QueuedRun
	for _, q := range startable {
		if s.dispatched[q.TaskID] {
			continue // already woken; its capacity is counted, not re-dispatched
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
		slog.Info("run queue: dispatching queued run", slog.String("task", r.TaskID), slog.String("repo", r.RepoKey), slog.String("org", r.Org))
		wake(r.TaskID, r.Reason)
	}
}

// planLocked walks the queue oldest first, assigning capacity to every queued
// run that fits next to the running ones and the queued runs before it. It
// returns that usage (running + planned) and the runs that fit. Runs waiting
// on a quota-locked pool never take planned capacity (they usually re-queue,
// so they must not hold slots from runs on other pools); withQuotaProbes
// still lists the ones that fit, so Pump retries them.
func (s *RunSlots) planLocked(withQuotaProbes bool) (usage, []QueuedRun) {
	u := s.activeUsageLocked()
	var fit []QueuedRun
	for _, q := range s.queue {
		k := q.key()
		err := s.refusalLocked(k, &u)
		if err == nil {
			// A run waiting on its parent's running-children cap is
			// refused by the claim SQL, not by these slot caps, so it
			// is probed like a quota wait instead of reserving capacity.
			if q.Wait == WaitQuota || q.Wait == WaitParent {
				if withQuotaProbes {
					fit = append(fit, q)
				}
				continue
			}
			u.add(k)
			fit = append(fit, q)
			continue
		}
		if !errors.Is(err, ErrOrgBusy) && !errors.Is(err, ErrRepoBusy) && !errors.Is(err, ErrPlainDirBusy) {
			break // global cap: nothing later fits either
		}
	}
	return u, fit
}

func (s *RunSlots) activeUsageLocked() usage {
	u := usage{dirs: make(map[string]int), orgs: make(map[string]int)}
	for _, k := range s.active {
		u.add(k)
	}
	return u
}

// refusalLocked returns the first cap key would exceed given usage u, or nil.
func (s *RunSlots) refusalLocked(k SlotKey, u *usage) error {
	if k.Dir != "" {
		if k.Plain {
			if u.dirs[k.Dir] >= 1 {
				return ErrPlainDirBusy
			}
		} else if u.dirs[k.Dir] >= s.limits.PerRepo {
			return ErrRepoBusy
		}
	}
	bucket := OrgBucket(k.Org)
	if u.orgs[bucket] >= s.limits.orgCap(bucket) {
		return ErrOrgBusy
	}
	if u.total >= s.limits.Global {
		return ErrConcurrencyCap
	}
	return nil
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
