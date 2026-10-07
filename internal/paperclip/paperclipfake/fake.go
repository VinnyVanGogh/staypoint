// Package paperclipfake is an in-memory Paperclip API for tests: companies,
// projects, issue lists (status filter, limit/offset paging, a 500-row
// default cap and 1200-char description truncation, like the real server)
// and issue detail.
package paperclipfake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
)

// TruncateAt is where list responses cut descriptions.
const TruncateAt = 1200

// Server is a fake Paperclip API.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	companies []paperclip.CompanyResponse
	projects  map[string][]map[string]any
	issues    map[string][]paperclip.ImportIssue // by company id
	// ListCalls counts issue list requests; DetailCalls issue detail requests.
	ListCalls   int
	DetailCalls int
	// IgnoreOffset makes the list endpoint return page 1 for every offset.
	IgnoreOffset bool
}

// New starts a fake server, closed when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{projects: map[string][]map[string]any{}, issues: map[string][]paperclip.ImportIssue{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// AddCompany registers a company.
func (s *Server) AddCompany(id, name, prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.companies = append(s.companies, paperclip.CompanyResponse{ID: id, Name: name, IssuePrefix: prefix, Status: "active"})
}

// AddProject registers a project; cwd ("" for none) becomes its primary
// workspace and localFolder its codebase folder.
func (s *Server) AddProject(companyID, id, name, cwd, localFolder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := map[string]any{"id": id, "name": name, "primaryWorkspace": nil,
		"codebase": map[string]any{"localFolder": nilIfEmpty(localFolder), "managedFolder": "/Users/x/.paperclip/instances/default/projects/" + id}}
	if cwd != "" {
		p["primaryWorkspace"] = map[string]any{"cwd": cwd}
	}
	s.projects[companyID] = append(s.projects[companyID], p)
}

func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// AddIssue registers an issue under its CompanyID.
func (s *Server) AddIssue(iss paperclip.ImportIssue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[iss.CompanyID] = append(s.issues[iss.CompanyID], iss)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method != http.MethodGet:
		http.Error(w, `{"error":"read-only fake"}`, http.StatusMethodNotAllowed)
	case len(parts) == 2 && parts[1] == "companies":
		_ = json.NewEncoder(w).Encode(s.companies)
	case len(parts) == 4 && parts[1] == "companies" && parts[3] == "projects":
		list := s.projects[parts[2]]
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)
	case len(parts) == 4 && parts[1] == "companies" && parts[3] == "issues":
		s.ListCalls++
		_ = json.NewEncoder(w).Encode(s.list(parts[2], r))
	case len(parts) == 3 && parts[1] == "issues":
		s.DetailCalls++
		for _, list := range s.issues {
			for _, iss := range list {
				if iss.ID == parts[2] {
					_ = json.NewEncoder(w).Encode(iss)
					return
				}
			}
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	default:
		http.Error(w, `{"error":"no route"}`, http.StatusNotFound)
	}
}

func (s *Server) list(companyID string, r *http.Request) []paperclip.ImportIssue {
	want := map[string]bool{}
	if st := r.URL.Query().Get("status"); st != "" {
		for _, v := range strings.Split(st, ",") {
			want[v] = true
		}
	}
	var out []paperclip.ImportIssue
	for _, iss := range s.issues[companyID] {
		if len(want) > 0 && !want[iss.Status] {
			continue
		}
		if len(iss.Description) > TruncateAt {
			iss.Description = iss.Description[:TruncateAt]
			iss.DescriptionTruncated = true
		}
		out = append(out, iss)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IssueNumber > out[j].IssueNumber })
	limit := 500
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o > 0 && !s.IgnoreOffset {
		offset = o
	}
	if offset > len(out) {
		offset = len(out)
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	page := out[offset:end]
	if page == nil {
		page = []paperclip.ImportIssue{}
	}
	return page
}
