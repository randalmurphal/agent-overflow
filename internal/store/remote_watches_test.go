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
	for _, stale := range []RemoteJob{{}, {ID: w.RequestID, State: "running"}, {ID: w.RequestID, State: "failed"}} {
		if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, stale, "old network failure", 30000); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil || got.Receipt.State != "succeeded" || got.Receipt.Output != "" || got.Notification != "pending" || got.Error != "" || got.NextCheck != 0 {
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

// A completion taken back out of the queue is owed again, in the same
// transaction as the delete, and the due list leaves it alone while its
// thread is archived.
func TestWithdrawnRemoteCompletionIsOwedAgainOnceItsThreadIsUnarchived(t *testing.T) {
	s := newTestStore(t)
	thread := Thread{ID: uuid.NewString(), Provider: "codex"}
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	finished := func() RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: thread.ID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		done := RemoteJob{ID: w.RequestID, SourceThreadID: thread.ID, State: "succeeded"}
		if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, done, "", 0); err != nil {
			t.Fatal(err)
		}
		return w
	}
	notification := func(w RemoteWatch) string {
		t.Helper()
		got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Notification
	}
	due := func() int {
		t.Helper()
		rows, err := s.ListRemoteWatches("", 1, 256)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	held := finished()
	item := FlushQueueItem{ID: "queue:" + held.RequestID, ThreadID: thread.ID, SendID: "remote-completion:" + held.ComputerID + ":" + held.RequestID, Message: "done"}
	if err := s.QueueRemoteCompletion(held.ComputerID, held.RequestID, item); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawRemoteCompletion(held.ComputerID, held.RequestID, item.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListFlushQueueItems(thread.ID); err != nil || len(rows) != 0 {
		t.Fatalf("withdrawn completion still queued: %+v %v", rows, err)
	}
	if got := notification(held); got != "pending" {
		t.Fatalf("withdrawn completion's watch = %q, want pending", got)
	}
	if n := due(); n != 0 {
		t.Fatalf("an archived thread's completion is due: %d rows", n)
	}
	if _, _, err := s.UnarchiveThread(thread.ID); err != nil {
		t.Fatal(err)
	}
	if n := due(); n != 1 {
		t.Fatalf("the unarchived thread's completion is not due: %d rows", n)
	}

	// A message already gone from the queue was delivered: its watch stays
	// with the queue.
	sent := finished()
	item = FlushQueueItem{ID: "queue:" + sent.RequestID, ThreadID: thread.ID, SendID: "remote-completion:" + sent.ComputerID + ":" + sent.RequestID, Message: "done"}
	if err := s.QueueRemoteCompletion(sent.ComputerID, sent.RequestID, item); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlushQueueItem(item.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawRemoteCompletion(sent.ComputerID, sent.RequestID, item.ID); err != nil {
		t.Fatal(err)
	}
	if got := notification(sent); got != "queued" {
		t.Fatalf("a delivered completion's watch = %q, want queued", got)
	}
}
