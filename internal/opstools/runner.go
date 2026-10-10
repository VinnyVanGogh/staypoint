package opstools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Cmd is one process an ops tool runs: an argv, never a shell line.
type Cmd struct {
	Name  string
	Args  []string
	Dir   string
	Stdin []byte
}

// Result is a finished Cmd. Err is set when the process could not run or
// was cut off (not for a non-zero exit).
type Result struct {
	// Output is stdout and stderr interleaved; Stdout is stdout alone.
	Output   string
	Stdout   string
	ExitCode int
	Err      error
}

// Format is the redacted text a tool returns: the exit status, then output.
func (r Result) Format() string {
	head := fmt.Sprintf("exit %d", r.ExitCode)
	if r.Err != nil {
		head += " (" + r.Err.Error() + ")"
	}
	out := strings.TrimRight(Redact(r.Output), "\n")
	if out == "" {
		return head
	}
	return head + "\n" + capOutput(out)
}

// Runner runs a Cmd; tests replace ExecRunner with a fake.
type Runner func(ctx context.Context, c Cmd) Result

// ExecRunner runs c with its combined output captured.
func ExecRunner(ctx context.Context, c Cmd) Result {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, combined bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, &combined)
	cmd.Stderr = &combined
	err := cmd.Run()
	res := Result{Output: combined.String(), Stdout: stdout.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.ExitCode, res.Err = -1, ctx.Err()
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode, res.Err = -1, err
	}
	return res
}
