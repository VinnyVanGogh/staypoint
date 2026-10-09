package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/reflection"
)

// ReflectHandler serves the Board's reflect view. Both routes are Board
// session only: the facts and summary span every org, work included.
type ReflectHandler struct {
	db     *sql.DB
	loadFn func() (*config.Config, error)
	// runner is the summary model; tests replace it.
	runner func(model string) reflection.Runner

	mu   sync.Mutex
	jobs map[int]*reflectJob
}

type reflectJob struct {
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at"`
	Error     string    `json:"error,omitempty"`
}

func NewReflectHandler(conn *sql.DB) *ReflectHandler {
	return &ReflectHandler{
		db:     conn,
		loadFn: config.LoadConfig,
		runner: func(model string) reflection.Runner { return reflection.PersonalClaude(model, 10*time.Minute) },
		jobs:   map[int]*reflectJob{},
	}
}

func (h *ReflectHandler) sources(cfg *config.Config) (reflection.Sources, func()) {
	src := reflection.Sources{Mesh: h.db, Seats: reflection.SeatsFromConfig(cfg)}
	var closers []*sql.DB
	if c, err := db.OpenReadOnly(cfg.TelemetryDBPath); err == nil {
		src.Telemetry = c
		closers = append(closers, c)
	}
	if c, err := archive.OpenIndexReadOnly(archive.DefaultDir(cfg.DataDir)); err == nil {
		src.Archive = c
		closers = append(closers, c)
	}
	return src, func() {
		for _, c := range closers {
			c.Close()
		}
	}
}

type reflectResponse struct {
	Facts   *reflection.Facts   `json:"facts"`
	Summary *reflection.Summary `json:"summary,omitempty"`
	Job     *reflectJob         `json:"summary_job,omitempty"`
	Archive *archive.Stats      `json:"archive,omitempty"`
}

// Get returns facts for ?since= plus the cached summary and job state.
func (h *ReflectHandler) Get(w http.ResponseWriter, r *http.Request) {
	days, err := reflection.ParsePeriod(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := h.loadFn()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	src, done := h.sources(cfg)
	defer done()
	resp := reflectResponse{Facts: reflection.Compute(src, days, time.Now())}
	dir := archive.DefaultDir(cfg.DataDir)
	if s, err := reflection.LoadSummary(dir, days); err == nil {
		resp.Summary = s
	}
	if src.Archive != nil {
		if st, err := archive.ComputeStats(src.Archive); err == nil {
			resp.Archive = st
		}
	}
	h.mu.Lock()
	if j := h.jobs[days]; j != nil {
		cp := *j
		resp.Job = &cp
	}
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// StartSummary runs the LLM summary for ?since= in the background (personal
// seat, work data excluded; --include-work is CLI only). 202 when started,
// 409 when one is already running for that period.
func (h *ReflectHandler) StartSummary(w http.ResponseWriter, r *http.Request) {
	days, err := reflection.ParsePeriod(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := h.loadFn()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	h.mu.Lock()
	if j := h.jobs[days]; j != nil && j.Running {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, "a summary for this period is already running")
		return
	}
	job := &reflectJob{Running: true, StartedAt: time.Now().UTC()}
	h.jobs[days] = job
	h.mu.Unlock()

	go h.runSummary(cfg, days, job)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(job)
}

func (h *ReflectHandler) runSummary(cfg *config.Config, days int, job *reflectJob) {
	const model = "sonnet"
	fail := func(err error) {
		slog.Warn("Reflect summary failed", slog.Int("days", days), slog.Any("error", err))
		h.mu.Lock()
		job.Running, job.Error = false, err.Error()
		h.mu.Unlock()
	}
	src, done := h.sources(cfg)
	defer done()
	now := time.Now()
	facts := reflection.Compute(src, days, now)
	dir := archive.DefaultDir(cfg.DataDir)
	opts := reflection.CorpusOptions{}
	corpus, err := reflection.BuildCorpus(src, dir, facts, opts)
	if err != nil {
		fail(err)
		return
	}
	s, err := reflection.Summarize(context.Background(), h.runner(model), facts, corpus, model, opts, now)
	if err != nil {
		fail(err)
		return
	}
	if err := reflection.SaveSummary(dir, s); err != nil {
		fail(err)
		return
	}
	h.mu.Lock()
	job.Running = false
	h.mu.Unlock()
}
