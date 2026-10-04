// Package shipreview — built-in Supabase local dev env support.
// Handles Docker auto-start, port-conflict resolution, DB-state detection,
// and idle spin-down so every review of a Supabase project runs against a
// fresh local DB and never touches production.
package shipreview

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
//  3. Copy .env.local from mainRepoPath; refuse unless it points at 127.0.0.1/localhost.
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
	if err := resolvePortConflicts(wtPath, prog); err != nil {
		return fmt.Errorf("port conflicts: %w", err)
	}

	// 3. .env.local safety check.
	prog("env", "Copying .env.local…", true)
	if err := copyAndValidateEnv(mainRepoPath, wtPath, prog); err != nil {
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
// hold the default Supabase ports. Data volumes are kept so the next review
// can resume quickly.
func resolvePortConflicts(wtPath string, prog func(step, msg string, ok bool)) error {
	// Derive the current project name from the worktree path to avoid
	// stopping ourselves if we are already running.
	currentProject := filepath.Base(wtPath)

	for _, port := range supabasePorts {
		holder, err := containerHoldingPort(port)
		if err != nil || holder == "" {
			continue
		}
		// Skip if it belongs to the current project.
		if strings.Contains(holder, currentProject) {
			continue
		}
		// The supabase container naming convention is supabase_<service>_<project>.
		// Extract the other project name and stop all its containers.
		otherProject := extractSupabaseProject(holder)
		if otherProject == "" {
			prog("ports", fmt.Sprintf("port %s held by %s (skipping)", port, holder), true)
			continue
		}
		prog("ports", fmt.Sprintf("port %s held by %s — stopping %s containers (volumes kept)", port, holder, otherProject), true)
		if err := stopProjectContainers(otherProject); err != nil {
			prog("ports", fmt.Sprintf("warning: could not stop %s: %v", otherProject, err), false)
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

// extractSupabaseProject extracts the project name from a Supabase container
// name like "supabase_db_my_project" → "my_project".
func extractSupabaseProject(containerName string) string {
	// supabase_<service>_<project>
	parts := strings.SplitN(containerName, "_", 3)
	if len(parts) == 3 && parts[0] == "supabase" {
		return parts[2]
	}
	return ""
}

// stopProjectContainers stops all Docker containers whose name ends with "_<project>".
func stopProjectContainers(project string) error {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output() //nolint:gosec
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
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

// copyAndValidateEnv copies mainRepoPath/.env.local into wtPath and verifies it
// points at 127.0.0.1 or localhost, refusing to run if it references prod.
func copyAndValidateEnv(mainRepoPath, wtPath string, prog func(step, msg string, ok bool)) error {
	src := filepath.Join(mainRepoPath, ".env.local")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		prog("env", "No .env.local found — skipping copy (project may not need it)", true)
		return nil
	}
	data, err := os.ReadFile(src) //nolint:gosec
	if err != nil {
		return fmt.Errorf("read .env.local: %w", err)
	}
	if !envIsLocal(string(data)) {
		prog("env", ".env.local does not point at 127.0.0.1 or localhost — refusing to start (safety: cannot use prod DB)", false)
		return fmt.Errorf(".env.local must point at 127.0.0.1 or localhost for local DB; refusing to start")
	}
	dst := filepath.Join(wtPath, ".env.local")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("write .env.local: %w", err)
	}
	prog("env", "Copied .env.local (confirmed local DB)", true)
	return nil
}

// envIsLocal returns true when the env file content references a loopback address.
func envIsLocal(content string) bool {
	return strings.Contains(content, "127.0.0.1") || strings.Contains(content, "localhost")
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
