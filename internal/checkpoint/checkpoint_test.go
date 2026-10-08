package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestGitRepo(t *testing.T) string {
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, string(out))
		}
	}

	run("init")
	run("config", "user.email", "test@agentmesh.dev")
	run("config", "user.name", "Agent Mesh Test")

	// Create initial file & commit
	fileA := filepath.Join(dir, "file_a.txt")
	if err := os.WriteFile(fileA, []byte("version 1"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "file_a.txt")
	run("commit", "-m", "initial commit")

	return dir
}

func TestCheckpointAndUndo(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()

	// 1. Modify file_a.txt and create untracked file_b.txt
	fileA := filepath.Join(dir, "file_a.txt")
	fileB := filepath.Join(dir, "file_b.txt")
	if err := os.WriteFile(fileA, []byte("version 2 (modified)"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("untracked content"), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Capture baseline HEAD commit
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	headOut, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	headSHA := strings.TrimSpace(string(headOut))

	// 3. Create Checkpoint
	cp, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "test-sess",
		Message:   "checkpoint before experiment",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	if cp.CommitSHA == "" {
		t.Errorf("expected valid commit SHA, got empty")
	}

	// Assert HEAD did NOT change
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	headAfter, _ := cmd.Output()
	if strings.TrimSpace(string(headAfter)) != headSHA {
		t.Errorf("HEAD moved! Expected %s, got %s", headSHA, string(headAfter))
	}

	// 4. Corrupt working tree: modify fileA, delete fileB, create garbage fileC
	fileC := filepath.Join(dir, "file_c.txt")
	_ = os.WriteFile(fileA, []byte("corrupted version 3"), 0644)
	_ = os.Remove(fileB)
	_ = os.WriteFile(fileC, []byte("garbage untracked file"), 0644)

	// 5. Test Dry-Run Undo
	dryRes, err := Undo(ctx, UndoOptions{
		WorkDir:      dir,
		CheckpointID: cp.ID,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("dry run undo failed: %v", err)
	}
	if len(dryRes.FilesReverted) == 0 {
		t.Errorf("expected dry-run to detect file_a.txt modification")
	}

	// 6. Execute Actual Undo
	undoRes, err := Undo(ctx, UndoOptions{
		WorkDir:      dir,
		CheckpointID: cp.ID,
	})
	if err != nil {
		t.Fatalf("Undo failed: %v", err)
	}

	if undoRes.SafetyCP == nil {
		t.Errorf("expected pre-undo safety checkpoint to be created")
	}

	// Verify fileA restored to "version 2 (modified)"
	contentA, _ := os.ReadFile(fileA)
	if string(contentA) != "version 2 (modified)" {
		t.Errorf("expected file_a to be 'version 2 (modified)', got %s", string(contentA))
	}

	// Verify fileB restored
	contentB, err := os.ReadFile(fileB)
	if err != nil || string(contentB) != "untracked content" {
		t.Errorf("expected file_b to be restored, err: %v, content: %s", err, string(contentB))
	}

	// Verify garbage fileC removed
	if _, err := os.Stat(fileC); !os.IsNotExist(err) {
		t.Errorf("expected file_c to be cleaned up, but it still exists")
	}

	// 7. Test Redo
	redoRes, err := Redo(ctx, dir, "test-sess")
	if err != nil {
		t.Fatalf("Redo failed: %v", err)
	}
	if redoRes == nil {
		t.Fatal("expected redo result")
	}

	// Verify fileA is back to corrupted version 3
	contentRedoA, _ := os.ReadFile(fileA)
	if string(contentRedoA) != "corrupted version 3" {
		t.Errorf("expected redo to restore 'corrupted version 3', got %s", string(contentRedoA))
	}
}

func TestListAndPruneCheckpoints(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		f := filepath.Join(dir, "file_a.txt")
		_ = os.WriteFile(f, []byte(string(rune('0'+i))), 0644)
		_, err := CreateCheckpoint(ctx, CreateOptions{
			WorkDir:   dir,
			SessionID: "sess-1",
			Message:   "iteration",
		})
		if err != nil {
			t.Fatalf("CreateCheckpoint %d failed: %v", i, err)
		}
	}

	cps, err := ListCheckpoints(ctx, dir, 0)
	if err != nil {
		t.Fatalf("ListCheckpoints failed: %v", err)
	}
	if len(cps) < 5 {
		t.Errorf("expected at least 5 checkpoints, got %d", len(cps))
	}

	// Prune keeping 2
	pruned, err := PruneCheckpoints(ctx, dir, 2)
	if err != nil {
		t.Fatalf("PruneCheckpoints failed: %v", err)
	}
	if pruned == 0 {
		t.Errorf("expected >0 checkpoints pruned")
	}
}

// TestDiffCheckpointFilesBareID verifies that DiffCheckpointFiles and
// DiffCheckpointFilesAgainstRef resolve bare cp_ IDs (not full refs/SHA) via
// for-each-ref, so file_stats is non-empty. Regression for STA-500 bug 1.
func TestDiffCheckpointFilesBareID(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()

	// Modify tracked file so there is a diff to report.
	fileA := filepath.Join(dir, "file_a.txt")
	if err := os.WriteFile(fileA, []byte("modified content"), 0644); err != nil {
		t.Fatal(err)
	}

	cp, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "test-bare-id",
		Message:   "checkpoint for bare-ID test",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	// Modify again so the working tree differs from the checkpoint.
	if err := os.WriteFile(fileA, []byte("content after checkpoint"), 0644); err != nil {
		t.Fatal(err)
	}

	// Use cp.ID (bare e.g. "cp_20261003_…") not the full ref.
	stats, err := DiffCheckpointFiles(ctx, dir, cp.ID)
	if err != nil {
		t.Fatalf("DiffCheckpointFiles with bare ID failed: %v", err)
	}
	if len(stats) == 0 {
		t.Errorf("expected file_stats to be non-empty for bare checkpoint ID %q, got []", cp.ID)
	}

	// Also verify DiffCheckpointFilesAgainstRef with bare ID.
	head, err := runGit(ctx, dir, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	statsRef, err := DiffCheckpointFilesAgainstRef(ctx, dir, cp.ID, head)
	if err != nil {
		t.Fatalf("DiffCheckpointFilesAgainstRef with bare ID failed: %v", err)
	}
	if len(statsRef) == 0 {
		t.Errorf("expected file_stats to be non-empty for bare checkpoint ID %q (AgainstRef), got []", cp.ID)
	}
}

// TestDiffCheckpointFilesRenamedAndSpaces verifies that DiffCheckpointFiles and
// DiffCheckpointFilesAgainstRef preserve full paths containing spaces and correctly
// report real paths when files are renamed or deleted, rather than mangling them
// into {old => new} or truncating on spaces. Regression for STA-755.
func TestDiffCheckpointFilesRenamedAndSpaces(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()

	// Create initial files and commit them
	migDir := filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldMig := filepath.Join(migDir, "026_alerts.sql")
	if err := os.WriteFile(oldMig, []byte("-- old migration\nSELECT 1;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	toDelete := filepath.Join(migDir, "005_to_delete.sql")
	if err := os.WriteFile(toDelete, []byte("-- to delete\nSELECT 5;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, string(out))
		}
	}
	run("add", ".")
	run("commit", "-m", "commit migrations")

	cp, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "test-rename-spaces",
		Message:   "checkpoint before rename",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	// 1. Rename 026_alerts.sql -> 027_alerts.sql
	run("mv", filepath.Join("migrations", "026_alerts.sql"), filepath.Join("migrations", "027_alerts.sql"))

	// 2. Add file with spaces
	fileWithSpaces := filepath.Join(migDir, "028 add new column.sql")
	if err := os.WriteFile(fileWithSpaces, []byte("-- spaces\nSELECT 28;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", filepath.Join("migrations", "028 add new column.sql"))

	// 3. Delete 005_to_delete.sql
	run("rm", filepath.Join("migrations", "005_to_delete.sql"))

	run("commit", "-m", "rename, spaces, and delete")

	head, err := runGit(ctx, dir, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	checkStats := func(stats []FileDiffStat, label string) {
		t.Helper()
		byPath := make(map[string]FileDiffStat)
		for _, s := range stats {
			byPath[s.Path] = s
		}

		// Verify renamed file: path must be the real path "migrations/027_alerts.sql"
		// and must not be mangled like "migrations/{026_alerts.sql"
		if _, ok := byPath["migrations/027_alerts.sql"]; !ok {
			t.Errorf("[%s] expected real renamed path migrations/027_alerts.sql, got stats: %+v", label, stats)
		}
		for p := range byPath {
			if strings.Contains(p, "{") || strings.Contains(p, "=>") {
				t.Errorf("[%s] path contains mangled rename syntax: %q", label, p)
			}
		}

		// Verify file with spaces: must have full name "migrations/028 add new column.sql"
		if _, ok := byPath["migrations/028 add new column.sql"]; !ok {
			t.Errorf("[%s] expected full path with spaces 'migrations/028 add new column.sql', got stats: %+v", label, stats)
		}

		// Verify deleted file is present with its exact real path
		if _, ok := byPath["migrations/005_to_delete.sql"]; !ok {
			t.Errorf("[%s] expected deleted path migrations/005_to_delete.sql, got stats: %+v", label, stats)
		}
	}

	stats, err := DiffCheckpointFiles(ctx, dir, cp.ID)
	if err != nil {
		t.Fatalf("DiffCheckpointFiles failed: %v", err)
	}
	checkStats(stats, "DiffCheckpointFiles")

	statsRef, err := DiffCheckpointFilesAgainstRef(ctx, dir, cp.ID, head)
	if err != nil {
		t.Fatalf("DiffCheckpointFilesAgainstRef failed: %v", err)
	}
	checkStats(statsRef, "DiffCheckpointFilesAgainstRef")
}

// TestFindPreRunCheckpoint verifies FindPreRunCheckpoint returns the pre-run
// checkpoint for a given task ID and ignores checkpoints from other tasks.
// Regression for STA-500 bug 2.
func TestFindPreRunCheckpoint(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()
	taskID := "task-abc123"

	// Create a non-pre-run checkpoint first.
	if err := os.WriteFile(filepath.Join(dir, "file_a.txt"), []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "sess-other",
		Message:   "some other checkpoint",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	// Create the pre-run checkpoint for our task.
	if err := os.WriteFile(filepath.Join(dir, "file_a.txt"), []byte("v3"), 0644); err != nil {
		t.Fatal(err)
	}
	preCP, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "run-xyz",
		Message:   "pre-run " + taskID,
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint pre-run failed: %v", err)
	}

	// Create another checkpoint after the pre-run (simulates turn checkpoints).
	if err := os.WriteFile(filepath.Join(dir, "file_a.txt"), []byte("v4"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "run-xyz",
		Message:   "turn 1 " + taskID,
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint turn failed: %v", err)
	}

	found, foundRef, err := FindPreRunCheckpointRef(ctx, dir, taskID)
	if err != nil {
		t.Fatalf("FindPreRunCheckpointRef failed: %v", err)
	}
	if found != preCP.ID {
		t.Errorf("FindPreRunCheckpointRef returned %q, want %q", found, preCP.ID)
	}
	if foundRef != preCP.Ref {
		t.Errorf("FindPreRunCheckpointRef ref = %q, want %q", foundRef, preCP.Ref)
	}

	// Should not match a different task.
	notFound, _, err := FindPreRunCheckpointRef(ctx, dir, "task-other")
	if err != nil {
		t.Fatalf("FindPreRunCheckpoint (other task) failed: %v", err)
	}
	if notFound != "" {
		t.Errorf("FindPreRunCheckpoint returned %q for unknown task, want empty", notFound)
	}
}

func TestUndoCleanIgnored(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()

	// 1. Add .gitignore and commit it
	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.ignored\nbuild/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add .gitignore failed: %v\nOutput: %s", err, string(out))
	}
	cmd = exec.Command("git", "commit", "-m", "add gitignore")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\nOutput: %s", err, string(out))
	}

	// 2. Create base checkpoint
	cp, err := CreateCheckpoint(ctx, CreateOptions{
		WorkDir:   dir,
		SessionID: "test-clean-ignored",
		Message:   "checkpoint before creating ignored files",
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint failed: %v", err)
	}

	// 3. Create untracked ignored file and directory
	ignoredFile := filepath.Join(dir, "temp.ignored")
	if err := os.WriteFile(ignoredFile, []byte("temporary ignored content"), 0644); err != nil {
		t.Fatal(err)
	}
	ignoredDir := filepath.Join(dir, "build")
	if err := os.MkdirAll(ignoredDir, 0755); err != nil {
		t.Fatal(err)
	}
	ignoredDirFile := filepath.Join(ignoredDir, "output.bin")
	if err := os.WriteFile(ignoredDirFile, []byte("binary data"), 0644); err != nil {
		t.Fatal(err)
	}

	// 4. Test Undo with CleanIgnored: false (ignored files must be retained)
	resRetained, err := Undo(ctx, UndoOptions{
		WorkDir:      dir,
		CheckpointID: cp.ID,
		CleanIgnored: false,
	})
	if err != nil {
		t.Fatalf("Undo with CleanIgnored: false failed: %v", err)
	}
	if len(resRetained.FilesIgnoredRemoved) != 0 {
		t.Errorf("expected 0 files in FilesIgnoredRemoved, got %d", len(resRetained.FilesIgnoredRemoved))
	}
	if _, err := os.Stat(ignoredFile); os.IsNotExist(err) {
		t.Errorf("expected ignored file to be retained when CleanIgnored: false")
	}
	if _, err := os.Stat(ignoredDirFile); os.IsNotExist(err) {
		t.Errorf("expected ignored directory file to be retained when CleanIgnored: false")
	}

	// 5. Test Dry-Run Undo with CleanIgnored: true (preview only, files kept)
	dryRes, err := Undo(ctx, UndoOptions{
		WorkDir:      dir,
		CheckpointID: cp.ID,
		CleanIgnored: true,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Undo dry run failed: %v", err)
	}
	if len(dryRes.FilesIgnoredRemoved) == 0 {
		t.Errorf("expected dry-run to identify ignored files to remove")
	}
	if _, err := os.Stat(ignoredFile); os.IsNotExist(err) {
		t.Errorf("expected ignored file to remain after dry run")
	}

	// 6. Test Actual Undo with CleanIgnored: true (ignored files must be deleted)
	resDeleted, err := Undo(ctx, UndoOptions{
		WorkDir:      dir,
		CheckpointID: cp.ID,
		CleanIgnored: true,
	})
	if err != nil {
		t.Fatalf("Undo with CleanIgnored: true failed: %v", err)
	}
	if len(resDeleted.FilesIgnoredRemoved) == 0 {
		t.Errorf("expected FilesIgnoredRemoved to list deleted ignored files")
	}
	if _, err := os.Stat(ignoredFile); !os.IsNotExist(err) {
		t.Errorf("expected ignored file to be deleted when CleanIgnored: true")
	}
	if _, err := os.Stat(ignoredDir); !os.IsNotExist(err) {
		t.Errorf("expected ignored directory to be deleted when CleanIgnored: true")
	}
}
