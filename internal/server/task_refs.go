package server

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/taskref"
)

// resolveTaskRefPaths lets every /api/tasks/{id}... endpoint take a task
// reference (STA-123, or a legacy label) where it takes a task id: the
// reference in the path is replaced by the task id before routing, so the
// handlers only ever see internal ids. An unknown reference is left as-is and
// the handler answers 404 as for any unknown id.
func resolveTaskRefPaths(db *sql.DB, next http.Handler) http.Handler {
	const prefix = "/api/tasks/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if db != nil && strings.HasPrefix(r.URL.Path, prefix) {
			rest := r.URL.Path[len(prefix):]
			seg, tail, _ := strings.Cut(rest, "/")
			if _, _, ok := taskref.Parse(seg); ok {
				if id, _, err := context.ResolveTaskRef(db, seg); err == nil {
					p := prefix + id
					if len(rest) > len(seg) {
						p += "/" + tail
					}
					r2 := r.Clone(r.Context())
					r2.URL.Path = p
					r2.URL.RawPath = ""
					r = r2
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// taskPageRoute decides what a task page URL serves. The canonical page is
// /STA-123/slug (slug optional, ignored for lookup). It returns the path to
// redirect to (301), or "" to serve the page as-is; notFound reports a
// reference that names no task.
//
//   - /STA-123 and /STA-123/<current slug>: served.
//   - /sta-123/..., /STA-123/<wrong slug>, or a legacy label (/STA-775 for an
//     imported task numbered otherwise): 301 to the canonical path.
//   - /tasks/<org>/<project>/<id> and /tasks/<id> (any id or reference the
//     daemon knows): 301 to the canonical path.
//   - Anything else under /tasks/ (a fleet-only task) is served as before.
func taskPageRoute(db *sql.DB, p string) (redirect string, notFound bool) {
	clean := strings.Trim(p, "/")
	if strings.HasPrefix(clean, "tasks/") || strings.HasPrefix(clean, "issues/") {
		segs := strings.Split(clean, "/")
		ident, err := url.PathUnescape(segs[len(segs)-1])
		if err != nil || ident == "" {
			return "", false
		}
		if t := lookupTaskForPage(db, ident); t != nil && t.Identifier != "" {
			return taskref.Path(t.Identifier, t.Slug), false
		}
		return "", false
	}

	refSeg, slug, _ := strings.Cut(clean, "/")
	if _, _, ok := taskref.Parse(refSeg); !ok {
		return "", false
	}
	id, legacy, err := context.ResolveTaskRef(db, refSeg)
	if err != nil {
		return "", true
	}
	t, err := context.GetTask(db, id)
	if err != nil || t.Identifier == "" {
		return "", true
	}
	if legacy || refSeg != t.Identifier || (slug != "" && slug != t.Slug) {
		return taskref.Path(t.Identifier, t.Slug), false
	}
	return "", false
}

// lookupTaskForPage finds the task an old-style URL names: a reference, a
// legacy label, or a task id (task-… exactly; an id prefix is not enough for
// a redirect). Nil when the daemon has no such task.
func lookupTaskForPage(db *sql.DB, ident string) *context.Task {
	if _, _, ok := taskref.Parse(ident); ok {
		id, _, err := context.ResolveTaskRef(db, ident)
		if err != nil {
			return nil
		}
		ident = id
	} else if !strings.HasPrefix(ident, "task-") {
		return nil
	}
	t, err := context.GetTask(db, ident)
	if err != nil || t.ID != ident {
		return nil
	}
	return t
}

// isTaskRefPath reports whether p is /STA-123 or /STA-123/<slug>.
func isTaskRefPath(p string) bool {
	refSeg, slug, _ := strings.Cut(strings.Trim(p, "/"), "/")
	if _, _, ok := taskref.Parse(refSeg); !ok {
		return false
	}
	return !strings.Contains(slug, "/")
}
