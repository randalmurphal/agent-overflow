package store

import (
	"testing"

	"agent-overflow/internal/transferwire"
)

func TestTransferOwnershipEpochsAdvanceAndRejectOldOffers(t *testing.T) {
	if maxOwnershipEpoch != transferwire.MaxOwnershipEpoch {
		t.Fatal("store and wire epoch bounds drifted")
	}
	s := newTestStore(t)
	mustCreateThread(t, s, "thread")
	out, err := s.CreateThreadTransfer(transferRequest("thread", "move", "outgoing"))
	if err != nil || out.OwnershipEpoch != 1 {
		t.Fatalf("first move: %+v %v", out, err)
	}
	advanceTransfer(t, s, out.ID, "prepared", "committed", "complete")
	for _, epoch := range []int64{-1, 0, 1, maxOwnershipEpoch + 1} {
		request := transferRequest("thread", "move", "incoming")
		request.OwnershipEpoch = epoch
		if _, err := s.CreateThreadTransfer(request); err == nil {
			t.Fatalf("accepted stale/invalid epoch %d", epoch)
		}
	}
	in := transferRequest("thread", "move", "incoming")
	in.OwnershipEpoch = 4 // A → B → C → D → A need not revisit every intermediate host.
	if _, err := s.CreateThreadTransfer(in); err != nil {
		t.Fatal(err)
	}
	activateTestTransfer(t, s, in)
	row, err := s.GetThread("thread")
	if err != nil || row.OwnershipEpoch != 4 {
		t.Fatalf("returned owner: %+v %v", row, err)
	}
	next, err := s.CreateThreadTransfer(transferRequest("thread", "move", "outgoing"))
	if err != nil || next.OwnershipEpoch != 5 {
		t.Fatalf("next move: %+v %v", next, err)
	}
}

func TestTransferCatalogsHideRetiredHistoryAndPendingIncoming(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "thread")
	if err := s.InsertItem(Item{ID: "message", ThreadID: "thread", Kind: "assistant_text", Role: "assistant", Summary: "needle"}); err != nil {
		t.Fatal(err)
	}
	assertCatalog := func(visible bool, epoch int64) {
		t.Helper()
		for name, list := range map[string]func() ([]Thread, error){"all": s.ListThreads, "with items": s.ListThreadsWithItems} {
			rows, err := list()
			if err != nil || (len(rows) != 0) != visible {
				t.Fatalf("%s: visible %v rows %d: %v", name, visible, len(rows), err)
			}
			if len(rows) > 0 && rows[0].OwnershipEpoch != epoch {
				t.Fatalf("%s epoch %d, want %d", name, rows[0].OwnershipEpoch, epoch)
			}
		}
		hits, err := s.SearchThreadMessages("needle", 10)
		if err != nil || (len(hits) != 0) != visible {
			t.Fatalf("search: visible %v rows %d: %v", visible, len(hits), err)
		}
	}
	assertCatalog(true, 0)
	out, err := s.CreateThreadTransfer(transferRequest("thread", "move", "outgoing"))
	if err != nil {
		t.Fatal(err)
	}
	assertCatalog(true, 0)
	advanceTransfer(t, s, out.ID, "prepared", "committed")
	assertCatalog(false, 0)
	advanceTransfer(t, s, out.ID, "complete")
	assertCatalog(false, 0)
	in := transferRequest("thread", "move", "incoming")
	in.OwnershipEpoch = 2
	if _, err := s.CreateThreadTransfer(in); err != nil {
		t.Fatal(err)
	}
	assertCatalog(false, 0)
	if _, err := s.CancelIncomingThreadTransfer(in.ID, transferTestSecret()); err != nil {
		t.Fatal(err)
	}
	assertCatalog(false, 0)
	// History remains available for recovery and return; only ownership catalogs hide it.
	if _, err := s.GetThread("thread"); err != nil {
		t.Fatal(err)
	}
}
