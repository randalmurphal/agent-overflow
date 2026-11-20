package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func mustGetMirrorBoard(t *testing.T, s *Store, root int64, boardID string) MirrorStructure {
	t.Helper()
	got, ok, err := s.GetMirrorBoard(root, boardID)
	if err != nil || !ok {
		t.Fatalf("read board %s: %v, %v", boardID, ok, err)
	}
	return got
}

func assertMirrorBoard(t *testing.T, s *Store, root int64, want MirrorStructure) {
	t.Helper()
	want.Catalog, want.ReadAt, want.Lifetime = MirrorCatalog{}, 0, currentLifetime(s, root, want.Board.ID)
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
		"board name":     func(st *MirrorStructure) { st.Board.Name = "Renamed" },
		"board project":  func(st *MirrorStructure) { st.Board.ProjectID = "" },
		"board kind":     func(st *MirrorStructure) { st.Board.PlansSprints = false },
		"estimate unit":  func(st *MirrorStructure) { st.Board.EstimateUnit = MirrorEstimateTime },
		"estimate data":  func(st *MirrorStructure) { st.Board.Data = []byte(`{"choices":[]}`) },
		"column name":    func(st *MirrorStructure) { st.Columns[1].Name = "Doing" },
		"status moved":   func(st *MirrorStructure) { st.Columns[0].Statuses, st.Columns[1].Statuses = []string{"1", "3"}, nil },
		"status order":   func(st *MirrorStructure) { st.Columns[2].Statuses = []string{"6", "10001"} },
		"status dropped": func(st *MirrorStructure) { st.Columns[2].Statuses = []string{"10001"} },
		"column dropped": func(st *MirrorStructure) { st.Columns = st.Columns[:2] },
		"column added":   func(st *MirrorStructure) { st.Columns = append(st.Columns, MirrorColumn{Name: "Released"}) },
		"sprint state":   func(st *MirrorStructure) { st.Sprints[1].State = MirrorSprintActive },
		"sprint dates":   func(st *MirrorStructure) { st.Sprints[0].EndAt = 2500 },
		"sprint origin":  func(st *MirrorStructure) { st.Sprints[0].OriginBoardID = "11" },
		"sprint dropped": func(st *MirrorStructure) { st.Sprints = st.Sprints[:2] },
		"sprint order":   func(st *MirrorStructure) { st.Sprints[0], st.Sprints[1] = st.Sprints[1], st.Sprints[0] },
		"user renamed":   func(st *MirrorStructure) { st.Catalog.Users[0].Name = "User One" },
		"user data":      func(st *MirrorStructure) { st.Catalog.Users[0].Data = nil },
		"status regroup": func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = MirrorGroupBacklog },
	} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		edited := webStructure()
		edit(&edited)
		edited.ReadAt = 2
		if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, edited)); err != nil || got.MirrorWrite != (MirrorWrite{Changed: true}) {
			t.Errorf("%s: %+v, %v", name, got, err)
			continue
		}
		readBack := edited
		readBack.Catalog, readBack.ReadAt, readBack.Lifetime = MirrorCatalog{}, 0, currentLifetime(s, root, "10")
		if got := mustGetMirrorBoard(t, s, root, "10"); !reflect.DeepEqual(got, readBack) {
			t.Errorf("%s: board = %+v\nwant %+v", name, got, readBack)
		}
		if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, edited)); err != nil || got.MirrorWrite != (MirrorWrite{}) {
			t.Errorf("%s again: %+v, %v", name, got, err)
		}
	}
}

// A structure read that began before the one that stored the board is
// superseded and stores nothing; one that began later replaces it, and
// an identical later read is admitted too: it moves the read time, so a
// different read that began between the two is superseded.
func TestMirrorStructureFollowsItsReadOrder(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	st := webStructure()
	st.ReadAt = 20
	mustReplaceStructure(t, s, root, st, true)
	early := webStructure()
	early.ReadAt = 10
	early.Board.Name = "Old name"
	early.Catalog.Users[0].Name = "Old user"
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, early)); err != nil || got.MirrorWrite != (MirrorWrite{Superseded: true}) {
		t.Fatalf("earlier read = %+v, %v", got, err)
	}
	assertMirrorBoard(t, s, root, webStructure())
	assertRows(t, s.db, `SELECT name FROM tracker_cache_users`, "User 1")
	st.ReadAt = 30
	mustReplaceStructure(t, s, root, st, false)
	assertRows(t, s.db, `SELECT read_at FROM tracker_cache_boards WHERE board_id = '10'`, "30")
	assertRows(t, s.db, `SELECT read_at || ' ' || changed_at FROM tracker_cache_users`, "30 20")
	early.ReadAt = 25
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, early)); err != nil || got.MirrorWrite != (MirrorWrite{Superseded: true}) {
		t.Fatalf("read between the two = %+v, %v", got, err)
	}
	assertRows(t, s.db, `SELECT read_at || ' ' || name FROM tracker_cache_boards WHERE board_id = '10'`, "30 WEB board")
	assertRows(t, s.db, `SELECT name FROM tracker_cache_users`, "User 1")
	early.ReadAt = 35
	mustReplaceStructure(t, s, root, early, true)
	assertRows(t, s.db, `SELECT read_at || ' ' || name FROM tracker_cache_boards WHERE board_id = '10'`, "35 Old name")
}

// A board no project lists, or one whose root is retired by its last
// listing going, refuses a structure and stores nothing.
func TestReplaceMirrorStructureRefusesAnUnlistedBoard(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	unlisted := webStructure()
	unlisted.Board.ID = "13"
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, unlisted)); !errors.Is(err, ErrTrackerCacheUnlisted) {
		t.Fatalf("unlisted board: %v", err)
	}
	if _, err := s.CreateProject(newProject("p2", "/tmp/p2", "P2")); err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, "p2", jira.ID, "13")
	mustReplaceStructure(t, s, root, unlisted, true)
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_users`, "1")
}

// The catalog belongs to the root: a read that does not name a user or a
// status leaves it, and a second board shares it. A catalog row is
// replaced only by a read that began no earlier than the one that stored
// it; a lookup returns what is stored, sorted by ID.
func TestMirrorCatalogIsNeverRemovedByAStructure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID, other.Board.Name, other.Catalog = "11", "WEB kanban", MirrorCatalog{}
	other.Sprints = nil
	mustReplaceStructure(t, s, root, other, true)
	catalog, err := s.GetMirrorCatalog(root, []string{"acc-user1", "acc-missing"}, []string{"1", "3", "10001", "6"})
	if err != nil {
		t.Fatal(err)
	}
	want := webStructure().Catalog
	want.Statuses = []MirrorStatus{want.Statuses[0], want.Statuses[2], want.Statuses[1], want.Statuses[3]}
	if !reflect.DeepEqual(catalog, want) {
		t.Fatalf("catalog = %+v\nwant %+v", catalog, want)
	}
	if catalog, err := s.GetMirrorCatalog(root, nil, []string{"6", "1"}); err != nil || !reflect.DeepEqual(catalog, MirrorCatalog{Statuses: []MirrorStatus{want.Statuses[0], want.Statuses[3]}}) {
		t.Fatalf("statuses by ID = %+v, %v", catalog, err)
	}

	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Users: []MirrorUser{{ID: "acc-user1", Name: "Newer name"}}}, 3); err != nil || !changes.Catalog {
		t.Fatalf("later catalog = %+v, %v", changes, err)
	}
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Users: []MirrorUser{{ID: "acc-user1", Name: "Older name"}}}, 2); err != nil || changes.Catalog {
		t.Fatalf("earlier catalog = %+v, %v", changes, err)
	}
	assertRows(t, s.db, `SELECT name || ' ' || read_at FROM tracker_cache_users`, "Newer name 3")
	newer, older := want.Statuses[0], want.Statuses[0]
	newer.Name, older.Name = "Newer status", "Older status"
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Statuses: []MirrorStatus{newer}}, 3); err != nil || !changes.Catalog {
		t.Fatalf("later status = %+v, %v", changes, err)
	}
	if changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Statuses: []MirrorStatus{older}}, 2); err != nil || changes.Catalog {
		t.Fatalf("earlier status = %+v, %v", changes, err)
	}
	assertRows(t, s.db, `SELECT name || ' ' || read_at FROM tracker_cache_statuses WHERE status_id = '`+newer.ID+`'`, "Newer status 3")

	if _, err := s.GetMirrorCatalog(root, make([]string, mirrorMaxLookup+1), nil); !errors.Is(err, ErrInvalidTrackerCache) {
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
		"no read time":     func(st *MirrorStructure) { st.ReadAt = 0 },
		"unknown unit":     func(st *MirrorStructure) { st.Board.EstimateUnit = 3 },
		"negative unit":    func(st *MirrorStructure) { st.Board.EstimateUnit = -1 },
		"status in two":    func(st *MirrorStructure) { st.Columns[1].Statuses = []string{"1"} },
		"empty status":     func(st *MirrorStructure) { st.Columns[1].Statuses = []string{""} },
		"too many columns": func(st *MirrorStructure) { st.Columns = make([]MirrorColumn, mirrorMaxColumns+1) },
		"too many sprints": func(st *MirrorStructure) {
			st.Sprints = nil
			for i := range mirrorMaxSprints + 1 {
				st.Sprints = append(st.Sprints, MirrorSprint{ID: fmt.Sprint(i), State: MirrorSprintClosed})
			}
		},
		"too many statuses": func(st *MirrorStructure) {
			st.Columns = []MirrorColumn{{Name: "All"}}
			for i := range mirrorMaxColumnStatuses + 1 {
				st.Columns[0].Statuses = append(st.Columns[0].Statuses, fmt.Sprint(i))
			}
		},
		"too many users": func(st *MirrorStructure) {
			for i := range mirrorMaxCatalog {
				st.Catalog.Users = append(st.Catalog.Users, MirrorUser{ID: fmt.Sprint("acc-", i)})
			}
		},
		"sprint repeated":      func(st *MirrorStructure) { st.Sprints[1].ID = st.Sprints[0].ID },
		"sprint without ID":    func(st *MirrorStructure) { st.Sprints[0].ID = "" },
		"sprint state":         func(st *MirrorStructure) { st.Sprints[0].State = 4 },
		"sprint state zero":    func(st *MirrorStructure) { st.Sprints[0].State = 0 },
		"sprint negative time": func(st *MirrorStructure) { st.Sprints[0].StartAt = -1 },
		"sprint negative end":  func(st *MirrorStructure) { st.Sprints[0].EndAt = -1 },
		"sprint negative done": func(st *MirrorStructure) { st.Sprints[0].CompletedAt = -1 },
		"status group":         func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = 6 },
		"status no group":      func(st *MirrorStructure) { st.Catalog.Statuses[0].Group = 0 },
		"status without ID":    func(st *MirrorStructure) { st.Catalog.Statuses[0].ID = "" },
		"status repeated":      func(st *MirrorStructure) { st.Catalog.Statuses[1].ID = st.Catalog.Statuses[0].ID },
		"user without ID":      func(st *MirrorStructure) { st.Catalog.Users[0].ID = "" },
		"user repeated":        func(st *MirrorStructure) { st.Catalog.Users = append(st.Catalog.Users, st.Catalog.Users[0]) },
	} {
		bad := webStructure()
		bad.Board.Name = "Changed"
		bad.ReadAt = 2
		edit(&bad)
		if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, bad)); !errors.Is(err, ErrInvalidTrackerCache) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertMirrorBoard(t, s, root, webStructure())
}

// A structure lands whole or not at all.
func TestReplaceMirrorStructureIsAtomic(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustExec(t, s.db, `CREATE TRIGGER test_refuse_sprint BEFORE INSERT ON tracker_cache_sprints BEGIN SELECT RAISE(ABORT, 'sprint refused'); END`)
	edited := webStructure()
	edited.ReadAt = 2
	edited.Board.Name = "Renamed"
	edited.Catalog.Users[0].Name = "Renamed user"
	edited.Sprints = append(edited.Sprints, MirrorSprint{ID: "23", State: MirrorSprintFuture})
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, edited)); err == nil {
		t.Fatal("structure stored with a sprint refused")
	}
	assertMirrorBoard(t, s, root, webStructure())
	assertRows(t, s.db, `SELECT name || ' ' || read_at FROM tracker_cache_users`, "User 1 1")
}
