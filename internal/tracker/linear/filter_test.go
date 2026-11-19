package linear

import (
	"encoding/json"
	"strings"
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
		{"ids", IssueFilter{IDs: []string{uuid1, uuid2}}, `{"and":[{"id":{"in":["` + uuid1 + `","` + uuid2 + `"]}}]}`},
		// One alternative per team key, case ignored, numbers deduplicated.
		{"identifiers", IssueFilter{Identifiers: []string{"WEB-12", "app-3", "web-13", "WEB-12"}, Teams: []string{"team-web"}},
			`{"and":[{"or":[{"number":{"in":[12,13]},"team":{"key":{"eqIgnoreCase":"WEB"}}},{"number":{"in":[3]},"team":{"key":{"eqIgnoreCase":"APP"}}}]},` +
				`{"team":{"id":{"in":["team-web"]}}}]}`},
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
		"identifier ID":  {IDs: []string{"WEB-1"}},
		"uppercase ID":   {IDs: []string{strings.ToUpper(uuid1)}},
		"empty ID":       {IDs: []string{""}},
		"bad identifier": {Identifiers: []string{"WEB-1", "WEB 2"}},
	} {
		if _, err := f.input(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseIdentifier(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]struct {
		key string
		n   int
		ok  bool
	}{
		"WEB-12":         {"WEB", 12, true},
		"web-1":          {"web", 1, true},
		"A1B-999":        {"A1B", 999, true},
		"MY-TEAM-7":      {}, // a hyphen is not part of a key
		"WEB-0":          {},
		"WEB-012":        {},
		"WEB-":           {},
		"-12":            {},
		"WEB12":          {},
		"WEB-1x":         {},
		"WEB-+1":         {},
		"WÉB-1":          {},
		"WEB-1000000000": {},
		"WEB-999999999":  {"WEB", 999999999, true},
	} {
		key, n, ok := ParseIdentifier(in)
		if key != want.key || n != want.n || ok != want.ok {
			t.Errorf("%q: %q %d %t, want %+v", in, key, n, ok, want)
		}
	}
}
