package security

import (
	"errors"
	"path/filepath"
	"sync"
)

// Reading a script exactly once (STA-868 follow-up).
//
// The bytes the classifier judges must be the bytes that run, and where the
// file really is must be decided from the file that was read, not from a
// path looked up again later. ReadScriptOnce resolves the path, opens it with
// O_NOFOLLOW|O_NONBLOCK (a symlink swapped in after resolution fails the open;
// a FIFO cannot block us), checks with fstat that it is a regular file of
// bounded size, asks the kernel where the open file actually lives, and reads
// it through that one descriptor. A Snapshotter caches the result per path so
// one hook run never reads the same script twice.

// ErrNotRegular is returned for anything but a plain file.
var ErrNotRegular = errors.New("not a regular file")

// ErrTooLarge is returned for scripts above maxScriptBytes.
var ErrTooLarge = errors.New("script too large to analyse")

// ReadScriptOnce reads path through a single descriptor. It returns the
// bytes and the real path of the opened file.
func ReadScriptOnce(path string) ([]byte, string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	return readResolved(resolved)
}

// Snapshot is one script read.
type Snapshot struct {
	Data []byte
	Real string // where the opened file actually is
	Err  error
}

// Snapshotter reads each script path at most once and hands every caller in
// the same hook run the same bytes.
type Snapshotter struct {
	mu   sync.Mutex
	m    map[string]Snapshot
	read func(string) ([]byte, string, error)
}

// NewSnapshotter returns a Snapshotter backed by ReadScriptOnce.
func NewSnapshotter() *Snapshotter {
	return &Snapshotter{m: map[string]Snapshot{}, read: ReadScriptOnce}
}

// Read returns the snapshot of abs, reading it on first use.
func (s *Snapshotter) Read(abs string) Snapshot {
	if s == nil {
		return Snapshot{Err: errors.New("script reading disabled")}
	}
	abs = filepath.Clean(abs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.m[abs]; ok {
		return snap
	}
	data, real, err := s.read(abs)
	snap := Snapshot{Data: data, Real: real, Err: err}
	s.m[abs] = snap
	return snap
}
