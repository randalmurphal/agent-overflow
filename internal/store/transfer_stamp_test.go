package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transferwire"
)

// TestEveryTransferStatusWriteStampsPastTheLast holds each writer of a
// transfer's published status to the ordering rule its readers rely on
// (nextTransferStamp). The row's stamp is first set ahead of the clock, which
// is what two writes in one millisecond or a clock step back look like to the
// second write: a writer that stamps the bare clock stamps below it.
func TestEveryTransferStatusWriteStampsPastTheLast(t *testing.T) {
	ahead := func(t *testing.T, s *Store, id string) int64 {
		t.Helper()
		stamp := time.Now().Add(time.Hour).UnixMilli()
		if _, err := s.db.Exec(`UPDATE thread_transfers SET updated_at = ? WHERE id = ?`, stamp, id); err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	stored := func(t *testing.T, s *Store, id string) int64 {
		t.Helper()
		row, err := s.GetThreadTransfer(id)
		if err != nil {
			t.Fatal(err)
		}
		return row.UpdatedAt
	}
	cases := []struct {
		name      string
		direction string
		// write changes the status and returns the stamp it handed its
		// caller, or 0 when it returns no row.
		write func(t *testing.T, s *Store, request ThreadTransfer) int64
	}{
		{"phase", "outgoing", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			row, err := s.AdvanceThreadTransfer(request.ID, "prepared", strings.Repeat("b", 64))
			if err != nil {
				t.Fatal(err)
			}
			return row.UpdatedAt
		}},
		{"destination offer", "outgoing", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			row, err := s.BindThreadTransferPeer(request.ID, json.RawMessage(`{"grant":"private-peer-grant"}`))
			if err != nil {
				t.Fatal(err)
			}
			return row.UpdatedAt
		}},
		{"archive identity", "outgoing", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			row, err := s.BindThreadTransferArchive(request.ID, transferwire.Upload{SHA256: strings.Repeat("a", 64), Size: 1024})
			if err != nil {
				t.Fatal(err)
			}
			return row.UpdatedAt
		}},
		{"cancellation request", "outgoing", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			row, err := s.RequestThreadTransferCancellation(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			return row.UpdatedAt
		}},
		{"attempt error", "outgoing", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			if err := s.FinishThreadTransferAttempt(request.ID, 0, 0, "Destination is offline."); err != nil {
				t.Fatal(err)
			}
			return 0
		}},
		{"unprepared incoming discard", "incoming", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			if err := s.DiscardUnpreparedIncomingTransfer(request.ID); err != nil {
				t.Fatal(err)
			}
			return 0
		}},
		{"incoming cancellation", "incoming", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			row, err := s.CancelIncomingThreadTransfer(request.ID, transferTestSecret())
			if err != nil {
				t.Fatal(err)
			}
			return row.UpdatedAt
		}},
		{"cleanup clearing its error", "incoming", func(t *testing.T, s *Store, request ThreadTransfer) int64 {
			if _, err := s.db.Exec(`UPDATE thread_transfers SET phase = 'canceled',cleanup_pending = 1,error = 'Cleanup failed.' WHERE id = ?`, request.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishThreadTransferCleanup(request.ID); err != nil {
				t.Fatal(err)
			}
			return 0
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			request := transferRequest("thread", "move", tc.direction)
			if _, err := s.CreateThreadTransfer(request); err != nil {
				t.Fatal(err)
			}
			before := ahead(t, s, request.ID)
			returned := tc.write(t, s, request)
			after := stored(t, s, request.ID)
			if after != before+1 {
				t.Fatalf("stored stamp %d after a write over %d, want %d", after, before, before+1)
			}
			if returned != 0 && returned != after {
				t.Fatalf("the write returned stamp %d and stored %d", returned, after)
			}
		})
	}

	t.Run("activation", func(t *testing.T) {
		s := newTestStore(t)
		request := transferRequest("incoming", "move", "incoming")
		if _, err := s.CreateThreadTransfer(request); err != nil {
			t.Fatal(err)
		}
		source := newTestStore(t)
		target := makeThread(request.ThreadID, "claude")
		if err := source.CreateThread(target); err != nil {
			t.Fatal(err)
		}
		var history strings.Builder
		if err := source.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
			t.Fatal(err)
		}
		advanceTransfer(t, s, request.ID, "prepared")
		before := ahead(t, s, request.ID)
		row, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), target, strings.NewReader(history.String()))
		if err != nil {
			t.Fatal(err)
		}
		if after := stored(t, s, request.ID); after != before+1 || row.UpdatedAt != after {
			t.Fatalf("stored stamp %d and returned %d after activation over %d, want %d", after, row.UpdatedAt, before, before+1)
		}
	})

	// An attempt that leaves the error as it was publishes nothing new, so
	// it leaves the stamp alone.
	t.Run("unchanged attempt error", func(t *testing.T) {
		s := newTestStore(t)
		request := transferRequest("thread", "move", "outgoing")
		if _, err := s.CreateThreadTransfer(request); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishThreadTransferAttempt(request.ID, 0, 0, "Destination is offline."); err != nil {
			t.Fatal(err)
		}
		before := stored(t, s, request.ID)
		if err := s.FinishThreadTransferAttempt(request.ID, 0, 1, "Destination is offline."); err != nil {
			t.Fatal(err)
		}
		if after := stored(t, s, request.ID); after != before {
			t.Fatalf("stamp moved from %d to %d with no status change", before, after)
		}
	})
}
