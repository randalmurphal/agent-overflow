package linear

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIssueFilterInput(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 2, 10, 0, 0, 123456789, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		name   string
		filter IssueFilter
		want   string
	}{
		{"zero", IssueFilter{}, `null`},
		{"teams", IssueFilter{Teams: []string{"team-web", "team-app"}}, `{"and":[{"team":{"id":{"in":["team-web","team-app"]}}}]}`},
		{"states and types", IssueFilter{States: []string{"s1"}, StateTypes: []StateType{StateStarted, StateUnstarted}},
			`{"and":[{"state":{"id":{"in":["s1"]}}},{"state":{"type":{"in":["started","unstarted"]}}}]}`},
		{"cycle or none", IssueFilter{Cycle: Match{IDs: []string{"c1"}, None: true}},
			`{"and":[{"or":[{"cycle":{"id":{"in":["c1"]}}},{"cycle":{"null":true}}]}]}`},
		{"unassigned", IssueFilter{Assignee: Match{None: true}}, `{"and":[{"assignee":{"null":true}}]}`},
		{"project and parent", IssueFilter{Project: Match{IDs: []string{"p1"}}, Parent: Match{IDs: []string{"WEB-1"}}},
			`{"and":[{"project":{"id":{"in":["p1"]}}},{"parent":{"id":{"in":["WEB-1"]}}}]}`},
		{"labels", IssueFilter{Labels: []string{"l1", "l2"}}, `{"and":[{"labels":{"some":{"id":{"in":["l1","l2"]}}}}]}`},
		{"updated since", IssueFilter{UpdatedSince: since}, `{"and":[{"updatedAt":{"gte":"2026-11-02T09:00:00.123Z"}}]}`},
	} {
		in, err := tc.filter.input()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got, _ := json.Marshal(in)
		if string(got) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
	for name, f := range map[string]IssueFilter{
		"empty team":     {Teams: []string{""}},
		"empty state":    {States: []string{"a", ""}},
		"empty type":     {StateTypes: []StateType{""}},
		"empty assignee": {Assignee: Match{IDs: []string{""}}},
		"empty label":    {Labels: []string{""}},
	} {
		if _, err := f.input(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
