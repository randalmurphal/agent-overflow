package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransferCancellationIntentSurvivesRestartAndPreventsRetirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	if err := copyTestStoreTemplate(path); err != nil {
		t.Fatal(err)
	}
	source, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	destination := newTestStore(t)
	request := transferRequest("thread", "move", "outgoing")
	if _, err := source.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	incoming := request
	incoming.Direction = "incoming"
	incoming.OwnershipEpoch = 1
	if _, err := destination.CreateThreadTransfer(incoming); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, source, request.ID, "prepared")
	advanceTransfer(t, destination, request.ID, "prepared")
	if _, err := source.RequestThreadTransferCancellation(request.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	source, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	row, err := source.GetThreadTransfer(request.ID)
	if err != nil || !row.CancelRequested || row.Phase != "prepared" {
		t.Fatalf("lost cancellation intent: %+v %v", row, err)
	}
	if _, err := source.AdvanceThreadTransfer(request.ID, "committed", strings.Repeat("b", 64)); err == nil {
		t.Fatal("retired after cancellation was requested")
	}
	var busy *ThreadTransferError
	if !errors.As(source.CheckThreadTransferAccess(request.ThreadID), &busy) {
		t.Fatal("released source before destination acknowledgment")
	}
	if _, err := destination.CancelIncomingThreadTransfer(request.ID, nil); err == nil {
		t.Fatal("canceled without source proof")
	}
	if _, err := destination.AdvanceThreadTransfer(request.ID, "canceled", strings.Repeat("b", 64)); err == nil {
		t.Fatal("phase-only cancellation bypassed proof")
	}
	for range 2 {
		if _, err := destination.CancelIncomingThreadTransfer(request.ID, transferTestSecret()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := destination.CheckThreadTransferActivation(request.ID, strings.Repeat("b", 64), transferTestSecret()); err == nil {
		t.Fatal("canceled destination may still activate")
	}
	advanceTransfer(t, source, request.ID, "canceled")
	if err := source.CheckThreadTransferAccess(request.ThreadID); err != nil {
		t.Fatal(err)
	}
}

func TestTransferCancellationCannotRewindCommittedSource(t *testing.T) {
	source := newTestStore(t)
	request := transferRequest("thread", "move", "outgoing")
	if _, err := source.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, source, request.ID, "prepared", "committed")
	if _, err := source.RequestThreadTransferCancellation(request.ID); err == nil {
		t.Fatal("cancel requested after retirement")
	}
	var moved *ThreadTransferError
	if !errors.As(source.CheckThreadTransferAccess(request.ThreadID), &moved) || !moved.Moved {
		t.Fatal("lost committed source fence")
	}
}
