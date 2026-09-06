package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransferReservesDestinationProjectUntilCanceled(t *testing.T) {
	s := newTestStore(t)
	project, err := s.CreateProject(Project{ID: "destination", Name: "Destination", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	request := transferRequest("incoming", "move", "incoming")
	request.ProjectID = project.ID
	row, err := s.CreateThreadTransfer(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(project.ID); !errors.Is(err, ErrProjectReceivingTransfer) {
		t.Fatalf("deleted reserved project: %v", err)
	}
	changed := request
	changed.ProjectID = "other-project"
	if _, err := s.CreateThreadTransfer(changed); err == nil {
		t.Fatal("retry changed destination project")
	}
	if _, err := s.CancelIncomingThreadTransfer(row.ID, transferTestSecret()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(project.ID); err != nil {
		t.Fatal(err)
	}
	request.ID = transferRequest("incoming", "move", "incoming").ID
	if _, err := s.CreateThreadTransfer(request); err == nil {
		t.Fatal("reserved a deleted project")
	}
}

func TestTransferPreventsRestoreInvalidatingAcceptedWork(t *testing.T) {
	s := newTestStore(t)
	snapshot := filepath.Join(t.TempDir(), "before-transfer.db")
	if err := s.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	request := transferRequest("received", "move", "incoming")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(snapshot); err == nil || !strings.Contains(err.Error(), "transfers") {
		t.Fatalf("restored over pending preparation: %v", err)
	}
	activateTestTransfer(t, s, request)
	if _, err := s.RestoreFrom(snapshot); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("restored away incoming owner history: %v", err)
	}
	if _, err := s.GetThread(request.ThreadID); err != nil {
		t.Fatal(err)
	}
	snapshot = filepath.Join(t.TempDir(), "after-transfer.db")
	if err := s.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(snapshot); err != nil {
		t.Fatalf("refused snapshot containing current ownership: %v", err)
	}
}
