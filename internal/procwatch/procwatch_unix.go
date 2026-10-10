//go:build !windows

package procwatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GroupAlive reports whether any process is in process group pgid.
func GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// PidAlive reports whether process pid exists.
func PidAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalGroup(pgid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pgid, sig)
}

// AgentsInDir lists daemon-spawned agent processes working in dir: their
// working directory is dir (or below it), they have no controlling terminal
// (a person's shell or interactive session has one), and their environment
// carries envMarker (e.g. STAYPOINT_TASK_ID=task-x, which the harness gives
// every agent it starts; ps shows a same-user process's environment). Other
// headless processes there, such as git's fsmonitor daemon, do not count.
// The caller's own process is left out. It catches agents no live_runs row
// recorded (orphans of a daemon that predates the record).
func AgentsInDir(dir, envMarker string) ([]int, error) {
	if dir == "" || envMarker == "" {
		return nil, nil
	}
	dir = strings.TrimRight(dir, "/")
	// lsof reports resolved paths (/private/var/... for /var/...).
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = append(dirs, strings.TrimRight(real, "/"))
	}
	under := func(p string) bool {
		for _, d := range dirs {
			if p == d || strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w", "-d", "cwd", "-F", "pn").Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	self := os.Getpid()
	var pids []string
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			p := line[1:]
			if pid > 1 && pid != self && under(p) {
				pids = append(pids, strconv.Itoa(pid))
			}
		}
	}
	if len(pids) == 0 {
		return nil, nil
	}
	// "e" appends each process's environment to its command.
	out, err = exec.CommandContext(ctx, "ps", "eww", "-o", "pid=,tty=,command=", "-p", strings.Join(pids, ",")).Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var agents []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if tty := f[1]; tty != "??" && tty != "?" && tty != "-" {
			continue
		}
		marked := false
		for _, tok := range f[2:] {
			if tok == envMarker {
				marked = true
				break
			}
		}
		if n, err := strconv.Atoi(f[0]); err == nil && marked {
			agents = append(agents, n)
		}
	}
	return agents, nil
}

// psStartLayout is ps -o lstart's format on macOS and Linux.
const psStartLayout = "Mon Jan _2 15:04:05 2006"

// leaderStart returns when process pid started. found is false when ps ran
// and there is no such process.
func leaderStart(pid int) (start time.Time, found bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	line := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && line == "" {
			return time.Time{}, false, nil // ps ran: no such process
		}
		return time.Time{}, false, err
	}
	if line == "" {
		return time.Time{}, false, nil
	}
	t, perr := time.ParseInLocation(psStartLayout, strings.Join(strings.Fields(line), " "), time.Local)
	if perr != nil {
		// "Oct  1" collapses to "Oct 1" above; _2 accepts both.
		return time.Time{}, false, perr
	}
	return t, true, nil
}
