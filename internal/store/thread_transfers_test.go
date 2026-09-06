package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/entityid"
)

func transferRequest(threadID, kind, direction string) ThreadTransfer {
	var epoch int64
	if kind == "move" && direction == "incoming" {
		epoch = 1
	}
	return ThreadTransfer{ID: entityid.New(), ThreadID: threadID, PeerBackendID: entityid.New(), Kind: kind, Direction: direction,
		OwnershipEpoch: epoch, ActivationHash: transferTestHash(), PrivateState: json.RawMessage(`{"grant":"private-transfer-grant"}`)}
}

func transferTestSecret() []byte { return bytes.Repeat([]byte{0xa5}, 32) }
func transferTestHash() string {
	hash := sha256.Sum256(transferTestSecret())
	return hex.EncodeToString(hash[:])
}

func TestTransferPublicStatusReportsSetupWithoutPrivatePeer(t *testing.T) {
	s := newTestStore(t)
	row, err := s.CreateThreadTransfer(transferRequest("source", "copy", "outgoing"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []bool{false, true} {
		if bound {
			if _, err := s.BindThreadTransferPeer(row.ID, json.RawMessage(`{"grant":"private-peer"}`)); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := s.ListRecentThreadTransfers()
		if err != nil || len(rows) != 1 {
			t.Fatalf("status: %+v %v", rows, err)
		}
		if rows[0].NeedsDestination == bound || len(rows[0].PeerState) != 0 || len(rows[0].PrivateState) != 0 {
			t.Fatalf("wrong setup presence or private state: %+v", rows[0])
		}
		encoded, err := json.Marshal(rows)
		if err != nil || bytes.Contains(encoded, []byte("private")) {
			t.Fatalf("private status: %s %v", encoded, err)
		}
	}
}

func activateTestTransfer(t *testing.T, s *Store, request ThreadTransfer) {
	t.Helper()
	source := newTestStore(t)
	target := makeThread(request.ThreadID, "claude")
	if err := source.CreateThread(target); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	if err := source.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, request.ID, "prepared")
	if _, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), target, &history); err != nil {
		t.Fatal(err)
	}
}

func advanceTransfer(t *testing.T, s *Store, id string, phases ...string) {
	t.Helper()
	for _, phase := range phases {
		if _, err := s.AdvanceThreadTransfer(id, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatalf("advance %s: %v", phase, err)
		}
	}
}

func TestTransferRetirementSurvivesRestartDeletionAndHistoryRestore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedSnapshotFixture(t, s, "thread-a", "before move")
	snapshot := filepath.Join(t.TempDir(), "before.db")
	if err := s.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	request := transferRequest("thread-a", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, request.ID, "prepared", "committed", "complete")
	if err := s.DeleteThread("thread-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RestoreFrom(snapshot); err != nil {
		t.Fatal(err)
	}
	var moved *ThreadTransferError
	if err := s.CheckThreadTransferAccess("thread-a"); !errors.As(err, &moved) || !moved.Moved || moved.BackendID != request.PeerBackendID {
		t.Fatalf("restored source runnable: %v", err)
	}
	if _, err := s.CreateThreadTransfer(transferRequest("thread-a", "copy", "outgoing")); !errors.As(err, &moved) {
		t.Fatalf("retired thread copied: %v", err)
	}
}

func TestTransferRequestIdentityAndMonotonicCommit(t *testing.T) {
	s := newTestStore(t)
	request := transferRequest("thread-a", "move", "outgoing")
	first, err := s.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateThreadTransfer(request)
	if err != nil || first.CreatedAt != again.CreatedAt {
		t.Fatalf("retry: %+v %v", again, err)
	}
	changed := request
	changed.PeerBackendID = entityid.New()
	if _, err := s.CreateThreadTransfer(changed); err == nil {
		t.Fatal("reused ID for another peer")
	}
	if _, err := s.AdvanceThreadTransfer(request.ID, "committed", strings.Repeat("b", 64)); err == nil {
		t.Fatal("committed unprepared bytes")
	}
	advanceTransfer(t, s, request.ID, "prepared", "prepared", "committed", "committed")
	if _, err := s.AdvanceThreadTransfer(request.ID, "canceled", strings.Repeat("b", 64)); err == nil {
		t.Fatal("unilaterally canceled committed move")
	}
	if _, err := s.AdvanceThreadTransfer(request.ID, "complete", strings.Repeat("c", 64)); err == nil {
		t.Fatal("changed committed snapshot")
	}
	if err := s.SetThreadTransferError(request.ID, "Destination is offline. Retry when it reconnects."); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListPendingThreadTransfers()
	if err != nil || len(rows) != 1 || rows[0].Phase != "committed" || rows[0].Error == "" {
		t.Fatalf("pending recovery: %+v %v", rows, err)
	}
	wire, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "private-transfer-grant") || strings.Contains(string(wire), request.ActivationHash) {
		t.Fatal("status leaks transfer authority")
	}
	advanceTransfer(t, s, request.ID, "complete")
	rows, err = s.ListPendingThreadTransfers()
	if err != nil || len(rows) != 0 {
		t.Fatalf("completed recovery rows: %+v %v", rows, err)
	}
}

func TestTransferCancelAndCopyReleaseTheThread(t *testing.T) {
	for _, test := range []struct {
		kind, direction string
		phases          []string
	}{
		{"move", "outgoing", []string{"prepared", "canceled"}},
		{"copy", "outgoing", []string{"prepared", "committed", "complete"}},
		{"move", "incoming", []string{"prepared", "committed", "complete"}},
	} {
		t.Run(test.kind+"-"+test.direction, func(t *testing.T) {
			s := newTestStore(t)
			request := transferRequest("thread-a", test.kind, test.direction)
			if _, err := s.CreateThreadTransfer(request); err != nil {
				t.Fatal(err)
			}
			var busy *ThreadTransferError
			if err := s.CheckThreadTransferAccess("thread-a"); !errors.As(err, &busy) || busy.Moved {
				t.Fatalf("pending gate: %v", err)
			}
			if _, err := s.CreateThreadTransfer(transferRequest("thread-a", "copy", "outgoing")); err == nil {
				t.Fatal("accepted concurrent transfer")
			}
			if err := s.CheckThreadTransferAccess("unrelated"); err != nil {
				t.Fatal(err)
			}
			if request.Direction == "incoming" {
				activateTestTransfer(t, s, request)
			} else {
				advanceTransfer(t, s, request.ID, test.phases...)
			}
			if err := s.CheckThreadTransferAccess("thread-a"); err != nil {
				t.Fatalf("finished transfer still blocks: %v", err)
			}
		})
	}
}

func TestTransferCanReturnFromItsNewOwner(t *testing.T) {
	s := newTestStore(t)
	outgoing := transferRequest("thread-a", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(outgoing); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, outgoing.ID, "prepared", "committed", "complete")
	incoming := transferRequest("thread-a", "move", "incoming")
	incoming.OwnershipEpoch = 2
	// It may have moved through several computers before returning here.
	if _, err := s.CreateThreadTransfer(incoming); err != nil {
		t.Fatal(err)
	}
	activateTestTransfer(t, s, incoming)
	if err := s.CheckThreadTransferAccess("thread-a"); err != nil {
		t.Fatalf("return did not restore ownership: %v", err)
	}
	next := transferRequest("thread-a", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(next); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, next.ID, "prepared", "committed", "complete")
	var moved *ThreadTransferError
	if err := s.CheckThreadTransferAccess("thread-a"); !errors.As(err, &moved) || moved.BackendID != next.PeerBackendID {
		t.Fatalf("new move lost owner: %v", err)
	}
}

func TestTransferPeerOfferBindsOnceAndStaysPrivate(t *testing.T) {
	s := newTestStore(t)
	request := transferRequest("thread", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	peer := json.RawMessage(`{"grant":"private-peer-grant"}`)
	for range 2 {
		if _, err := s.BindThreadTransferPeer(request.ID, peer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.BindThreadTransferPeer(request.ID, json.RawMessage(`{"grant":"replacement"}`)); err == nil {
		t.Fatal("redirected bound transfer")
	}
	advanceTransfer(t, s, request.ID, "prepared", "committed")
	row, err := s.BindThreadTransferPeer(request.ID, peer)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "private-peer-grant") {
		t.Fatal("peer grant leaked into status")
	}
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatalf("binding changed immutable request: %v", err)
	}
}
