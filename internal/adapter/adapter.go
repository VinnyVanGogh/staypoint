package adapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// testBinKey is the context key existing tests use to substitute a fake CLI
// binary for every provider. The plain string type is kept for compatibility.
const testBinKey = "testBin"

// ctxExtraEnvKey is the context key for harness-supplied extra env vars.
type ctxExtraEnvKey struct{}

// WithExtraEnv attaches harness-sanitized extra environment variables to ctx
// so RunAdapter can forward them to the child process without changing its signature.
func WithExtraEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, ctxExtraEnvKey{}, env)
}

// runCommandWithEnv executes a command with optional extra environment variables,
// anti-stall keepalive newlines, and unthrottled 32KB chunk streaming.
func runCommandWithEnv(ctx context.Context, dir string, bin string, args []string, extraEnv []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	cmdExec := exec.Command(bin, args...)
	cmdExec.Dir = dir
	if stdin != nil {
		cmdExec.Stdin = stdin
	}
	cmdExec.Stderr = stderr
	if len(extraEnv) > 0 {
		cmdExec.Env = append(os.Environ(), extraEnv...)
	}
	setProcessGroup(cmdExec)

	cmdStdout, err := cmdExec.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmdExec.Start(); err != nil {
		return fmt.Errorf("start error: %w", err)
	}

	ch := make(chan []byte, 1000)
	errCh := make(chan error, 1)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := cmdStdout.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				ch <- chunk
			}
			if err != nil {
				if err != io.EOF {
					errCh <- err
				}
				close(ch)
				return
			}
		}
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				err := cmdExec.Wait()
				if err != nil {
					return fmt.Errorf("agent exited with error: %w", err)
				}
				return nil
			}
			if _, err := stdout.Write(chunk); err != nil {
				return err
			}
		case <-ticker.C:
			// Emit newline keepalive to keep Paperclip stream consumer alive
			_, _ = stdout.Write([]byte("\n"))
		case err := <-errCh:
			return err
		case <-ctx.Done():
			killProcessGroup(cmdExec, 1500*time.Millisecond)
			go func() {
				_ = cmdExec.Wait()
			}()
			return ctx.Err()
		}
	}
}

// ChatMessageInput represents a normalized message turn passed to an adapter.
type ChatMessageInput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ParsedOptions holds common execution options extracted from adapter command arguments.
type ParsedOptions struct {
	Prompt         string
	Model          string
	Effort         string
	ConversationID string
	OutputFormat   string
	AddDirs        []string
	History        []ChatMessageInput
}

func parseRawArgs(rawArgs []string) ParsedOptions {
	var opts ParsedOptions
	opts.OutputFormat = "stream-json"

argLoop:
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		switch {
		case arg == "--approval-mode":
			i++
		case arg == "--sandbox=none" || arg == "--sandbox" || arg == "-acp" || arg == "--acp":
			// Drop unsupported sandbox and acp flags
		case arg == "--resume" || arg == "--conversation":
			if i+1 < len(rawArgs) {
				opts.ConversationID = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--conversation="):
			opts.ConversationID = strings.TrimPrefix(arg, "--conversation=")
		case strings.HasPrefix(arg, "--resume="):
			opts.ConversationID = strings.TrimPrefix(arg, "--resume=")
		case arg == "--model":
			if i+1 < len(rawArgs) {
				val := strings.TrimPrefix(rawArgs[i+1], "google/")
				re := regexp.MustCompile(`^(.*)-(low|medium|high)$`)
				if m := re.FindStringSubmatch(val); m != nil {
					opts.Model = m[1]
					opts.Effort = m[2]
				} else {
					opts.Model = val
				}
				i++
			}
		case strings.HasPrefix(arg, "--model="):
			val := strings.TrimPrefix(strings.TrimPrefix(arg, "--model="), "google/")
			re := regexp.MustCompile(`^(.*)-(low|medium|high)$`)
			if m := re.FindStringSubmatch(val); m != nil {
				opts.Model = m[1]
				opts.Effort = m[2]
			} else {
				opts.Model = val
			}
		case arg == "--effort":
			if i+1 < len(rawArgs) {
				opts.Effort = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--effort="):
			opts.Effort = strings.TrimPrefix(arg, "--effort=")
		case arg == "--output-format":
			if i+1 < len(rawArgs) {
				opts.OutputFormat = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--output-format="):
			opts.OutputFormat = strings.TrimPrefix(arg, "--output-format=")
		case arg == "--add-dir":
			if i+1 < len(rawArgs) {
				opts.AddDirs = append(opts.AddDirs, rawArgs[i+1])
				i++
			}
		case strings.HasPrefix(arg, "--add-dir="):
			opts.AddDirs = append(opts.AddDirs, strings.TrimPrefix(arg, "--add-dir="))
		case arg == "--prompt":
			if i+1 < len(rawArgs) {
				opts.Prompt = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--prompt="):
			opts.Prompt = strings.TrimPrefix(arg, "--prompt=")
		case strings.HasPrefix(arg, "--print=") || strings.HasPrefix(arg, "-p="):
			parts := strings.SplitN(arg, "=", 2)
			if len(parts) > 1 {
				opts.Prompt = parts[1]
			}
		case arg == "--print" || arg == "-p":
			if i+1 < len(rawArgs) && !strings.HasPrefix(rawArgs[i+1], "-") {
				opts.Prompt = rawArgs[i+1]
				i++
			}
		case arg == "--dangerously-skip-permissions":
			// Handled unconditionally
		case arg == "--verbose" || arg == "-v" || arg == "--strict-mcp-config" || arg == "--no-session-persistence" || arg == "--include-hook-events" || arg == "--include-partial-messages" || arg == "--replay-user-messages" || arg == "--restricted" || arg == "--safe-mode" || arg == "--chrome":
			// Drop unsupported verbose and strict flags
		case arg == "--mcp-config" || arg == "--config-dir" || arg == "--cwd" || arg == "--settings" || arg == "--permission-mode" || arg == "--permission-prompts" || arg == "--setting-sources" || arg == "--max-turns" || arg == "--max-budget-usd" || arg == "--tools" || arg == "--system-prompt" || arg == "--system-prompt-snapshot" || arg == "--append-system-prompt-file":
			if i+1 < len(rawArgs) {
				i++ // Drop flag with its parameter
			}
		case arg == "--":
			if i+1 < len(rawArgs) {
				opts.Prompt = strings.Join(rawArgs[i+1:], " ")
			}
			break argLoop
		default:
			if !strings.HasPrefix(arg, "-") && opts.Prompt == "" {
				opts.Prompt = arg
			}
		}
	}

	if opts.Effort == "" {
		if strings.Contains(opts.Model, "gemini-3.1-pro") {
			opts.Effort = "high"
		} else if strings.Contains(opts.Model, "gemini-3.8-flash") {
			opts.Effort = "medium"
		}
	}

	return opts
}

// providerCandidate represents a single provider in the failover chain.
type providerCandidate struct {
	Name     string        // "gemini", "work-claude", "personal-claude"
	PoolID   router.PoolID // for quota lock check
	Adapter  ProviderAdapter
	ExtraEnv []string // e.g. CLAUDE_CONFIG_DIR for work claude
	// Slot is set for route-pinned candidates (RunRoute). Its Model, when
	// non-empty, overrides any --model in rawArgs for this candidate.
	Slot *router.RouteSlot
}

// isPoolLocked returns true if the quota pool cannot take work right now
// (hard lock, or spent 5h/weekly window). See router.PoolLockReason.
func isPoolLocked(pool *router.QuotaPool) bool {
	locked, _ := router.PoolLockReason(pool, time.Now())
	return locked
}

// BuildProviderChain constructs the ordered failover chain based on repo type and starting provider.
//
// Gemini is never a fallback (router.GeminiCodeForbidden): only an explicit
// provider=gemini (or agy) starts on it. Every other provider, including an
// empty or unknown one, is Claude only.
//
// Work repo chains:
//
//	provider=gemini: [gemini, work-claude, personal-claude]
//	otherwise:       [work-claude, personal-claude]
//
// Personal repo chains:
//
//	provider=gemini: [gemini, personal-claude]
//	otherwise:       [personal-claude]
//
// Opt-in provider (no failover):
//
//	provider=cloud_session: [cloud_session]  — burns $250 cloud credit, not quota
func BuildProviderChain(isWork bool, provider string) []providerCandidate {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}

	gemini := providerCandidate{
		Name:    "gemini",
		PoolID:  router.PoolGeminiNative,
		Adapter: AgyAdapter{},
	}

	workClaude := providerCandidate{
		Name:     "work-claude",
		PoolID:   router.PoolWorkClaude,
		Adapter:  ClaudeAdapter{},
		ExtraEnv: []string{"CLAUDE_CONFIG_DIR=" + home + "/.claude-work"},
	}

	personalClaude := providerCandidate{
		Name:    "personal-claude",
		PoolID:  router.PoolPersonalClaude,
		Adapter: ClaudeAdapter{},
	}

	// cloud_session is opt-in and never participates in failover chains.
	// It burns the $250 Anthropic-managed cloud credit, not local quota pools.
	if provider == "cloud_session" {
		return []providerCandidate{{
			Name:    "cloud_session",
			PoolID:  "cloud-session", // not tracked by pacer; isPoolLocked returns false for unknown pools
			Adapter: CloudSessionAdapter{},
		}}
	}

	explicitGemini := provider == "gemini" || provider == "agy"
	if isWork {
		if explicitGemini {
			return []providerCandidate{gemini, workClaude, personalClaude}
		}
		return []providerCandidate{workClaude, personalClaude}
	}

	// Personal repo
	if explicitGemini {
		return []providerCandidate{gemini, personalClaude}
	}
	return []providerCandidate{personalClaude}
}

// RunAdapter executes the requested agent provider with chain-based failover.
// Every invocation re-evaluates quota fresh. No sticky state between runs.
func RunAdapter(ctx context.Context, cwd string, pacerState *router.PacerState, provider string, rawArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	var resolve func(ProviderAdapter) (string, error)
	if testBin, ok := ctx.Value(testBinKey).(string); ok && testBin != "" {
		resolve = func(ProviderAdapter) (string, error) { return testBin, nil }
	} else {
		resolve = NewDefaultResolver().Resolve
	}
	return runWithFailover(ctx, cwd, pacerState, provider, rawArgs, stdin, stdout, stderr, resolve)
}

func runWithFailover(ctx context.Context, cwd string, pacerState *router.PacerState, provider string, rawArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, resolve func(ProviderAdapter) (string, error)) error {
	// Detect work vs personal repo
	isWork, workSrc, _ := router.IsWorkRepo(cwd)

	// Build the provider chain
	chain := BuildProviderChain(isWork, provider)

	if isWork && workSrc != "" {
		fmt.Fprintf(stderr, "[staypoint-adapter] work repo detected (%s), chain: ", workSrc)
	} else {
		fmt.Fprintf(stderr, "[staypoint-adapter] personal repo, chain: ")
	}
	return runChain(ctx, cwd, pacerState, chain, rawArgs, stdin, stdout, stderr, resolve)
}

// AttemptInfo describes one provider spawn inside a run, reported to the
// observer registered with WithAttemptObserver before the CLI starts.
type AttemptInfo struct {
	// Name is the candidate name ("personal-claude", "work-claude", "gemini").
	Name string
	// Provider is the adapter/stream-parser key ("claude", "gemini", "cloud_session").
	Provider string
	// Slot is the routed slot for RunRoute candidates; zero for legacy chains.
	Slot router.RouteSlot
	// FallbackReason is non-empty when an earlier planned candidate was skipped
	// or failed before this one: "work seat locked (5h limit)".
	FallbackReason string
}

type ctxAttemptObserverKey struct{}

// WithAttemptObserver registers fn to be called (synchronously, on the calling
// goroutine) before each provider spawn, so callers can label the run with the
// provider that actually runs and pick the matching stream parser.
func WithAttemptObserver(ctx context.Context, fn func(AttemptInfo)) context.Context {
	return context.WithValue(ctx, ctxAttemptObserverKey{}, fn)
}

func candidateProvider(c providerCandidate) string {
	switch c.Adapter.(type) {
	case ClaudeAdapter:
		return "claude"
	case CloudSessionAdapter:
		return "cloud_session"
	default:
		return "gemini"
	}
}

// candidateLabel is the human label used in fallback reasons.
func candidateLabel(c providerCandidate) string {
	if c.Slot != nil {
		return c.Slot.Label()
	}
	return candidateDisplayName(c.Name)
}

// candidateLockReason renders "work seat locked (5h limit)" for a locked candidate.
func candidateLockReason(c providerCandidate, why string) string {
	if c.Slot != nil {
		return c.Slot.LockedReason(why)
	}
	return candidateDisplayName(c.Name) + " locked (" + why + ")"
}

// splitModelEffort splits a routed model like "gemini-3.1-pro-high" into the
// CLI model and effort ("gemini-3.1-pro", "high"); Claude models pass through.
func splitModelEffort(model string) (string, string) {
	if m := modelEffortRe.FindStringSubmatch(model); m != nil {
		return m[1], m[2]
	}
	return model, ""
}

var modelEffortRe = regexp.MustCompile(`^(.*)-(low|medium|high)$`)

// CandidatesForRoute turns a resolved kind route into the pinned spawn chain:
// one candidate per viable slot, in route order, with the seat's env (work
// Claude gets CLAUDE_CONFIG_DIR=~/.claude-work) and the slot's model.
func CandidatesForRoute(r router.KindRoute) []providerCandidate {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	out := make([]providerCandidate, 0, len(r.Candidates))
	for i := range r.Candidates {
		slot := r.Candidates[i]
		c := providerCandidate{PoolID: slot.PoolID, Slot: &slot}
		switch slot.Family {
		case router.FamilyClaude:
			c.Adapter = ClaudeAdapter{}
			if slot.Seat == router.SeatWork {
				c.Name = "work-claude"
				c.ExtraEnv = []string{"CLAUDE_CONFIG_DIR=" + home + "/.claude-work"}
			} else {
				c.Name = "personal-claude"
			}
		case router.FamilyCloud:
			c.Name = "cloud_session"
			c.Adapter = CloudSessionAdapter{}
		default:
			c.Name = "gemini"
			c.Adapter = AgyAdapter{}
		}
		out = append(out, c)
	}
	return out
}

// RunRoute runs one turn on the chain pinned by a resolved kind route. Quota is
// re-checked against pacer at spawn time; a slot that locked since the route
// was resolved is skipped, and the switch is reported via WithAttemptObserver.
func RunRoute(ctx context.Context, cwd string, pacer *router.PacerState, route router.KindRoute, rawArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if route.AllLocked() {
		return fmt.Errorf("[staypoint-adapter] all providers locked: %s", route.Body())
	}
	var resolve func(ProviderAdapter) (string, error)
	if testBin, ok := ctx.Value(testBinKey).(string); ok && testBin != "" {
		resolve = func(ProviderAdapter) (string, error) { return testBin, nil }
	} else {
		resolve = NewDefaultResolver().Resolve
	}
	fmt.Fprintf(stderr, "[staypoint-adapter] routed (%s, work=%v), chain: ", route.Kind, route.IsWork)
	return runChain(ctx, cwd, pacer, CandidatesForRoute(route), rawArgs, stdin, stdout, stderr, resolve)
}

func runChain(ctx context.Context, cwd string, pacerState *router.PacerState, chain []providerCandidate, rawArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, resolve func(ProviderAdapter) (string, error)) error {
	opts := parseRawArgs(rawArgs)

	// Buffer stdin once so the fallback CLI receives the same input as the primary.
	var stdinBytes []byte
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err == nil && len(data) > 0 {
			stdinBytes = data
			if opts.Prompt == "" {
				opts.Prompt = string(data)
			}
		}
	}

	names := make([]string, len(chain))
	for i, c := range chain {
		names[i] = c.Name
		if c.Slot != nil && c.Slot.Model != "" {
			names[i] += "(" + c.Slot.Model + ")"
		}
	}
	fmt.Fprintf(stderr, "%s\n", strings.Join(names, " > "))

	observe, _ := ctx.Value(ctxAttemptObserverKey{}).(func(AttemptInfo))
	var fallbackReasons []string

	if pacerState == nil {
		pacerState = &router.PacerState{
			Pools: make(map[router.PoolID]*router.QuotaPool),
		}
	}

	// Merge harness-supplied extra env (sanitized upstream) with per-candidate env.
	var ctxEnv []string
	if v, ok := ctx.Value(ctxExtraEnvKey{}).([]string); ok {
		ctxEnv = v
	}

	var lastErr error
	firstAttempt := true
	for _, candidate := range chain {
		pool := pacerState.Pools[candidate.PoolID]
		if locked, why := router.PoolLockReason(pool, time.Now()); locked {
			fmt.Fprintf(stderr, "[staypoint-adapter] skipping %s (quota locked: %s)\n", candidate.Name, why)
			fallbackReasons = append(fallbackReasons, candidateLockReason(candidate, why))
			continue
		}

		// Clear conversation ID for fallback candidates.
		// Session IDs are provider-specific: a Claude session cannot be resumed in agy.
		candidateOpts := opts
		if !firstAttempt {
			candidateOpts.ConversationID = ""
		}
		firstAttempt = false
		if candidate.Slot != nil && candidate.Slot.Model != "" {
			candidateOpts.Model, candidateOpts.Effort = splitModelEffort(candidate.Slot.Model)
		}

		fmt.Fprintf(stderr, "[staypoint-adapter] trying %s...\n", candidate.Name)

		bin, err := resolve(candidate.Adapter)
		if err != nil {
			fmt.Fprintf(stderr, "[staypoint-adapter] %s failed to resolve (%v), trying next provider...\n", candidate.Name, err)
			fallbackReasons = append(fallbackReasons, candidateLabel(candidate)+" unavailable ("+err.Error()+")")
			lastErr = err
			continue
		}

		if observe != nil {
			info := AttemptInfo{
				Name:           candidate.Name,
				Provider:       candidateProvider(candidate),
				FallbackReason: strings.Join(fallbackReasons, "; "),
			}
			if candidate.Slot != nil {
				info.Slot = *candidate.Slot
			}
			observe(info)
		}

		extraEnv := make([]string, 0, len(ctxEnv)+len(candidate.ExtraEnv))
		extraEnv = append(extraEnv, ctxEnv...)
		extraEnv = append(extraEnv, candidate.ExtraEnv...)

		// Run the candidate with a pipe so we can stream to stdout as the
		// provider emits events. We commit (start forwarding) on the first
		// assistant-class event. Before that point the output is held in a
		// small buffer so we can still fall back to the next candidate on a
		// pre-commit failure.
		pr, pw := io.Pipe()
		execErrCh := make(chan error, 1)
		go func(c providerCandidate, b string, opts ParsedOptions, env []string) {
			e := c.Adapter.Execute(ctx, ExecRequest{
				Bin:      b,
				Dir:      cwd,
				Opts:     opts,
				ExtraEnv: env,
				Stdin:    bytes.NewReader(stdinBytes),
				Stdout:   pw,
				Stderr:   stderr,
			})
			execErrCh <- e
			if e != nil {
				pw.CloseWithError(e)
			} else {
				pw.Close()
			}
		}(candidate, bin, candidateOpts, extraEnv)

		parse := candidate.Adapter.ParseStreamDelta
		committed, prebuf := streamWithCommit(pr, stdout, func(line []byte) bool {
			return isAssistantEvent(line, parse)
		})
		execErr := <-execErrCh

		if committed || execErr == nil {
			if !committed && len(prebuf) > 0 {
				// Provider succeeded without a commit-class event: forward whatever it wrote.
				_, _ = stdout.Write(prebuf)
			}
			return execErr
		}

		// Provider failed before committing: safe to fall back.
		fmt.Fprintf(stderr, "[staypoint-adapter] %s failed (%v), trying next provider...\n", candidate.Name, execErr)
		fallbackReasons = append(fallbackReasons, candidateLabel(candidate)+" failed ("+execErr.Error()+")")
		lastErr = execErr
	}

	// All candidates exhausted
	if lastErr != nil {
		return fmt.Errorf("[staypoint-adapter] all providers exhausted: last error: %w", lastErr)
	}
	return fmt.Errorf("[staypoint-adapter] all providers exhausted or locked for this environment")
}

// ChainResolution describes which provider was selected for a run.
type ChainResolution struct {
	// SelectedDisplay is the human-readable name of the candidate that will run.
	SelectedDisplay string
	// FallbackFromDisplay is the primary candidate's name when a fallback
	// occurred (i.e. the primary was quota-locked). Empty when no fallback.
	FallbackFromDisplay string
	// AllLocked is set when every candidate in the chain is quota-locked.
	AllLocked bool
	// IsCloud is set when the selected candidate is the cloud_session provider.
	IsCloud bool
}

// candidateDisplayName maps a providerCandidate.Name to a human-readable label.
func candidateDisplayName(name string) string {
	switch name {
	case "gemini":
		return "Gemini"
	case "work-claude":
		return "Claude (work)"
	case "personal-claude":
		return "Claude"
	case "cloud_session":
		return "Claude Cloud"
	default:
		return name
	}
}

// ResolveProviderChain builds the failover chain for isWork+provider, walks it
// using the supplied pacer quota state (nil = no locks), and returns which
// candidate would actually run. This is the single source of truth for the
// route-row label so it matches what RunAdapter executes.
func ResolveProviderChain(isWork bool, provider string, pacer *router.PacerState) ChainResolution {
	chain := BuildProviderChain(isWork, provider)
	if len(chain) == 0 {
		return ChainResolution{AllLocked: true}
	}
	for i, c := range chain {
		pool := (*router.QuotaPool)(nil)
		if pacer != nil {
			pool = pacer.Pools[c.PoolID]
		}
		if isPoolLocked(pool) {
			continue
		}
		res := ChainResolution{
			SelectedDisplay: candidateDisplayName(c.Name),
			IsCloud:         c.Name == "cloud_session",
		}
		if i > 0 {
			res.FallbackFromDisplay = candidateDisplayName(chain[0].Name)
		}
		return res
	}
	return ChainResolution{AllLocked: true}
}
