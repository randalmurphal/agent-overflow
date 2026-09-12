package usagecost

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Rate holds standard per-million-token USD rates in effect from a date.
// The ledger cannot reconstruct per-request context tiers, service tiers,
// regional premiums or cache TTLs; Claude cache writes use the 1h rate.
// All prices remain estimates.
type Rate struct {
	// From is the first UTC day (YYYY-MM-DD) this rate applies to. Empty
	// covers all earlier usage; a model's first entry is normally empty.
	From       string  `json:"from,omitempty"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Alias is a moving model pointer resolved by date, like a Rate.
type Alias struct {
	From  string `json:"from,omitempty"`
	Model string `json:"model"`
}

type catalog struct {
	Sources []string           `json:"sources"`
	Aliases map[string][]Alias `json:"aliases"`
	Rates   map[string][]Rate  `json:"rates"`
}

//go:embed rates.json
var rateFile []byte
var rates = loadCatalog(rateFile)

const dateLayout = "2006-01-02"

func loadCatalog(data []byte) catalog {
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		panic(fmt.Errorf("usage pricing: %w", err))
	}
	if len(c.Sources) == 0 || len(c.Rates) == 0 {
		panic("usage pricing: empty catalog")
	}
	for model, entries := range c.Rates {
		if model == "" {
			panic("usage pricing: empty model id")
		}
		previous := ""
		for i, r := range entries {
			for _, v := range []float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					panic(fmt.Sprintf("usage pricing: invalid rate for %s", model))
				}
			}
			if err := validateFrom(r.From, i, previous); err != nil {
				panic(fmt.Sprintf("usage pricing: %s: %v", model, err))
			}
			previous = r.From
		}
	}
	for alias, entries := range c.Aliases {
		if alias == "" || len(entries) == 0 {
			panic("usage pricing: invalid alias")
		}
		previous := ""
		for i, a := range entries {
			if _, ok := c.Rates[a.Model]; !ok {
				panic(fmt.Sprintf("usage pricing: alias %s targets unknown model %q", alias, a.Model))
			}
			if err := validateFrom(a.From, i, previous); err != nil {
				panic(fmt.Sprintf("usage pricing: alias %s: %v", alias, err))
			}
			previous = a.From
		}
	}
	return c
}

// validateFrom keeps each model's entries in effective-date order so the
// last entry at or before a date is the one in effect.
func validateFrom(from string, index int, previous string) error {
	if from == "" {
		if index != 0 {
			return fmt.Errorf("only the first entry may omit from")
		}
		return nil
	}
	if _, err := time.Parse(dateLayout, from); err != nil {
		return fmt.Errorf("invalid from %q", from)
	}
	if from <= previous {
		return fmt.Errorf("from %q must be later than %q", from, previous)
	}
	return nil
}

// Price estimates one ledger group's USD cost at the rate in effect on the
// UTC day of `at`. ok is false for unknown models, usage before a model's
// first dated rate, negative counts, and cache writes on a model with no
// published write rate. Reasoning tokens are already included in output.
func Price(model string, at time.Time, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (float64, bool) {
	return rates.price(model, at, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens)
}

func (c catalog) price(model string, at time.Time, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (float64, bool) {
	if inputTokens < 0 || outputTokens < 0 || cacheReadTokens < 0 || cacheWriteTokens < 0 {
		return 0, false
	}
	day := at.UTC().Format(dateLayout)
	rate, ok := c.rateOn(model, day)
	if !ok {
		return 0, false
	}
	if cacheWriteTokens > 0 && rate.CacheWrite == 0 {
		return 0, false
	}
	const million = 1_000_000.0
	return float64(inputTokens)/million*rate.Input +
		float64(outputTokens)/million*rate.Output +
		float64(cacheReadTokens)/million*rate.CacheRead +
		float64(cacheWriteTokens)/million*rate.CacheWrite, true
}

// rateOn resolves a model spelling to the rate in effect on a UTC day.
// Only exact IDs, explicit aliases and date-suffixed snapshots of known IDs
// match; new variants must get their own entry rather than inherit a price.
func (c catalog) rateOn(model, day string) (Rate, bool) {
	model = strings.TrimSuffix(model, "[1m]")
	if entries, ok := c.Rates[model]; ok {
		return effectiveRate(entries, day)
	}
	if entries, ok := c.Aliases[model]; ok {
		target, found := effectiveAlias(entries, day)
		if !found {
			return Rate{}, false
		}
		return effectiveRate(c.Rates[target], day)
	}
	for _, layout := range []string{"2006-01-02", "20060102"} {
		n := len(layout)
		if len(model) > n+1 && model[len(model)-n-1] == '-' {
			if _, err := time.Parse(layout, model[len(model)-n:]); err == nil {
				entries, ok := c.Rates[model[:len(model)-n-1]]
				if !ok {
					return Rate{}, false
				}
				return effectiveRate(entries, day)
			}
		}
	}
	return Rate{}, false
}

func effectiveRate(entries []Rate, day string) (Rate, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].From <= day {
			return entries[i], true
		}
	}
	return Rate{}, false
}

func effectiveAlias(entries []Alias, day string) (string, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].From <= day {
			return entries[i].Model, true
		}
	}
	return "", false
}
