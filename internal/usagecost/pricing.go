package usagecost

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// CurrentVersion identifies the bundled rate snapshot stamped on new ledger rows.
// Keep older snapshots when publishing new rates so recorded estimates stay stable.
const CurrentVersion = "2026-09-09"

// Rate holds standard per-million-token USD rates. The ledger cannot reconstruct
// per-request context tiers, service tiers, regional premiums or cache TTLs.
// Claude cache writes use the 1h rate; all prices remain estimates.
type Rate struct {
	// Backfill confirms these rates also apply to earlier, unpriced usage of
	// this model. Set only after verifying its historical pricing.
	Backfill   bool    `json:"backfill,omitempty"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Alias backfill verifies both the historical target and its rates. A model's
// own launch-price approval cannot establish a moving alias's earlier target.
type Alias struct {
	Model    string `json:"model"`
	Backfill bool   `json:"backfill,omitempty"`
}

type catalog struct {
	Sources []string         `json:"sources"`
	Aliases map[string]Alias `json:"aliases"`
	Rates   map[string]Rate  `json:"rates"`
}

// catalogSet retains publication order so backfilled estimates keep the first
// applicable rate even after a later release changes that model's price.
type catalogSet struct {
	byVersion map[string]catalog
	versions  []string
}

func newCatalogSet(catalogs map[string]catalog) catalogSet {
	versions := make([]string, 0, len(catalogs))
	for version := range catalogs {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return catalogSet{byVersion: catalogs, versions: versions}
}

//go:embed rates/*.json
var rateFiles embed.FS
var catalogs = loadCatalogs()

func loadCatalogs() catalogSet {
	entries, err := rateFiles.ReadDir("rates")
	if err != nil {
		panic(err)
	}
	result := make(map[string]catalog, len(entries))
	for _, entry := range entries {
		data, err := rateFiles.ReadFile("rates/" + entry.Name())
		if err != nil {
			panic(err)
		}
		var c catalog
		if err := json.Unmarshal(data, &c); err != nil {
			panic(fmt.Errorf("usage pricing %s: %w", entry.Name(), err))
		}
		if len(c.Sources) == 0 || len(c.Rates) == 0 {
			panic("usage pricing: empty catalog")
		}
		for name, r := range c.Rates {
			for _, v := range []float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
				if name == "" || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					panic("usage pricing: invalid rate")
				}
			}
		}
		for alias, target := range c.Aliases {
			if _, ok := c.Rates[target.Model]; !ok || alias == "" {
				panic("usage pricing: invalid alias")
			}
		}
		result[strings.TrimSuffix(entry.Name(), ".json")] = c
	}
	if _, ok := result[CurrentVersion]; !ok {
		panic("usage pricing: current snapshot missing")
	}
	return newCatalogSet(result)
}

func Price(model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (float64, bool) {
	return PriceVersion(CurrentVersion, model, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens)
}

// PriceVersion prices one ledger group without guessing unknown model families.
// Missing prices can use the first later snapshot explicitly verified for backfill.
// A blank version is allowed for in-memory callers and uses the current snapshot.
// Reasoning tokens are already included in output tokens.
func PriceVersion(version, model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (float64, bool) {
	return catalogs.priceVersion(version, model, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens)
}

func (cs catalogSet) priceVersion(version, model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) (float64, bool) {
	if version == "" {
		version = CurrentVersion
	}
	c, ok := cs.byVersion[version]
	if !ok {
		return 0, false
	}
	if inputTokens < 0 || outputTokens < 0 || cacheReadTokens < 0 || cacheWriteTokens < 0 {
		return 0, false
	}
	rate, modelID, ok := c.matchRate(model)
	index, _ := slices.BinarySearch(cs.versions, version)
	if !ok {
		// Stop at the first published match. Skipping an ineligible price could
		// incorrectly apply a later price change or alias target to old usage.
		for index++; index < len(cs.versions); index++ {
			rate, modelID, ok = cs.byVersion[cs.versions[index]].matchRate(model)
			if ok {
				if !rate.Backfill {
					return 0, false
				}
				break
			}
		}
	}
	if !ok {
		return 0, false
	}
	if cacheWriteTokens > 0 && rate.CacheWrite == 0 {
		// Preserve the original input/output/read rates and alias target when
		// filling a previously unknown billing class.
		for index++; index < len(cs.versions); index++ {
			later, found := cs.byVersion[cs.versions[index]].Rates[modelID]
			if found && later.CacheWrite > 0 {
				if !later.Backfill {
					return 0, false
				}
				rate.CacheWrite = later.CacheWrite
				break
			}
		}
		if rate.CacheWrite == 0 {
			return 0, false
		}
	}
	const million = 1_000_000.0
	return float64(inputTokens)/million*rate.Input + float64(outputTokens)/million*rate.Output + float64(cacheReadTokens)/million*rate.CacheRead + float64(cacheWriteTokens)/million*rate.CacheWrite, true
}

func (c catalog) matchRate(model string) (Rate, string, bool) {
	model = strings.TrimSuffix(model, "[1m]")
	if r, ok := c.Rates[model]; ok {
		return r, model, true
	}
	if alias, ok := c.Aliases[model]; ok {
		rate := c.Rates[alias.Model]
		rate.Backfill = alias.Backfill
		return rate, alias.Model, true
	}
	// Only documented date suffix shapes are normalized; new model variants
	// must have their own rate instead of inheriting a different model's price.
	for _, layout := range []string{"2006-01-02", "20060102"} {
		n := len(layout)
		if len(model) > n+1 && model[len(model)-n-1] == '-' {
			if _, err := time.Parse(layout, model[len(model)-n:]); err == nil {
				id := model[:len(model)-n-1]
				r, ok := c.Rates[id]
				return r, id, ok
			}
		}
	}
	return Rate{}, "", false
}
