package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/context"
)

// ListDocuments handles GET /api/documents: the latest version of every task
// document across tasks, for the Artifacts page.
//
// Query: org (organization, case-insensitive), task (task id or
// identifier), kind (doc_key; "description" lists task briefs, which are
// otherwise left out).
func (h *TasksHandler) ListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := context.DocumentFilter{
		Organization: strings.TrimSpace(q.Get("org")),
		DocKey:       strings.TrimSpace(q.Get("kind")),
	}
	if ref := strings.TrimSpace(q.Get("task")); ref != "" {
		task, err := context.GetTask(h.db, ref)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		f.TaskID = task.ID
	}
	docs, err := context.ListDocumentSummaries(h.db, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONUnescaped(w, map[string]any{"documents": docs, "count": len(docs)})
}

// ListTaskDocuments handles GET /api/tasks/{id}/documents: every doc_key on
// the task with its latest version and version history (no content).
func (h *TasksHandler) ListTaskDocuments(w http.ResponseWriter, r *http.Request) {
	task, err := context.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	docs, err := context.ListTaskDocumentSummaries(h.db, task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if docs == nil {
		docs = []context.DocumentSummary{}
	}
	writeJSONUnescaped(w, map[string]any{"task_id": task.ID, "documents": docs})
}

// GetTaskDocument handles GET /api/tasks/{id}/documents/{key}: the latest
// version of one document, or ?version=N for an older one.
func (h *TasksHandler) GetTaskDocument(w http.ResponseWriter, r *http.Request) {
	task, err := context.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	key := r.PathValue("key")
	var doc *context.TaskDocument
	if vs := strings.TrimSpace(r.URL.Query().Get("version")); vs != "" {
		v, convErr := strconv.Atoi(vs)
		if convErr != nil || v < 1 {
			writeError(w, http.StatusBadRequest, "version must be a positive integer")
			return
		}
		doc, err = context.GetTaskDocumentRevision(h.db, task.ID, key, v)
	} else {
		doc, err = context.GetLatestTaskDocument(h.db, task.ID, key)
	}
	if errors.Is(err, context.ErrDocumentNotFound) {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONUnescaped(w, doc)
}
