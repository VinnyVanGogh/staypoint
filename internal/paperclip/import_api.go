package paperclip

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Read-only endpoints used by `staypoint import paperclip`.

// OpenIssueStatuses are the Paperclip statuses an import brings over:
// everything except done and cancelled.
var OpenIssueStatuses = []string{"backlog", "todo", "in_progress", "in_review", "blocked"}

// IssuePageSize is the page size ListIssues requests. The issue list
// endpoint defaults to (and caps at) 500 rows when no limit is given, so a
// company with more issues needs limit/offset paging.
const IssuePageSize = 200

// ImportIssue is the subset of a Paperclip issue an import keeps.
type ImportIssue struct {
	ID                   string `json:"id"`
	Identifier           string `json:"identifier"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	DescriptionTruncated bool   `json:"descriptionTruncated"`
	Status               string `json:"status"`
	Priority             string `json:"priority"`
	CompanyID            string `json:"companyId"`
	ProjectID            string `json:"projectId"`
	ParentID             string `json:"parentId"`
	IssueNumber          int    `json:"issueNumber"`
}

// ImportProject is a Paperclip project with the fields that locate its repo.
type ImportProject struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	PrimaryWorkspace *struct {
		Cwd string `json:"cwd"`
	} `json:"primaryWorkspace"`
	Codebase *struct {
		LocalFolder string `json:"localFolder"`
	} `json:"codebase"`
}

// RepoPath is the project's local checkout: the primary workspace's cwd,
// else the codebase's configured local folder. Paperclip's own managed
// folders (~/.paperclip/...) are not used. Empty when neither is set.
func (p ImportProject) RepoPath() string {
	if p.PrimaryWorkspace != nil && strings.TrimSpace(p.PrimaryWorkspace.Cwd) != "" {
		return strings.TrimSpace(p.PrimaryWorkspace.Cwd)
	}
	if p.Codebase != nil && strings.TrimSpace(p.Codebase.LocalFolder) != "" {
		return strings.TrimSpace(p.Codebase.LocalFolder)
	}
	return ""
}

// getJSON GETs path (relative to BaseURL) into out, retrying once without the
// agent key on 401/403 (local board mode), like the other client calls.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	do := func(withKey bool) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
		if err != nil {
			return nil, err
		}
		if withKey && c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		return c.HTTPClient.Do(req)
	}
	resp, err := do(true)
	if err != nil {
		return fmt.Errorf("paperclip connection error: %w", err)
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.APIKey != "" {
		resp.Body.Close()
		if resp, err = do(false); err != nil {
			return fmt.Errorf("paperclip connection error: %w", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

// ListIssues returns every issue of companyID in statuses, paging with
// limit/offset until a short page. It fails rather than loop if the server
// ignores offset (a page that repeats an issue already seen).
func (c *Client) ListIssues(ctx context.Context, companyID string, statuses []string) ([]ImportIssue, error) {
	var all []ImportIssue
	seen := map[string]bool{}
	for offset := 0; ; offset += IssuePageSize {
		q := url.Values{}
		q.Set("status", strings.Join(statuses, ","))
		q.Set("limit", fmt.Sprint(IssuePageSize))
		q.Set("offset", fmt.Sprint(offset))
		var page []ImportIssue
		if err := c.getJSON(ctx, "/api/companies/"+url.PathEscape(companyID)+"/issues?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, iss := range page {
			if seen[iss.ID] {
				return nil, fmt.Errorf("issue list for %s repeated %s at offset %d: server is not paging", companyID, iss.Identifier, offset)
			}
			seen[iss.ID] = true
			all = append(all, iss)
		}
		if len(page) < IssuePageSize {
			return all, nil
		}
	}
}

// GetIssue fetches one issue in full (the list endpoint truncates long
// descriptions and sets descriptionTruncated).
func (c *Client) GetIssue(ctx context.Context, issueID string) (*ImportIssue, error) {
	var iss ImportIssue
	if err := c.getJSON(ctx, "/api/issues/"+url.PathEscape(issueID), &iss); err != nil {
		return nil, err
	}
	return &iss, nil
}

// ListImportProjects returns a company's projects with workspace fields.
func (c *Client) ListImportProjects(ctx context.Context, companyID string) ([]ImportProject, error) {
	var projects []ImportProject
	if err := c.getJSON(ctx, "/api/companies/"+url.PathEscape(companyID)+"/projects", &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// ListImportCompanies returns all companies.
func (c *Client) ListImportCompanies(ctx context.Context) ([]CompanyResponse, error) {
	var companies []CompanyResponse
	if err := c.getJSON(ctx, "/api/companies", &companies); err != nil {
		return nil, err
	}
	return companies, nil
}
