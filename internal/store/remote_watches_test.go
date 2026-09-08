package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRemoteWatchQueueHandoffIsAtomicAndSurvivesRestore(t *testing.T) {
	s := newTestStore(t)
	thread := Thread{ID: uuid.NewString(), Provider: "codex"}
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: thread.ID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
	if _, err := s.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	conflict := w
	conflict.Fingerprint = strings.Repeat("b", 64)
	if _, err := s.RegisterRemoteWatch(conflict); err == nil {
		t.Fatal("changed request accepted")
	}
	path := filepath.Join(t.TempDir(), "before.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	item := FlushQueueItem{ID: "", ThreadID: thread.ID, SendID: "remote-completion", Message: "done"}
	if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err == nil {
		t.Fatal("invalid message accepted")
	}
	got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || got.Notification != "pending" {
		t.Fatalf("failed enqueue spent watch: %+v %v", got, err)
	}
	item.ID = "queue:remote-test"
	if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err == nil {
		t.Fatal("duplicate handoff accepted")
	}
	rows, err := s.ListFlushQueueItems(thread.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || got.Notification != "queued" {
		t.Fatalf("history rollback revived completion: %+v %v", got, err)
	}
	if _, err := s.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRemoteWatch(w.ComputerID, w.RequestID)
	if got.Notification != "queued" {
		t.Fatal("run retry revived completion")
	}
}

func TestRemoteWatchObservationCannotRegressAcceptedCompletion(t *testing.T) {
	s := newTestStore(t)
	w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64)}
	if fresh, err := s.RegisterRemoteWatch(w); err != nil || !fresh {
		t.Fatalf("registration: %v %v", fresh, err)
	}
	if err := s.DismissRemoteWatch(w.ComputerID, w.RequestID); err != nil {
		t.Fatal(err)
	}
	// A previously refused attempt can be retried with new refusal cleanup.
	if fresh, err := s.RegisterRemoteWatch(w); err != nil || !fresh {
		t.Fatalf("retry: %v %v", fresh, err)
	}
	if err := s.DismissRemoteWatch(w.ComputerID, w.RequestID); err != nil {
		t.Fatal(err)
	}
	done := RemoteJob{ID: w.RequestID, SourceThreadID: w.ThreadID, State: "succeeded", Output: "never retained on source"}
	if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, done, "", 0); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []RemoteJob{{}, {ID: w.RequestID, State: "running"}} {
		if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, stale, "", 0); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil || got.Receipt.State != "succeeded" || got.Receipt.Output != "" || got.Notification != "pending" {
			t.Fatalf("late reply lost accepted completion: %+v %v", got, err)
		}
	}
	if err := s.DismissRemoteWatch(w.ComputerID, w.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, done, "", 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRemoteWatch(w.ComputerID, w.RequestID)
	if got.Notification != "dismissed" {
		t.Fatal("a repeated receipt revived a deliberately dismissed accepted job")
	}
}
