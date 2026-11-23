package store

import (
	"reflect"
	"testing"
)

// unlistedDelta writes board 10's listed row 10001 and, a millisecond
// later, delta row 10002, which the board's search listing does not show
// yet, both from reads begun at wall time before on s's clock.
func unlistedDelta(t *testing.T, s *Store, root, before int64) {
	t.Helper()
	writeMirrorBoard(t, s, root, "10", nil)
	listed := currentPage(s, root, webPage("10", webRow("10001", 5, "a")))
	listed.ReadAt = beginRead(t, s, root, before)
	mustWriteRowPage(t, s, root, listed, nil)
	delta := currentPage(s, root, webPage("10", webRow("10002", 6, "b")))
	delta.ReadAt = beginRead(t, s, root, before+1)
	mustWriteRowPage(t, s, root, delta, nil)
}

// A reconcile reads no order time as elapsed time: a row a delta read just
// before a restart, which the listing does not show yet, is not removed by
// a reconcile begun after the restart, the wall clock stepped forward an
// hour across it or not, nor after a burst of reads at a frozen clock
// moved the order time two minutes past the row's. The listing leaves it
// to a read by ID (Unsettled).
func TestAReconcileRemovesNoRowByItsOrderTime(t *testing.T) {
	t.Parallel()
	const before = 1_000_000
	for name, reconcileRead := range map[string]func(t *testing.T, s *Store, root int64) (*Store, MirrorRead){
		"a restart": func(t *testing.T, s *Store, root int64) (*Store, MirrorRead) {
			s = reopenStore(t, s)
			return s, beginRead(t, s, root, before+1_000)
		},
		"a forward clock step across a restart": func(t *testing.T, s *Store, root int64) (*Store, MirrorRead) {
			s = reopenStore(t, s)
			return s, beginRead(t, s, root, before+3_600_000)
		},
		"a burst of reads at a frozen clock": func(t *testing.T, s *Store, root int64) (*Store, MirrorRead) {
			for range 120_000 {
				beginRead(t, s, root, before+1)
			}
			return s, beginRead(t, s, root, before+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			unlistedDelta(t, s, root, before)
			s, read := reconcileRead(t, s, root)
			got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), []string{"10001"}, true, read)
			if err != nil {
				t.Fatal(err)
			}
			if want := (MirrorReconcile{Unsettled: []string{"10002"}}); !reflect.DeepEqual(got, want) {
				t.Fatalf("the reconcile = %+v; want %+v", got, want)
			}
			assertRows(t, s.db, `SELECT issue_id FROM tracker_cache_board_issues ORDER BY issue_id`, "10001,10002")
		})
	}
}
