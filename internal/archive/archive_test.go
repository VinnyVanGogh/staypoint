package archive

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { excludeFromBackup = func(string) {} }

// Fake secrets assembled at runtime so the repo's own secret scanners stay quiet.
var (
	fakeAnthropic = "sk-ant-" + strings.Repeat("a1B2", 8)
	fakeGitHub    = "ghp_" + strings.Repeat("Z9", 20)
	fakePEM       = "-----BEGIN RSA PRIVATE KEY-----\nMIIEow" + strings.Repeat("Q", 40) + "\n-----END RSA PRIVATE KEY-----"
)

func claudeLine(ts, msgID, text string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","cwd":"/r/agent-mesh/.worktrees/task-0123abcd","sessionId":"s1","requestId":"req-` + msgID +
		`","message":{"id":"` + msgID + `","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"` + text +
		`"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}}`
}

type fixture struct {
	root, dir string
	src       Source
	file      string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{root: filepath.Join(base, "projects"), dir: filepath.Join(base, "archive")}
	f.src = DefaultSources(base)[0]
	f.src.Root = f.root
	f.file = filepath.Join(f.root, "-r-agent-mesh", "s1.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-10-01T10:00:00.123Z","cwd":"/r/agent-mesh/.worktrees/task-0123abcd","sessionId":"s1","message":{"role":"user","content":"please use ` + fakeAnthropic + ` here"}}`,
		claudeLine("2026-10-01T10:01:00Z", "m1", "token "+fakeGitHub),
		// Same message id again (second content block): usage must count once.
		claudeLine("2026-10-01T10:01:01Z", "m1", "more"),
		`{"type":"user","timestamp":"2026-10-01T10:02:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		fakePEM,
		claudeLine("2026-10-01T11:00:00Z", "m2", "done"),
	}
	writeFile(t, f.file, strings.Join(lines, "\n")+"\n")
	return f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) open(t *testing.T) *Archiver {
	t.Helper()
	a, err := Open(Options{Dir: f.dir, Sources: []Source{f.src}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func readArchived(t *testing.T, a *Archiver, source, rel string) string {
	t.Helper()
	rc, err := OpenTranscript(a.archivePath(source, rel))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestArchiveRedactsSecrets(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := readArchived(t, a, f.src.Name, "-r-agent-mesh/s1.jsonl")
	for _, secret := range []string{fakeAnthropic, fakeGitHub, "MIIEow", "BEGIN RSA PRIVATE KEY"} {
		if strings.Contains(got, secret) {
			t.Fatalf("archive still contains secret %q", secret)
		}
	}
	for _, marker := range []string{"[REDACTED:anthropic-key]", "[REDACTED:github-token]", "[REDACTED:pem-private-key]"} {
		if !strings.Contains(got, marker) {
			t.Fatalf("archive missing %s", marker)
		}
	}
	if !strings.Contains(got, `"text":"done"`) {
		t.Fatal("archive lost ordinary content")
	}
	// The stored copy must also be unreadable as plain text on disk.
	raw, _ := os.ReadFile(a.archivePath(f.src.Name, "-r-agent-mesh/s1.jsonl"))
	if len(raw) < 4 || string(raw[:4]) != "\x28\xb5\x2f\xfd" {
		t.Fatal("archive file is not zstd")
	}
}

func TestArchiveIdempotentAndNeverTouchesOriginals(t *testing.T) {
	f := newFixture(t)
	before := fileSum(t, f.file)
	a := f.open(t)
	r1, err := a.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r1.Archived != 1 || r1.Failed != 0 {
		t.Fatalf("first run: %+v", r1)
	}
	dest := a.archivePath(f.src.Name, "-r-agent-mesh/s1.jsonl")
	st1, _ := os.Stat(dest)

	r2, err := a.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r2.Archived != 0 || r2.Unchanged != 1 {
		t.Fatalf("second run should be a no-op: %+v", r2)
	}
	st2, _ := os.Stat(dest)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("unchanged transcript was rewritten")
	}
	var n int
	a.DB().QueryRow(`SELECT COUNT(*) FROM transcripts`).Scan(&n)
	if n != 1 {
		t.Fatalf("index rows = %d, want 1", n)
	}
	if fileSum(t, f.file) != before {
		t.Fatal("original transcript was modified")
	}

	// Append: re-archived with the new line.
	fh, _ := os.OpenFile(f.file, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(claudeLine("2026-10-02T09:00:00Z", "m3", "appended") + "\n")
	fh.Close()
	os.Chtimes(f.file, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	r3, _ := a.Run(context.Background())
	if r3.Archived != 1 {
		t.Fatalf("changed transcript not re-archived: %+v", r3)
	}
	if !strings.Contains(readArchived(t, a, f.src.Name, "-r-agent-mesh/s1.jsonl"), "appended") {
		t.Fatal("re-archive missing appended line")
	}
	var ended string
	a.DB().QueryRow(`SELECT ended_at FROM transcripts`).Scan(&ended)
	if ended != "2026-10-02T09:00:00Z" {
		t.Fatalf("ended_at = %q", ended)
	}
}

func TestArchiveKeepsCopyAfterOriginalDeleted(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	a.Run(context.Background())
	if err := os.Remove(f.file); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readArchived(t, a, f.src.Name, "-r-agent-mesh/s1.jsonl"), "done") {
		t.Fatal("archive lost the copy when the original was deleted")
	}
	st, err := ComputeStats(a.DB())
	if err != nil {
		t.Fatal(err)
	}
	if st.Transcripts != 1 || st.Sources[0].OnlyInArchive != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestArchiveIndexMetadata(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	a.Run(context.Background())
	var sid, profile, repo, task, start, end, model string
	var in, out, cr, cc, users int64
	err := a.DB().QueryRow(`SELECT session_id, profile, repo, task_id, started_at, ended_at, model,
		input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, user_msgs FROM transcripts`).
		Scan(&sid, &profile, &repo, &task, &start, &end, &model, &in, &out, &cr, &cc, &users)
	if err != nil {
		t.Fatal(err)
	}
	if sid != "s1" || profile != "personal" || repo != "/r/agent-mesh" || task != "task-0123abcd" {
		t.Fatalf("identity: %s %s %s %s", sid, profile, repo, task)
	}
	if start != "2026-10-01T10:00:00Z" || end != "2026-10-01T11:00:00Z" || model != "claude-opus-5-5" {
		t.Fatalf("times/model: %s %s %s", start, end, model)
	}
	// m1 (twice) counted once + m2.
	if in != 20 || out != 10 || cr != 200 || cc != 14 {
		t.Fatalf("tokens: in=%d out=%d cr=%d cc=%d", in, out, cr, cc)
	}
	if users != 1 {
		t.Fatalf("user_msgs = %d, want 1 (tool results are not typed text)", users)
	}
}

func TestArchivePermissionsArePrivate(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.dir, 0o755) // pre-existing, too open
	a := f.open(t)
	a.Run(context.Background())
	for _, d := range []string{f.dir, filepath.Join(f.dir, "transcripts"), filepath.Dir(a.archivePath(f.src.Name, "-r-agent-mesh/s1.jsonl"))} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v, want 0700", d, fi.Mode().Perm())
		}
	}
	for _, p := range []string{a.archivePath(f.src.Name, "-r-agent-mesh/s1.jsonl"), IndexPath(f.dir)} {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
}

func TestArchivePathCannotEscape(t *testing.T) {
	a := &Archiver{opts: Options{Dir: "/arch"}}
	got := a.archivePath("claude-personal", "../../../etc/passwd")
	if !strings.HasPrefix(got, "/arch/transcripts/claude-personal/") {
		t.Fatalf("escaped: %s", got)
	}
}

func TestArchiveConcurrentRunsSerialise(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(f.root, "p", "x"+string(rune('a'+i))+".jsonl"), claudeLine("2026-10-03T00:00:00Z", "q", "x")+"\n")
	}
	a1 := f.open(t)
	a2 := f.open(t)
	var wg sync.WaitGroup
	reps := make([]*Report, 2)
	for i, a := range []*Archiver{a1, a2} {
		wg.Add(1)
		go func(i int, a *Archiver) {
			defer wg.Done()
			r, err := a.Run(context.Background())
			if err != nil {
				t.Error(err)
			}
			reps[i] = r
		}(i, a)
	}
	wg.Wait()
	if reps[0].Failed+reps[1].Failed != 0 {
		t.Fatalf("failures: %+v %+v", reps[0], reps[1])
	}
	if reps[0].Archived+reps[1].Archived != 6 {
		t.Fatalf("each file archived exactly once overall: %d + %d", reps[0].Archived, reps[1].Archived)
	}
}

func TestStatsProjection(t *testing.T) {
	days, gb := project("2026-10-01T00:00:00Z", "2026-10-11T00:00:00Z", 1e9)
	if days != 10 || gb < 36.4 || gb > 36.6 {
		t.Fatalf("days=%v gb=%v", days, gb)
	}
}

func TestNextNightly(t *testing.T) {
	loc := time.FixedZone("x", -5*3600)
	if got := NextNightly(time.Date(2026, 10, 9, 2, 59, 0, 0, loc)); got.Day() != 9 || got.Hour() != 3 {
		t.Fatalf("before 3am: %v", got)
	}
	if got := NextNightly(time.Date(2026, 10, 9, 3, 0, 0, 0, loc)); got.Day() != 10 {
		t.Fatalf("at 3am: %v", got)
	}
}

func TestSessionFromPath(t *testing.T) {
	cases := map[[2]string]string{
		{"claude-personal", "proj/abc.jsonl"}:                   "abc",
		{"claude-personal", "proj/abc/subagents/agent-1.jsonl"}: "abc",
		{"agy", "conv-9/transcript.jsonl"}:                      "conv-9",
		{"gemini-cli", "h/chats/session-1.json"}:                "session-1",
	}
	for in, want := range cases {
		if got := sessionFromPath(in[0], in[1]); got != want {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}
