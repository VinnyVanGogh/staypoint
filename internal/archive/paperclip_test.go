//go:build !windows

package archive

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePSQL answers the table listing and per-table SELECTs, and records
// every query and the PGOPTIONS it ran with.
func fakePSQL(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
q=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-c" ]; then q="$2"; shift; fi
  shift
done
printf '%s\t%s\n' "$PGOPTIONS" "$q" >> "` + logf + `"
case "$q" in
  *information_schema.tables*) printf 'issues|8192\nissue_comments|4096\nheartbeat_runs|1000\n' ;;
  *'public."issues"'*) printf '{"id":"STA-1","title":"first"}\n{"id":"STA-2","title":"key ` + fakeAnthropic + `"}\n' ;;
  *'public."issue_comments"'*) printf '{"id":1,"body":"looks good"}\n' ;;
  *'public."heartbeat_runs"'*) echo 'ERROR: boom' >&2; exit 3 ;;
esac
`
	p := filepath.Join(dir, "psql")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, logf
}

func readZst(t *testing.T, path string) string {
	t.Helper()
	rc, err := OpenTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestPaperclipExportReadOnlyRedactedVerified(t *testing.T) {
	psql, logf := fakePSQL(t)
	out := filepath.Join(t.TempDir(), "archive")
	o := PaperclipOptions{PSQL: psql, DSN: DefaultPaperclipDSN, OutDir: out, Exclude: []string{"heartbeat_runs"},
		Now: func() time.Time { return time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC) }}
	m, err := ExportPaperclip(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Complete || len(m.Tables) != 2 || m.Host != "127.0.0.1:54329" || m.Database != "paperclip" {
		t.Fatalf("manifest: %+v", m)
	}
	byName := map[string]PaperclipTable{}
	for _, tb := range m.Tables {
		byName[tb.Name] = tb
	}
	if byName["issues"].Rows != 2 || byName["issue_comments"].Rows != 1 || byName["issues"].SourceBytes != 8192 {
		t.Fatalf("rows: %+v", m.Tables)
	}
	got := readZst(t, filepath.Join(m.Dir, byName["issues"].File))
	if strings.Contains(got, fakeAnthropic) || !strings.Contains(got, "[REDACTED:anthropic-key]") || !strings.Contains(got, `"STA-1"`) {
		t.Fatalf("issues export: %s", got)
	}
	var disk PaperclipManifest
	b, _ := os.ReadFile(filepath.Join(m.Dir, "manifest.json"))
	if json.Unmarshal(b, &disk) != nil || !disk.Complete {
		t.Fatal("manifest.json not written")
	}
	if fi, _ := os.Stat(m.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("export dir mode %v", fi.Mode().Perm())
	}
	calls, _ := os.ReadFile(logf)
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(l, "-c default_transaction_read_only=on\t") {
			t.Fatalf("psql call without read-only transaction: %q", l)
		}
		if strings.Contains(l, "heartbeat_runs\" t") {
			t.Fatal("excluded table was exported")
		}
	}
}

func TestPaperclipExportFailsLoudlyAndKeepsPartialManifest(t *testing.T) {
	psql, _ := fakePSQL(t)
	out := filepath.Join(t.TempDir(), "archive")
	m, err := ExportPaperclip(context.Background(), PaperclipOptions{PSQL: psql, DSN: DefaultPaperclipDSN, OutDir: out})
	if err == nil || !strings.Contains(err.Error(), "heartbeat_runs") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want psql failure surfaced, got %v", err)
	}
	if m == nil || m.Complete {
		t.Fatalf("partial export must not claim complete: %+v", m)
	}
}

func TestPaperclipExportStopsWhenDiskLow(t *testing.T) {
	psql, _ := fakePSQL(t)
	o := PaperclipOptions{PSQL: psql, DSN: DefaultPaperclipDSN, OutDir: filepath.Join(t.TempDir(), "a"),
		freeBytes: func(string) (uint64, error) { return 10 << 20, nil }}
	m, err := ExportPaperclip(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "free on disk") || len(m.Tables) != 0 {
		t.Fatalf("want disk guard, got %v %+v", err, m)
	}
}

func TestPaperclipPasswordNotInArgv(t *testing.T) {
	o := PaperclipOptions{PSQL: "/bin/true", DSN: "postgres://paperclip:s3cret@127.0.0.1:54329/paperclip"}
	cmd := o.psql(context.Background(), "SELECT 1")
	if strings.Contains(strings.Join(cmd.Args, " "), "s3cret") {
		t.Fatalf("password in argv: %v", cmd.Args)
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "PGPASSWORD=s3cret" {
			found = true
		}
	}
	if !found {
		t.Fatal("password not passed via PGPASSWORD")
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b; DROP TABLE x`); got != `"a""b; DROP TABLE x"` {
		t.Fatal(got)
	}
	if got := safeFileName("../x y"); got != "___x_y" {
		t.Fatal(got)
	}
}
