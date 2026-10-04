// Package shipreview — built-in Supabase local dev env support.
// Handles Docker auto-start, port-conflict resolution, DB-state detection,
// and idle spin-down so every review of a Supabase project runs against a
// fresh local DB and never touches production.
package shipreview

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// HasSupabaseConfig returns true when repoPath contains supabase/config.toml.
func HasSupabaseConfig(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, "supabase", "config.toml"))
	return err == nil
}

// HasEdgeRuntime returns true when repoPath/supabase/functions exists and is
// non-empty. When false, supabase must be started with -x edge-runtime.
func HasEdgeRuntime(repoPath string) bool {
	entries, err := os.ReadDir(filepath.Join(repoPath, "supabase", "functions"))
	return err == nil && len(entries) > 0
}

// supabaseProjectID reads the project_id from supabase/config.toml.
// Returns "" when the file cannot be parsed.
func supabaseProjectID(repoPath string) string {
	type supabaseCfg struct {
		ProjectID string `toml:"project_id"`
	}
	cfgPath := filepath.Join(repoPath, "supabase", "config.toml")
	data, err := os.ReadFile(cfgPath) //nolint:gosec
	if err != nil {
		return ""
	}
	var cfg supabaseCfg
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.ProjectID
}

// ProposeSupabaseDevConfig returns a pre-filled ProjectDevConfig for a
// Supabase project. Callers should upsert this if no config exists yet.
func ProposeSupabaseDevConfig(repoPath string) *ProjectDevConfig {
	devCmd := "bun run dev"
	if _, err := os.Stat(filepath.Join(repoPath, "package.json")); err != nil {
		devCmd = ""
	}
	return &ProjectDevConfig{
		RepoPath:        repoPath,
		DevCommand:      devCmd,
		DevURL:          "http://127.0.0.1:5173",
		SupabaseEnabled: true,
	}
}

// supabasePorts are the default local Supabase port range (API, DB, Studio, …).
var supabasePorts = []string{"54321", "54322", "54323", "54324"}

// supabaseServices is the canonical list of Supabase CLI service names used in
// container names (supabase_<service>_<project>). Multi-word names are joined
// with underscores by the CLI.
var supabaseServices = map[string]bool{
	"db": true, "kong": true, "auth": true, "rest": true, "realtime": true,
	"storage": true, "imgproxy": true, "pg_meta": true, "studio": true,
	"inbucket": true, "mailpit": true, "edge_runtime": true,
	"analytics": true, "vector": true, "pooler": true,
}

// SupabaseProgress describes a single progress update emitted during setup.
type SupabaseProgress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

// StartSupabaseDevEnv sets up the full local Supabase stack for a review
// worktree. It emits progress messages via the report callback.
//
// Steps executed:
//  1. Docker: wait up to 3 min if not running.
//  2. Port conflicts: stop any other project's containers holding 54321–54324.
//  3. Copy .env.local from mainRepoPath; refuse if missing or if any Supabase
//     URL key points outside 127.0.0.1/localhost.
//  4. supabase start (with -x edge-runtime when no functions dir).
//  5. DB state: fresh → db reset --local + stop/start; existing → migration up.
func StartSupabaseDevEnv(mainRepoPath, wtPath string, report func(SupabaseProgress)) error {
	prog := func(step, msg string, ok bool) {
		if report != nil {
			report(SupabaseProgress{Step: step, Message: msg, OK: ok})
		}
	}

	// 1. Docker.
	prog("docker", "Checking Docker…", true)
	if err := ensureDocker(prog); err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	// 2. Port conflicts.
	prog("ports", "Checking port conflicts (54321–54324)…", true)
	if err := resolvePortConflicts(mainRepoPath, prog); err != nil {
		return fmt.Errorf("port conflicts: %w", err)
	}

	// 3. .env.local safety check (must exist and validate for Supabase projects).
	prog("env", "Validating .env.local…", true)
	if err := CopyAndValidateEnv(mainRepoPath, wtPath, prog); err != nil {
		return fmt.Errorf("env: %w", err)
	}

	// 4. supabase start.
	prog("db_start", "Starting local Supabase DB…", true)
	if err := supabaseStart(wtPath, prog); err != nil {
		return fmt.Errorf("supabase start: %w", err)
	}

	// 5. Apply migrations.
	prog("migrations", "Checking DB state…", true)
	if err := applyMigrationsOrReset(wtPath, prog); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	prog("ready", "Local Supabase DB is ready", true)
	return nil
}

// StopSupabaseDevEnv runs supabase stop (data volumes kept) in wtPath.
func StopSupabaseDevEnv(wtPath string) error {
	cmd := exec.Command("supabase", "stop") //nolint:gosec
	cmd.Dir = wtPath
	cmd.Env = append(os.Environ(), "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("supabase stop: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureDocker opens Docker Desktop if not running and waits up to 3 min.
func ensureDocker(prog func(step, msg string, ok bool)) error {
	if dockerRunning() {
		prog("docker", "Docker is running", true)
		return nil
	}
	prog("docker", "Docker not running — opening Docker Desktop…", true)
	_ = exec.Command("open", "-ga", "Docker").Run() //nolint:gosec

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if dockerRunning() {
			prog("docker", "Docker started", true)
			return nil
		}
	}
	prog("docker", "Docker did not start within 3 minutes", false)
	return fmt.Errorf("docker did not start within 3 minutes")
}

func dockerRunning() bool {
	err := exec.Command("docker", "info").Run() //nolint:gosec
	return err == nil
}

// resolvePortConflicts stops any *other* project's Supabase containers that
// hold the default Supabase ports. mainRepoPath is used to read this project's
// project_id from supabase/config.toml so we never stop our own stack.
func resolvePortConflicts(mainRepoPath string, prog func(step, msg string, ok bool)) error {
	// Read the current project's id from config.toml so we can skip its containers.
	currentProjectID := supabaseProjectID(mainRepoPath)

	for _, port := range supabasePorts {
		holder, err := containerHoldingPort(port)
		if err != nil || holder == "" {
			continue
		}

		// First try Docker label — authoritative, works for any naming scheme.
		holderProject := containerProjectFromLabel(holder)
		if holderProject == "" {
			// Fall back to name-based extraction.
			holderProject = ExtractSupabaseProject(holder)
		}
		if holderProject == "" {
			prog("ports", fmt.Sprintf("port %s held by %s (not a Supabase container — skipping)", port, holder), true)
			continue
		}

		// Skip if it's our own project.
		if currentProjectID != "" && holderProject == currentProjectID {
			prog("ports", fmt.Sprintf("port %s held by own project %s — ok", port, holderProject), true)
			continue
		}

		prog("ports", fmt.Sprintf("port %s held by %s (project %s) — stopping (volumes kept)", port, holder, holderProject), true)
		if err := stopProjectContainers(holderProject); err != nil {
			prog("ports", fmt.Sprintf("warning: could not stop %s: %v", holderProject, err), false)
		}
	}
	return nil
}

// containerHoldingPort returns the first container name publishing the given port.
func containerHoldingPort(port string) (string, error) {
	out, err := exec.Command("docker", "ps", "--filter", "publish="+port, "--format", "{{.Names}}").Output() //nolint:gosec
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()), nil
	}
	return "", nil
}

// containerProjectFromLabel reads the com.supabase.cli.project label from a
// running container. Returns "" on any error or when the label is absent.
func containerProjectFromLabel(containerName string) string {
	out, err := exec.Command( //nolint:gosec
		"docker", "inspect",
		"--format", `{{index .Config.Labels "com.supabase.cli.project"}}`,
		containerName,
	).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ExtractSupabaseProject extracts the project name from a Supabase container
// name of the form supabase_<service>_<project>, where <service> may itself
// contain underscores (e.g. pg_meta, edge_runtime).
//
// Strategy: strip the leading "supabase_" prefix, then greedily match the
// longest known service name at the start of the remainder. What follows is
// the project id.
func ExtractSupabaseProject(containerName string) string {
	const prefix = "supabase_"
	if !strings.HasPrefix(containerName, prefix) {
		return ""
	}
	rest := containerName[len(prefix):] // "<service>_<project>"

	// Try matching known service names from longest to shortest to avoid
	// partial matches (e.g. "pg_meta" before "pg").
	for svc := range supabaseServices {
		svcPrefix := svc + "_"
		if strings.HasPrefix(rest, svcPrefix) {
			project := rest[len(svcPrefix):]
			if project != "" {
				return project
			}
		}
	}
	return ""
}

// stopProjectContainers stops all running containers labelled with a given
// Supabase project id. Falls back to name-suffix matching if labels are absent.
func stopProjectContainers(project string) error {
	// Prefer label-based filter (exact, works regardless of naming).
	labelOut, err := exec.Command( //nolint:gosec
		"docker", "ps",
		"--filter", "label=com.supabase.cli.project="+project,
		"--format", "{{.Names}}",
	).Output()
	if err == nil {
		var toStop []string
		scanner := bufio.NewScanner(bytes.NewReader(labelOut))
		for scanner.Scan() {
			if name := strings.TrimSpace(scanner.Text()); name != "" {
				toStop = append(toStop, name)
			}
		}
		if len(toStop) > 0 {
			args := append([]string{"stop"}, toStop...)
			return exec.Command("docker", args...).Run() //nolint:gosec
		}
	}

	// Fallback: name suffix matching.
	nameOut, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output() //nolint:gosec
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(nameOut))
	var toStop []string
	suffix := "_" + project
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if strings.HasSuffix(name, suffix) {
			toStop = append(toStop, name)
		}
	}
	if len(toStop) == 0 {
		return nil
	}
	args := append([]string{"stop"}, toStop...)
	return exec.Command("docker", args...).Run() //nolint:gosec
}

// supabaseURLKeys are the env var names that must point at a local Supabase.
// Any file that sets one of these to a non-local host is refused.
var supabaseURLKeys = []string{
	"VITE_SUPABASE_URL",
	"NEXT_PUBLIC_SUPABASE_URL",
	"SUPABASE_URL",
}

// viteEnvPrecedence lists env files Vite loads in dev mode, from lowest to
// highest priority. Higher-index files override earlier ones (same as Vite).
// .env.production is intentionally excluded — it is never loaded in dev.
var viteEnvPrecedence = []string{
	".env",
	".env.development",
	".env.local",
	".env.development.local",
}

// loadEffectiveViteEnv builds the merged env map for Vite's dev mode by reading
// all env files from repoPath in precedence order. Missing files are skipped.
func loadEffectiveViteEnv(repoPath string) map[string]string {
	effective := make(map[string]string)
	for _, fname := range viteEnvPrecedence {
		data, err := os.ReadFile(filepath.Join(repoPath, fname)) //nolint:gosec
		if err != nil {
			continue
		}
		for k, v := range parseEnvKeysAll(string(data)) {
			effective[k] = v
		}
	}
	return effective
}

// isLocalHost returns true when host resolves to the loopback interface.
// Uses net.ParseIP so all 127.0.0.0/8 addresses and ::1 are accepted, and
// arbitrary strings like "127.evil.com" are rejected.
func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateEffectiveSupabaseURLs checks the merged Vite env map. It returns an
// error if:
//   - any Supabase URL key resolves to a non-local host, OR
//   - no Supabase URL key is present at all (cannot confirm local target).
func ValidateEffectiveSupabaseURLs(effective map[string]string) error {
	found := 0
	for _, key := range supabaseURLKeys {
		raw, ok := effective[key]
		if !ok || raw == "" {
			continue
		}
		found++
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s=%q is not a valid URL: %w", key, raw, err)
		}
		if !isLocalHost(u.Hostname()) {
			return fmt.Errorf("%s=%q (from merged Vite env) points at %q, not 127.0.0.1/localhost — refusing to start; check .env or .env.development", key, raw, u.Hostname())
		}
	}
	if found == 0 {
		return fmt.Errorf("no Supabase URL key (%s) found in any Vite env file — cannot confirm local DB target; add VITE_SUPABASE_URL=http://127.0.0.1:54321 to .env.local",
			strings.Join(supabaseURLKeys, ", "))
	}
	return nil
}

// CopyAndValidateEnv copies mainRepoPath/.env.local into wtPath and verifies
// the effective Vite dev env across all env files resolves all Supabase URL
// keys to 127.0.0.1 or localhost.
//
// .env.local is mandatory: if it is absent the function refuses so the review
// never silently falls back to .env / .env.production (which point at prod).
// After existence check, the merged effective env is validated so a key absent
// from .env.local but set to prod in .env is still caught.
// CopyAndValidateEnv is exported for testing.
func CopyAndValidateEnv(mainRepoPath, wtPath string, prog func(step, msg string, ok bool)) error {
	src := filepath.Join(mainRepoPath, ".env.local")
	data, err := os.ReadFile(src) //nolint:gosec
	if os.IsNotExist(err) {
		prog("env", "No .env.local found — refusing to start (Supabase projects require a local env file; without it Vite loads .env which may point at prod)", false)
		return fmt.Errorf(".env.local is required for Supabase projects but was not found at %s", src)
	}
	if err != nil {
		return fmt.Errorf("read .env.local: %w", err)
	}

	// Copy .env.local into the worktree FIRST so loadEffectiveViteEnv(wtPath)
	// sees the final set of files the app will actually use (committed .env /
	// .env.development from the branch + our .env.local override).
	dst := filepath.Join(wtPath, ".env.local")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("write .env.local: %w", err)
	}

	// Validate the effective merged Vite env in wtPath — this is what Vite will
	// actually load, so it catches a key absent from .env.local that falls back
	// to a prod value in the branch's committed .env or .env.development.
	effective := loadEffectiveViteEnv(wtPath)
	if err := ValidateEffectiveSupabaseURLs(effective); err != nil {
		// Remove the copied file so the worktree isn't left in a half-valid state.
		_ = os.Remove(dst)
		prog("env", "Effective Vite env (worktree) failed safety check: "+err.Error(), false)
		return err
	}

	prog("env", "Copied .env.local (effective Vite env in worktree confirms all Supabase URLs local)", true)
	return nil
}

// ValidateSupabaseEnvURLs parses a single env file's content and returns an
// error if any Supabase URL key has a non-local host. Absent keys are not an
// error here — use ValidateEffectiveSupabaseURLs for the merged-file check.
// Kept for unit tests that exercise single-file semantics.
func ValidateSupabaseEnvURLs(content string) error {
	vals := parseEnvKeys(content, supabaseURLKeys)
	for _, key := range supabaseURLKeys {
		raw, found := vals[key]
		if !found || raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("%s=%q is not a valid URL: %w", key, raw, err)
		}
		if !isLocalHost(u.Hostname()) {
			return fmt.Errorf("%s=%q points at %q, not 127.0.0.1 or localhost — refusing to start (would hit non-local DB)", key, raw, u.Hostname())
		}
	}
	return nil
}

// parseEnvKeysAll parses all KEY=VALUE pairs from a dotenv-style file.
func parseEnvKeysAll(content string) map[string]string {
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result
}

// parseEnvKeys scans a dotenv-style file for the given keys and returns their
// raw (unquoted) values. Only lines of the form KEY=VALUE are considered;
// comments and blank lines are skipped.
func parseEnvKeys(content string, keys []string) map[string]string {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if !want[key] {
			continue
		}
		val := strings.TrimSpace(line[idx+1:])
		// Strip optional surrounding quotes.
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result
}

// supabaseStart runs supabase start, omitting edge-runtime when no functions dir exists.
func supabaseStart(wtPath string, prog func(step, msg string, ok bool)) error {
	args := []string{"start"}
	if !HasEdgeRuntime(wtPath) {
		args = append(args, "-x", "edge-runtime")
		prog("db_start", "Starting Supabase (no edge-runtime — no functions dir)", true)
	} else {
		prog("db_start", "Starting Supabase (with edge-runtime)", true)
	}
	env := append(os.Environ(), "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH")) //nolint:gocritic
	cmd := exec.Command("supabase", args...)                                                               //nolint:gosec
	cmd.Dir = wtPath
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		prog("db_start", fmt.Sprintf("supabase start failed: %s", string(out)), false)
		return fmt.Errorf("supabase start: %w", err)
	}
	return nil
}

// applyMigrationsOrReset detects whether the DB is fresh or existing and
// applies the right strategy:
//   - Fresh DB (no known anchor table): supabase db reset --local, then stop+start
//     to recover from the trailing 503 that reset causes.
//   - Existing DB: supabase migration up --local --include-all
func applyMigrationsOrReset(wtPath string, prog func(step, msg string, ok bool)) error {
	env := append(os.Environ(), "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH")) //nolint:gocritic

	isFresh, err := dbIsFresh(wtPath)
	if err != nil {
		prog("migrations", fmt.Sprintf("could not check DB state: %v — assuming fresh", err), true)
		isFresh = true
	}

	if isFresh {
		prog("migrations", "Fresh DB detected — running db reset --local (applies all migrations + seed)", true)
		resetCmd := exec.Command("supabase", "db", "reset", "--local") //nolint:gosec
		resetCmd.Dir = wtPath
		resetCmd.Env = env
		if out, err := resetCmd.CombinedOutput(); err != nil {
			prog("migrations", fmt.Sprintf("db reset failed: %s", string(out)), false)
			return fmt.Errorf("db reset --local: %w", err)
		}
		// db reset's "Restarting containers" step can 503; do a clean stop/start.
		prog("migrations", "Cycling containers after reset…", true)
		_ = exec.Command("supabase", "stop").Run() //nolint:gosec
		return supabaseStart(wtPath, prog)
	}

	prog("migrations", "Existing DB — applying pending migrations", true)
	upCmd := exec.Command("supabase", "migration", "up", "--local", "--include-all") //nolint:gosec
	upCmd.Dir = wtPath
	upCmd.Env = env
	if out, err := upCmd.CombinedOutput(); err != nil {
		prog("migrations", fmt.Sprintf("migration up failed: %s", string(out)), false)
		return fmt.Errorf("supabase migration up: %w", err)
	}
	prog("migrations", "Migrations applied", true)
	return nil
}

// dbIsFresh returns true if the local Supabase DB appears to have no app
// schema (i.e. no public tables other than Postgres system tables). Uses psql
// against the local DB on 127.0.0.1:54322.
func dbIsFresh(wtPath string) (bool, error) {
	out, err := exec.Command( //nolint:gosec
		"psql",
		"postgresql://postgres:postgres@127.0.0.1:54322/postgres",
		"-Atc",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_type='BASE TABLE'",
	).Output()
	if err != nil {
		return true, fmt.Errorf("psql: %w", err)
	}
	count := strings.TrimSpace(string(out))
	return count == "0", nil
}
