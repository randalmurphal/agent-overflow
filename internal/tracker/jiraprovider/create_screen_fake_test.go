package jiraprovider

import (
	"slices"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// The create form's fields are read from Jira each time they are shown:
// two reads within the client's minute, a component added on Jira
// between them, show it the second time. The create that follows takes
// the second read and reads the screen no more: it can pick the component
// that read added, and Jira stores it.
func TestFakeTheCreateFormReadsItsScreenEachTime(t *testing.T) {
	components := func(names ...string) []trackerfake.JiraNamed {
		out := make([]trackerfake.JiraNamed, len(names))
		for i, n := range names {
			out[i] = trackerfake.JiraNamed{ID: string(rune('1'+i)) + "0001", Name: n}
		}
		return out
	}
	s := newEditedScenario(t, nil, func(site *trackerfake.JiraSite) {
		site.Tempo = nil
		site.Projects[0].Components = components("Web")
	})
	offered := func() []string {
		t.Helper()
		fs := scenarioMust[[]tracker.ScreenField](t)(s.p.CreateFields(s.ctx, "10000", "10002"))
		i := slices.IndexFunc(fs, func(f tracker.ScreenField) bool { return f.ID == "components" })
		if i < 0 {
			t.Fatalf("no components field in %+v", fs)
		}
		var names []string
		for _, c := range fs[i].Allowed {
			names = append(names, c.Name)
		}
		return names
	}
	if got := offered(); !slices.Equal(got, []string{"Web"}) {
		t.Fatalf("first read offers %v", got)
	}
	f := s.e.Snapshot()
	f.Jira[0].Token, f.Jira[0].Members[0].Token = "scenario-jira-1", scenarioMemberToken
	f.Jira[0].Projects[0].Components = components("Web", "API")
	if err := s.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	if got := offered(); !slices.Equal(got, []string{"Web", "API"}) {
		t.Fatalf("a read within the minute after the change offers %v", got)
	}
	allApplied(t, "create", s.p.Create(s.ctx, tracker.CreateInput{Project: "10000", Type: "10002", Title: "Created task",
		Fields: []tracker.FieldEdit{{Field: "components", Value: tracker.PickValues("20001")}}}))
	created := s.e.Snapshot().Jira[0].Issues
	if is := created[len(created)-1]; is.Summary != "Created task" || !slices.Equal(is.Components, []string{"20001"}) {
		t.Errorf("Jira holds %q with components %v; want Created task with API (20001)", is.Summary, is.Components)
	}
	if n := countRoute(s.e, "GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}"); n != 2 {
		t.Errorf("%d screen reads; want 2, the form's, with none for the create", n)
	}
}

// countRoute is how many requests the fake answered on route.
func countRoute(e *trackerfake.Engine, route string) int {
	n := 0
	for _, r := range e.Requests(0).Requests {
		if r.Route == route {
			n++
		}
	}
	return n
}
