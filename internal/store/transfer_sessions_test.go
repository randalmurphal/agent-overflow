package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func bindTransferSessions(t *testing.T, s *Store, row ThreadTransfer, refs ...TransferSession) {
	t.Helper()
	if err := s.BindThreadTransferSessions(row.ID, refs); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTransferRetirementSurvivesHistoryDeletionAndRestore(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("native-owner", "claude")
	thread.SessionRef = "native-session"
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "before.db")
	if err := s.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	request := transferRequest(thread.ID, "move", "outgoing")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	bindTransferSessions(t, s, request, TransferSession{"claude", thread.SessionRef})
	advanceTransfer(t, s, request.ID, "prepared", "committed", "complete")
	if err := s.DeleteThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	for _, restore := range []bool{false, true} {
		if restore {
			if _, err := s.RestoreFrom(snapshot); err != nil {
				t.Fatal(err)
			}
		}
		alias := thread
		alias.ID = "another-ao-id"
		var moved *ThreadTransferError
		if err := s.CheckThreadExecutionAccess(alias); !errors.As(err, &moved) || !moved.Moved {
			t.Fatalf("alias revived session: %v", err)
		}
		if err := s.CheckNativeSessionImport("claude", thread.SessionRef); !errors.As(err, &moved) {
			t.Fatalf("reimport revived session: %v", err)
		}
		refs, err := s.ListImportedSessionRefs()
		if err != nil || refs[ProviderSessionRef{"claude", thread.SessionRef}] == "" {
			t.Fatalf("scan forgot native ownership: %v %v", refs, err)
		}
		alias.PendingForkRef = thread.SessionRef
		if err := s.CheckThreadExecutionAccess(alias); err != nil {
			t.Fatalf("independent fork of preserved history: %v", err)
		}
	}
}

func TestNativeTransferClosureBindsOnceAndReturnsToSameConversation(t *testing.T) {
	s := newTestStore(t)
	ref := TransferSession{"codex", "native-root"}
	child := TransferSession{"codex", "native-child"}
	out := transferRequest("thread-a", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(out); err != nil {
		t.Fatal(err)
	}
	bindTransferSessions(t, s, out, ref, child)
	bindTransferSessions(t, s, out, child, ref)
	if err := s.BindThreadTransferSessions(out.ID, []TransferSession{ref}); err == nil {
		t.Fatal("changed closure")
	}
	advanceTransfer(t, s, out.ID, "prepared", "committed", "complete")
	in := transferRequest("thread-a", "move", "incoming")
	in.OwnershipEpoch = 2
	if _, err := s.CreateThreadTransfer(in); err != nil {
		t.Fatal(err)
	}
	bindTransferSessions(t, s, in, ref, child)
	if err := s.CheckNativeThreadTransferAccess(ref.Provider, ref.Ref); err == nil {
		t.Fatal("unactivated destination executable")
	}
	activateTestTransfer(t, s, in)
	if err := s.CheckNativeThreadTransferAccess(ref.Provider, ref.Ref); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckNativeSessionImport(ref.Provider, ref.Ref); err == nil {
		t.Fatal("completed destination offered for reimport")
	}
	next := transferRequest("thread-a", "move", "outgoing")
	if _, err := s.CreateThreadTransfer(next); err != nil {
		t.Fatal(err)
	}
	bindTransferSessions(t, s, next, ref, child)
	advanceTransfer(t, s, next.ID, "prepared", "committed", "complete")
	wrong := transferRequest("unrelated-thread", "move", "incoming")
	if _, err := s.CreateThreadTransfer(wrong); err != nil {
		t.Fatal(err)
	}
	if err := s.BindThreadTransferSessions(wrong.ID, []TransferSession{child, ref}); err == nil {
		t.Fatal("native session returned as unrelated AO identity")
	}
}

func TestNativeCopyUsesNewIdentityAndCancellationRestoresPriorReservation(t *testing.T) {
	s := newTestStore(t)
	request := transferRequest("original", "copy", "outgoing")
	copy, err := s.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateThreadTransfer(request)
	if err != nil || copy.TargetThreadID == "original" || copy.TargetThreadID != again.TargetThreadID {
		t.Fatalf("unstable copy identity: %+v %+v %v", copy, again, err)
	}
	ref := TransferSession{"claude", "fresh-native-copy"}
	bindTransferSessions(t, s, copy, ref)
	if err := s.CheckNativeThreadTransferAccess("claude", "original-native"); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, copy.ID, "prepared", "committed", "complete")
	var moved *ThreadTransferError
	if err := s.CheckNativeThreadTransferAccess(ref.Provider, ref.Ref); !errors.As(err, &moved) || !moved.Moved {
		t.Fatalf("copied native fork executable on source: %v", err)
	}
	in := transferRequest(copy.TargetThreadID, "move", "incoming")
	if _, err := s.CreateThreadTransfer(in); err != nil {
		t.Fatal(err)
	}
	bindTransferSessions(t, s, in, ref)
	if _, err := s.CancelIncomingThreadTransfer(in.ID, transferTestSecret()); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckNativeThreadTransferAccess(ref.Provider, ref.Ref); !errors.As(err, &moved) || !moved.Moved || moved.OperationID != copy.ID {
		t.Fatalf("cancel erased earlier native retirement: %v", err)
	}
}
