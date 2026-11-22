package jiraprovider

import (
	"fmt"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker/jira"
)

// The allocation bounds of the provider's mapping of decoded issues: each
// is the count testing.AllocsPerRun measured, with the headroom stated
// beside it, so a change that makes the mapping allocate more fails here.
// The site, the board and the issues are what the provider and its jira
// client read from the fake as Cards and ChangedRows read them; only the
// mapping is measured, since the fake's allocations would count too.

// mappingRun is a provider on the cost fixture with 1,500 more custom
// fields, and its site as the provider reads it.
func mappingRun(t *testing.T) (*costRun, site) {
	t.Helper()
	r := newCostRunOf(t, false, func(s *trackerfake.JiraSite) {
		for i := range 1500 {
			s.Fields = append(s.Fields, trackerfake.JiraField{ID: fmt.Sprintf("customfield_%d", 11000+i), Name: fmt.Sprintf("Field %d", i),
				Schema: trackerfake.JiraSchema{Type: "string", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:textfield", CustomID: int64(11000 + i)}})
		}
	})
	s, err := r.p.site(r.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.fields.Len() < 1500 || s.sprint == "" {
		t.Fatalf("a site of %d fields, sprint field %q", s.fields.Len(), s.sprint)
	}
	return r, s
}

// TestCardMappingAllocs bounds the mapping of 100 issues to cards
// against a catalog of over 1,500 fields, as Cards, Lookup and Search map
// them, per issue. Measured: 8.7 allocations an issue; bound 9.2 (6%
// headroom).
func TestCardMappingAllocs(t *testing.T) {
	plainBuild(t)
	r, s := mappingRun(t)
	refs := make([]string, 100)
	for i := range refs {
		refs[i] = costIssueID(i)
	}
	res, err := r.p.jira.BulkFetch(t.Context(), refs, s.cardFields()...)
	if err != nil || len(res.Issues) != 100 {
		t.Fatalf("%d issues, %v", len(res.Issues), err)
	}
	n := testing.AllocsPerRun(20, func() {
		cat := newCatalog()
		for _, is := range res.Issues {
			if _, err := s.cardOf(is, cat); err != nil {
				t.Fatal(err)
			}
		}
	})
	if per := n / 100; per > 9.2 {
		t.Errorf("%.2f allocations an issue, at most 9.2", per)
	}
}

// TestDeltaPageMappingAllocs bounds the mapping of a 100-issue board
// page to rows, as ChangedRows maps a delta page and BoardRows a
// listing, per row. Measured: 12.6 allocations a row; bound 13.3 (6%
// headroom).
func TestDeltaPageMappingAllocs(t *testing.T) {
	plainBuild(t)
	r, s := mappingRun(t)
	ctx := r.t.Context()
	b, err := r.p.board(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.p.jira.ListIssues(ctx, jira.ListQuery{List: jira.BoardIssues(b.cfg.BoardID), Fields: b.rowFields(s), MaxResults: 100})
	if err != nil || len(page.Issues) != 100 {
		t.Fatalf("%d issues, %v", len(page.Issues), err)
	}
	n := testing.AllocsPerRun(20, func() {
		cat := newCatalog()
		for _, is := range page.Issues {
			if _, err := b.rowOf(s, is, cat); err != nil {
				t.Fatal(err)
			}
		}
	})
	if per := n / 100; per > 13.3 {
		t.Errorf("%.2f allocations a row, at most 13.3", per)
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
