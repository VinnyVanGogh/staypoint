package archive

import (
	"database/sql"
	"time"
)

// SourceStats is the archive's footprint for one source.
type SourceStats struct {
	Source      string `json:"source"`
	Transcripts int    `json:"transcripts"`
	// OnlyInArchive counts transcripts whose original has since been deleted.
	OnlyInArchive int    `json:"only_in_archive"`
	SrcBytes      int64  `json:"src_bytes"`
	StoredBytes   int64  `json:"stored_bytes"`
	Oldest        string `json:"oldest"`
	Newest        string `json:"newest"`
}

// Stats summarises the whole archive.
type Stats struct {
	Sources     []SourceStats `json:"sources"`
	Transcripts int           `json:"transcripts"`
	SrcBytes    int64         `json:"src_bytes"`
	StoredBytes int64         `json:"stored_bytes"`
	// Ratio is source bytes per stored byte (higher is better).
	Ratio float64 `json:"ratio"`
	// SpanDays is how many days of activity the archive covers.
	SpanDays float64 `json:"span_days"`
	// ProjectedGBPerYear extrapolates stored bytes per covered day to 365 days.
	ProjectedGBPerYear float64 `json:"projected_gb_per_year"`
	LastRun            string  `json:"last_run,omitempty"`
}

// ComputeStats reads the index.
func ComputeStats(conn *sql.DB) (*Stats, error) {
	rows, err := conn.Query(`SELECT source, COUNT(*), COALESCE(SUM(src_missing),0), COALESCE(SUM(src_size),0),
		COALESCE(SUM(stored_bytes),0), COALESCE(MIN(NULLIF(started_at,'')),''), COALESCE(MAX(ended_at),'')
		FROM transcripts GROUP BY source ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := &Stats{}
	var oldest, newest string
	for rows.Next() {
		var s SourceStats
		if err := rows.Scan(&s.Source, &s.Transcripts, &s.OnlyInArchive, &s.SrcBytes, &s.StoredBytes, &s.Oldest, &s.Newest); err != nil {
			return nil, err
		}
		st.Sources = append(st.Sources, s)
		st.Transcripts += s.Transcripts
		st.SrcBytes += s.SrcBytes
		st.StoredBytes += s.StoredBytes
		if s.Oldest != "" && (oldest == "" || s.Oldest < oldest) {
			oldest = s.Oldest
		}
		if s.Newest > newest {
			newest = s.Newest
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if st.StoredBytes > 0 {
		st.Ratio = float64(st.SrcBytes) / float64(st.StoredBytes)
	}
	st.SpanDays, st.ProjectedGBPerYear = project(oldest, newest, st.StoredBytes)
	_ = conn.QueryRow(`SELECT finished_at FROM archive_runs ORDER BY id DESC LIMIT 1`).Scan(&st.LastRun)
	return st, nil
}

func project(oldest, newest string, stored int64) (float64, float64) {
	a, err1 := time.Parse(time.RFC3339, oldest)
	b, err2 := time.Parse(time.RFC3339, newest)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	days := b.Sub(a).Hours() / 24
	if days < 1 {
		days = 1
	}
	return days, float64(stored) / days * 365 / 1e9
}
