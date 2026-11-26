package jiraprovider

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// A detail read carries the issue's value of each number field it
// returned, null as none, as the board's rows read it: WEB-1's story
// points and original time estimate (seconds), WEB-2's none. The board
// estimating by story points reads the same value from the detail as from
// its row.
func TestADetailCarriesItsNumberFieldsAsEstimates(t *testing.T) {
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		secs := int64(7200)
		site.Issues[0].OriginalEstimate = &secs
	})
	points := tracker.Estimate{Unit: tracker.UnitPoints, Field: "customfield_10016"}
	byTime := tracker.Estimate{Unit: tracker.UnitTime, Field: timeEstimateField}
	first := s.detail(t, "10101")
	for _, c := range []struct {
		e    tracker.Estimate
		want float64
	}{{points, 5}, {byTime, 7200}} {
		if v, carried := first.EstimateBy(c.e); !carried || v == nil || *v != c.want {
			t.Errorf("WEB-1 by %s = %v, %v; want %v", c.e.Field, describe(v), carried, c.want)
		}
	}
	if row := s.row(t, "10101"); row.Estimate == nil || *row.Estimate != 5 {
		t.Errorf("WEB-1's row estimate %v; want the detail's 5", describe(row.Estimate))
	}
	second := s.detail(t, "10102")
	if v, carried := second.EstimateBy(points); !carried || v != nil {
		t.Errorf("WEB-2 by story points = %v, %v; want carried as none", describe(v), carried)
	}
	if _, carried := first.EstimateBy(tracker.Estimate{Unit: tracker.UnitNone}); carried {
		t.Error("a board with no estimate reads one from the detail")
	}
}

// A number field the read left out, as Jira leaves out one outside the
// issue's context, is not carried, and neither is a field that is not a
// number or a value that is not one.
func TestEstimatesOfLeavesOutWhatTheReadDidNotReturn(t *testing.T) {
	s := site{fields: jira.NewCatalog([]jira.Field{
		{ID: "customfield_1", Name: "Points", Schema: jira.Schema{Type: "number"}},
		{ID: "customfield_2", Name: "Other points", Schema: jira.Schema{Type: "number"}},
		{ID: "customfield_3", Name: "Team", Schema: jira.Schema{Type: "string"}},
		{ID: "customfield_4", Name: "Broken", Schema: jira.Schema{Type: "number"}},
	})}
	is := &jira.Issue{Key: "WEB-1", Raw: map[string]json.RawMessage{
		"customfield_1": json.RawMessage(`3`), "customfield_3": json.RawMessage(`"7"`), "customfield_4": json.RawMessage(`"x"`),
	}}
	got := estimatesOf(s, is)
	if len(got) != 1 || got[0].Field != "customfield_1" || got[0].Value == nil || *got[0].Value != 3 {
		t.Fatalf("estimates %+v; want customfield_1's 3 alone", got)
	}
}

func describe(v *float64) any {
	if v == nil {
		return "none"
	}
	return *v
}
