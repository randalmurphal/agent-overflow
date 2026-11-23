package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// mustCreateTrackerProject creates a project with no threads and returns
// its ID.
func mustCreateTrackerProject(t *testing.T, s *Store, id string) string {
	t.Helper()
	if _, err := s.CreateProject(Project{ID: id, Path: "/projects/" + id, Name: id, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return id
}

func TestReplaceProjectTrackerBoards(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := mustCreateTrackerProject(t, s, "p")
	q := mustCreateTrackerProject(t, s, "q")
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 1})

	replace := func(project string, boards []ProjectTrackerBoard, want bool) {
		t.Helper()
		changed, err := s.ReplaceProjectTrackerBoards(project, boards)
		if err != nil || changed != want {
			t.Fatalf("replace %s with %+v: changed %v, %v; want %v", project, boards, changed, err, want)
		}
		got, err := s.ListProjectTrackerBoards(project)
		if err != nil || !slices.Equal(got, boards) {
			t.Fatalf("list %s = %+v, %v; want %+v", project, got, err, boards)
		}
	}
	list := []ProjectTrackerBoard{
		{ConnectionID: linear.ID, BoardID: "team-a"},
		{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true},
		{ConnectionID: jira.ID, BoardID: "11"},
	}
	replace(p, list, true)
	replace(p, list, false)
	reordered := []ProjectTrackerBoard{list[2], list[0], list[1]}
	replace(p, reordered, true)
	moved := []ProjectTrackerBoard{list[0], {ConnectionID: jira.ID, BoardID: "10"}, {ConnectionID: jira.ID, BoardID: "11", OpensFirst: true}}
	replace(p, moved, true)
	none := []ProjectTrackerBoard{list[0], {ConnectionID: jira.ID, BoardID: "10"}, {ConnectionID: jira.ID, BoardID: "11"}}
	replace(p, none, true)
	replace(q, []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}, {ConnectionID: jira.ID, BoardID: "30"}}, true)

	listed, err := s.ListTrackerBoardsListedByProjects()
	want := []TrackerBoardRef{{jira.ID, "10"}, {jira.ID, "11"}, {jira.ID, "30"}, {linear.ID, "team-a"}}
	slices.SortFunc(want, func(a, b TrackerBoardRef) int {
		if a.ConnectionID != b.ConnectionID {
			return strings.Compare(a.ConnectionID, b.ConnectionID)
		}
		return strings.Compare(a.BoardID, b.BoardID)
	})
	if err != nil || !slices.Equal(listed, want) {
		t.Fatalf("listed = %+v, %v; want %+v", listed, err, want)
	}
	if projects, err := s.ListProjectsForTrackerBoard(TrackerBoardRef{jira.ID, "10"}); err != nil || !slices.Equal(projects, []string{p, q}) {
		t.Fatalf("projects of board 10 = %v, %v", projects, err)
	}
	if projects, err := s.ListProjectsForTrackerBoard(TrackerBoardRef{jira.ID, "30"}); err != nil || !slices.Equal(projects, []string{q}) {
		t.Fatalf("projects of board 30 = %v, %v", projects, err)
	}

	replace(p, nil, true)
	replace(p, []ProjectTrackerBoard{}, false)
	if projects, err := s.ListProjectsForTrackerBoard(TrackerBoardRef{jira.ID, "10"}); err != nil || !slices.Equal(projects, []string{q}) {
		t.Fatalf("projects of board 10 after p emptied its list = %v, %v", projects, err)
	}

	// A project's delete takes its list.
	if err := s.DeleteProject(q); err != nil {
		t.Fatal(err)
	}
	if listed, err := s.ListTrackerBoardsListedByProjects(); err != nil || len(listed) != 0 {
		t.Fatalf("listed after the project's delete = %+v, %v", listed, err)
	}
}

// Every refusal leaves the stored list as it was.
func TestReplaceProjectTrackerBoardsRefusals(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := mustCreateTrackerProject(t, s, "p")
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	tempo := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindTempo, Site: "https://api.eu.tempo.io", ParentID: jira.ID, CreatedAt: 1})
	stored := []ProjectTrackerBoard{{ConnectionID: jira.ID, BoardID: "10", OpensFirst: true}}
	if _, err := s.ReplaceProjectTrackerBoards(p, stored); err != nil {
		t.Fatal(err)
	}

	for name, boards := range map[string][]ProjectTrackerBoard{
		"listed twice":       {{ConnectionID: jira.ID, BoardID: "11"}, {ConnectionID: jira.ID, BoardID: "11"}},
		"two open first":     {{ConnectionID: jira.ID, BoardID: "11", OpensFirst: true}, {ConnectionID: jira.ID, BoardID: "12", OpensFirst: true}},
		"no board ID":        {{ConnectionID: jira.ID}},
		"no connection":      {{BoardID: "11"}},
		"missing connection": {{ConnectionID: "missing", BoardID: "11"}},
		"tempo connection":   {{ConnectionID: tempo.ID, BoardID: "11"}},
	} {
		if _, err := s.ReplaceProjectTrackerBoards(p, boards); !errors.Is(err, ErrInvalidTrackerBoards) {
			t.Errorf("%s: %v; want ErrInvalidTrackerBoards", name, err)
		}
	}
	if _, err := s.ReplaceProjectTrackerBoards("missing", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing project: %v; want sql.ErrNoRows", err)
	}
	if got, err := s.ListProjectTrackerBoards(p); err != nil || !slices.Equal(got, stored) {
		t.Fatalf("after refusals = %+v, %v; want %+v", got, err, stored)
	}
}
