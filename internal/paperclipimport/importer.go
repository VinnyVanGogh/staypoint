// Package paperclipimport brings open Paperclip issues into StayPoint as
// parked backlog tasks (`staypoint import paperclip`, a Board command).
//
// Each Paperclip company gets one parent task, "Paperclip backlog —
// <Company>", in the matching StayPoint organization; its open issues become
// children of it (or of their imported Paperclip parent), all in backlog,
// unassigned, origin paperclip_import. Only the title (prefixed with the
// identifier), description, priority, company and a source reference
// (identifier + uuid) are kept. Re-running skips issues already imported.
package paperclipimport

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
)

// Source is the Paperclip API surface the import reads.
type Source interface {
	ListImportCompanies(ctx context.Context) ([]paperclip.CompanyResponse, error)
	ListImportProjects(ctx context.Context, companyID string) ([]paperclip.ImportProject, error)
	ListIssues(ctx context.Context, companyID string, statuses []string) ([]paperclip.ImportIssue, error)
	GetIssue(ctx context.Context, issueID string) (*paperclip.ImportIssue, error)
}

// organizationByPrefix maps Paperclip issue prefixes to the StayPoint
// organization names already in use. Unknown companies use their own name.
var organizationByPrefix = map[string]string{
	"STA": "StayPoint",
	"MAN": "Managed Solution",
	"RES": "Research",
	"PER": "Maintenance",
	"RUN": "RuneLite",
}

// workPrefixes are companies whose tasks run on the work account.
var workPrefixes = map[string]bool{"MAN": true}

// OrganizationFor returns the StayPoint organization for a company.
func OrganizationFor(c paperclip.CompanyResponse) string {
	if org, ok := organizationByPrefix[strings.ToUpper(c.IssuePrefix)]; ok {
		return org
	}
	return strings.TrimSpace(c.Name)
}

// ParentTitle is the name of a company's import parent task.
func ParentTitle(c paperclip.CompanyResponse) string {
	return "Paperclip backlog — " + strings.TrimSpace(c.Name)
}

// Options configures BuildPlan.
type Options struct {
	// Companies limits the import to these issue prefixes or company ids
	// (case-insensitive). Empty means every company.
	Companies []string
}

// PlannedIssue is one issue the import will create.
type PlannedIssue struct {
	Issue paperclip.ImportIssue
	// ParentIssueID is the Paperclip issue (uuid) to nest under; empty means
	// the company parent task.
	ParentIssueID string
	// FlattenedFrom is the identifier of the Paperclip parent the issue was
	// lifted out from because the nesting exceeded tasks.max_child_depth.
	FlattenedFrom string
	RepoPath      string
	depth         int
}

// CompanyPlan is the import plan for one company.
type CompanyPlan struct {
	Company      paperclip.CompanyResponse
	Organization string
	// ParentTaskID is the existing company parent task, if a previous import
	// created it.
	ParentTaskID    string
	Open            int
	ByStatus        map[string]int
	AlreadyImported int
	ToImport        []PlannedIssue
	Flattened       int
	WithRepo        int
}

// Plan is the whole import.
type Plan struct {
	Companies []CompanyPlan
	MaxDepth  int
}

// ToImport is the number of issues the plan creates across companies.
func (p *Plan) ToImport() int {
	n := 0
	for _, c := range p.Companies {
		n += len(c.ToImport)
	}
	return n
}

// importedTasks maps source_id -> task id for tasks a previous import made.
// A database without the source_id column (a read-only dry run against a
// daemon that has not migrated yet) has none.
func importedTasks(conn *sql.DB) (map[string]string, error) {
	out := map[string]string{}
	if !db.HasColumn(conn, "tasks", "source_id") {
		return out, nil
	}
	rows, err := conn.Query(`SELECT source_id, id FROM tasks WHERE source_id != ''`)
	if err != nil {
		return nil, fmt.Errorf("read imported tasks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, id string
		if err := rows.Scan(&src, &id); err != nil {
			return nil, err
		}
		out[src] = id
	}
	return out, rows.Err()
}

func wantCompany(c paperclip.CompanyResponse, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		f = strings.TrimSpace(f)
		if strings.EqualFold(f, c.IssuePrefix) || strings.EqualFold(f, c.ID) {
			return true
		}
	}
	return false
}

// BuildPlan reads Paperclip and the task database (which may be read-only)
// and returns what an import would create. It writes nothing.
func BuildPlan(ctx context.Context, src Source, conn *sql.DB, opts Options) (*Plan, error) {
	imported, err := importedTasks(conn)
	if err != nil {
		return nil, err
	}
	_, maxDepth := meshContext.ChildTaskLimits(conn)
	if maxDepth < 1 {
		maxDepth = 1
	}
	companies, err := src.ListImportCompanies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list companies: %w", err)
	}
	sort.Slice(companies, func(i, j int) bool { return companies[i].Name < companies[j].Name })

	plan := &Plan{MaxDepth: maxDepth}
	for _, c := range companies {
		if !wantCompany(c, opts.Companies) {
			continue
		}
		cp, err := planCompany(ctx, src, c, imported, maxDepth)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		plan.Companies = append(plan.Companies, *cp)
	}
	return plan, nil
}

func planCompany(ctx context.Context, src Source, c paperclip.CompanyResponse, imported map[string]string, maxDepth int) (*CompanyPlan, error) {
	issues, err := src.ListIssues(ctx, c.ID, paperclip.OpenIssueStatuses)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	projects, err := src.ListImportProjects(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	repoByProject := map[string]string{}
	for _, p := range projects {
		repoByProject[p.ID] = p.RepoPath()
	}

	cp := &CompanyPlan{
		Company:      c,
		Organization: OrganizationFor(c),
		ParentTaskID: imported[c.ID],
		ByStatus:     map[string]int{},
	}
	open := map[string]paperclip.ImportIssue{}
	for _, iss := range issues {
		if iss.Status == "done" || iss.Status == "cancelled" {
			continue // the server filters these; never trust that blindly
		}
		open[iss.ID] = iss
	}
	cp.Open = len(open)

	for _, iss := range open {
		cp.ByStatus[iss.Status]++
		if _, done := imported[iss.ID]; done {
			cp.AlreadyImported++
			continue
		}
		pi := PlannedIssue{Issue: iss, RepoPath: repoByProject[iss.ProjectID]}
		// Open ancestors, nearest first. Parents that are closed (or in
		// another company) are not imported, so the chain stops there.
		var chain []string
		seen := map[string]bool{iss.ID: true}
		for p := iss.ParentID; p != "" && !seen[p]; {
			parent, ok := open[p]
			if !ok {
				break
			}
			seen[p] = true
			chain = append(chain, p)
			p = parent.ParentID
		}
		// The company parent is depth 0, a top-level issue depth 1.
		pi.depth = 1 + len(chain)
		switch {
		case len(chain) == 0:
		case pi.depth <= maxDepth:
			pi.ParentIssueID = chain[0]
		default:
			// Too deep: hang it under the ancestor at depth maxDepth-1
			// (or the company parent when that is 0).
			pi.FlattenedFrom = open[chain[0]].Identifier
			if k := len(chain) - (maxDepth - 1); maxDepth > 1 {
				pi.ParentIssueID = chain[k]
			}
			pi.depth = maxDepth
			cp.Flattened++
		}
		if pi.RepoPath != "" {
			cp.WithRepo++
		}
		cp.ToImport = append(cp.ToImport, pi)
	}
	sort.Slice(cp.ToImport, func(i, j int) bool {
		a, b := cp.ToImport[i], cp.ToImport[j]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if a.Issue.IssueNumber != b.Issue.IssueNumber {
			return a.Issue.IssueNumber < b.Issue.IssueNumber
		}
		return a.Issue.Identifier < b.Issue.Identifier
	})
	return cp, nil
}

// TaskTitle is an imported task's name: "[STA-772] <title>".
func TaskTitle(iss paperclip.ImportIssue) string {
	title := strings.TrimSpace(iss.Title)
	if iss.Identifier == "" {
		return title
	}
	return "[" + iss.Identifier + "] " + title
}

// TaskDescription is the Paperclip description plus, for a flattened issue,
// a note naming the parent it was lifted out from.
func TaskDescription(pi PlannedIssue, maxDepth int) string {
	desc := strings.TrimSpace(pi.Issue.Description)
	if pi.FlattenedFrom != "" {
		note := fmt.Sprintf("Note (import): in Paperclip this is a child of %s. That nesting is deeper than tasks.max_child_depth (%d), so it was flattened to a higher level here.", pi.FlattenedFrom, maxDepth)
		if desc == "" {
			return note
		}
		return desc + "\n\n---\n" + note
	}
	return desc
}

// Result reports what Apply created.
type Result struct {
	ParentsCreated int
	TasksCreated   map[string]int // by organization
	Skipped        int            // imported concurrently / already present
}

// Apply creates the plan's tasks. It fetches the full description of every
// issue the list endpoint truncated, creates (or reuses) each company's
// parent task, then the issues parent-first. It bypasses tasks.max_children
// by inserting children directly; the depth cap was applied by BuildPlan.
// Safe to re-run: issues already imported are skipped.
func Apply(ctx context.Context, src Source, conn *sql.DB, plan *Plan, now time.Time) (*Result, error) {
	if !db.HasColumn(conn, "tasks", "source_id") {
		return nil, fmt.Errorf("task database is not migrated (tasks.source_id missing); open it with db.Open first")
	}
	if err := fillDescriptions(ctx, src, plan); err != nil {
		return nil, err
	}
	imported, err := importedTasks(conn)
	if err != nil {
		return nil, err
	}
	res := &Result{TasksCreated: map[string]int{}}
	for ci := range plan.Companies {
		cp := &plan.Companies[ci]
		if len(cp.ToImport) == 0 {
			continue
		}
		role := "personal"
		if workPrefixes[strings.ToUpper(cp.Company.IssuePrefix)] {
			role = "work"
		}
		parentID := imported[cp.Company.ID]
		if parentID == "" {
			parent, err := meshContext.CreateTaskWithOptions(conn, meshContext.TaskCreateOptions{
				Name:           ParentTitle(cp.Company),
				AccountRole:    role,
				Organization:   cp.Organization,
				ExecutionStage: governance.StageBacklog,
				Origin:         meshContext.OriginPaperclipImport,
				SourceRef:      cp.Company.IssuePrefix,
				SourceID:       cp.Company.ID,
				NoRepo:         true,
				Description: fmt.Sprintf("Open issues imported from the Paperclip company %s (%s) on %s. Each child is a parked backlog task; set its repo and move it to todo to run it.",
					cp.Company.Name, cp.Company.IssuePrefix, now.UTC().Format("2006-01-02")),
			})
			if err != nil {
				return res, fmt.Errorf("%s: create parent task: %w", cp.Company.Name, err)
			}
			parentID = parent.ID
			imported[cp.Company.ID] = parent.ID
			res.ParentsCreated++
		}
		cp.ParentTaskID = parentID

		branches := map[string]string{}
		for _, pi := range cp.ToImport {
			if _, done := imported[pi.Issue.ID]; done {
				res.Skipped++
				continue
			}
			attach := parentID
			if pi.ParentIssueID != "" {
				if id, ok := imported[pi.ParentIssueID]; ok {
					attach = id
				}
			}
			opts := meshContext.TaskCreateOptions{
				Name:           TaskTitle(pi.Issue),
				AccountRole:    role,
				Organization:   cp.Organization,
				ParentID:       attach,
				ExecutionStage: governance.StageBacklog,
				Origin:         meshContext.OriginPaperclipImport,
				Priority:       pi.Issue.Priority,
				SourceRef:      pi.Issue.Identifier,
				SourceID:       pi.Issue.ID,
				Description:    TaskDescription(pi, plan.MaxDepth),
			}
			if pi.RepoPath == "" {
				opts.NoRepo = true
			} else {
				opts.RepoPath = pi.RepoPath
				if _, ok := branches[pi.RepoPath]; !ok {
					branches[pi.RepoPath] = meshContext.GetCurrentGitBranch(pi.RepoPath)
				}
				opts.GitBranch = branches[pi.RepoPath]
			}
			task, err := meshContext.CreateTaskWithOptions(conn, opts)
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint failed") {
					res.Skipped++
					continue
				}
				return res, fmt.Errorf("%s: import %s: %w", cp.Company.Name, pi.Issue.Identifier, err)
			}
			imported[pi.Issue.ID] = task.ID
			res.TasksCreated[cp.Organization]++
		}
	}
	return res, nil
}

// fillDescriptions replaces truncated list descriptions with the full text,
// four requests at a time (the Paperclip API is slow).
func fillDescriptions(ctx context.Context, src Source, plan *Plan) error {
	type ref struct{ c, i int }
	var todo []ref
	for c := range plan.Companies {
		for i, pi := range plan.Companies[c].ToImport {
			if pi.Issue.DescriptionTruncated {
				todo = append(todo, ref{c, i})
			}
		}
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, 4)
	)
	for _, r := range todo {
		r := r
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pi := &plan.Companies[r.c].ToImport[r.i]
			full, err := src.GetIssue(ctx, pi.Issue.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("fetch %s description: %w", pi.Issue.Identifier, err)
				}
				return
			}
			pi.Issue.Description = full.Description
			pi.Issue.DescriptionTruncated = false
		}()
	}
	wg.Wait()
	return firstErr
}
