package jiraprovider

import (
	"testing"

	"agent-overflow/internal/tracker/jira"
)

// The allocation bounds of the provider's mapping of decoded issues: each
// is the count testing.AllocsPerRun measured, with the headroom stated
// beside it, so a change that makes the mapping allocate more fails here.

// TestCardMappingAllocs bounds the mapping of 100 decoded issues to
// cards against a 1,500-field catalog, as Cards, Lookup and Search map
// them, per issue. Measured: 7.1 allocations an issue; bound 8.
func TestCardMappingAllocs(t *testing.T) {
	plainBuild(t)
	s := benchSite(1500)
	issues := benchIssues(100)
	n := testing.AllocsPerRun(20, func() {
		cat := newCatalog()
		for _, is := range issues {
			if _, err := s.cardOf(is, cat); err != nil {
				t.Fatal(err)
			}
		}
	})
	if per := n / 100; per > 8 {
		t.Errorf("%.1f allocations an issue, at most 8", per)
	}
}

// TestDeltaPageMappingAllocs bounds the mapping of a decoded 100-issue
// page to board rows, as ChangedRows maps a delta page and BoardRows a
// listing, per row. Measured: 10.4 allocations a row; bound 11.
func TestDeltaPageMappingAllocs(t *testing.T) {
	plainBuild(t)
	s := benchSite(1500)
	bd := board{id: "1", rank: "customfield_10019", cfg: jira.BoardConfig{BoardID: 1, RankFieldID: 10019,
		Estimation: jira.Estimation{Type: jira.EstimateField, FieldID: "customfield_10016"}}}
	issues := benchIssues(100)
	n := testing.AllocsPerRun(20, func() {
		cat := newCatalog()
		for _, is := range issues {
			if _, err := bd.rowOf(s, is, cat); err != nil {
				t.Fatal(err)
			}
		}
	})
	if per := n / 100; per > 11 {
		t.Errorf("%.1f allocations a row, at most 11", per)
	}
}

// plainBuild skips an allocation bound under the race detector, whose
// instrumentation changes what escapes; the bounds hold for a plain
// build, which make go-test without -race runs.
func plainBuild(t *testing.T) {
	t.Helper()
	if raceOn {
		t.Skip("allocation counts differ under the race detector")
	}
}
