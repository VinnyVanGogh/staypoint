package shipreview_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// STA-774: a run that committed nothing gets no card, so no Approve.
func TestBuildAndStartCard_AnswerOnlyRunGetsNoCard(t *testing.T) {
	const taskID = "t-answer-only"
	db := openTestDB(t)
	repoDir, _, _ := setupGitRepo(t)
	gitT(t, repoDir, "branch", "staypoint/"+taskID, "main")
	recordMainBase(t, db, repoDir, taskID)

	_, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repoDir, []string{"1. Check"}, "", nil)
	if !errors.Is(err, shipreview.ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges", err)
	}
	if _, err := shipreview.GetCard(db, taskID); !errors.Is(err, shipreview.ErrNoCard) {
		t.Fatalf("a card exists for an answer-only run (err %v)", err)
	}
}

// Every way of failing to verify the base refuses the card (fail closed).
func TestBuildAndStartCard_UnverifiedBaseGetsNoCard(t *testing.T) {
	cases := []struct {
		name   string
		record bool
		tamper bool
		want   error
	}{
		{name: "no recorded base", want: workspace.ErrNoTaskBase},
		{name: "pin moved by agent", record: true, tamper: true, want: workspace.ErrTaskBaseTampered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const taskID = "t-unverified"
			db := openTestDB(t)
			ensureTaskBaseTable(t, db)
			repoDir, _, featureSHA := setupGitRepo(t)
			gitT(t, repoDir, "branch", "staypoint/"+taskID, featureSHA)
			if tc.record {
				recordMainBase(t, db, repoDir, taskID)
			}
			if tc.tamper {
				gitT(t, repoDir, "update-ref", workspace.BaseRef(taskID), featureSHA)
			}
			_, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repoDir, []string{"1. Check"}, "", nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if _, err := shipreview.GetCard(db, taskID); !errors.Is(err, shipreview.ErrNoCard) {
				t.Fatalf("card created over an unverified base (err %v)", err)
			}
		})
	}
}

// A DB without the bases table (unmigrated) refuses the card too.
func TestBuildAndStartCard_UnreadableBaseStoreGetsNoCard(t *testing.T) {
	const taskID = "t-nostore"
	db := openTestDB(t)
	repoDir, _, featureSHA := setupGitRepo(t)
	gitT(t, repoDir, "branch", "staypoint/"+taskID, featureSHA)
	if _, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repoDir, []string{"1. Check"}, "", nil); err == nil {
		t.Fatal("card created with no readable base store")
	}
	if _, err := shipreview.GetCard(db, taskID); !errors.Is(err, shipreview.ErrNoCard) {
		t.Fatalf("card created (err %v)", err)
	}
}

func TestBuildAndStartCard_NoRepoPathGetsNoCard(t *testing.T) {
	db := openTestDB(t)
	if _, err := shipreview.BuildAndStartCard(context.Background(), db, "t-norepo", "", []string{"1. Check"}, "", nil); !errors.Is(err, workspace.ErrNoTaskBase) {
		t.Fatalf("err = %v, want ErrNoTaskBase", err)
	}
}

// The card lists only the task's own files, measured from the recorded base,
// and VerifyCardChanges (the Approve gate) fails closed once the pin moves.
func TestVerifyCardChanges(t *testing.T) {
	const taskID = "t-verify"
	db := openTestDB(t)
	repoDir, _, featureSHA := setupGitRepo(t)
	gitT(t, repoDir, "branch", "staypoint/"+taskID, featureSHA)
	recordMainBase(t, db, repoDir, taskID)

	card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repoDir, []string{"1. Check"}, "", nil)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	if len(card.FilesChanged) != 1 || card.FilesChanged[0] != "feature.txt" {
		t.Fatalf("files = %v, want [feature.txt]", card.FilesChanged)
	}
	files, err := shipreview.VerifyCardChanges(context.Background(), db, card, repoDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("VerifyCardChanges = %v, %v", files, err)
	}

	gitT(t, repoDir, "update-ref", workspace.BaseRef(taskID), featureSHA)
	if _, err := shipreview.VerifyCardChanges(context.Background(), db, card, repoDir); !errors.Is(err, workspace.ErrTaskBaseTampered) {
		t.Fatalf("after tamper err = %v, want ErrTaskBaseTampered", err)
	}
}
