package server

// Pull Requests page (task-9fb380ef). GET lists the open PRs of every repo
// StayPoint tasks work in; the merge and combine actions are Board actions
// (routed through WrapBoardAction: Board session cookie + passkey, the same
// gate ship review's Approve uses), so an agent token alone is refused.
// GitHub is reached through shipreview.GHAuth, the repo's own gh login or the
// project's gh_config_dir; no new token source.

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// PRClient is the GitHub surface the Pull Requests page needs.
// shipreview.GHAuth implements it; tests swap in a fake.
type PRClient interface {
	ListOpenPRs(ctx gocontext.Context) ([]shipreview.OpenPR, error)
	MergePRAt(ctx gocontext.Context, number int, headSHA string) error
	CombinePRs(ctx gocontext.Context, req shipreview.CombineRequest) (*shipreview.CombineResult, error)
}

// PRClientFactory returns the PR client for a local repo checkout.
type PRClientFactory func(db *sql.DB, repoDir string) (PRClient, error)

// defaultPRClient resolves the repo's gh identity exactly as ship review does.
func defaultPRClient(db *sql.DB, repoDir string) (PRClient, error) {
	cfg, err := shipreview.GetProjectDevConfig(db, repoDir)
	if err != nil {
		return nil, fmt.Errorf("load project config: %w", err)
	}
	auth, err := shipreview.ResolveGHAuth(cfg, repoDir)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

// prActionTimeout bounds one merge/combine request; each gh call inside has
// its own shorter timeout.
const prActionTimeout = 10 * time.Minute

// PullRequestsHandler serves /api/pull-requests*.
type PullRequestsHandler struct {
	db *sql.DB

	mu      sync.Mutex
	factory PRClientFactory
}

// NewPullRequestsHandler returns a handler using the gh-backed client.
func NewPullRequestsHandler(db *sql.DB) *PullRequestsHandler {
	return &PullRequestsHandler{db: db, factory: defaultPRClient}
}

func (h *PullRequestsHandler) setFactory(f PRClientFactory) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f == nil {
		f = defaultPRClient
	}
	h.factory = f
}

func (h *PullRequestsHandler) client(repoDir string) (PRClient, error) {
	h.mu.Lock()
	f := h.factory
	h.mu.Unlock()
	return f(h.db, repoDir)
}

// stayPointRepoNames are the repo folder names the page always lists.
var stayPointRepoNames = map[string]bool{"agent-mesh": true, "staypoint": true}

// prRepos is the set of local repos the page covers: StayPoint's own repo
// (any task repo named agent-mesh or staypoint) plus every repo of a task
// that registered a pr work product. Only existing directories count. The
// set is also the allow-list for the action endpoints.
func (h *PullRequestsHandler) prRepos() ([]string, error) {
	rows, err := h.db.Query(`
		SELECT DISTINCT t.repo_path,
		       EXISTS (SELECT 1 FROM task_work_products wp WHERE wp.task_id = t.id AND wp.product_type IN ('pr', 'pull_request'))
		FROM tasks t
		WHERE COALESCE(t.repo_path, '') != '' AND t.status != 'soft_deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var repo string
		var hasPR bool
		if err := rows.Scan(&repo, &hasPR); err != nil {
			return nil, err
		}
		repo = filepath.Clean(repo)
		if seen[repo] {
			continue
		}
		if !hasPR && !stayPointRepoNames[strings.ToLower(filepath.Base(repo))] {
			continue
		}
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			continue
		}
		seen[repo] = true
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// allowedRepo reports whether repo is one the page lists.
func (h *PullRequestsHandler) allowedRepo(repo string) (string, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", false
	}
	repo = filepath.Clean(repo)
	repos, err := h.prRepos()
	if err != nil {
		return "", false
	}
	for _, r := range repos {
		if r == repo {
			return r, true
		}
	}
	return "", false
}

// LinkedTask is the StayPoint task a PR belongs to.
type LinkedTask struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Stage string `json:"stage"`
}

// prLinks maps PR URLs and branch names to tasks in repo.
type prLinks struct {
	byURL    map[string]LinkedTask
	byBranch map[string]LinkedTask
}

var prRefNumberRe = regexp.MustCompile(`/pull/(\d+)`)

func normPRURL(u string) string {
	u = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(u), "/"))
	if i := strings.IndexAny(u, "#?"); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(strings.TrimSuffix(u, "/files"))
}

func (h *PullRequestsHandler) links(repo string) prLinks {
	l := prLinks{byURL: map[string]LinkedTask{}, byBranch: map[string]LinkedTask{}}
	rows, err := h.db.Query(`
		SELECT wp.reference, t.id, t.name, COALESCE(t.execution_stage, '')
		FROM task_work_products wp JOIN tasks t ON t.id = wp.task_id
		WHERE wp.product_type IN ('pr', 'pull_request') AND t.status != 'soft_deleted'
		ORDER BY wp.id`)
	if err == nil {
		for rows.Next() {
			var ref string
			var lt LinkedTask
			if rows.Scan(&ref, &lt.ID, &lt.Name, &lt.Stage) == nil && prRefNumberRe.MatchString(ref) {
				l.byURL[normPRURL(ref)] = lt
			}
		}
		rows.Close()
	}
	rows, err = h.db.Query(`
		SELECT id, name, COALESCE(execution_stage, '')
		FROM tasks WHERE repo_path = ? AND status != 'soft_deleted'`, repo)
	if err == nil {
		// Task runs work on staypoint/<task id> (ship review's branch).
		for rows.Next() {
			var lt LinkedTask
			if rows.Scan(&lt.ID, &lt.Name, &lt.Stage) == nil {
				l.byBranch["staypoint/"+lt.ID] = lt
			}
		}
		rows.Close()
	}
	return l
}

func (l prLinks) find(pr shipreview.OpenPR) *LinkedTask {
	if lt, ok := l.byURL[normPRURL(pr.URL)]; ok {
		return &lt
	}
	if pr.IsCrossRepo {
		return nil
	}
	if lt, ok := l.byBranch[pr.HeadRefName]; ok {
		return &lt
	}
	return nil
}

type pullRequestView struct {
	shipreview.OpenPR
	AgeSec int         `json:"age_sec"`
	Task   *LinkedTask `json:"task,omitempty"`
}

type prRepoView struct {
	RepoPath string            `json:"repo_path"`
	Name     string            `json:"name"`
	PRs      []pullRequestView `json:"prs"`
	Error    string            `json:"error,omitempty"`
}

// List handles GET /api/pull-requests: open PRs per repo. A repo whose gh
// call fails is listed with its error, not dropped.
func (h *PullRequestsHandler) List(w http.ResponseWriter, r *http.Request) {
	repos, err := h.prRepos()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list repos: "+err.Error())
		return
	}
	now := time.Now().UTC()
	out := make([]prRepoView, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func(i int, repo string) {
			defer wg.Done()
			view := prRepoView{RepoPath: repo, Name: filepath.Base(repo), PRs: []pullRequestView{}}
			defer func() { out[i] = view }()
			c, err := h.client(repo)
			if err != nil {
				view.Error = err.Error()
				return
			}
			prs, err := c.ListOpenPRs(r.Context())
			if err != nil {
				view.Error = err.Error()
				return
			}
			links := h.links(repo)
			for _, pr := range prs {
				pv := pullRequestView{OpenPR: pr, Task: links.find(pr)}
				if t, err := time.Parse(time.RFC3339, pr.CreatedAt); err == nil {
					pv.AgeSec = int(now.Sub(t).Seconds())
				}
				view.PRs = append(view.PRs, pv)
			}
		}(i, repo)
	}
	wg.Wait()
	writeJSONStatus(w, http.StatusOK, map[string]any{"repos": out})
}

type prPin struct {
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
	Title   string `json:"title,omitempty"`
}

type prActionRequest struct {
	RepoPath string  `json:"repo_path"`
	PRs      []prPin `json:"prs"`
	// Combine only.
	Base  string `json:"base"`
	Title string `json:"title"`
}

// decodeAction reads and validates an action body. On failure it has written
// the response.
func (h *PullRequestsHandler) decodeAction(w http.ResponseWriter, r *http.Request, minPRs int) (*prActionRequest, PRClient, bool) {
	var req prActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return nil, nil, false
	}
	if len(req.PRs) < minPRs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("select at least %d pull request(s)", minPRs))
		return nil, nil, false
	}
	seen := map[int]bool{}
	for _, p := range req.PRs {
		if p.Number <= 0 || strings.TrimSpace(p.HeadSHA) == "" {
			writeError(w, http.StatusBadRequest, "every pull request needs number and head_sha (the head shown on the page)")
			return nil, nil, false
		}
		if seen[p.Number] {
			writeError(w, http.StatusBadRequest, "pull request #"+strconv.Itoa(p.Number)+" is listed twice")
			return nil, nil, false
		}
		seen[p.Number] = true
	}
	repo, ok := h.allowedRepo(req.RepoPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "repo_path is not one of the repos on the Pull Requests page")
		return nil, nil, false
	}
	req.RepoPath = repo
	c, err := h.client(repo)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "gh_auth", "message": err.Error()})
		return nil, nil, false
	}
	return &req, c, true
}

func mergeErrorCode(err error) string {
	switch {
	case errors.Is(err, shipreview.ErrHeadMoved):
		return "head_moved"
	case errors.Is(err, shipreview.ErrPRNotMergeable):
		return "not_mergeable"
	case errors.Is(err, shipreview.ErrPRNotOpen):
		return "not_open"
	case errors.Is(err, shipreview.ErrGitHubRefused):
		return "github_refused"
	}
	return "merge_failed"
}

type prFailure struct {
	Number  int    `json:"number"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Merge handles POST /api/pull-requests/merge (Board action):
// {"repo_path", "prs":[{"number","head_sha"}]}. The PRs merge one at a time
// in the order given, each pinned to its head_sha and re-checked for
// mergeability right before its merge. The first failure stops the batch;
// the response lists what merged, what failed and what was not attempted.
func (h *PullRequestsHandler) Merge(w http.ResponseWriter, r *http.Request) {
	req, c, ok := h.decodeAction(w, r, 1)
	if !ok {
		return
	}
	ctx, cancel := gocontext.WithTimeout(r.Context(), prActionTimeout)
	defer cancel()
	merged := []int{}
	notAttempted := []int{}
	var failed *prFailure
	for i, p := range req.PRs {
		if failed != nil {
			notAttempted = append(notAttempted, p.Number)
			continue
		}
		if err := c.MergePRAt(ctx, p.Number, p.HeadSHA); err != nil {
			failed = &prFailure{Number: p.Number, Error: mergeErrorCode(err), Message: err.Error()}
			for _, rest := range req.PRs[i+1:] {
				notAttempted = append(notAttempted, rest.Number)
			}
			break
		}
		merged = append(merged, p.Number)
	}
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
		"action": "pull_requests_merge", "repo_path": req.RepoPath, "requested": req.PRs,
		"merged": merged, "failed": failed, "not_attempted": notAttempted,
	})
	status := http.StatusOK
	if failed != nil {
		status = http.StatusConflict
		if failed.Error == "merge_failed" || failed.Error == "github_refused" {
			status = http.StatusBadGateway
		}
	}
	writeJSONStatus(w, status, map[string]any{
		"merged": merged, "failed": failed, "not_attempted": notAttempted,
	})
}

var combineBranchUnsafe = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)

// Combine handles POST /api/pull-requests/combine (Board action):
// {"repo_path", "base", "title", "prs":[{"number","head_sha","title"}]}. It
// builds a new integration branch from base, merges each PR head in order,
// pushes it and opens one PR whose body lists the source PRs. A conflict
// stops it before anything is pushed. Source PRs are left open.
func (h *PullRequestsHandler) Combine(w http.ResponseWriter, r *http.Request) {
	req, c, ok := h.decodeAction(w, r, 2)
	if !ok {
		return
	}
	base := strings.TrimSpace(req.Base)
	if base == "" {
		base = "main"
	}
	nums := make([]string, len(req.PRs))
	sources := make([]shipreview.CombineSource, len(req.PRs))
	for i, p := range req.PRs {
		nums[i] = strconv.Itoa(p.Number)
		sources[i] = shipreview.CombineSource{Number: p.Number, HeadSHA: strings.TrimSpace(p.HeadSHA), Title: p.Title}
	}
	branch := combineBranchUnsafe.ReplaceAllString(fmt.Sprintf("staypoint/combine-%s-%s-%d", base, strings.Join(nums, "-"), time.Now().Unix()), "-")
	ctx, cancel := gocontext.WithTimeout(r.Context(), prActionTimeout)
	defer cancel()
	res, err := c.CombinePRs(ctx, shipreview.CombineRequest{Base: base, Branch: branch, Title: req.Title, Sources: sources})
	audit := map[string]any{"action": "pull_requests_combine", "repo_path": req.RepoPath, "base": base, "branch": branch, "sources": sources}
	if err != nil {
		audit["error"] = err.Error()
		_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, audit)
		body := map[string]any{"error": "combine_failed", "message": err.Error(), "branch": branch}
		var ce *shipreview.CombineError
		if errors.As(err, &ce) {
			body["failed_pr"] = ce.Number
			switch {
			case errors.Is(err, shipreview.ErrCombineConflict):
				body["error"] = "conflict"
			case errors.Is(err, shipreview.ErrHeadMoved):
				body["error"] = "head_moved"
			}
		}
		writeJSONStatus(w, http.StatusConflict, body)
		return
	}
	audit["pr_url"] = res.URL
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, audit)
	writeJSONStatus(w, http.StatusCreated, map[string]any{"pr": res, "sources": sources})
}
