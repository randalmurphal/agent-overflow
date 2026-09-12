package usagecost

import (
	"math"
	"testing"
	"time"
)

func almostEqual(a, b, tolerance float64) bool { return math.Abs(a-b) < tolerance }

var today = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func TestPublishedStandardRates(t *testing.T) {
	// One million tokens in each of the four billing classes.
	for model, want := range map[string]float64{
		"gpt-6-astra": 73.5,
		"gpt-5.6-sol": 29.4, "gpt-daybreak-blue-latest": 29.4,
		"gpt-5.6-terra": 16.7, "gpt-5.6-luna": 1.67,
		"gpt-5.6-cyber": 104.375, "gpt-daybreak-red-latest": 104.375,
		"claude-fable-5": 81, "claude-fable-5-1": 80.25,
		"claude-mythos-5-1": 80.25, "claude-opus-4-1": 121.5,
		"claude-opus-4-6": 40.5, "claude-sonnet-5": 16.2,
		"claude-sonnet-4-6": 24.3, "claude-haiku-4-5": 8.1,
	} {
		t.Run(model, func(t *testing.T) {
			got, ok := Price(model, today, 1_000_000, 1_000_000, 1_000_000, 1_000_000)
			if !ok || !almostEqual(got, want, 0.000001) {
				t.Fatalf("got %v, %v; want %v", got, ok, want)
			}
		})
	}
}

func TestOlderModelsInputOutputAndCacheRead(t *testing.T) {
	for model, want := range map[string]float64{
		"gpt-5.3-codex": 15.925, "gpt-5.2-codex": 15.925,
		"gpt-5.1-codex": 11.375, "gpt-5-codex": 11.375,
		"gpt-5.5": 35.5, "gpt-5.4": 17.75, "gpt-5.4-mini": 5.325,
		"gpt-5": 11.375, "o3": 10.5, "o4-mini": 5.775,
	} {
		got, ok := Price(model, today, 1_000_000, 1_000_000, 1_000_000, 0)
		if !ok || !almostEqual(got, want, 0.000001) {
			t.Errorf("%s: %v, %v; want %v", model, got, ok, want)
		}
	}
}

func TestPriceDoesNotGuessFutureVariants(t *testing.T) {
	for _, model := range []string{"unknown", "gpt-5.6", "gpt-5.99", "gpt-5.6-ultra", "claude-sonnet-99", "o3-pro", "gpt-5.4-invalid-date"} {
		if got, ok := Price(model, today, 1_000_000, 0, 0, 0); ok || got != 0 {
			t.Errorf("%s guessed %v", model, got)
		}
	}
}

func TestPriceDocumentedSuffixes(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5[1m]", "claude-sonnet-5-20260801", "claude-sonnet-5-2026-08-01"} {
		got, ok := Price(model, today, 1_000_000, 0, 0, 0)
		if !ok || got != 2 {
			t.Errorf("%s: %v, %v", model, got, ok)
		}
	}
}

func TestPriceRejectsNegativeTokensAndUnpublishedCacheWrites(t *testing.T) {
	if got, ok := Price("gpt-6-astra", today, 0, 0, 0, 0); !ok || got != 0 {
		t.Fatalf("known zero: %v, %v", got, ok)
	}
	if _, ok := Price("gpt-6-astra", today, -1, 0, 0, 0); ok {
		t.Fatal("negative tokens priced")
	}
	if _, ok := Price("gpt-5.4", today, 0, 0, 0, 10); ok {
		t.Fatal("unpublished cache-write rate treated as free")
	}
}

// Rates are selected by the usage's own UTC day: a change appended later
// reprices exactly the days it applied to, and a model's first dated entry
// leaves earlier usage unpriced rather than guessed.
func TestPriceSelectsTheRateInEffectOnTheUsageDay(t *testing.T) {
	c := loadCatalog([]byte(`{
	  "sources": ["test"],
	  "aliases": {"pointer": [{"model": "promo"}, {"from": "2026-11-21", "model": "launched"}]},
	  "rates": {
	    "promo": [{"input": 4, "output": 20, "cacheRead": 0.4, "cacheWrite": 5},
	              {"from": "2026-11-21", "input": 8, "output": 40, "cacheRead": 0.8, "cacheWrite": 10}],
	    "launched": [{"from": "2026-10-01", "input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 0}]
	  }}`))
	price := func(model string, at time.Time) (float64, bool) {
		return c.price(model, at, 1_000_000, 0, 0, 0)
	}
	nov20 := time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC)
	nov21 := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	if got, ok := price("promo", nov20); !ok || got != 4 {
		t.Fatalf("day before change: %v, %v", got, ok)
	}
	if got, ok := price("promo", nov21); !ok || got != 8 {
		t.Fatalf("day of change: %v, %v", got, ok)
	}
	if got, ok := price("promo-20260601", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); !ok || got != 4 {
		t.Fatalf("undated first entry covers earlier usage: %v, %v", got, ok)
	}
	// Local time west of UTC is still priced by the UTC day.
	west := time.FixedZone("west", -5*3600)
	if got, ok := price("promo", time.Date(2026, 11, 20, 20, 0, 0, 0, west)); !ok || got != 8 {
		t.Fatalf("UTC day selection: %v, %v", got, ok)
	}
	if _, ok := price("launched", time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("usage before a model's first dated rate was priced")
	}
	if got, ok := price("launched", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); !ok || got != 1 {
		t.Fatalf("first dated rate: %v, %v", got, ok)
	}
	if _, ok := c.price("launched", today, 0, 0, 0, 10); ok {
		t.Fatal("unpublished cache-write rate treated as free")
	}
	if got, ok := price("pointer", nov20); !ok || got != 4 {
		t.Fatalf("alias before move: %v, %v", got, ok)
	}
	if got, ok := price("pointer", nov21); !ok || got != 1 {
		t.Fatalf("alias after move: %v, %v", got, ok)
	}
}

func TestLoadCatalogRejectsMisorderedOrMalformedEntries(t *testing.T) {
	for name, body := range map[string]string{
		"undated later entry": `{"sources":["s"],"rates":{"m":[{"input":1},{"input":2}]}}`,
		"descending dates":    `{"sources":["s"],"rates":{"m":[{"from":"2026-02-01","input":1},{"from":"2026-01-01","input":2}]}}`,
		"bad date":            `{"sources":["s"],"rates":{"m":[{"from":"2026-1-1","input":1}]}}`,
		"negative rate":       `{"sources":["s"],"rates":{"m":[{"input":-1}]}}`,
		"alias to unknown":    `{"sources":["s"],"aliases":{"a":[{"model":"x"}]},"rates":{"m":[{"input":1}]}}`,
		"empty alias":         `{"sources":["s"],"aliases":{"a":[]},"rates":{"m":[{"input":1}]}}`,
		"no sources":          `{"rates":{"m":[{"input":1}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("catalog accepted")
				}
			}()
			loadCatalog([]byte(body))
		})
	}
}
