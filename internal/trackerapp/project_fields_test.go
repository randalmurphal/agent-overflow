package trackerapp

import (
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A project's fields are read once for each Jira site of its boards and
// of its pins: a Linear board's site is not read, two connections to one
// site read it once through the one that is on, a site whose only
// connection is off says so beside the others, a site the setup pins but
// no board lists is read through another connection to it, and one no
// connection reaches is listed with why.
func TestProjectFieldsReadEachJiraSiteOnceAndSayEachSitesFailure(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	notes := tracker.Field{ID: "customfield_10040", Name: "Notes", Kind: tracker.KindRichText}
	m.p.fields = func() ([]tracker.Field, error) { return []tracker.Field{notes}, nil }
	linear := m.addLinear()
	second := m.add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: "user2@example.com"})
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	project := func(id string, boards ...store.ProjectTrackerBoard) string {
		t.Helper()
		if _, err := m.st.CreateProject(store.Project{ID: id, Path: "/tmp/" + id, Name: id, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.st.ReplaceProjectTrackerBoards(id, nil, boards); err != nil {
			t.Fatal(err)
		}
		return id
	}
	read := func(id string) []SiteFields {
		t.Helper()
		got, err := m.svc.ProjectFields(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	both := project("fields-both", store.ProjectTrackerBoard{ConnectionID: linear.ID, BoardID: "team-1"},
		store.ProjectTrackerBoard{ConnectionID: m.conn.ID, BoardID: string(testBoard)}, store.ProjectTrackerBoard{ConnectionID: second.ID, BoardID: "2"})
	got := read(both)
	if len(got) != 1 || got[0].Tracker != store.TrackerKindJira || got[0].Site != m.conn.Site || got[0].Err != nil ||
		!slices.Equal(got[0].Fields, []PinField{{ID: "customfield_10040", Name: "Notes", Kind: "rich_text"}}) {
		t.Fatalf("the project's fields = %+v", got)
	}
	if n := countOf(m.p.callsInOrder(), "Fields"); n != 1 {
		t.Fatalf("%d fields reads, want one for the one Jira site", n)
	}

	// The fixture's project lists a board of the connection that is off:
	// its site is read through the other connection to it.
	if got := read(m.project()); len(got) != 1 || got[0].Err != nil || len(got[0].Fields) != 1 {
		t.Fatalf("a site whose board's connection is off, with another on = %+v", got)
	}
	if _, err := m.svc.SetEnabled(second.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := read(m.project()); len(got) != 1 || !errors.Is(got[0].Err, ErrDisabled) || len(got[0].Fields) != 0 {
		t.Fatalf("a site whose connections are all off = %+v; want ErrDisabled", got)
	}
	if _, err := m.svc.SetEnabled(second.ID, true); err != nil {
		t.Fatal(err)
	}

	// No board, and pins of this site and of one no connection reaches.
	pinned := project("fields-pinned")
	m.saveSetup(t, pinned, Setup{TicketPins: []SitePins{
		{Tracker: "jira", Site: m.conn.Site, Fields: &[]string{"customfield_10040"}},
		{Tracker: "jira", Site: "https://other.atlassian.net", Fields: &[]string{"customfield_1"}},
	}})
	got = read(pinned)
	if len(got) != 2 || got[0].Site != m.conn.Site || got[0].Err != nil || len(got[0].Fields) != 1 ||
		got[1].Site != "https://other.atlassian.net" || !errors.Is(got[1].Err, ErrNoSiteConnection) {
		t.Fatalf("the sites of a project's pins = %+v", got)
	}
	if got := read(project("fields-none")); len(got) != 0 {
		t.Fatalf("a project that lists no board and pins nothing = %+v", got)
	}
	if _, err := m.svc.ProjectFields(t.Context(), ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("no project = %v", err)
	}
}

func countOf(names []string, name string) int {
	return len(slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n != name }))
}
