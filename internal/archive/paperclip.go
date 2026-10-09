package archive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/klauspost/compress/zstd"
)

// DefaultPaperclipDSN is Paperclip's embedded Postgres (port 54329,
// user/password/db "paperclip"); override with --dsn or PAPERCLIP_DATABASE_URL.
const DefaultPaperclipDSN = "postgres://paperclip:paperclip@127.0.0.1:54329/paperclip"

// PaperclipOptions configures a one-time export.
type PaperclipOptions struct {
	PSQL    string // psql binary
	DSN     string
	OutDir  string // export lands in OutDir/paperclip/<timestamp>
	Tables  []string
	Exclude []string
	// MinFreeBytes aborts before a table when the disk has less free (default 1 GiB).
	MinFreeBytes uint64
	Now          func() time.Time
	// freeBytes is overridable for tests.
	freeBytes func(dir string) (uint64, error)
}

// PaperclipTable is one exported table.
type PaperclipTable struct {
	Name        string `json:"name"`
	Rows        int64  `json:"rows"`
	SourceBytes int64  `json:"source_bytes"` // pg_total_relation_size
	JSONBytes   int64  `json:"json_bytes"`
	StoredBytes int64  `json:"stored_bytes"`
	File        string `json:"file"`
	SHA256      string `json:"sha256"`
}

// PaperclipManifest describes an export; written as manifest.json beside the tables.
type PaperclipManifest struct {
	ExportedAt string           `json:"exported_at"`
	Host       string           `json:"host"`
	Database   string           `json:"database"`
	Dir        string           `json:"dir"`
	Tables     []PaperclipTable `json:"tables"`
	Complete   bool             `json:"complete"`
}

// FindPSQL locates psql: PATH first, then Paperclip's embedded Postgres under ~/.paperclip.
func FindPSQL(home string) (string, error) {
	if p, err := exec.LookPath("psql"); err == nil {
		return p, nil
	}
	for _, pat := range []string{
		filepath.Join(home, ".paperclip", "*", "bin", "psql"),
		filepath.Join(home, ".paperclip", "*", "*", "bin", "psql"),
		filepath.Join(home, ".paperclip", "*", "*", "*", "bin", "psql"),
		filepath.Join(home, ".npm", "_npx", "*", "node_modules", "@embedded-postgres", "*", "native", "bin", "psql"),
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0], nil
		}
	}
	return "", errors.New("psql not found on PATH or under ~/.paperclip; pass --psql")
}

func (o *PaperclipOptions) psql(ctx context.Context, query string) *exec.Cmd {
	// -X: no ~/.psqlrc; -A -t: one bare value per line; FETCH_COUNT streams via a cursor.
	cmd := exec.CommandContext(ctx, o.PSQL, "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-v", "FETCH_COUNT=2000", "-d", o.DSN, "-c", query)
	// Every statement runs in a read-only transaction: the export cannot write to Paperclip.
	cmd.Env = append(security.ChildEnv("PGPASSWORD"), "PGOPTIONS=-c default_transaction_read_only=on")
	return cmd
}

func (o *PaperclipOptions) query(ctx context.Context, q string) ([]string, error) {
	cmd := o.psql(ctx, q)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("psql: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// quoteIdent double-quotes a Postgres identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// PlanPaperclip lists the public tables to export with their on-disk size.
func PlanPaperclip(ctx context.Context, o PaperclipOptions) ([]PaperclipTable, error) {
	lines, err := o.query(ctx, `SELECT table_name || '|' || pg_total_relation_size(format('%I.%I', table_schema, table_name)::regclass) `+
		`FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	want := setOf(o.Tables)
	skip := setOf(o.Exclude)
	var out []PaperclipTable
	for _, l := range lines {
		name, size, _ := strings.Cut(l, "|")
		if (len(want) > 0 && !want[name]) || skip[name] {
			continue
		}
		n, _ := strconv.ParseInt(size, 10, 64)
		out = append(out, PaperclipTable{Name: name, SourceBytes: n})
	}
	return out, nil
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			m[x] = true
		}
	}
	return m
}

// ExportPaperclip writes every planned table as redacted JSONL.zst plus a
// manifest. It is read-only on Paperclip and never deletes anything.
func ExportPaperclip(ctx context.Context, o PaperclipOptions) (*PaperclipManifest, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MinFreeBytes == 0 {
		o.MinFreeBytes = 1 << 30
	}
	if o.freeBytes == nil {
		o.freeBytes = diskFree
	}
	tables, err := PlanPaperclip(ctx, o)
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, errors.New("paperclip export: no tables matched")
	}
	stamp := o.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(o.OutDir, "paperclip", stamp)
	if err := ensurePrivateDir(o.OutDir); err != nil {
		return nil, err
	}
	if err := ensurePrivateDirs(o.OutDir, dir); err != nil {
		return nil, err
	}
	m := &PaperclipManifest{ExportedAt: o.Now().UTC().Format(time.RFC3339), Dir: dir}
	if u, err := url.Parse(o.DSN); err == nil {
		m.Host, m.Database = u.Host, strings.TrimPrefix(u.Path, "/")
	}
	for _, t := range tables {
		if free, err := o.freeBytes(dir); err == nil && free < o.MinFreeBytes {
			_ = writeManifest(dir, m)
			return m, fmt.Errorf("paperclip export: stopped before %s, only %d MiB free on disk", t.Name, free>>20)
		}
		if err := o.exportTable(ctx, dir, &t); err != nil {
			_ = writeManifest(dir, m)
			return m, fmt.Errorf("paperclip export %s: %w", t.Name, err)
		}
		m.Tables = append(m.Tables, t)
	}
	m.Complete = true
	return m, writeManifest(dir, m)
}

func (o *PaperclipOptions) exportTable(ctx context.Context, dir string, t *PaperclipTable) error {
	cmd := o.psql(ctx, "SELECT row_to_json(t)::text FROM public."+quoteIdent(t.Name)+" t")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	t.File = safeFileName(t.Name) + ".jsonl.zst"
	dest := filepath.Join(dir, t.File)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		f.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	h := sha256.New()
	cw := &countWriter{}
	red := security.NewWriter(io.MultiWriter(enc, h, cw))
	r := bufio.NewReaderSize(stdout, 1<<20)
	var copyErr error
	for {
		line, rerr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			t.Rows++
			if _, err := red.Write(line); err != nil {
				copyErr = err
				break
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			copyErr = rerr
			break
		}
	}
	waitErr := cmd.Wait()
	for _, err := range []error{red.Close(), enc.Close(), f.Close()} {
		if copyErr == nil {
			copyErr = err
		}
	}
	if waitErr != nil {
		return fmt.Errorf("psql: %w: %s", waitErr, strings.TrimSpace(errb.String()))
	}
	if copyErr != nil {
		return copyErr
	}
	got, n, err := checksumCompressed(dest)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	t.SHA256 = hex.EncodeToString(h.Sum(nil))
	if got != t.SHA256 || n != cw.n {
		return errors.New("verify: checksum mismatch after compression")
	}
	t.JSONBytes = cw.n
	if fi, err := os.Stat(dest); err == nil {
		t.StoredBytes = fi.Size()
	}
	return nil
}

func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeManifest(dir string, m *PaperclipManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o600)
}
