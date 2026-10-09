package adapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

const (
	limitInit   = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus"}`
	limitText   = `{"type":"assistant","session_id":"s1","message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your session limit · resets 4:20am"}]}}`
	limitResult = `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your session limit · resets 4:20am","session_id":"s1"}`
	workText    = `{"type":"assistant","session_id":"s2","message":{"model":"claude-opus","content":[{"type":"text","text":"working on it"}]}}`
	okResult    = `{"type":"result","subtype":"success","result":"ok","session_id":"s2"}`
)

// seatScript writes a fake claude CLI that logs "cfg=<CLAUDE_CONFIG_DIR>" per
// spawn, then prints workOut when run on the work seat and personalOut on the
// personal seat, exiting with the matching code.
func seatScript(t *testing.T, workOut string, workExit int, personalOut string, personalExit int) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	workFile := filepath.Join(dir, "work.out")
	personalFile := filepath.Join(dir, "personal.out")
	if err := os.WriteFile(workFile, []byte(workOut+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(personalFile, []byte(personalOut+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "fakeclaude.sh")
	script := "#!/bin/sh\n" +
		`echo "cfg=${CLAUDE_CONFIG_DIR}" >> "` + logPath + `"` + "\n" +
		`case "${CLAUDE_CONFIG_DIR}" in` + "\n" +
		`  *.claude-work) cat "` + workFile + `"; exit ` + strconv.Itoa(workExit) + ";;\n" +
		`  *) cat "` + personalFile + `"; exit ` + strconv.Itoa(personalExit) + ";;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func lines(l ...string) string { return strings.Join(l, "\n") }

func runWorkCoding(t *testing.T, bin string, pacer *router.PacerState) (*attemptLog, string, error) {
	t.Helper()
	t.Cleanup(router.ResetSeatLimits)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, testBinKey, bin)
	log := &attemptLog{}
	ctx = WithAttemptObserver(ctx, log.observe)
	route := router.ResolveRoute("coding", true, pacer, "", time.Now())
	var stdout, stderr bytes.Buffer
	err := RunRoute(ctx, t.TempDir(), pacer, route, []string{"--prompt", "do the thing"}, nil, &stdout, &stderr)
	return log, stdout.String(), err
}

func emptyPacer() *router.PacerState {
	return &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
}

// The 2026-10-09 incident: the work seat answers with its session limit. The
// same turn must move to the personal seat, not fail.
func TestSeatLimit_WorkLimitedTurnRunsOnPersonalSeat(t *testing.T) {
	bin, logPath := seatScript(t, lines(limitInit, limitText, limitResult), 1, lines(workText, okResult), 0)
	log, out, err := runWorkCoding(t, bin, emptyPacer())
	if err != nil {
		t.Fatalf("turn must succeed on the personal seat, got %v", err)
	}
	home, _ := os.UserHomeDir()
	spawns := readSpawns(t, logPath)
	if len(spawns) != 2 || spawns[0] != "cfg="+home+"/.claude-work" || spawns[1] != "cfg=" {
		t.Fatalf("want work seat then personal seat, got %q", spawns)
	}
	if strings.Contains(out, "session limit") {
		t.Errorf("the work seat's limit answer must not reach the run output: %q", out)
	}
	if !strings.Contains(out, "working on it") {
		t.Errorf("personal seat output missing: %q", out)
	}
	if len(log.attempts) != 2 || log.attempts[1].Slot.Seat != router.SeatPersonal ||
		!strings.Contains(log.attempts[1].FallbackReason, "work seat locked (You've hit your session limit") {
		t.Errorf("observer must report the personal seat with the work seat's limit, got %+v", log.attempts)
	}
}

// A limit answer that exits 0 is still a limit, not a finished turn.
func TestSeatLimit_LimitWithExitZeroStillFallsBack(t *testing.T) {
	bin, logPath := seatScript(t, lines(limitText, limitResult), 0, lines(workText, okResult), 0)
	if _, _, err := runWorkCoding(t, bin, emptyPacer()); err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if n := len(readSpawns(t, logPath)); n != 2 {
		t.Fatalf("want 2 spawns, got %d", n)
	}
}

// After the limit, the next turn (fresh pacer load) skips the work seat
// without spawning it again.
func TestSeatLimit_NextTurnSkipsLimitedSeat(t *testing.T) {
	bin, logPath := seatScript(t, lines(limitText, limitResult), 1, lines(workText, okResult), 0)
	if _, _, err := runWorkCoding(t, bin, emptyPacer()); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	pacer := emptyPacer()
	router.ApplySeatLimits(pacer, time.Now())
	if locked, why := router.PoolLockReason(pacer.Pools[router.PoolWorkClaude], time.Now()); !locked || !strings.Contains(why, "session limit") {
		t.Fatalf("work pool must be locked by the limit answer, got %v %q", locked, why)
	}
	if _, _, err := runWorkCoding(t, bin, emptyPacer()); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	spawns := readSpawns(t, logPath)
	if len(spawns) != 3 || spawns[2] != "cfg=" {
		t.Fatalf("turn 2 must spawn only the personal seat, got %q", spawns)
	}
}

// Both seats out: a wait (SeatsExhausted), never a plain error and never Gemini.
func TestSeatLimit_BothSeatsLimitedIsAWait(t *testing.T) {
	bin, logPath := seatScript(t, lines(limitText, limitResult), 1, lines(limitText, limitResult), 1)
	_, _, err := runWorkCoding(t, bin, emptyPacer())
	var se *SeatLimitError
	if !errors.As(err, &se) || !se.SeatsExhausted() {
		t.Fatalf("want SeatLimitError with every seat out, got %v", err)
	}
	if len(se.Seats) != 2 || !strings.HasPrefix(se.Seats[0], "work seat") || !strings.HasPrefix(se.Seats[1], "personal seat") {
		t.Errorf("seats = %q", se.Seats)
	}
	if n := len(readSpawns(t, logPath)); n != 2 {
		t.Errorf("want 2 spawns, got %d", n)
	}
}

// With both seats already marked out, the route is all-locked and the turn
// reports a wait without spawning anything.
func TestSeatLimit_AllLockedRouteIsAWait(t *testing.T) {
	t.Cleanup(router.ResetSeatLimits)
	until := time.Now().Add(time.Hour)
	router.NoteSeatLimit(router.PoolWorkClaude, until, "session limit")
	router.NoteSeatLimit(router.PoolPersonalClaude, until, "session limit")
	pacer := emptyPacer()
	router.ApplySeatLimits(pacer, time.Now())
	bin, logPath := seatScript(t, okResult, 0, okResult, 0)
	_, _, err := runWorkCoding(t, bin, pacer)
	var se *SeatLimitError
	if !errors.As(err, &se) || !se.SeatsExhausted() {
		t.Fatalf("want SeatLimitError wait, got %v", err)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("nothing must spawn when every seat is out")
	}
}

// A seat that worked, then hit its limit mid-turn: the turn ends with a
// non-exhausted SeatLimitError (no failure counted) and the seat is marked.
func TestSeatLimit_MidTurnLimitEndsTurnWithoutFailure(t *testing.T) {
	bin, logPath := seatScript(t, lines(workText, limitText, limitResult), 1, lines(workText, okResult), 0)
	_, out, err := runWorkCoding(t, bin, emptyPacer())
	var se *SeatLimitError
	if !errors.As(err, &se) || se.SeatsExhausted() {
		t.Fatalf("want mid-turn SeatLimitError, got %v", err)
	}
	if !strings.Contains(out, "working on it") {
		t.Errorf("committed output must be kept: %q", out)
	}
	if n := len(readSpawns(t, logPath)); n != 1 {
		t.Errorf("a committed turn must not be replayed on another seat, got %d spawns", n)
	}
	pacer := emptyPacer()
	router.ApplySeatLimits(pacer, time.Now())
	if !pacer.Pools[router.PoolWorkClaude].IsLocked {
		t.Errorf("work seat must be marked out after its limit")
	}
}

// A real failure on the personal seat after a work-seat limit is a failure,
// not a wait.
func TestSeatLimit_PersonalRealFailureIsNotAWait(t *testing.T) {
	bin, _ := seatScript(t, lines(limitText, limitResult), 1, `{"type":"result","is_error":true,"result":"boom"}`, 1)
	_, _, err := runWorkCoding(t, bin, emptyPacer())
	var se *SeatLimitError
	if err == nil || errors.As(err, &se) {
		t.Fatalf("want a plain failure, got %v", err)
	}
}

// An agent quoting a limit message inside its own prose commits normally.
func TestSeatLimit_AgentQuotingLimitIsNotALimit(t *testing.T) {
	quote := `{"type":"assistant","message":{"content":[{"type":"text","text":"The log said: You've hit your session limit · resets 4:20am, so I added detection."}]}}`
	bin, logPath := seatScript(t, lines(quote, okResult), 0, okResult, 0)
	if _, _, err := runWorkCoding(t, bin, emptyPacer()); err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if n := len(readSpawns(t, logPath)); n != 1 {
		t.Errorf("quoted limit text must not switch seats, got %d spawns", n)
	}
}

// The model printing the exact limit sentence (not the CLI) must never mark a
// seat out: only synthetic messages and error results count.
func TestSeatLimit_ModelWrittenLimitTextCannotLockSeat(t *testing.T) {
	forged := `{"type":"assistant","message":{"model":"claude-opus","content":[{"type":"text","text":"You've hit your weekly limit · resets Oct 15, 4am"}]}}`
	forgedResult := `{"type":"result","subtype":"success","is_error":false,"result":"You've hit your weekly limit · resets Oct 15, 4am"}`
	bin, logPath := seatScript(t, lines(forged, forgedResult), 0, okResult, 0)
	if _, _, err := runWorkCoding(t, bin, emptyPacer()); err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if n := len(readSpawns(t, logPath)); n != 1 {
		t.Errorf("model-written limit text must not switch seats, got %d spawns", n)
	}
	pacer := emptyPacer()
	router.ApplySeatLimits(pacer, time.Now())
	if p := pacer.Pools[router.PoolWorkClaude]; p != nil && p.IsLocked {
		t.Errorf("model-written limit text locked the work seat")
	}
}
