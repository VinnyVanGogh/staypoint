// Package archive keeps a compressed, redacted copy of every agent transcript
// (Claude Code on both profiles, Antigravity, Gemini CLI) under
// ~/.staypoint/archive so history survives the providers' own cleanup
// (Claude Code's cleanupPeriodDays). It only ever reads the originals.
package archive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

// Source is one tree of transcripts to copy.
type Source struct {
	// Name is stable and part of the archive path and index key: claude-personal,
	// claude-work, agy, gemini-cli.
	Name string
	// Profile is the seat the transcripts belong to: personal, work or gemini.
	Profile string
	Root    string
	// Match reports whether a file (path relative to Root, slash separated) is a transcript.
	Match func(rel string) bool
}

// DefaultSources lists the transcript trees on this machine.
func DefaultSources(home string) []Source {
	jsonl := func(rel string) bool { return strings.HasSuffix(rel, ".jsonl") }
	return []Source{
		{Name: "claude-personal", Profile: "personal", Root: filepath.Join(home, ".claude", "projects"), Match: jsonl},
		{Name: "claude-work", Profile: "work", Root: filepath.Join(home, ".claude-work", "projects"), Match: jsonl},
		{Name: "agy", Profile: "gemini", Root: filepath.Join(home, ".gemini", "antigravity-cli", "brain"), Match: func(rel string) bool {
			return strings.HasSuffix(rel, ".jsonl") || strings.HasSuffix(rel, ".json")
		}},
		{Name: "gemini-cli", Profile: "gemini", Root: filepath.Join(home, ".gemini", "tmp"), Match: func(rel string) bool {
			return strings.Contains(rel, "/chats/") && strings.HasSuffix(rel, ".json")
		}},
	}
}

// DefaultDir is the archive root for a StayPoint data dir.
func DefaultDir(dataDir string) string { return filepath.Join(dataDir, "archive") }

// Options configures an Archiver.
type Options struct {
	Dir     string
	Sources []Source
	// ExcludeFromBackup marks Dir excluded from Time Machine (macOS) so client
	// data from work sessions never leaves the Mac through a backup.
	ExcludeFromBackup bool
	// Now is overridable for tests.
	Now func() time.Time
}

// Archiver copies transcripts into Dir.
type Archiver struct {
	opts Options
	db   *sql.DB
}

// Report summarises one archive pass.
type Report struct {
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Scanned     int       `json:"scanned"`
	Archived    int       `json:"archived"`
	Unchanged   int       `json:"unchanged"`
	Failed      int       `json:"failed"`
	SrcBytes    int64     `json:"src_bytes"`
	StoredBytes int64     `json:"stored_bytes"`
	Errors      []string  `json:"errors,omitempty"`
}

const indexSchema = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS transcripts (
    source              TEXT NOT NULL,
    rel_path            TEXT NOT NULL,
    session_id          TEXT NOT NULL DEFAULT '',
    profile             TEXT NOT NULL DEFAULT '',
    repo                TEXT NOT NULL DEFAULT '',
    cwd                 TEXT NOT NULL DEFAULT '',
    task_id             TEXT NOT NULL DEFAULT '',
    started_at          TEXT NOT NULL DEFAULT '',
    ended_at            TEXT NOT NULL DEFAULT '',
    model               TEXT NOT NULL DEFAULT '',
    lines               INTEGER NOT NULL DEFAULT 0,
    user_msgs           INTEGER NOT NULL DEFAULT 0,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0,
    src_size            INTEGER NOT NULL DEFAULT 0,
    src_mtime_ns        INTEGER NOT NULL DEFAULT 0,
    raw_bytes           INTEGER NOT NULL DEFAULT 0,
    stored_bytes        INTEGER NOT NULL DEFAULT 0,
    sha256              TEXT NOT NULL DEFAULT '',
    archive_path        TEXT NOT NULL DEFAULT '',
    archived_at         TEXT NOT NULL DEFAULT '',
    verified            INTEGER NOT NULL DEFAULT 0,
    src_missing         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source, rel_path)
);
CREATE INDEX IF NOT EXISTS idx_transcripts_session ON transcripts (session_id);
CREATE INDEX IF NOT EXISTS idx_transcripts_started ON transcripts (started_at);
CREATE INDEX IF NOT EXISTS idx_transcripts_task ON transcripts (task_id) WHERE task_id != '';
CREATE TABLE IF NOT EXISTS archive_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at   TEXT NOT NULL,
    finished_at  TEXT NOT NULL,
    scanned      INTEGER NOT NULL,
    archived     INTEGER NOT NULL,
    unchanged    INTEGER NOT NULL,
    failed       INTEGER NOT NULL,
    src_bytes    INTEGER NOT NULL,
    stored_bytes INTEGER NOT NULL,
    errors       TEXT NOT NULL DEFAULT ''
);
`

// IndexPath is the index database inside an archive dir.
func IndexPath(dir string) string { return filepath.Join(dir, "index.db") }

// Open creates the archive dir (0700) and opens its index.
func Open(opts Options) (*Archiver, error) {
	if opts.Dir == "" {
		return nil, errors.New("archive: Dir is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := ensurePrivateDir(opts.Dir); err != nil {
		return nil, err
	}
	conn, err := OpenIndex(opts.Dir)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(indexSchema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("archive: index schema: %w", err)
	}
	_ = os.Chmod(IndexPath(opts.Dir), 0o600)
	if opts.ExcludeFromBackup {
		excludeFromBackup(opts.Dir)
	}
	return &Archiver{opts: opts, db: conn}, nil
}

// OpenIndex opens the index database read-write without touching the schema.
func OpenIndex(dir string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", "file:"+IndexPath(dir)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("archive: open index: %w", err)
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

// OpenIndexReadOnly opens an existing index read-only (for reflect).
func OpenIndexReadOnly(dir string) (*sql.DB, error) {
	if _, err := os.Stat(IndexPath(dir)); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", "file:"+IndexPath(dir)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

// Close closes the index.
func (a *Archiver) Close() error { return a.db.Close() }

// DB exposes the index (tests, stats).
func (a *Archiver) DB() *sql.DB { return a.db }

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", dir, err)
	}
	// MkdirAll leaves an existing dir's mode alone.
	return os.Chmod(dir, 0o700)
}

// excludeFromBackup is a variable so tests never shell out.
var excludeFromBackup = func(dir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	// Sticky exclusion: travels with the folder, needs no root.
	_ = exec.Command("tmutil", "addexclusion", dir).Run()
}

// Run archives every new or changed transcript. Safe to call concurrently
// from several processes: a file lock serialises passes.
func (a *Archiver) Run(ctx context.Context) (*Report, error) {
	unlock, err := lockDir(a.opts.Dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rep := &Report{StartedAt: a.opts.Now().UTC()}
	for _, src := range a.opts.Sources {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		a.runSource(ctx, src, rep)
	}
	rep.FinishedAt = a.opts.Now().UTC()
	_, err = a.db.Exec(`INSERT INTO archive_runs (started_at, finished_at, scanned, archived, unchanged, failed, src_bytes, stored_bytes, errors)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rep.StartedAt.Format(time.RFC3339), rep.FinishedAt.Format(time.RFC3339),
		rep.Scanned, rep.Archived, rep.Unchanged, rep.Failed, rep.SrcBytes, rep.StoredBytes,
		strings.Join(firstN(rep.Errors, 20), "\n"))
	return rep, err
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (a *Archiver) runSource(ctx context.Context, src Source, rep *Report) {
	if fi, err := os.Stat(src.Root); err != nil || !fi.IsDir() {
		return
	}
	seen := map[string]bool{}
	_ = filepath.WalkDir(src.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtree: note it and keep going.
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(src.Root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !src.Match(rel) {
			return nil
		}
		seen[rel] = true
		rep.Scanned++
		switch changed, err := a.archiveFile(src, rel, path, rep); {
		case err != nil:
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s/%s: %v", src.Name, rel, err))
		case changed:
			rep.Archived++
		default:
			rep.Unchanged++
		}
		return nil
	})
	a.markMissing(src.Name, seen)
}

// markMissing flags index rows whose original is gone (e.g. deleted by
// cleanupPeriodDays) so status can show what the archive alone now holds.
func (a *Archiver) markMissing(source string, seen map[string]bool) {
	rows, err := a.db.Query(`SELECT rel_path, src_missing FROM transcripts WHERE source = ?`, source)
	if err != nil {
		return
	}
	type flip struct {
		rel     string
		missing int
	}
	var flips []flip
	for rows.Next() {
		var rel string
		var missing int
		if rows.Scan(&rel, &missing) != nil {
			continue
		}
		want := 0
		if !seen[rel] {
			want = 1
		}
		if want != missing {
			flips = append(flips, flip{rel, want})
		}
	}
	rows.Close()
	for _, f := range flips {
		_, _ = a.db.Exec(`UPDATE transcripts SET src_missing = ? WHERE source = ? AND rel_path = ?`, f.missing, source, f.rel)
	}
}

// archivePath maps a transcript to its compressed copy inside Dir.
func (a *Archiver) archivePath(source, rel string) string {
	clean := filepath.Clean("/" + rel) // strips any ../
	return filepath.Join(a.opts.Dir, "transcripts", source, filepath.FromSlash(clean)+".zst")
}

func (a *Archiver) archiveFile(src Source, rel, path string, rep *Report) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()
	var oldSize, oldMtime int64
	var verified int
	err = a.db.QueryRow(`SELECT src_size, src_mtime_ns, verified FROM transcripts WHERE source = ? AND rel_path = ?`,
		src.Name, rel).Scan(&oldSize, &oldMtime, &verified)
	dest := a.archivePath(src.Name, rel)
	if err == nil && oldSize == size && oldMtime == mtime && verified == 1 {
		if _, serr := os.Stat(dest); serr == nil {
			return false, nil
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	if err := ensurePrivateDirs(a.opts.Dir, filepath.Dir(dest)); err != nil {
		return false, err
	}
	meta, sum, rawBytes, err := writeCompressed(path, size, dest)
	if err != nil {
		return false, err
	}
	stored, err := os.Stat(dest)
	if err != nil {
		return false, err
	}
	meta.finish(src, rel)
	if meta.EndedAt == "" {
		// Formats without per-line timestamps (Gemini JSON): the file's mtime.
		ts := fi.ModTime().UTC().Format(time.RFC3339)
		meta.StartedAt, meta.EndedAt = ts, ts
	}
	_, err = a.db.Exec(`INSERT INTO transcripts (
			source, rel_path, session_id, profile, repo, cwd, task_id, started_at, ended_at, model,
			lines, user_msgs, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
			src_size, src_mtime_ns, raw_bytes, stored_bytes, sha256, archive_path, archived_at, verified, src_missing)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0)
		ON CONFLICT (source, rel_path) DO UPDATE SET
			session_id = excluded.session_id, profile = excluded.profile, repo = excluded.repo, cwd = excluded.cwd,
			task_id = excluded.task_id, started_at = excluded.started_at, ended_at = excluded.ended_at,
			model = excluded.model, lines = excluded.lines, user_msgs = excluded.user_msgs,
			input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens, cache_create_tokens = excluded.cache_create_tokens,
			src_size = excluded.src_size, src_mtime_ns = excluded.src_mtime_ns, raw_bytes = excluded.raw_bytes,
			stored_bytes = excluded.stored_bytes, sha256 = excluded.sha256, archive_path = excluded.archive_path,
			archived_at = excluded.archived_at, verified = 1, src_missing = 0`,
		src.Name, rel, meta.SessionID, src.Profile, meta.Repo, meta.CWD, meta.TaskID, meta.StartedAt, meta.EndedAt, meta.Model,
		meta.Lines, meta.UserMsgs, meta.Input, meta.Output, meta.CacheRead, meta.CacheCreate,
		size, mtime, rawBytes, stored.Size(), sum, dest, a.opts.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rep.SrcBytes += size
	rep.StoredBytes += stored.Size()
	return true, nil
}

// ensurePrivateDirs makes every dir from root down to leaf exist with mode 0700.
func ensurePrivateDirs(root, leaf string) error {
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		return err
	}
	for d := leaf; strings.HasPrefix(d, root) && d != root; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// writeCompressed streams the first size bytes of path through secret
// redaction into a zstd file at dest. It writes a temp file, re-reads it to
// check the checksum, and only then renames it over dest, so a crash never
// leaves a truncated archive in place of a good one.
func writeCompressed(path string, size int64, dest string) (*Meta, string, int64, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, "", 0, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*.zst")
	if err != nil {
		return nil, "", 0, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, "", 0, err
	}

	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, "", 0, err
	}
	h := sha256.New()
	cw := &countWriter{}
	red := security.NewWriter(io.MultiWriter(enc, h, cw))
	meta := &Meta{}

	r := bufio.NewReaderSize(io.LimitReader(in, size), 256<<10)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			meta.observe(bytes.TrimRight(line, "\r\n"))
			if _, err := red.Write(line); err != nil {
				return nil, "", 0, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, "", 0, rerr
		}
	}
	if err := red.Close(); err != nil {
		return nil, "", 0, err
	}
	if err := enc.Close(); err != nil {
		return nil, "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return nil, "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	got, n, err := checksumCompressed(tmpName)
	if err != nil {
		return nil, "", 0, fmt.Errorf("verify: %w", err)
	}
	if got != sum || n != cw.n {
		return nil, "", 0, fmt.Errorf("verify: checksum mismatch after compression")
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return nil, "", 0, err
	}
	ok = true
	return meta, sum, cw.n, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func checksumCompressed(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return "", 0, err
	}
	defer dec.Close()
	h := sha256.New()
	n, err := io.Copy(h, dec)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// OpenTranscript returns a reader over an archived transcript's redacted text.
func OpenTranscript(archivePath string) (io.ReadCloser, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &decReader{dec: dec, f: f}, nil
}

type decReader struct {
	dec *zstd.Decoder
	f   *os.File
}

func (d *decReader) Read(p []byte) (int, error) { return d.dec.Read(p) }
func (d *decReader) Close() error               { d.dec.Close(); return d.f.Close() }
