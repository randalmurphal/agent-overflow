package store

import (
	"fmt"
	"testing"
)

// mirrorWithStatuses is a mirror whose root's catalog holds n statuses,
// half of them done, with one issue in board 10's backlog.
func mirrorWithStatuses(t *testing.T, n int) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	for i := len(st.Catalog.Statuses); i < n; i++ {
		group := MirrorGroupDone
		if i%2 == 0 {
			group = MirrorGroupToDo
		}
		st.Catalog.Statuses = append(st.Catalog.Statuses, MirrorStatus{ID: fmt.Sprintf("x%d", i), Name: fmt.Sprintf("Status %d", i), Group: group})
	}
	mustReplaceStructure(t, s, root, st, true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	return s, root
}

// A board read with no regrouping costs what it costs whatever the size
// of the root's status catalog: it reads a row's status by its key, as
// the statements did before regrouping, and lists no catalog. Not
// parallel: it counts VM steps.
func TestABoardWithNoRegroupingReadsNoCatalog(t *testing.T) {
	steps := map[int][2]int{}
	for _, n := range []int{5, 1000} {
		s, root := mirrorWithStatuses(t, n)
		backlog := []MirrorSection{{Kind: MirrorSectionBacklog}}
		var held MirrorTotalsAt
		view, ok, err := s.ReadMirrorBoardView(root, "10", MirrorRegroup{}, theseSections(backlog))
		if err != nil || !ok || len(view.Pages) != 1 || len(view.Pages[0].Rows) != 1 {
			t.Fatalf("%d statuses: the board view = %+v, %v, %v; want the backlog's one row", n, view, ok, err)
		}
		held = MirrorTotalsAt{Lifetime: view.Structure.Lifetime, Revision: view.Counts.TotalsRevision}
		var patchSteps, viewSteps int
		patchSteps = countMirrorVMSteps(t, func() {
			patch, ok, err := s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, backlog, []string{"10001"}, held)
			if err != nil || !ok || patch.Counts != nil || len(patch.Sections[0]) != 1 {
				t.Fatalf("%d statuses: the patch = %+v, %v, %v; want the one row and no totals", n, patch, ok, err)
			}
		})
		viewSteps = countMirrorVMSteps(t, func() {
			if _, _, err := s.ReadMirrorBoardView(root, "10", MirrorRegroup{}, theseSections(backlog)); err != nil {
				t.Fatal(err)
			}
		})
		steps[n] = [2]int{patchSteps, viewSteps}
	}
	t.Logf("VM steps (one-issue backlog patch, board view): 5 statuses %v, 1,000 statuses %v", steps[5], steps[1000])
	if steps[5] != steps[1000] {
		t.Errorf("VM steps (one-issue backlog patch, board view) = %v at 5 statuses, %v at 1,000; want the same", steps[5], steps[1000])
	}
}
