package trackerapp

import (
	"errors"
	"slices"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A project's fields are read once for each site of its boards whose
// tickets take pins: a Linear board's site is not read, two connections
// to one site read it once through the one that is on, a site whose
// only connection is off fails as it does, and a project that lists no
// board reads nothing.
func TestProjectFieldsReadEachJiraSiteOfTheBoardsOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	notes := tracker.Field{ID: "customfield_10040", Name: "Notes", Kind: tracker.KindRichText}
	m.p.fields = func() ([]tracker.Field, error) { return []tracker.Field{notes}, nil }
	linear := m.addLinear()
	second := m.add(NewConnection{Kind: store.TrackerKindJira, Site: jiraSiteIn, Account: "user2@example.com"})
	if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
		t.Fatal(err)
	}
	const project = "fields-project"
	if _, err := m.st.CreateProject(store.Project{ID: project, Path: "/tmp/" + project, Name: project, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.st.ReplaceProjectTrackerBoards(project, nil, []store.ProjectTrackerBoard{
		{ConnectionID: linear.ID, BoardID: "team-1"}, {ConnectionID: m.conn.ID, BoardID: string(testBoard)}, {ConnectionID: second.ID, BoardID: "2"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := m.svc.ProjectFields(t.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tracker != store.TrackerKindJira || got[0].Site != m.conn.Site || len(got[0].Fields) != 1 ||
		got[0].Fields[0].ID != "customfield_10040" || got[0].Fields[0].Kind != "rich_text" {
		t.Fatalf("the project's fields = %+v", got)
	}
	if n := countOf(m.p.callsInOrder(), "Fields"); n != 1 {
		t.Fatalf("%d fields reads, want one for the one Jira site", n)
	}
	// A site whose only connection is off fails as that connection does.
	if got, err := m.svc.ProjectFields(t.Context(), m.project()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a site whose connection is off = %+v, %v; want ErrDisabled", got, err)
	}
	if _, err := m.svc.ProjectFields(t.Context(), "no-such-project"); err != nil {
		t.Fatalf("a project that lists no board = %v", err)
	}
	if _, err := m.svc.ProjectFields(t.Context(), ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("no project = %v", err)
	}
}

func countOf(names []string, name string) int {
	return len(slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n != name }))
}
