package router

import (
	stdcontext "context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// Tokyo Night Palette
const (
	Cyan    = "\033[38;2;125;207;255m"
	Blue    = "\033[38;2;122;162;247m"
	Purple  = "\033[38;2;187;154;247m"
	Green   = "\033[38;2;158;206;106m"
	Yellow  = "\033[38;2;224;175;104m"
	Red     = "\033[38;2;247;118;142m"
	Orange  = "\033[38;2;255;158;100m"
	Magenta = "\033[38;2;255;121;198m"
	Gray    = "\033[38;2;86;95;137m"
	Teal    = "\033[38;2;115;218;202m"
	Dim     = "\033[2m"
	Bold    = "\033[1m"
	Reset   = "\033[0m"

	BarBG    = "\033[48;2;0;0;0m"
	BarEmpty = "\033[38;2;54;60;88m"
	Sep      = " \033[38;2;86;95;137m│\033[0m "
)

type RGB struct {
	R, G, B int
}

var (
	RampCtxCool  = RGB{125, 207, 255}
	RampCtxHot   = RGB{198, 88, 196}
	RampFiveCool = RGB{255, 158, 100}
	RampFiveHot  = RGB{232, 93, 62}
	RampWeekCool = RGB{255, 121, 198}
	RampWeekHot  = RGB{168, 46, 88}

	cachedConfig     *config.Config
	cachedConfigTime time.Time
)

func getCachedConfig() *config.Config {
	if cachedConfig != nil && time.Since(cachedConfigTime) < 10*time.Second {
		return cachedConfig
	}
	cfg, err := config.LoadConfig()
	if err == nil {
		cachedConfig = cfg
		cachedConfigTime = time.Now()
	}
	return cachedConfig
}

type StatuslinePayload struct {
	Model struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	Cwd        string `json:"cwd"`
	SessionID  string `json:"session_id"`
	VimMode    string `json:"vim_mode"`
	RateLimits struct {
		FiveHour struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *float64 `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *float64 `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
	ContextWindow struct {
		UsedPercentage    *float64 `json:"used_percentage"`
		RemainingTokens   *int64   `json:"remaining_tokens"`
		ContextWindowSize *int64   `json:"context_window_size"`
	} `json:"context_window"`
	Cost struct {
		TotalCostUSD *float64 `json:"total_cost_usd"`
	} `json:"cost"`
	PlanTier string `json:"plan_tier"`
	Email    string `json:"email"`
}

type gitCacheInfo struct {
	Branch    string `json:"branch"`
	Dirty     string `json:"dirty"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	RemoteURL string `json:"remote_url"`
}

// BuildBar constructs a Tokyo Night progress bar with continuous non-linear color interpolation.
func BuildBar(pct int, cool, hot RGB, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	t := math.Pow(float64(pct)/100.0, 1.4)
	r := int(math.Round(float64(cool.R) + float64(hot.R-cool.R)*t))
	g := int(math.Round(float64(cool.G) + float64(hot.G-cool.G)*t))
	b := int(math.Round(float64(cool.B) + float64(hot.B-cool.B)*t))
	fill := fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)

	filled := pct * width / 100
	if pct > 0 && filled == 0 {
		filled = 1
	}
	empty := width - filled

	return fmt.Sprintf("%s%s▏%s%s%s%s%s▕%s",
		BarBG, Gray, fill, strings.Repeat("█", filled),
		BarEmpty, strings.Repeat("░", empty), Gray, Reset)
}

// FormatTokens formats token counts cleanly (e.g. 58k, 1.2M, 8.1B).
func FormatTokens(tokens int64) string {
	if tokens >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(tokens)/1_000_000_000.0)
	} else if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000.0)
	} else if tokens >= 1_000 {
		return fmt.Sprintf("%dk", tokens/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

func formatResetTime(t time.Time, includeDay bool) string {
	if t.IsZero() {
		return ""
	}
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err == nil {
		t = t.In(loc)
	}
	if includeDay {
		return strings.ToLower(t.Format("Mon 3:04pm"))
	}
	return strings.ToLower(t.Format("3:04pm"))
}

// fastGitInfo reads git branch and cache without launching slow subprocesses.
func fastGitInfo(dir string) (branch, dirty, sync string) {
	// 1. Check git cache in /tmp first (<0.1ms)
	h := md5.Sum([]byte(fmt.Sprintf("%d:%s", os.Getuid(), dir)))
	cacheKey := hex.EncodeToString(h[:8])
	cacheFile := filepath.Join(os.TempDir(), fmt.Sprintf("statusline-git-u%d-%s.cache", os.Getuid(), cacheKey))
	if stat, err := os.Stat(cacheFile); err == nil && time.Since(stat.ModTime()) < 20*time.Second {
		if data, err := os.ReadFile(cacheFile); err == nil {
			var gc gitCacheInfo
			if json.Unmarshal(data, &gc) == nil && gc.Branch != "" {
				s := ""
				if gc.Ahead > 0 {
					s += fmt.Sprintf("⇡%d", gc.Ahead)
				}
				if gc.Behind > 0 {
					s += fmt.Sprintf("⇣%d", gc.Behind)
				}
				return gc.Branch, gc.Dirty, s
			}
		}
	}

	// 2. Direct read of .git/HEAD (<0.05ms)
	gitDir := filepath.Join(dir, ".git")
	headPath := filepath.Join(gitDir, "HEAD")

	// Handle worktrees/submodules where .git is a file
	if fi, err := os.Stat(gitDir); err == nil && !fi.IsDir() {
		if content, err := os.ReadFile(gitDir); err == nil {
			line := strings.TrimSpace(string(content))
			if strings.HasPrefix(line, "gitdir:") {
				rel := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(rel) {
					rel = filepath.Join(dir, rel)
				}
				headPath = filepath.Join(rel, "HEAD")
			}
		}
	}

	if headContent, err := os.ReadFile(headPath); err == nil {
		headStr := strings.TrimSpace(string(headContent))
		if strings.HasPrefix(headStr, "ref: refs/heads/") {
			branch = strings.TrimPrefix(headStr, "ref: refs/heads/")
		} else if len(headStr) >= 7 {
			branch = headStr[:7]
		}
	}

	return branch, "", ""
}

// RenderStatusline produces the Tokyo Night multi-line statusline in <5ms.
// Its plan line routes the current directory without the SSH probe.
func RenderStatusline(w io.Writer, r io.Reader) error {
	return RenderStatuslineFor(w, r, nil)
}

// RenderStatuslineFor is RenderStatusline with the plan line taken from a
// route decision the caller already made (the smart launch), so the banner and
// the launched tool are one answer. A nil decision routes here.
func RenderStatuslineFor(w io.Writer, r io.Reader, decision *RouteDecision) error {
	// 1. Read input payload if piped (e.g. from Claude Code) with 25ms timeout
	var payload StatuslinePayload
	hasPipedInput := false

	data := readPipedInput(r, 25*time.Millisecond)
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
		hasPipedInput = true
	}

	// 2. Determine directory
	dir := payload.Workspace.CurrentDir
	if dir == "" {
		dir = payload.Cwd
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	dirName := filepath.Base(dir)

	// 3. Load live pacer state (<1ms)
	pacerState, _ := LoadPacerState()

	// 4. Determine work vs personal repo
	isWork, _, _ := IsWorkRepo(dir)
	// The Claude seat this session is logged into comes from its
	// CLAUDE_CONFIG_DIR, not the repo: a work repo can run on the personal
	// seat and vice versa. Only a Claude session (piped payload) has one.
	seatIsWork := isWork
	if hasPipedInput {
		home, _ := os.UserHomeDir()
		seatIsWork = claudeSessionIsWorkSeat(os.Getenv("CLAUDE_CONFIG_DIR"), home, isWork)
	}

	// The route this directory takes. Without a session payload (a launch
	// banner) it also names the model and seat, so every line of the banner
	// is the same answer as the launch.
	if decision == nil && pacerState != nil {
		decision, _ = Route(stdcontext.Background(), dir, pacerState, RouteOptions{})
	}
	if !hasPipedInput && decision != nil && decision.AccountRole != "" {
		seatIsWork = decision.AccountRole == "work"
	}

	// 5. Build Model badge
	modelName := payload.Model.DisplayName
	if modelName == "" {
		modelName = payload.Model.ID
	}
	if modelName == "" {
		if hasPipedInput {
			modelName = "Claude"
		} else if decision != nil && decision.Model != "" {
			modelName = decision.Model
		} else if isWork {
			modelName = "Claude (Work)"
		} else {
			modelName = "Claude"
		}
	}

	modelIcon := "󰚩"
	if strings.Contains(strings.ToLower(modelName), "gemini") {
		modelIcon = "󰛡"
	}
	modelPart := fmt.Sprintf("%s %s%s%s", modelIcon, Cyan, modelName, Reset)

	// 6. Plan tier badge
	planTier := payload.PlanTier
	cfg := getCachedConfig()
	if planTier == "" || planTier == "Google AI Pro" {
		if strings.Contains(strings.ToLower(modelName), "gemini") {
			if cfg != nil && cfg.GooglePlanTier != "" {
				planTier = cfg.GooglePlanTier
			} else {
				planTier = "Google AI Ultra"
			}
		} else if cfg != nil && cfg.ClaudePlanTier != "" {
			planTier = cfg.ClaudePlanTier
		}
	}

	// 7. Account badge
	var accountBadge string
	if payload.Email != "" {
		accountBadge = fmt.Sprintf("🪪 %s%s%s", Gray, payload.Email, Reset)
	} else if seatIsWork {
		accountBadge = fmt.Sprintf("🪪 %swork%s", Teal, Reset)
	} else {
		accountBadge = fmt.Sprintf("🪪 %spersonal%s", Gray, Reset)
	}

	// 8. Cost badge
	var costBadge string
	if payload.Cost.TotalCostUSD != nil && *payload.Cost.TotalCostUSD > 0.001 {
		costBadge = fmt.Sprintf("💰 %s$%.2f%s", Yellow, *payload.Cost.TotalCostUSD, Reset)
	}

	// 9. Line 1
	line1Parts := []string{modelPart, accountBadge}
	if planTier != "" {
		line1Parts = append(line1Parts, fmt.Sprintf("%s✨ %s%s", Magenta, planTier, Reset))
	}
	if costBadge != "" {
		line1Parts = append(line1Parts, costBadge)
	}
	line1Parts = append(line1Parts, fmt.Sprintf("%s⚡ staypoint:active%s", Teal, Reset))

	// 9. Git info & badges for Line 2
	branch, dirty, sync := fastGitInfo(dir)
	var gitPart string
	if branch != "" {
		gitPart = fmt.Sprintf("%s🐙 %s%s", Purple, branch, Reset)
		if dirty != "" {
			gitPart += fmt.Sprintf(" %s", dirty)
		}
		if sync != "" {
			gitPart += fmt.Sprintf(" %s%s%s", Yellow, sync, Reset)
		}
	}

	line2Parts := []string{fmt.Sprintf("%s📁 %s%s", Blue, dirName, Reset)}
	if gitPart != "" {
		line2Parts = append(line2Parts, gitPart)
	}

	if payload.VimMode != "" {
		vimColor := Green
		if payload.VimMode != "INSERT" {
			vimColor = Blue
		}
		line2Parts = append(line2Parts, fmt.Sprintf("%s%s%s%s", vimColor, Bold, payload.VimMode, Reset))
	}

	// Pending code review badge for Claude
	if rev := getPendingReviewForClaude(dir); rev != nil {
		vCol := Green
		switch rev.Verdict {
		case "FAIL", "DEFECT", "REJECT":
			vCol = Red
		case "WARN", "WARNING", "CONCERN":
			vCol = Yellow
		}
		shaStr := ""
		if rev.SHA != "" {
			shaStr = fmt.Sprintf(" (%s)", rev.SHA)
		}
		line2Parts = append(line2Parts, fmt.Sprintf("%s%s⚖️ REVIEW: %s%s%s", vCol, Bold, rev.Verdict, shaStr, Reset))
	}

	// Check Caveman mode
	home, _ := os.UserHomeDir()
	cavemanFile := filepath.Join(home, ".claude", ".caveman-mode")
	if _, err := os.Stat(cavemanFile); err == nil {
		line2Parts = append(line2Parts, fmt.Sprintf("%s🦴 CAVEMAN%s", Yellow, Reset))
	}

	// Deploy drain (task-db71fba9): "Draining for deploy: N runs left, M queued".
	if label := drainLabel(filepath.Join(home, ".staypoint", "drain.json"), time.Now()); label != "" {
		line2Parts = append(line2Parts, fmt.Sprintf("%s%s🚧 %s%s", Yellow, Bold, label, Reset))
	}

	// 10. Quotas & Meters
	var fivePct, weekPct float64
	var fiveResetsAt, weekResetsAt time.Time
	var isLocked bool
	var lockoutLabel string

	// Select matching pool
	activePoolID := PoolPersonalClaude
	if seatIsWork {
		activePoolID = PoolWorkClaude
	} else if strings.Contains(strings.ToLower(modelName), "gemini") {
		activePoolID = PoolGeminiNative
	}

	if pacerState != nil && pacerState.Pools[activePoolID] != nil {
		p := pacerState.Pools[activePoolID]
		fivePct = p.FiveHour.UsedPct
		fiveResetsAt = p.FiveHour.ResetsAt
		weekPct = p.Weekly.UsedPct
		weekResetsAt = p.Weekly.ResetsAt
		isLocked = p.IsLocked
		if isLocked {
			lockoutLabel = formatResetTime(p.LockoutUntil, false)
			if lockoutLabel == "" {
				lockoutLabel = "soon"
			}
		}
	}

	// Override from Claude payload if provided
	if payload.RateLimits.FiveHour.UsedPercentage != nil {
		fivePct = *payload.RateLimits.FiveHour.UsedPercentage
		if payload.RateLimits.FiveHour.ResetsAt != nil && *payload.RateLimits.FiveHour.ResetsAt > 0 {
			fiveResetsAt = time.Unix(int64(*payload.RateLimits.FiveHour.ResetsAt), 0)
		}
	}
	if payload.RateLimits.SevenDay.UsedPercentage != nil {
		weekPct = *payload.RateLimits.SevenDay.UsedPercentage
		if payload.RateLimits.SevenDay.ResetsAt != nil && *payload.RateLimits.SevenDay.ResetsAt > 0 {
			weekResetsAt = time.Unix(int64(*payload.RateLimits.SevenDay.ResetsAt), 0)
		}
	}

	if fivePct >= 100.0 {
		isLocked = true
		lockoutLabel = formatResetTime(fiveResetsAt, false)
	}

	if isLocked {
		line2Parts = append(line2Parts, fmt.Sprintf("%s%s🔒 5H LOCKED (@%s)%s", Red, Bold, lockoutLabel, Reset))
		if !seatIsWork {
			line2Parts = append(line2Parts, fmt.Sprintf("%s%s[⚡ Switch -> agy / Gemini]%s", Yellow, Bold, Reset))
		}
	}

	// Print Line 1 & Line 2

	// 4.5. Get Active Task and Blocker Status
	activeTaskStr := ""
	if store, err := db.Open(config.DefaultConfig().DBPath); err == nil {
		if t, err := context.GetActiveTaskForRepo(store.DB(), dir); err == nil && t != nil {
			activeTaskStr = fmt.Sprintf(" %s[%s]%s", Dim, t.ID, Reset)
			if t.IsBlocked {
				activeTaskStr += fmt.Sprintf(" %s[BLOCKED]%s", Red, Reset)
			}
		}
		store.Close()
	}

	fmt.Fprintln(w, strings.Join(line1Parts, Sep))
	fmt.Fprintln(w, strings.Join(line2Parts, Sep))

	// Line 3: Context meter (if available in payload)
	if payload.ContextWindow.UsedPercentage != nil {
		ctxPct := int(math.Round(*payload.ContextWindow.UsedPercentage))
		remTokensStr := ""
		if payload.ContextWindow.RemainingTokens != nil {
			remTokensStr = fmt.Sprintf(" %s~%s left%s", Dim, FormatTokens(*payload.ContextWindow.RemainingTokens), Reset)
		}
		bar := BuildBar(ctxPct, RampCtxCool, RampCtxHot, 20)
		fmt.Fprintf(w, "%s %sctx:%d%%%s%s\n", bar, Cyan, ctxPct, Reset, remTokensStr)
	}

	// Line 4: 5-Hour Session meter
	fiveInt := int(math.Round(fivePct))
	rem5h := math.Max(0, 100.0-fivePct)
	reset5hStr := ""
	if rStr := formatResetTime(fiveResetsAt, false); rStr != "" {
		reset5hStr = fmt.Sprintf(" %s@%s%s", Dim, rStr, Reset)
	}
	bar5h := BuildBar(fiveInt, RampFiveCool, RampFiveHot, 20)
	fmt.Fprintf(w, "%s %ssession:%d%%%s %s~%.0f%% left%s%s\n",
		bar5h, Orange, fiveInt, Reset, Dim, rem5h, Reset, reset5hStr)

	// Line 5: Weekly meter
	weekInt := int(math.Round(weekPct))
	remW := math.Max(0, 100.0-weekPct)
	resetWStr := ""
	if rStr := formatResetTime(weekResetsAt, true); rStr != "" {
		resetWStr = fmt.Sprintf(" %s@%s%s", Dim, rStr, Reset)
	}
	barW := BuildBar(weekInt, RampWeekCool, RampWeekHot, 20)
	fmt.Fprintf(w, "%s %sweekly:%d%%%s %s~%.0f%% left%s%s\n",
		barW, Magenta, weekInt, Reset, Dim, remW, Reset, resetWStr)

	// Line 6: Plan / Dynamic Route line. It is the router's own decision, so
	// the banner never names a tool the launch will not run.
	if line := planLine(decision); line != "" {
		fmt.Fprintln(w, line)
	}

	return nil
}

// planLine renders the statusline "plan:" row for a route decision: the
// tool and model the launch runs, the seat, and whether it waits for a lock.
func planLine(d *RouteDecision) string {
	if d == nil || d.Tool == "" {
		return ""
	}
	color := Green
	if d.Waiting {
		color = Yellow
	}
	parts := []string{fmt.Sprintf("route ▸ %s%s (%s)%s", color, d.Model, d.Tool, Reset)}
	if d.AccountRole != "" {
		parts = append(parts, "seat: "+d.AccountRole)
	}
	if d.Waiting {
		parts = append(parts, fmt.Sprintf("%swaiting for a Claude seat%s", Yellow, Reset))
	} else if d.PacerState != nil {
		pool := PoolPersonalClaude
		if d.AccountRole == "work" {
			pool = PoolWorkClaude
		}
		if p := d.PacerState.Pools[pool]; p != nil {
			parts = append(parts, fmt.Sprintf("runway: %d turns", p.TurnsRunway))
		}
	}
	bullet := fmt.Sprintf(" %s·%s ", Gray, Reset)
	return fmt.Sprintf("%splan:%s %s", Green, Reset, strings.Join(parts, bullet))
}

// claudeSessionIsWorkSeat reports whether a Claude Code session runs on the
// work seat. CLAUDE_CONFIG_DIR is authoritative: unset is the default
// (personal) profile, ~/.claude-work is the work seat. Any other config dir is
// not one StayPoint manages, so the repo classification decides.
func claudeSessionIsWorkSeat(configDir, home string, isWorkRepo bool) bool {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return false
	}
	if home != "" && filepath.Clean(configDir) == filepath.Join(home, ".claude-work") {
		return true
	}
	return isWorkRepo
}

func readPipedInput(r io.Reader, timeout time.Duration) []byte {
	if r == nil {
		return nil
	}
	if f, ok := r.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil || (fi.Mode()&os.ModeCharDevice) != 0 {
			return nil
		}
	}

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data: data, err: err}
	}()

	select {
	case res := <-ch:
		if res.err == nil {
			return res.data
		}
		return nil
	case <-time.After(timeout):
		return nil
	}
}

type pendingReviewInfo struct {
	SHA     string
	Verdict string
}

func matchRepoPath(dir, recRepoPath, recRepo string) bool {
	if recRepoPath != "" {
		cleanDir := filepath.Clean(dir)
		cleanRP := filepath.Clean(recRepoPath)
		if cleanDir == cleanRP || strings.HasPrefix(cleanDir, cleanRP+string(filepath.Separator)) {
			return true
		}
		// When repo_path is explicitly set but does not match, do not fall back to basename.
		return false
	}
	// Fall back to repo basename only when repo_path is empty.
	if recRepo != "" {
		return strings.EqualFold(filepath.Base(dir), recRepo)
	}
	// If both are empty, never match.
	return false
}

func getPendingReviewFromDir(dir string, pendingDir string) *pendingReviewInfo {
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil
	}

	var newestTime time.Time
	var bestMatch *pendingReviewInfo

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(pendingDir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var rec struct {
			Repo      string      `json:"repo"`
			RepoPath  string      `json:"repo_path"`
			SHA       string      `json:"sha"`
			Verdict   string      `json:"verdict"`
			Severity  interface{} `json:"severity"`
			Mismatch  string      `json:"mismatch"`
			SessionID string      `json:"session_id"`
		}

		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}

		// session_id is intentionally not filtered here: any pending review for this
		// repository is surfaced on the statusline so the user sees it immediately.
		if !matchRepoPath(dir, rec.RepoPath, rec.Repo) {
			continue
		}

		if bestMatch == nil || fi.ModTime().After(newestTime) {
			verdict := strings.ToUpper(rec.Verdict)
			if verdict == "" {
				sev := 0
				switch v := rec.Severity.(type) {
				case float64:
					sev = int(v)
				case int:
					sev = v
				}

				if sev >= 3 {
					verdict = "FAIL"
				} else if sev >= 2 || strings.EqualFold(rec.Mismatch, "yes") {
					verdict = "WARN"
				} else if sev == 1 {
					verdict = "NIT"
				} else {
					verdict = "PASS"
				}
			}

			sha := rec.SHA
			if len(sha) > 7 {
				sha = sha[:7]
			}

			newestTime = fi.ModTime()
			bestMatch = &pendingReviewInfo{
				SHA:     sha,
				Verdict: verdict,
			}
		}
	}

	return bestMatch
}

func getPendingReviewForClaude(dir string) *pendingReviewInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	pendingDir := filepath.Join(home, ".claude", "reviews", "pending")
	return getPendingReviewFromDir(dir, pendingDir)
}

// drainStaleAfter: staypointd rewrites drain.json every 2s while it drains,
// so an older file was left by a daemon that died and is not shown.
const drainStaleAfter = 10 * time.Second

// drainLabel returns the daemon's deploy-drain line from path, or "" when the
// daemon is not draining (no file, unreadable, or stale).
func drainLabel(path string, now time.Time) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var st struct {
		Label     string    `json:"label"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if json.Unmarshal(b, &st) != nil || st.Label == "" {
		return ""
	}
	if now.Sub(st.UpdatedAt) > drainStaleAfter {
		return ""
	}
	return st.Label
}
