package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		changed, _, err := s.ReplaceProjectTrackerBoards(project, boards)
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
	if _, _, err := s.ReplaceProjectTrackerBoards(p, stored); err != nil {
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
		if _, _, err := s.ReplaceProjectTrackerBoards(p, boards); !errors.Is(err, ErrInvalidTrackerBoards) {
			t.Errorf("%s: %v; want ErrInvalidTrackerBoards", name, err)
		}
	}
	if _, _, err := s.ReplaceProjectTrackerBoards("missing", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing project: %v; want sql.ErrNoRows", err)
	}
	if got, err := s.ListProjectTrackerBoards(p); err != nil || !slices.Equal(got, stored) {
		t.Fatalf("after refusals = %+v, %v; want %+v", got, err, stored)
	}
}

// A replace names, from its own delete, the boards whose last listing it
// removed, and the boards it added whose stored copy is retired and not
// yet cleared; a reorder or a move of the board that opens first names
// neither.
func TestReplaceProjectTrackerBoardsNamesWhatItDidToTheStoredBoards(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	p := mustCreateTrackerProject(t, s, "p")
	q := mustCreateTrackerProject(t, s, "q")
	jira := mustCreateTrackerConnection(t, s, jiraConnection("https://example.atlassian.net", "user1@example.com", 1))
	if _, _, err := s.GetOrCreateMirrorRoot(jira.ID); err != nil {
		t.Fatal(err)
	}
	board := func(id string) ProjectTrackerBoard { return ProjectTrackerBoard{ConnectionID: jira.ID, BoardID: id} }
	ref := func(id string) TrackerBoardRef { return TrackerBoardRef{jira.ID, id} }
	replace := func(project string, boards []ProjectTrackerBoard, want TrackerBoardListing) {
		t.Helper()
		_, got, err := s.ReplaceProjectTrackerBoards(project, boards)
		// The order of each list is not specified: its consumer reads
		// membership.
		for _, list := range [][]TrackerBoardRef{got.Unlisted, got.Relisted} {
			slices.SortFunc(list, func(a, b TrackerBoardRef) int { return strings.Compare(a.BoardID, b.BoardID) })
		}
		if err != nil || !slices.Equal(got.Unlisted, want.Unlisted) || !slices.Equal(got.Relisted, want.Relisted) {
			t.Fatalf("replace %s with %+v: %+v, %v; want %+v", project, boards, got, err, want)
		}
	}
	replace(p, []ProjectTrackerBoard{board("10"), board("11"), board("12")}, TrackerBoardListing{})
	replace(q, []ProjectTrackerBoard{board("10")}, TrackerBoardListing{})
	// 10 stays listed by q; 11 goes; 12 stays.
	replace(p, []ProjectTrackerBoard{board("12")}, TrackerBoardListing{Unlisted: []TrackerBoardRef{ref("11")}})
	// A same list, a reorder and an opens-first move name nothing.
	replace(p, []ProjectTrackerBoard{board("12")}, TrackerBoardListing{})
	opens := board("12")
	opens.OpensFirst = true
	replace(p, []ProjectTrackerBoard{opens}, TrackerBoardListing{})
	replace(q, []ProjectTrackerBoard{board("13"), board("10")}, TrackerBoardListing{})
	replace(q, []ProjectTrackerBoard{board("10"), board("13")}, TrackerBoardListing{})
	// 11 is retired and not yet cleared: listing it again names it, and
	// so does q listing it while p lists it again in the same state.
	replace(p, []ProjectTrackerBoard{board("12"), board("11")}, TrackerBoardListing{Relisted: []TrackerBoardRef{ref("11")}})
	replace(q, []ProjectTrackerBoard{board("10"), board("13"), board("11")}, TrackerBoardListing{Relisted: []TrackerBoardRef{ref("11")}})
	replace(q, nil, TrackerBoardListing{Unlisted: []TrackerBoardRef{ref("10"), ref("13")}})
}

// ReadyTrackerBoards drains the named retired boards alone and starts
// each one's next lifetime: another retired board keeps its rows, and a
// board already readied is passed over.
func TestReadyTrackerBoardsReadiesOnlyTheBoardsItNames(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10002", 5, "b")})
	old := currentLifetime(s, root, "12")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10")
	_, listing, err := s.ReplaceProjectTrackerBoards(defaultTestProjectID, []ProjectTrackerBoard{
		{ConnectionID: jira.ID, BoardID: "10"}, {ConnectionID: jira.ID, BoardID: "12"}})
	if err != nil || !slices.Equal(listing.Relisted, []TrackerBoardRef{{jira.ID, "12"}}) {
		t.Fatalf("listing 12 again: %+v, %v", listing, err)
	}
	for range 2 {
		if err := s.ReadyTrackerBoards(context.Background(), listing.Relisted); err != nil {
			t.Fatal(err)
		}
	}
	if next := currentLifetime(s, root, "12"); next <= old {
		t.Fatalf("board 12's lifetime after readying = %d, want one after %d", next, old)
	}
	assertRows(t, s.db, `SELECT board_id || ':' || retired FROM tracker_cache_boards WHERE board_id IN ('11', '12') ORDER BY board_id`, "11:1,12:0")
	assertRows(t, s.db, `SELECT board_id || ':' || count(*) FROM tracker_cache_board_issues GROUP BY board_id ORDER BY board_id`, "11:1")
}

// A drain of a board that another drain overtook, after which the board
// lived again, filled and was retired again, deletes nothing at its end:
// the board's delete would cascade through the new lifetime's rows. The
// second drain runs in the first's pause before its final transaction,
// and the new lifetime keeps only its rows, no columns or sprints.
func TestAnOvertakenDrainKeepsTheBoardsNewRows(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10002", 5, "b")})
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11", "12")
	empty := func() bool {
		t.Helper()
		var n int
		for _, table := range mirrorBoardTables {
			var rows int
			if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE root_id = ? AND board_id = '12'`, root).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			n += rows
		}
		return n == 0
	}
	overtaken := false
	between := func() error {
		if overtaken || !empty() {
			return nil
		}
		// The first drain emptied the board and is about to delete it.
		overtaken = true
		if err := s.drainMirrorBoard(root, "12", noMirrorPause); err != nil {
			return err
		}
		writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10003", 6, "c"), webRow("10004", 6, "d")})
		// Its rows alone are left, so they alone keep it.
		for _, table := range []string{"tracker_cache_board_columns", "tracker_cache_sprints", "tracker_cache_board_unexplained"} {
			if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE root_id = ? AND board_id = '12'`, root); err != nil {
				t.Fatal(err)
			}
		}
		listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
		return nil
	}
	if err := s.drainMirrorBoard(root, "12", between); err != nil {
		t.Fatal(err)
	}
	if !overtaken {
		t.Fatal("the first drain never paused with the board emptied")
	}
	assertRows(t, s.db, `SELECT retired || ':' || (SELECT count(*) FROM tracker_cache_board_issues WHERE root_id = b.root_id AND board_id = '12')
		|| ':' || (SELECT count(*) FROM tracker_cache_board_columns WHERE root_id = b.root_id AND board_id = '12')
		|| ':' || (SELECT count(*) FROM tracker_cache_sprints WHERE root_id = b.root_id AND board_id = '12')
		FROM tracker_cache_boards b WHERE root_id = `+fmt.Sprint(root)+` AND board_id = '12'`, "1:2:0:0")
}
