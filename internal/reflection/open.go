package reflection

import (
	"database/sql"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
)

// OpenSources opens every input read-only. A missing input is left nil (its
// facts become a warning); the returned func closes what was opened.
func OpenSources(cfg *config.Config) (Sources, func()) {
	var src Sources
	var opened []*sql.DB
	if c, err := db.OpenReadOnly(cfg.DBPath); err == nil {
		src.Mesh = c
		opened = append(opened, c)
	}
	if c, err := db.OpenReadOnly(cfg.TelemetryDBPath); err == nil {
		src.Telemetry = c
		opened = append(opened, c)
	}
	if c, err := archive.OpenIndexReadOnly(archive.DefaultDir(cfg.DataDir)); err == nil {
		src.Archive = c
		opened = append(opened, c)
	}
	src.Seats = SeatsFromConfig(cfg)
	return src, func() {
		for _, c := range opened {
			c.Close()
		}
	}
}

// SeatsFromConfig maps the configured account emails to seat labels.
func SeatsFromConfig(cfg *config.Config) map[string]string {
	seats := map[string]string{}
	if e := strings.ToLower(strings.TrimSpace(cfg.WorkEmail)); e != "" {
		seats[e] = "work"
	}
	if e := strings.ToLower(strings.TrimSpace(cfg.PersonalEmail)); e != "" {
		seats[e] = "personal"
	}
	return seats
}
