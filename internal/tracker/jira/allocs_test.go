package jira

import (
	"encoding/json"
	"testing"
)

// The allocation bounds of the client's hot reads: each is the count
// testing.AllocsPerRun measured, with the headroom stated beside it, so
// a change that makes the read allocate more fails here.

// TestCatalogLookupAllocs bounds a cached Fields read of a 1,500-field
// catalog and a lookup by ID in it, which every provider read that needs
// the sprint field makes. Measured: 0 allocations; bound 0.
func TestCatalogLookupAllocs(t *testing.T) {
	plainBuild(t)
	now := signedIn(t, nil).api.CredentialMark
	c := newCache[struct{}, *Catalog](1, nil, now)
	c.put(c.epoch(), now(), struct{}{}, newCatalog(costCatalog(1500)))
	n := testing.AllocsPerRun(100, func() {
		cat, ok := c.get(struct{}{})
		if !ok {
			t.Fatal("miss")
		}
		if _, ok := cat.Field("customfield_11499"); !ok {
			t.Fatal("lookup")
		}
	})
	if n > 0 {
		t.Errorf("%.1f allocations a lookup, at most 0", n)
	}
}

// TestIssueDecodeAllocs bounds the decode of a 100-issue row page as
// Search and ListIssues decode it, per issue. Measured: 49.1 allocations
// an issue; bound 52 (6% headroom).
func TestIssueDecodeAllocs(t *testing.T) {
	plainBuild(t)
	body := costRowPage(100)
	n := testing.AllocsPerRun(20, func() {
		var v searchAnswer
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		for _, is := range v.Issues {
			_ = is.decode()
		}
	})
	if per := n / 100; per > 52 {
		t.Errorf("%.1f allocations an issue, at most 52", per)
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
