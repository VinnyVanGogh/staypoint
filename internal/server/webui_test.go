package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterUIRoutes_Root(t *testing.T) {
	mux := http.NewServeMux()
	const token = "test-tok-abc"
	RegisterUIRoutes(mux, token, nil)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "staypoint-token") {
		t.Error("response missing staypoint-token meta tag")
	}
	if !strings.Contains(body, token) {
		t.Errorf("response missing injected token %q", token)
	}
	if !strings.Contains(body, "StayPoint") {
		t.Error("response missing StayPoint title")
	}
}

func TestRegisterUIRoutes_StaticAssets(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "tok", nil)

	for _, path := range []string{"/ui/style.css", "/ui/app.js"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d", path, w.Code)
		}
	}
}

func TestRegisterUIRoutes_NotFoundForUnknownPath(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "tok", nil)

	req := httptest.NewRequest("GET", "/unknown/path", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown path, got %d", w.Code)
	}
}

func TestRegisterUIRoutes_TokenXSSEscape(t *testing.T) {
	mux := http.NewServeMux()
	// Token with characters that must be HTML-escaped in attribute context
	const maliciousToken = `"><script>alert(1)</script>`
	RegisterUIRoutes(mux, maliciousToken, nil)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("token was not HTML-escaped — XSS risk in meta content attribute")
	}
}

func TestRegisterUIRoutes_SPARoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	routes := []string{
		"/checklist",
		"/projects",
		"/agents",
		"/cost",
		"/task-status",
		"/recent-tasks",
		"/settings",
		"/org/StayPoint",
		"/tasks",
		"/issues",
		"/tasks/STA/default/STA-168",
		"/tasks/STA/default/STA-211",
		"/tasks/RES/research/RES-42",
		"/tasks/STA/STA-168",
		"/tasks/6f1222f9-85c6-4155-94c4-3867c2472b2d",
	}

	for _, route := range routes {
		req := httptest.NewRequest("GET", route, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for SPA route %q, got %d", route, w.Code)
		}
		if !strings.Contains(w.Body.String(), "staypoint-token") {
			t.Errorf("SPA route %q response missing staypoint-token meta tag", route)
		}
	}
}

func TestRegisterUIRoutes_HierarchicalTaskRoutes(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-hierarchical-tok", nil)

	// 1. Verify hierarchical SPA URL paths resolve with 200 OK
	hierarchicalPaths := []string{
		"/tasks/STA/default/STA-168",
		"/tasks/STA/default/STA-211",
		"/tasks/RES/core/RES-42",
		"/tasks/MAN/default/MAN-10",
		"/tasks/STA/task-4f810a72",
		"/issues/STA/default/STA-168",
	}

	for _, p := range hierarchicalPaths {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 OK for %q, got %d", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), "test-hierarchical-tok") {
			t.Errorf("expected token injection for %q", p)
		}
	}

	// 2. Verify app.js serves hierarchical URL routing and deep linking logic
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	appJS := wJS.Body.String()

	requiredPatterns := []string{
		"function taskToPath",
		"function findTask",
		"function pathToRoute",
		"/tasks/:org/:project/:identifier",
		"isFleetTaskId",
		"history.pushState",
		"history.replaceState",
	}

	for _, pattern := range requiredPatterns {
		if !strings.Contains(appJS, pattern) {
			t.Errorf("app.js missing required pattern %q for hierarchical routing", pattern)
		}
	}
}

func TestWebUI_DetailPanelPolishAndDismiss(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// Verify app.js serves required detail panel behavior
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	appJS := wJS.Body.String()

	// 1. Click-outside-to-dismiss behavior
	if !strings.Contains(appJS, "closeDetailPanel") {
		t.Error("app.js missing closeDetailPanel function")
	}
	if !strings.Contains(appJS, "lastDetailOpenTime") {
		t.Error("app.js missing lastDetailOpenTime guard against bubbling click dismissal")
	}

	// 2. The drawer renders the task page layout (STA-700): renderTaskPage in
	// drawer mode, stacked by the .task-page-drawer class and sized by a
	// container query on the drawer, not the viewport. A sequence guard keeps
	// a slow drawer load from rendering after a newer open.
	for _, pattern := range []string{
		"renderTaskPage(content, task, comments, interactions, undefined, undefined, task.runErrors, shipCard, { drawer: true })",
		"let taskViewSeq",
		"if (seq !== taskViewSeq) return;",
		"classList.add('task-page-drawer')",
	} {
		if !strings.Contains(appJS, pattern) {
			t.Errorf("app.js missing drawer layout pattern %q", pattern)
		}
	}
	// The old one-column drawer layout is gone.
	for _, gone := range []string{"function renderDetailContent", "function buildChatSection", "panel-chat-messages"} {
		if strings.Contains(appJS, gone) {
			t.Errorf("app.js still contains the old drawer layout: %q", gone)
		}
	}

	// Verify style.css serves the drawer layout rules
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for style.css, got %d", wCSS.Code)
	}
	styleCSS := wCSS.Body.String()

	for _, rule := range []string{
		"#panel-content.task-page-drawer",
		"container: task-drawer / inline-size",
		"@container task-drawer (min-width: 900px)",
	} {
		if !strings.Contains(styleCSS, rule) {
			t.Errorf("style.css missing drawer layout rule %q", rule)
		}
	}
}

func TestWebUI_SettingsQuotaAndFleetModal(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-token", nil)

	// 1. Verify index.html contains the modal markup
	reqRoot := httptest.NewRequest("GET", "/", nil)
	wRoot := httptest.NewRecorder()
	mux.ServeHTTP(wRoot, reqRoot)
	if wRoot.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wRoot.Code)
	}
	html := wRoot.Body.String()
	for _, expected := range []string{
		`id="fleet-modal"`,
		`id="fleet-modal-title"`,
		`id="modal-tab-orgs"`,
		`id="modal-tab-tasks"`,
		`id="modal-tab-agents"`,
		`id="fleet-modal-body"`,
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("index.html missing expected element %q", expected)
		}
	}

	// 2. Verify app.js contains quota telemetry breakdown & fleet modal functions
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	js := wJS.Body.String()
	for _, expected := range []string{
		"openFleetInfoModal",
		"closeFleetInfoModal",
		"switchFleetModalTab",
		"renderFleetModalOrganizations",
		"renderFleetModalTasks",
		"renderFleetModalAgents",
		"settings-provider-card",
		"settings-pool-box",
		"headroom-badge",
		"settings-row-clickable",
	} {
		if !strings.Contains(js, expected) {
			t.Errorf("app.js missing expected symbol/class %q", expected)
		}
	}

	// 3. Verify style.css contains modal and settings quota classes
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for style.css, got %d", wCSS.Code)
	}
	css := wCSS.Body.String()
	for _, expected := range []string{
		".modal-overlay",
		".modal-container",
		".modal-tabs",
		".modal-tab-btn",
		".settings-provider-card",
		".settings-pool-box",
		".headroom-badge",
		".settings-row-clickable",
		".settings-val-interactive",
	} {
		if !strings.Contains(css, expected) {
			t.Errorf("style.css missing expected CSS class %q", expected)
		}
	}
}

func TestRegisterUIRoutes_ProjectsViewElements(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify index.html contains projects multi-org controls and status filters
	req := httptest.NewRequest("GET", "/projects", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /projects, got %d", w.Code)
	}
	body := w.Body.String()
	requiredElements := []string{
		`id="projects-org-multiselect"`,
		`id="projects-org-multiselect-btn"`,
		`id="projects-org-menu"`,
		`id="projects-org-options"`,
		`id="projects-status-filter"`,
		`id="projects-grid"`,
		`class="projects-container"`,
	}
	for _, el := range requiredElements {
		if !strings.Contains(body, el) {
			t.Errorf("/projects response missing expected element: %s", el)
		}
	}

	// 2. Verify app.js contains multi-org and card status filtering logic
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/app.js, got %d", wJS.Code)
	}
	jsBody := wJS.Body.String()
	requiredJS := []string{
		"populateProjectsOrgFilter",
		"STORAGE_PROJECTS_ORGS_KEY",
		"STORAGE_PROJECTS_STATUS_KEY",
		"STORAGE_PROJECTS_CARD_STATUS_KEY",
		"project-org-group",
		"project-card-filter-pill",
		"project-stat-clickable",
	}
	for _, symbol := range requiredJS {
		if !strings.Contains(jsBody, symbol) {
			t.Errorf("/ui/app.js missing expected symbol: %s", symbol)
		}
	}

	// 3. Verify style.css contains styles for projects grouped headers and multi-select
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/style.css, got %d", wCSS.Code)
	}
	cssBody := wCSS.Body.String()
	requiredCSS := []string{
		".project-org-group",
		".project-org-header",
		".project-cards-subgrid",
		".project-card-filter-pill",
		".multiselect-dropdown",
		".multiselect-menu",
	}
	for _, selector := range requiredCSS {
		if !strings.Contains(cssBody, selector) {
			t.Errorf("/ui/style.css missing expected CSS selector: %s", selector)
		}
	}
}

func TestRegisterUIRoutes_STA194_ClickableKPIsAndBossCarousel(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify index.html contains clickable KPI cards for Running, Active, Blocked, Done, Total, Agents, Spend
	req := httptest.NewRequest("GET", "/overview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /overview, got %d", w.Code)
	}
	body := w.Body.String()
	requiredKPIs := []string{
		`id="kpi-card-running"`,
		`id="kpi-card-active"`,
		`id="kpi-card-blocked"`,
		`id="kpi-card-done"`,
		`id="kpi-card-total"`,
		`id="kpi-card-agents"`,
		`id="kpi-card-cost"`,
	}
	for _, kpi := range requiredKPIs {
		if !strings.Contains(body, kpi) {
			t.Errorf("expected overview to contain KPI card %s", kpi)
		}
	}

	// 2. Verify app.js contains drill-down navigation and Boss Card carousel logic
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/app.js, got %d", wJS.Code)
	}
	jsBody := wJS.Body.String()
	requiredJS := []string{
		"drillDownToTasks",
		"drillDownToAgents",
		"drillDownToCost",
		"buildBossReportCarousel",
		"openBossReportModal",
		"BOSS_REPORTS",
		"boss-carousel-card",
		"boss-preview-iframe",
	}
	for _, symbol := range requiredJS {
		if !strings.Contains(jsBody, symbol) {
			t.Errorf("expected app.js to contain %s", symbol)
		}
	}

	// 3. Verify style.css contains styles for clickable cards and boss report preview carousel
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/style.css, got %d", wCSS.Code)
	}
	cssBody := wCSS.Body.String()
	requiredCSS := []string{
		".kpi-card.clickable",
		".boss-carousel-card",
		".boss-carousel-nav",
		".boss-carousel-tabs",
		".boss-preview-sheet",
		".boss-preview-iframe",
		".boss-modal-backdrop",
	}
	for _, selector := range requiredCSS {
		if !strings.Contains(cssBody, selector) {
			t.Errorf("expected style.css to contain %s", selector)
		}
	}
}

func TestRegisterUIRoutes_TableSortingAndSearchElements(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Check table IDs
	if !strings.Contains(body, `id="overview-task-table"`) {
		t.Error("missing id=\"overview-task-table\" in HTML")
	}
	if !strings.Contains(body, `id="ts-task-table"`) {
		t.Error("missing id=\"ts-task-table\" in HTML")
	}

	// Check sortable columns
	expectedCols := []string{
		`data-col="identifier"`,
		`data-col="task"`,
		`data-col="organization"`,
		`data-col="status"`,
		`data-col="priority"`,
		`data-col="cost"`,
		`data-col="updated"`,
		`data-col="project"`,
		`data-col="assignee"`,
	}
	for _, col := range expectedCols {
		if !strings.Contains(body, col) {
			t.Errorf("missing sort column %s in HTML", col)
		}
	}

	// Check deep search placeholders
	if !strings.Contains(body, `placeholder="Search tasks, descriptions, comments, orgs…"`) {
		t.Error("missing deep search placeholder in search inputs")
	}

	// Check CSS includes sortable and snippet classes
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	css := wCSS.Body.String()
	if !strings.Contains(css, ".sortable-th") {
		t.Error("style.css missing .sortable-th")
	}
	if !strings.Contains(css, ".task-search-snippet") {
		t.Error("style.css missing .task-search-snippet")
	}

	// Check app.js includes sorting and deep search functions
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	js := wJS.Body.String()
	for _, fn := range []string{"getPrioritySeverity", "sortTasks", "renderTableSortHeaders", "getTaskSearchMatch"} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js missing %s function", fn)
		}
	}
}

func TestRegisterUIRoutes_RecentTasksSubtaskTreeAndFilterOrder(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify HTML: Recent Tasks dropdown filters order is Organization, Project, Priority
	req := httptest.NewRequest("GET", "/recent-tasks", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /recent-tasks, got %d", w.Code)
	}
	body := w.Body.String()

	orgIdx := strings.Index(body, `id="recent-tasks-org-filter"`)
	projIdx := strings.Index(body, `id="recent-tasks-project-filter"`)
	priIdx := strings.Index(body, `id="recent-tasks-priority-filter"`)

	if orgIdx == -1 {
		t.Error("missing id=\"recent-tasks-org-filter\" in HTML")
	}
	if projIdx == -1 {
		t.Error("missing id=\"recent-tasks-project-filter\" in HTML")
	}
	if priIdx == -1 {
		t.Error("missing id=\"recent-tasks-priority-filter\" in HTML")
	}

	if !(orgIdx < projIdx && projIdx < priIdx) {
		t.Errorf("expected dropdown order Organization < Project < Priority, got indices: org=%d, proj=%d, pri=%d",
			orgIdx, projIdx, priIdx)
	}

	// 2. Verify style.css includes subtask tree and connector styling
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	css := wCSS.Body.String()

	for _, selector := range []string{
		".activity-subtasks-tree",
		".activity-subtask-row",
		".activity-subtask-branch",
		".activity-subtask-dot",
		".activity-subtask-toggle",
		".activity-subtask-title",
		".activity-subtask-meta",
	} {
		if !strings.Contains(css, selector) {
			t.Errorf("style.css missing expected class %q", selector)
		}
	}

	// 3. Verify app.js includes subtask tree hierarchy, connectors, toggle, and row navigation
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	js := wJS.Body.String()

	for _, expected := range []string{
		"renderSubtaskTree",
		"renderRecentTasks",
		"activity-subtasks-tree",
		"activity-subtask-branch",
		"activity-subtask-dot",
		"activity-subtask-toggle",
		"├──",
		"└──",
		"[+]",
		"[-]",
		"openDetail",
	} {
		if !strings.Contains(js, expected) {
			t.Errorf("app.js missing expected symbol/pattern %q", expected)
		}
	}
}

func TestRegisterUIRoutes_STA208_TableSortingAndFilterControls(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify index.html contains standardized filter order across views
	reqHTML := httptest.NewRequest("GET", "/", nil)
	wHTML := httptest.NewRecorder()
	mux.ServeHTTP(wHTML, reqHTML)
	if wHTML.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wHTML.Code)
	}
	html := wHTML.Body.String()

	// Overview filter order: Organization -> Project -> Priority -> Status
	idxTaskOrg := strings.Index(html, `id="task-org-filter"`)
	idxTaskProj := strings.Index(html, `id="task-project-filter"`)
	idxTaskPri := strings.Index(html, `id="task-priority-filter"`)
	idxTaskStat := strings.Index(html, `id="task-status-filter"`)
	if idxTaskOrg == -1 || idxTaskProj == -1 || idxTaskPri == -1 || idxTaskStat == -1 {
		t.Fatalf("overview filter controls missing in index.html (org=%d, proj=%d, pri=%d, stat=%d)",
			idxTaskOrg, idxTaskProj, idxTaskPri, idxTaskStat)
	}
	if !(idxTaskOrg < idxTaskProj && idxTaskProj < idxTaskPri && idxTaskPri < idxTaskStat) {
		t.Errorf("overview filter order incorrect: expected Org < Project < Priority < Status, got %d, %d, %d, %d",
			idxTaskOrg, idxTaskProj, idxTaskPri, idxTaskStat)
	}

	// Recent Tasks filter order: Organization -> Project -> Priority
	idxRecentOrg := strings.Index(html, `id="recent-tasks-org-filter"`)
	idxRecentProj := strings.Index(html, `id="recent-tasks-project-filter"`)
	idxRecentPri := strings.Index(html, `id="recent-tasks-priority-filter"`)
	if idxRecentOrg == -1 || idxRecentProj == -1 || idxRecentPri == -1 {
		t.Fatalf("recent tasks filter controls missing in index.html (org=%d, proj=%d, pri=%d)",
			idxRecentOrg, idxRecentProj, idxRecentPri)
	}
	if !(idxRecentOrg < idxRecentProj && idxRecentProj < idxRecentPri) {
		t.Errorf("recent tasks filter order incorrect: expected Org < Project < Priority, got %d, %d, %d",
			idxRecentOrg, idxRecentProj, idxRecentPri)
	}

	// Task Status page filter order: Organization -> Project -> Priority -> Status
	idxTSOrg := strings.Index(html, `id="ts-org-filter"`)
	idxTSProj := strings.Index(html, `id="ts-project-filter"`)
	idxTSPri := strings.Index(html, `id="ts-priority-filter"`)
	idxTSStat := strings.Index(html, `id="ts-status-filter"`)
	if idxTSOrg == -1 || idxTSProj == -1 || idxTSPri == -1 || idxTSStat == -1 {
		t.Fatalf("task status filter controls missing in index.html (org=%d, proj=%d, pri=%d, stat=%d)",
			idxTSOrg, idxTSProj, idxTSPri, idxTSStat)
	}
	if !(idxTSOrg < idxTSProj && idxTSProj < idxTSPri && idxTSPri < idxTSStat) {
		t.Errorf("task status filter order incorrect: expected Org < Project < Priority < Status, got %d, %d, %d, %d",
			idxTSOrg, idxTSProj, idxTSPri, idxTSStat)
	}

	// 2. Verify app.js sorting and filter behaviors
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for /ui/app.js, got %d", wJS.Code)
	}
	js := wJS.Body.String()

	requiredSymbols := []string{
		"getNextSort",
		"DEFAULT_OVERVIEW_SORT",
		"DEFAULT_TS_SORT",
		"DEFAULT_ORG_SORT",
		"populateOverviewProjectFilter",
		"populateTSProjectFilter",
		"tableEl._currentSort = currentSort",
		"tableEl._defaultSort = defaultSort",
		"tableEl._onSort = onSort",
		"'task-project-filter'",
		"'task-priority-filter'",
		"'ts-project-filter'",
		"'ts-priority-filter'",
	}
	for _, sym := range requiredSymbols {
		if !strings.Contains(js, sym) {
			t.Errorf("expected app.js to contain %s", sym)
		}
	}
}

func TestRegisterUIRoutes_AgentsCascadingProjectFilter(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify index.html contains the necessary filter elements
	reqHTML := httptest.NewRequest("GET", "/", nil)
	wHTML := httptest.NewRecorder()
	mux.ServeHTTP(wHTML, reqHTML)
	if wHTML.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wHTML.Code)
	}
	html := wHTML.Body.String()
	if !strings.Contains(html, `id="agents-org-filter"`) {
		t.Error("index.html missing agents-org-filter")
	}
	if !strings.Contains(html, `id="agents-project-filter"`) {
		t.Error("index.html missing agents-project-filter")
	}

	// 2. Verify app.js contains cascading filter functions and state logic
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wJS.Code)
	}
	js := wJS.Body.String()

	requiredFuncs := []string{
		"populateAgentsFilters",
		"populateAgentsOrgFilter",
		"populateAgentsProjectFilter",
		"getProjectsForOrg",
		"getAgentProjects",
		"resetAgentFilters",
		"orgMatches",
	}
	for _, fn := range requiredFuncs {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js missing expected function: %s", fn)
		}
	}

	// Verify project filter reset logic exists on org change and filter reset
	if !strings.Contains(js, "state.agentsFilter.project = 'all'") {
		t.Error("app.js missing state.agentsFilter.project = 'all' reset assignment")
	}
	if !strings.Contains(js, "populateAgentsProjectFilter()") {
		t.Error("app.js missing populateAgentsProjectFilter() calls")
	}
}

func TestRegisterUIRoutes_STA213_BossCardCachingAndFullPageTaskView(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify index.html contains expand button & header actions in detail panel
	reqHTML := httptest.NewRequest("GET", "/", nil)
	wHTML := httptest.NewRecorder()
	mux.ServeHTTP(wHTML, reqHTML)
	if wHTML.Code != http.StatusOK {
		t.Fatalf("expected 200 for index.html, got %d", wHTML.Code)
	}
	html := wHTML.Body.String()

	for _, token := range []string{
		`id="detail-panel"`,
		`id="panel-expand"`,
		`class="panel-expand"`,
		`class="panel-header-actions"`,
		`id="panel-close"`,
	} {
		if !strings.Contains(html, token) {
			t.Errorf("index.html missing %s", token)
		}
	}

	// 2. Verify style.css contains styles for full-page mode and expand button
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for style.css, got %d", wCSS.Code)
	}
	css := wCSS.Body.String()

	for _, token := range []string{
		".panel-header-actions",
		".panel-expand",
		".panel-close",
		".detail-panel.full-page",
		"left: var(--sidebar-w)",
		"body.sidebar-collapsed .detail-panel.full-page",
	} {
		if !strings.Contains(css, token) {
			t.Errorf("style.css missing %s", token)
		}
	}

	// 3. Verify app.js contains Boss Card caching, preloading, and full-page task mode logic
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	js := wJS.Body.String()

	for _, token := range []string{
		"bossReportCache",
		"BOSS_REPORT_CACHE_KEY",
		"BOSS_REPORT_CACHE_TTL_MS",
		"loadBossReportCache",
		"saveBossReportCache",
		"preloadBossReports",
		"toggleDetailFullPage",
		"taskDetailFullPage",
		"panel-expand",
		"sidebar-collapsed",
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing %s", token)
		}
	}
}

func TestWebUI_ChecklistCommitHashGate(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. Verify style.css includes commit gate banner, table, and blocked button styles
	reqCSS := httptest.NewRequest("GET", "/ui/style.css", nil)
	wCSS := httptest.NewRecorder()
	mux.ServeHTTP(wCSS, reqCSS)
	if wCSS.Code != http.StatusOK {
		t.Fatalf("expected 200 for style.css, got %d", wCSS.Code)
	}
	css := wCSS.Body.String()
	for _, selector := range []string{
		".checklist-commit-gate-banner",
		".cl-gate-header",
		".cl-gate-title",
		".cl-gate-log-table",
		".badge-commit-blocked",
		".cl-btn.cl-btn-blocked",
	} {
		if !strings.Contains(css, selector) {
			t.Errorf("style.css missing expected selector %q", selector)
		}
	}

	// 2. Verify app.js includes commit verification state, gate banner rendering, and blocked controls
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	js := wJS.Body.String()
	for _, expected := range []string{
		"checklistCommitVerification",
		"isItemCommitBlocked",
		"getItemCommitBlockReason",
		"checklist-commit-gate-banner",
		"cl-gate-log-table",
		"badge-commit-blocked",
		"cl-btn-blocked",
		"Checklist Commit-Hash Gate: Missing or Unmerged Commits Detected",
	} {
		if !strings.Contains(js, expected) {
			t.Errorf("app.js missing expected symbol/pattern %q", expected)
		}
	}
}

// STA-413: New Task form must have "Kind of work" select instead of Assignee dropdown.
func TestWebUI_CreateTaskKindOfWork(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "test-tok", nil)

	// 1. index.html has ct-work-kind select with all 4 options; no ct-assignee
	reqRoot := httptest.NewRequest("GET", "/", nil)
	wRoot := httptest.NewRecorder()
	mux.ServeHTTP(wRoot, reqRoot)
	if wRoot.Code != http.StatusOK {
		t.Fatalf("expected 200 for root, got %d", wRoot.Code)
	}
	html := wRoot.Body.String()

	if strings.Contains(html, `id="ct-assignee"`) {
		t.Error("index.html must not contain the old ct-assignee dropdown (STA-413)")
	}
	if !strings.Contains(html, `id="ct-work-kind"`) {
		t.Error("index.html missing ct-work-kind select")
	}
	for _, kind := range []string{"coding", "review", "architecture", "planning", "qa", "docs"} {
		if !strings.Contains(html, `value="`+kind+`"`) {
			t.Errorf("index.html missing work_kind option %q", kind)
		}
	}
	if !strings.Contains(html, "Kind of work") {
		t.Error("index.html missing 'Kind of work' label")
	}

	// 2. app.js sends work_kind, not assignee_agent_id; detail panel shows kind label
	reqJS := httptest.NewRequest("GET", "/ui/app.js", nil)
	wJS := httptest.NewRecorder()
	mux.ServeHTTP(wJS, reqJS)
	if wJS.Code != http.StatusOK {
		t.Fatalf("expected 200 for app.js, got %d", wJS.Code)
	}
	js := wJS.Body.String()

	if strings.Contains(js, "assignee_agent_id: assigneeSel") {
		t.Error("app.js must not reference assigneeSel (STA-413)")
	}
	if !strings.Contains(js, "work_kind:") {
		t.Error("app.js must send work_kind in create-task body")
	}
	// STA-838: provider select on create and on the task page; Gemini options
	// gated off for code kinds.
	if !strings.Contains(html, `id="ct-provider"`) {
		t.Error("index.html missing ct-provider select")
	}
	for _, want := range []string{"splitProviderChoice(providerSel", "buildProviderField(task)", "gateGeminiOptions", "/provider`", "NON_CODE_KINDS"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if !strings.Contains(js, "WORK_KIND_LABELS") {
		t.Error("app.js missing WORK_KIND_LABELS map")
	}
	if !strings.Contains(js, "workKindLabel") {
		t.Error("app.js missing workKindLabel helper")
	}
	if !strings.Contains(js, "'Kind of work'") {
		t.Error("app.js missing 'Kind of work' detail panel field")
	}
	for _, label := range []string{"Coding —", "Review —", "Architecture", "Planning —", "Docs —", "QA & testing"} {
		if !strings.Contains(js, label) {
			t.Errorf("app.js missing work_kind label %q", label)
		}
	}
}
