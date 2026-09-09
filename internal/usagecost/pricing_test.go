package usagecost

import (
	"math"
	"testing"
)

func almostEqual(a, b, tolerance float64) bool { return math.Abs(a-b) < tolerance }

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
			got, ok := Price(model, 1_000_000, 1_000_000, 1_000_000, 1_000_000)
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
		got, ok := Price(model, 1_000_000, 1_000_000, 1_000_000, 0)
		if !ok || !almostEqual(got, want, 0.000001) {
			t.Errorf("%s: %v, %v; want %v", model, got, ok, want)
		}
	}
}

func TestPriceDoesNotGuessFutureVariants(t *testing.T) {
	for _, model := range []string{"unknown", "gpt-5.6", "gpt-5.99", "gpt-5.6-ultra", "claude-sonnet-99", "o3-pro", "gpt-5.4-invalid-date"} {
		if got, ok := Price(model, 1_000_000, 0, 0, 0); ok || got != 0 {
			t.Errorf("%s guessed %v", model, got)
		}
	}
}

func TestPriceDocumentedSuffixes(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5[1m]", "claude-sonnet-5-20260801", "claude-sonnet-5-2026-08-01"} {
		got, ok := Price(model, 1_000_000, 0, 0, 0)
		if !ok || got != 2 {
			t.Errorf("%s: %v, %v", model, got, ok)
		}
	}
}

func TestPriceVersionAndMissingClasses(t *testing.T) {
	if got, ok := PriceVersion(CurrentVersion, "gpt-6-astra", 0, 0, 0, 0); !ok || got != 0 {
		t.Fatalf("known zero: %v, %v", got, ok)
	}
	if _, ok := PriceVersion("missing-snapshot", "gpt-6-astra", 100, 0, 0, 0); ok {
		t.Fatal("unknown snapshot used current pricing")
	}
	if _, ok := Price("gpt-6-astra", -1, 0, 0, 0); ok {
		t.Fatal("negative tokens priced")
	}
	if _, ok := Price("gpt-5.4", 0, 0, 0, 10); ok {
		t.Fatal("unpublished cache-write rate treated as free")
	}
}

func TestNewModelBackfillsWithoutFollowingLaterPriceChanges(t *testing.T) {
	const oldVersion = "2026-09-01"
	old := catalog{Rates: map[string]Rate{"known-model": {Input: 2}}}
	launch := catalog{Rates: map[string]Rate{
		"known-model": {Input: 3},
		"new-model":   {Input: 4, Output: 20, Backfill: true},
	}, Aliases: map[string]Alias{"new-alias": {Model: "new-model", Backfill: true}}}
	changed := catalog{Rates: map[string]Rate{
		"known-model": {Input: 8},
		"new-model":   {Input: 9, Output: 40},
		"replacement": {Input: 15, Backfill: true},
	}, Aliases: map[string]Alias{"new-alias": {Model: "replacement", Backfill: true}}}
	snapshots := map[string]catalog{oldVersion: old}
	price := func(model string, want float64, wantOK bool) {
		t.Helper()
		got, ok := newCatalogSet(snapshots).priceVersion(oldVersion, model, 1_000_000, 0, 0, 0)
		if got != want || ok != wantOK {
			t.Fatalf("%s: got %v,%v; want %v,%v", model, got, ok, want, wantOK)
		}
	}
	price("new-model", 0, false)
	price("known-model", 2, true)
	snapshots["2026-09-10"] = launch
	for _, model := range []string{"new-model", "new-alias", "new-model-20260905", "new-model[1m]"} {
		price(model, 4, true)
	}
	snapshots["2026-10-01"] = changed
	for range 3 {
		price("new-model", 4, true)
		price("new-alias", 4, true)
		price("known-model", 2, true)
		price("new-model-ultra", 0, false)
	}
	got, ok := newCatalogSet(snapshots).priceVersion("2026-10-01", "new-model", 1_000_000, 0, 0, 0)
	if !ok || got != 9 {
		t.Fatalf("new usage missed changed rate: %v,%v", got, ok)
	}
	if _, found := old.Rates["new-model"]; found {
		t.Fatal("backfill mutated the original snapshot")
	}
}

func TestBackfillRequiresVerifiedHistoricalApplicability(t *testing.T) {
	cs := newCatalogSet(map[string]catalog{
		"2026-09-01": {Rates: map[string]Rate{}},
		"2026-09-10": {Rates: map[string]Rate{"new-model": {Input: 4}}},
		"2026-10-01": {Rates: map[string]Rate{"new-model": {Input: 9, Backfill: true}}},
	})
	if _, ok := cs.priceVersion("2026-09-01", "new-model", 100, 0, 0, 0); ok {
		t.Fatal("skipped an unverified historical price to use a later one")
	}
	if _, ok := cs.priceVersion("missing-snapshot", "new-model", 100, 0, 0, 0); ok {
		t.Fatal("backfill concealed an unknown pricing version")
	}
}

func TestBackfillMissingCacheWriteKeepsOriginalRatesAndAlias(t *testing.T) {
	snapshots := map[string]catalog{
		"2026-09-01": {Rates: map[string]Rate{"model": {Input: 2, Output: 10, CacheRead: 0.2}}, Aliases: map[string]Alias{"alias": {Model: "model"}}},
		"2026-09-10": {Rates: map[string]Rate{
			"model":       {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 2.5, Backfill: true},
			"replacement": {Input: 50, CacheWrite: 60, Backfill: true},
		}, Aliases: map[string]Alias{"alias": {Model: "replacement", Backfill: true}}},
	}
	before := newCatalogSet(map[string]catalog{"2026-09-01": snapshots["2026-09-01"]})
	if _, ok := before.priceVersion("2026-09-01", "alias", 1_000_000, 1_000_000, 1_000_000, 1_000_000); ok {
		t.Fatal("missing write price was not unpriced")
	}
	cs := newCatalogSet(snapshots)
	for range 3 {
		got, ok := cs.priceVersion("2026-09-01", "alias", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
		if !ok || !almostEqual(got, 14.7, 1e-9) {
			t.Fatalf("backfill repriced original components or alias: %v,%v", got, ok)
		}
		got, ok = cs.priceVersion("2026-09-01", "alias", 1_000_000, 0, 0, 0)
		if !ok || got != 2 {
			t.Fatalf("already-priced usage changed: %v,%v", got, ok)
		}
	}
	if snapshots["2026-09-01"].Rates["model"].CacheWrite != 0 {
		t.Fatal("query modified pinned rates")
	}
	unverified := snapshots["2026-09-10"].Rates["model"]
	unverified.Backfill = false
	snapshots["2026-09-10"].Rates["model"] = unverified
	if _, ok := cs.priceVersion("2026-09-01", "alias", 0, 0, 0, 100); ok {
		t.Fatal("unverified cache-write price backfilled")
	}
}

func TestModelBackfillDoesNotGuessAnAliasesHistoricalTarget(t *testing.T) {
	cs := newCatalogSet(map[string]catalog{
		"2026-09-01": {Rates: map[string]Rate{}},
		"2026-09-10": {Rates: map[string]Rate{"model": {Input: 4, Backfill: true}}, Aliases: map[string]Alias{"moving-alias": {Model: "model"}}},
		"2026-10-01": {Rates: map[string]Rate{"model": {Input: 4, Backfill: true}}, Aliases: map[string]Alias{"moving-alias": {Model: "model", Backfill: true}}},
	})
	if got, ok := cs.priceVersion("2026-09-01", "model", 1_000_000, 0, 0, 0); !ok || got != 4 {
		t.Fatalf("verified model did not backfill: %v,%v", got, ok)
	}
	if _, ok := cs.priceVersion("2026-09-01", "moving-alias", 1_000_000, 0, 0, 0); ok {
		t.Fatal("model price approval inferred an unverified historical alias target")
	}
}
