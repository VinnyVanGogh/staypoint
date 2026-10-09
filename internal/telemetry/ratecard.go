package telemetry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// The embedded rate card is a snapshot of https://models.dev/api.json
// (MIT, Copyright (c) 2025 models.dev; see ratecard/LICENSE-models.dev).
// Regenerate with scripts/gen_ratecard.py. Costs are computed offline at
// ingest; nothing here touches the network.
//
//go:embed ratecard/ratecard.json
var rateCardJSON []byte

// Usage is one request's token counts. CacheCreation5m/1h split the
// cache-write tokens by TTL; when the source has no split, put everything in
// CacheCreation5m (the cheaper rate, so the error understates).
type Usage struct {
	Input           int64
	Output          int64
	CacheRead       int64
	CacheCreation5m int64
	CacheCreation1h int64
}

// rate holds integer micro-dollars per million tokens ($/Mtok * 1e6).
type rate struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	CacheWrite1h int64 `json:"cache_write_1h"`
}

type tierRate struct {
	rate
	ContextOver int64 `json:"context_over"`
}

type modelRate struct {
	rate
	Tiers []tierRate `json:"tiers"`
}

type rateCard struct {
	Source  string               `json:"source"`
	License string               `json:"license"`
	Models  map[string]modelRate `json:"models"`
}

var (
	cardOnce sync.Once
	card     *rateCard
	cardErr  error
)

func loadRateCard() (*rateCard, error) {
	cardOnce.Do(func() {
		var c rateCard
		if err := json.Unmarshal(rateCardJSON, &c); err != nil {
			cardErr = fmt.Errorf("decode embedded rate card: %w", err)
			return
		}
		card = &c
	})
	return card, cardErr
}

var (
	datedSuffix = regexp.MustCompile(`-\d{8}$`)
	ctxSuffix   = regexp.MustCompile(`\[[^\]]*\]$`)
)

// lookupModel resolves a transcript model id to a rate card entry. It strips
// provider prefixes ("anthropic/"), "[1m]"-style context suffixes, "@version"
// suffixes and trailing date stamps before giving up.
func lookupModel(c *rateCard, model string) (modelRate, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, "@"); i >= 0 {
		id = id[:i]
	}
	id = ctxSuffix.ReplaceAllString(id, "")
	if m, ok := c.Models[id]; ok {
		return m, true
	}
	if stripped := datedSuffix.ReplaceAllString(id, ""); stripped != id {
		if m, ok := c.Models[stripped]; ok {
			return m, true
		}
	}
	// Antigravity names models with an effort suffix ("gemini-3.1-pro-high");
	// the rate card lists the base id, sometimes with a "-preview" suffix.
	if strings.HasPrefix(id, "gemini-") {
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(id, "-high"), "-low"), "-medium")
		for _, cand := range []string{base, base + "-preview"} {
			if m, ok := c.Models[cand]; ok {
				return m, true
			}
		}
	}
	return modelRate{}, false
}

// effective returns the rate that applies to a request whose prompt context
// is ctx tokens: the highest tier whose threshold ctx exceeds, else the base.
// The tier applies to the whole request, matching provider billing.
func (m modelRate) effective(ctx int64) rate {
	r := m.rate
	var best int64 = -1
	for _, t := range m.Tiers {
		if ctx > t.ContextOver && t.ContextOver > best {
			best = t.ContextOver
			r = t.rate
		}
	}
	return r
}

// ComputeCostMicros prices one request in integer micro-dollars from the
// embedded rate card. ok is false when the model is not on the card, in which
// case the cost is 0 and the caller decides how to treat it.
func ComputeCostMicros(model string, u Usage) (micros int64, ok bool) {
	c, err := loadRateCard()
	if err != nil {
		return 0, false
	}
	m, found := lookupModel(c, model)
	if !found {
		return 0, false
	}
	return priceMicros(m, u), true
}

func priceMicros(m modelRate, u Usage) int64 {
	ctx := u.Input + u.CacheRead + u.CacheCreation5m + u.CacheCreation1h
	r := m.effective(ctx)

	// A model with no 1h rate bills 1h cache writes at its 5m write rate.
	write1h := r.CacheWrite1h
	if write1h == 0 {
		write1h = r.CacheWrite
	}

	// rate is micro-USD per Mtok, so tokens*rate/1e6 is micro-USD. Sum the
	// numerators and round once (half up) so error is at most half a micro-dollar
	// per request rather than per component.
	num := u.Input*r.Input +
		u.Output*r.Output +
		u.CacheRead*r.CacheRead +
		u.CacheCreation5m*r.CacheWrite +
		u.CacheCreation1h*write1h
	return (num + 500_000) / 1_000_000
}

// RateCardSource returns the upstream source URL of the embedded rate card.
func RateCardSource() string {
	c, err := loadRateCard()
	if err != nil || c == nil || c.Source == "" {
		return "https://models.dev/api.json"
	}
	return c.Source
}
