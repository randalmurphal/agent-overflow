package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func mustGetMirrorBoard(t *testing.T, s *Store, root, boardID string) MirrorStructure {
	t.Helper()
	got, ok, err := s.GetMirrorBoard(root, boardID)
	if err != nil || !ok {
		t.Fatalf("read board %s: %v, %v", boardID, ok, err)
	}
	return got
}

func assertMirrorBoard(t *testing.T, s *Store, root string, want MirrorStructure) {
	t.Helper()
	want.Catalog = MirrorCatalog{}
	if got := mustGetMirrorBoard(t, s, root, want.Board.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("board = %+v\nwant %+v", got, want)
	}
}

// A structure read stores whole, reads back as written, and a read
// identical to the stored one changes nothing. Each part that differs
// reports a change on its own.
func TestReplaceMirrorStructureReportsChanges(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	st := webStructure()
	mustReplaceStructure(t, s, root, st, true)
	assertMirrorBoard(t, s, root, st)
	mustReplaceStructure(t, s, root, st, false)

	for name, edit := range map[string]func(*MirrorStructure){
		"board name":      func(st *MirrorStructure) { st.Board.Name = "Renamed" },
		"board kind":      func(st *MirrorStructure) { st.Board.PlansSprints = false },
		"estimate unit":   func(st *MirrorStructure) { st.Board.EstimateUnit = MirrorEstimateTime },
		"estimate data":   func(st *MirrorStructure) { st.Board.Data = []byte(`{"choices":[]}`) },
		"column name":     func(st *MirrorStructure) { st.Columns[1].Name = "Doing" },
		"status moved":    func(st *MirrorStructure) { st.Columns[0].Statuses, st.Columns[1].Statuses = []string{"1", "3"}, nil },
		"status order":    func(st *MirrorStructure) { st.Columns[2].Statuses = []string{"6", "10001"} },
		"status dropped":  func(st *MirrorStructure) { st.Columns[2].Statuses = []string{"10001"} },
		"column dropped":  func(st *MirrorStructure) { st.Columns = st.Columns[:2] },
		"column added":    func(st *MirrorStructure) { st.Columns = append(st.Columns, MirrorColumn{Name: "Released"}) },
		"sprint state":    func(st *MirrorStructure) { st.Sprints[1].State = MirrorSprintActive },
		"sprint dates":    func(st *MirrorStructure) { st.Sprints[0].EndAt = 2500 },
		"sprint dropped":  func(st *MirrorStructure) { st.Sprints = st.Sprints[:2] },
		"sprint order":    func(st *MirrorStructure) { st.Sprints[0], st.Sprints[1] = st.Sprints[1], st.Sprints[0] },
		"user renamed":    func(st *MirrorStructure) { st.Catalog.Users[0].Name = "User One" },
		"status regroup":  func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = MirrorGroupBacklog },
		"project renamed": func(st *MirrorStructure) { st.Catalog.Projects[0].Key = "APP" },
	} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		edited := webStructure()
		edit(&edited)
		if changed, err := s.ReplaceMirrorStructure(root, edited); err != nil || !changed {
			t.Errorf("%s: changed %v, %v", name, changed, err)
			continue
		}
		edited.Catalog = MirrorCatalog{}
		if got := mustGetMirrorBoard(t, s, root, "10"); !reflect.DeepEqual(got, edited) {
			t.Errorf("%s: board = %+v\nwant %+v", name, got, edited)
		}
		if changed, err := s.ReplaceMirrorStructure(root, edited); err != nil || changed {
			t.Errorf("%s again: changed %v, %v", name, changed, err)
		}
	}
}

// The catalog belongs to the root: a read that does not name a user or a
// status leaves it, and a second board shares it.
func TestMirrorCatalogIsNeverRemovedByAStructure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID, other.Board.Name, other.Catalog = "11", "WEB kanban", MirrorCatalog{}
	other.Sprints = nil
	mustReplaceStructure(t, s, root, other, true)
	catalog, err := s.GetMirrorCatalog(root, []string{"100"}, []string{"acc-user1", "acc-missing"}, []string{"1", "3", "10001", "6"})
	if err != nil {
		t.Fatal(err)
	}
	want := webStructure().Catalog
	want.Statuses = []MirrorStatus{want.Statuses[0], want.Statuses[2], want.Statuses[1], want.Statuses[3]}
	if !reflect.DeepEqual(catalog, want) {
		t.Fatalf("catalog = %+v\nwant %+v", catalog, want)
	}
	if _, err := s.GetMirrorCatalog(root, make([]string, mirrorMaxLookup+1), nil, nil); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("lookup over the bound: %v", err)
	}
}

// Each refused structure leaves the stored one as it was.
func TestReplaceMirrorStructureRefusesInvalidReads(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	for name, edit := range map[string]func(*MirrorStructure){
		"no board ID":      func(st *MirrorStructure) { st.Board.ID = "" },
		"unknown unit":     func(st *MirrorStructure) { st.Board.EstimateUnit = 3 },
		"status in two":    func(st *MirrorStructure) { st.Columns[1].Statuses = []string{"1"} },
		"empty status":     func(st *MirrorStructure) { st.Columns[1].Statuses = []string{""} },
		"too many columns": func(st *MirrorStructure) { st.Columns = make([]MirrorColumn, mirrorMaxColumns+1) },
		"too many sprints": func(st *MirrorStructure) {
			st.Sprints = nil
			for i := range mirrorMaxSprints + 1 {
				st.Sprints = append(st.Sprints, MirrorSprint{ID: fmt.Sprint(i), State: MirrorSprintClosed})
			}
		},
		"sprint repeated":      func(st *MirrorStructure) { st.Sprints[1].ID = st.Sprints[0].ID },
		"sprint state":         func(st *MirrorStructure) { st.Sprints[0].State = 4 },
		"sprint negative time": func(st *MirrorStructure) { st.Sprints[0].StartAt = -1 },
		"status group":         func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = 6 },
		"user repeated":        func(st *MirrorStructure) { st.Catalog.Users = append(st.Catalog.Users, st.Catalog.Users[0]) },
		"project without ID":   func(st *MirrorStructure) { st.Catalog.Projects[0].ID = "" },
		"project repeated":     func(st *MirrorStructure) { st.Catalog.Projects = append(st.Catalog.Projects, st.Catalog.Projects[0]) },
		"user without ID":      func(st *MirrorStructure) { st.Catalog.Users[0].ID = "" },
		"status without ID":    func(st *MirrorStructure) { st.Catalog.Statuses[0].ID = "" },
		"status repeated":      func(st *MirrorStructure) { st.Catalog.Statuses[1].ID = st.Catalog.Statuses[0].ID },
		"status no group":      func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = 0 },
		"negative unit":        func(st *MirrorStructure) { st.Board.EstimateUnit = -1 },
		"sprint without ID":    func(st *MirrorStructure) { st.Sprints[0].ID = "" },
		"sprint state zero":    func(st *MirrorStructure) { st.Sprints[0].State = 0 },
		"sprint negative end":  func(st *MirrorStructure) { st.Sprints[0].EndAt = -1 },
		"sprint negative done": func(st *MirrorStructure) { st.Sprints[0].CompletedAt = -1 },
		"too many statuses": func(st *MirrorStructure) {
			st.Columns = []MirrorColumn{{Name: "All"}}
			for i := range mirrorMaxColumnStatuses + 1 {
				st.Columns[0].Statuses = append(st.Columns[0].Statuses, fmt.Sprint(i))
			}
		},
	} {
		bad := webStructure()
		bad.Board.Name = "Changed"
		edit(&bad)
		if _, err := s.ReplaceMirrorStructure(root, bad); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertMirrorBoard(t, s, root, webStructure())
}

// Deleting a board takes its structure, rows and sync state, and the
// issues no other board has and no thread links.
func TestDeleteMirrorBoardPrunesItsIssues(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID = "11"
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b"), webRow("10003", 5, "c")}}, []byte("c1"))
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "11", Rows: []MirrorRow{webRow("10001", 5, "a")}}, []byte("c2"))
	linkIssue(t, s, "linked", "10003")

	removal, err := s.DeleteMirrorBoard(root, "10")
	if err != nil || !removal.Board || !reflect.DeepEqual(removal.Issues, []string{"10002"}) {
		t.Fatalf("delete = %+v, %v; want the board and 10002", removal, err)
	}
	assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_issues ORDER BY 1`, "10001,10003")
	assertRows(t, s.db, `SELECT board_id FROM tracker_cache_sync`, "11")
	assertRows(t, s.db, `SELECT DISTINCT board_id FROM tracker_cache_sprints`, "11")
	if _, ok, err := s.GetMirrorBoard(root, "10"); err != nil || ok {
		t.Fatalf("deleted board reads: %v, %v", ok, err)
	}
	if removal, err := s.DeleteMirrorBoard(root, "10"); err != nil || removal.Board || removal.Issues != nil {
		t.Fatalf("second delete = %+v, %v", removal, err)
	}
}
