package usageledger

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestAdd_WireRowsSumReportedCost(t *testing.T) {
	var s Spend
	for _, cost := range []float64{0.25, 1.5} {
		if err := s.Add(store.UsageDetailRow{CostSource: "wire", CostUSD: cost}); err != nil {
			t.Fatalf("Add(wire): %v", err)
		}
	}
	if s.WireUSD != 1.75 || s.EstimatedUSD != 0 || s.UnpricedRows != 0 {
		t.Fatalf("wire fold = %+v, want WireUSD=1.75 and nothing estimated", s)
	}
	if s.Estimated() {
		t.Fatalf("wire-only spend must report itself exact")
	}
}

func TestAdd_TokenOnlyRowsPriceThroughTheRateTable(t *testing.T) {
	var s Spend
	// A model the rate table knows (family-prefix matched); the exact figure
	// is usagecost's business, so assert the classification, not the number.
	err := s.Add(store.UsageDetailRow{
		CostSource:   "none",
		Model:        "gpt-5.2-codex",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("Add(none, priced model): %v", err)
	}
	if s.EstimatedUSD <= 0 || s.WireUSD != 0 || s.UnpricedRows != 0 {
		t.Fatalf("token-only fold = %+v, want a positive estimate and nothing else", s)
	}
	if !s.Estimated() || s.TotalUSD() != s.EstimatedUSD {
		t.Fatalf("estimated spend must say so: %+v", s)
	}
}

func TestAdd_UnpriceableRowsAreCountedLowerBound(t *testing.T) {
	var s Spend
	err := s.Add(store.UsageDetailRow{
		CostSource:  "none",
		Model:       "some-model-no-rate-table-will-ever-know",
		InputTokens: 500,
		Rows:        3,
	})
	if err != nil {
		t.Fatalf("Add(none, unpriceable): %v", err)
	}
	if s.UnpricedRows != 3 || s.EstimatedUSD != 0 {
		t.Fatalf("unpriceable fold = %+v, want UnpricedRows=3 and no estimate", s)
	}
	if !s.Estimated() {
		t.Fatalf("a total that omits rows must not present itself as exact")
	}
}

// TestAdd_UnknownCostSourceIsAnError pins the package's load-bearing rule: the
// cost_source column has two legal values, so a third is corruption that must
// fail the read, never silently subtract from the total a budget is enforced
// against (root/store lens finding 3, 2026-08-25).
func TestAdd_UnknownCostSourceIsAnError(t *testing.T) {
	var s Spend
	err := s.Add(store.UsageDetailRow{CostSource: "estimated", Model: "m", CostUSD: 5})
	if err == nil || !strings.Contains(err.Error(), `cost_source "estimated"`) {
		t.Fatalf("Add(unknown cost_source) = %v, want an error naming the value", err)
	}
	if s != (Spend{}) {
		t.Fatalf("a failed Add must not fold anything: %+v", s)
	}
}

func TestPriceGroups_FailsWholeOnOneBadGroup(t *testing.T) {
	groups := []store.UsageDetailRow{
		{CostSource: "wire", CostUSD: 1},
		{CostSource: "bogus"},
	}
	if _, err := PriceGroups(groups); err == nil {
		t.Fatalf("PriceGroups with a corrupt group must error, not return a partial total")
	}
}
