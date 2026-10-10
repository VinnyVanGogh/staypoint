package context

import (
	"database/sql"
	"sort"
	"strings"
)

// DescriptionDocKey is the task_documents key that holds a task's own brief.
// It is shown on the Brief tab, so document listings leave it out unless it
// is asked for by kind.
const DescriptionDocKey = "description"

// DocumentVersion is one stored revision of a task document, without content.
type DocumentVersion struct {
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	Size      int    `json:"size"`
}

// DocumentSummary is one doc_key on one task: its latest version and, from
// ListTaskDocumentSummaries, its version history.
type DocumentSummary struct {
	TaskID         string            `json:"task_id"`
	TaskName       string            `json:"task_name"`
	TaskIdentifier string            `json:"task_identifier,omitempty"`
	Organization   string            `json:"organization"`
	Project        string            `json:"project"`
	WorkKind       string            `json:"work_kind"`
	DocKey         string            `json:"doc_key"`
	LatestVersion  int               `json:"latest_version"`
	VersionCount   int               `json:"version_count"`
	Size           int               `json:"size"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	Versions       []DocumentVersion `json:"versions,omitempty"`
}

// DocumentFilter narrows ListDocumentSummaries. Empty fields match everything.
// TaskID must be a resolved task id; Organization matches case-insensitively.
type DocumentFilter struct {
	Organization string
	TaskID       string
	DocKey       string
}

// ListDocumentSummaries lists the latest version of every task document across
// tasks, newest first. Soft-deleted tasks and description documents are left
// out (a description is listed only when DocKey asks for it).
func ListDocumentSummaries(db *sql.DB, f DocumentFilter) ([]DocumentSummary, error) {
	where := []string{"t.deleted_at IS NULL"}
	var args []any
	if f.DocKey != "" {
		where = append(where, "d.doc_key = ?")
		args = append(args, f.DocKey)
	} else {
		where = append(where, "d.doc_key != ?")
		args = append(args, DescriptionDocKey)
	}
	if f.TaskID != "" {
		where = append(where, "d.task_id = ?")
		args = append(args, f.TaskID)
	}
	if org := strings.TrimSpace(f.Organization); org != "" {
		where = append(where, "LOWER(COALESCE(t.organization, '')) = LOWER(?)")
		args = append(args, org)
	}
	query := `
		SELECT d.task_id, t.name, COALESCE(t.org_key || '-' || t.number, t.source_ref, ''), COALESCE(t.organization, ''),
		       COALESCE(t.project, ''), COALESCE(t.work_kind, 'coding'), d.doc_key,
		       MAX(d.version), COUNT(*), MIN(d.created_at), MAX(d.created_at),
		       (SELECT LENGTH(CAST(x.content AS BLOB)) FROM task_documents x
		         WHERE x.task_id = d.task_id AND x.doc_key = d.doc_key
		         ORDER BY x.version DESC LIMIT 1)
		FROM task_documents d
		JOIN tasks t ON t.id = d.task_id
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY d.task_id, d.doc_key
		ORDER BY MAX(d.created_at) DESC, d.task_id ASC, d.doc_key ASC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	docs := []DocumentSummary{}
	for rows.Next() {
		var s DocumentSummary
		if err := rows.Scan(&s.TaskID, &s.TaskName, &s.TaskIdentifier, &s.Organization, &s.Project, &s.WorkKind,
			&s.DocKey, &s.LatestVersion, &s.VersionCount, &s.CreatedAt, &s.UpdatedAt, &s.Size); err != nil {
			return nil, err
		}
		docs = append(docs, s)
	}
	return docs, rows.Err()
}

// ListTaskDocumentSummaries lists one task's documents (description excluded)
// sorted by doc_key, each with its version history newest first.
func ListTaskDocumentSummaries(db *sql.DB, taskID string) ([]DocumentSummary, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	docs, err := ListDocumentSummaries(db, DocumentFilter{TaskID: task.ID})
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT doc_key, version, created_at, LENGTH(CAST(content AS BLOB))
		FROM task_documents WHERE task_id = ? AND doc_key != ? ORDER BY doc_key ASC, version DESC`,
		task.ID, DescriptionDocKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := map[string][]DocumentVersion{}
	for rows.Next() {
		var key string
		var v DocumentVersion
		if err := rows.Scan(&key, &v.Version, &v.CreatedAt, &v.Size); err != nil {
			return nil, err
		}
		versions[key] = append(versions[key], v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range docs {
		docs[i].Versions = versions[docs[i].DocKey]
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].DocKey < docs[j].DocKey })
	return docs, nil
}
