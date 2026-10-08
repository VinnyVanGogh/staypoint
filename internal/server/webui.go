package server

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed webui/* webui/lib/*
var webuiFiles embed.FS

// webuiFS strips the leading "webui/" prefix so paths resolve as "/ui/style.css" etc.
var webuiFS, _ = fs.Sub(webuiFiles, "webui")

// indexTmpl is the root HTML page; it injects the local auth token so JS can
// pass it to the Bearer-protected API endpoints.
var indexTmpl = template.Must(template.ParseFS(webuiFiles, "webui/index.html"))

// validSPARoutes lists known client-side routes that should serve index.html.
var validSPARoutes = map[string]bool{
	"":              true,
	"overview":      true,
	"projects":      true,
	"agents":        true,
	"kanban":        true,
	"recent-tasks":  true,
	"task-status":   true,
	"all-tasks":     true,
	"cost":          true,
	"boss":          true,
	"checklist":     true,
	"settings":      true,
	"routines":      true,
	"artifacts":     true,
	"skills":        true,
	"connectors":    true,
	"audit":         true,
	"gates":         true,
	"pull-requests": true,
	"tasks":         true,
	"issues":        true,
	"task-page":     true,
	"logs":          true,
}

func isSPARoute(p string) bool {
	if p == "/" {
		return true
	}
	clean := strings.Trim(p, "/")
	if strings.HasPrefix(clean, "org/") || strings.HasPrefix(clean, "tasks/") || strings.HasPrefix(clean, "issues/") || clean == "tasks" || clean == "issues" {
		return true
	}
	return validSPARoutes[clean]
}

// RegisterUIRoutes mounts the embedded web UI onto the given mux.
//
// GET /            → index.html (with auth token injected into meta tag)
// GET /{spa_route} → index.html for client-side routing
// GET /ui/         → embedded static assets (CSS, JS)
//
// The board token is NOT injected here; it is delivered only as an HttpOnly cookie
// during the ?token= bootstrap redirect handled by SecurityMiddleware.Wrap.
func RegisterUIRoutes(mux *http.ServeMux, authToken string) {
	fileServer := http.FileServer(http.FS(webuiFS))

	// Serve static assets under /ui/
	mux.Handle("GET /ui/", http.StripPrefix("/ui", fileServer))

	// Root and all SPA client routes → index.html with auth token injected
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Do not intercept API, static assets under /ui/, or SSE events
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ui/") || r.URL.Path == "/events" {
			http.NotFound(w, r)
			return
		}
		if !isSPARoute(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = indexTmpl.Execute(w, map[string]string{
			"Token": authToken,
		})
	})
}
