package store

import (
	"fmt"
	"reflect"
	"testing"
)

const statusBoards, statusCatalog = 100, 100

// statusCatalogOf is statuses "1" to "100", each named name, in group(i).
func statusCatalogOf(name string, group func(i int) int) MirrorCatalog {
	c := MirrorCatalog{}
	for i := range statusCatalog {
		c.Statuses = append(c.Statuses, MirrorStatus{ID: fmt.Sprint(i + 1), Name: name, Group: group(i)})
	}
	return c
}

// seedStatusBoards lists statusBoards boards, "100" on, each with a row in
// status b%2+1 ("1" or "2"), the last with one row of each.
func seedStatusBoards(t *testing.T, s *Store, connectionID string, root int64) []string {
	t.Helper()
	ids := make([]string, statusBoards)
	for b := range ids {
		ids[b] = fmt.Sprint(100 + b)
	}
	listMirrorBoards(t, s, defaultTestProjectID, connectionID, ids...)
	for b, id := range ids {
		st := webStructure()
		st.Board.ID, st.Catalog = id, statusCatalogOf("Open", func(int) int { return MirrorGroupToDo })
		mustReplaceStructure(t, s, root, st, true)
		row := webRow(fmt.Sprint(20000+b), 5, "a")
		row.Card.StatusID = fmt.Sprint(b%2 + 1)
		rows := []MirrorRow{row}
		if b == statusBoards-1 {
			other := webRow("29999", 5, "b")
			other.Card.StatusID = "1"
			rows = append(rows, other)
		}
		mustWriteRowPage(t, s, root, webPage(id, rows...), nil)
	}
	return ids
}

func boardRevisions(t *testing.T, s *Store, root int64) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	err := scanMirrorRows(s.db, `SELECT board_id, revision FROM tracker_cache_boards WHERE root_id = `+boundInt, []any{root}, func(r rowScanner) error {
		var id string
		var revision int64
		err := r.Scan(&id, &revision)
		got[id] = revision
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A catalog write advances the revision only of the boards with a row
// whose status entered or left the done groups, once each per write, so a
// read of every status of a root with many boards writes only the
// statuses. A board none of whose rows has the status keeps its revision.
func TestMirrorStatusGroupBumpsOnlyTheBoardsUsingIt(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	ids := seedStatusBoards(t, s, jira.ID, root)
	const boards, statuses = statusBoards, statusCatalog
	catalog := statusCatalogOf
	revisions := func() map[string]int64 { return boardRevisions(t, s, root) }
	recorder := recordMirrorWrites(t, s)
	for i, step := range []struct {
		name    string
		catalog MirrorCatalog
		bumped  func(b int) bool
	}{
		{"every status renamed and regrouped among the open groups", catalog("Doing", func(int) int { return MirrorGroupInProgress }),
			func(int) bool { return false }},
		{"status 1 done", catalog("Doing", func(i int) int { return []int{MirrorGroupDone, MirrorGroupInProgress}[min(i, 1)] }),
			func(b int) bool { return b%2 == 0 || b == boards-1 }},
		{"status 1 reopened, 2 to do", catalog("Doing", func(int) int { return MirrorGroupToDo }),
			func(b int) bool { return b%2 == 0 || b == boards-1 }},
		{"statuses 1 and 2 canceled", catalog("Doing", func(i int) int { return []int{MirrorGroupCanceled, MirrorGroupCanceled, MirrorGroupToDo}[min(i, 2)] }),
			func(int) bool { return true }},
		{"done to canceled", catalog("Doing", func(i int) int { return []int{MirrorGroupDone, MirrorGroupCanceled, MirrorGroupToDo}[min(i, 2)] }),
			func(int) bool { return false }},
	} {
		before := revisions()
		recorder.reset()
		readAt := int64(10 + i)
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, step.catalog, mirrorReadAt(readAt)); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		after := revisions()
		bumped := 0
		for b, id := range ids {
			want := before[id]
			if step.bumped(b) {
				want, bumped = want+1, bumped+1
			}
			if after[id] != want {
				t.Errorf("%s: board %s revision %d -> %d; want %d", step.name, id, before[id], after[id], want)
			}
		}
		wantWrites := map[string]int{"tracker_cache_statuses": statuses}
		if bumped > 0 {
			wantWrites["tracker_cache_boards"] = bumped
		}
		if writes := recorder.rowWrites(); !reflect.DeepEqual(writes, wantWrites) {
			t.Errorf("%s: wrote %v; want %v", step.name, writes, wantWrites)
		}
		// The same read again moves no watermark and bumps nothing.
		recorder.reset()
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, step.catalog, mirrorReadAt(readAt)); err != nil || len(recorder.rowWrites()) > 0 {
			t.Errorf("%s again: wrote %v, %v", step.name, recorder.rowWrites(), err)
		}
	}
}
