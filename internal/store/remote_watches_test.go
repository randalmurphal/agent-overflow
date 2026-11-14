package store

import (
	"path/filepath"
	"slices"
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
	threadID := uuid.NewString()
	mustCreateThread(t, s, threadID)
	w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64)}
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
	if err := s.WithdrawRemoteCompletion(held.ComputerID, held.RequestID, item.ID, WithdrawQueued, nil); err != nil {
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
	if err := s.WithdrawRemoteCompletion(sent.ComputerID, sent.RequestID, item.ID, WithdrawQueued, nil); err != nil {
		t.Fatal(err)
	}
	if got := notification(sent); got != "queued" {
		t.Fatalf("a delivered completion's watch = %q, want queued", got)
	}
	// One the provider never confirmed goes back with no row left.
	if err := s.WithdrawRemoteCompletion(sent.ComputerID, sent.RequestID, item.ID, WithdrawUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if got := notification(sent); got != "pending" {
		t.Fatalf("an unconfirmed completion's watch = %q, want pending", got)
	}
}

// Archive drops only a notification whose job has not finished, and the
// completions it keeps are the only ones forgetting their computer drops.
func TestArchivedRemoteCompletionsAreKeptUntilTheirComputerIsForgotten(t *testing.T) {
	s := newTestStore(t)
	archived := Thread{ID: uuid.NewString(), Provider: "codex"}
	live := Thread{ID: uuid.NewString(), Provider: "codex"}
	for _, thread := range []Thread{archived, live} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	computer, elsewhere := uuid.NewString(), uuid.NewString()
	watch := func(computerID, threadID, state string) RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: computerID, RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		receipt := RemoteJob{ID: w.RequestID, SourceThreadID: threadID, State: state}
		if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, receipt, "", 0); err != nil {
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

	finished := watch(computer, archived.ID, "succeeded")
	running := watch(computer, archived.ID, "running")
	for _, w := range []RemoteWatch{finished, running} {
		if err := s.DismissUnfinishedRemoteWatch(w.ComputerID, w.RequestID); err != nil {
			t.Fatal(err)
		}
	}
	if notification(finished) != "pending" || notification(running) != "dismissed" {
		t.Fatalf("archive's dismissal: finished %q, running %q, want pending and dismissed", notification(finished), notification(running))
	}

	if _, _, err := s.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	unfinished := watch(computer, archived.ID, "running")
	liveFinished := watch(computer, live.ID, "failed")
	otherComputer := watch(elsewhere, archived.ID, "succeeded")
	held, other, err := s.PendingRemoteWatchesForComputer(computer)
	if err != nil || held != 1 || other != 2 {
		t.Fatalf("pending for the computer: held %d, other %d, %v; want 1 and 2", held, other, err)
	}
	threads, err := s.DismissArchivedRemoteCompletions(computer)
	if err != nil || !slices.Equal(threads, []string{archived.ID}) {
		t.Fatalf("dismissed for %q, %v; want the archived thread", threads, err)
	}
	if notification(finished) != "dismissed" || notification(unfinished) != "pending" || notification(liveFinished) != "pending" || notification(otherComputer) != "pending" {
		t.Fatalf("forgetting dropped the wrong watches: held %q, unfinished %q, live %q, other computer %q",
			notification(finished), notification(unfinished), notification(liveFinished), notification(otherComputer))
	}
	if held, other, err := s.PendingRemoteWatchesForComputer(computer); err != nil || held != 0 || other != 2 {
		t.Fatalf("after the dismissal: held %d, other %d, %v; want 0 and 2", held, other, err)
	}
}

// The tracking cap counts the completions held for archived conversations,
// so its refusal names them and how to receive them.
func TestRemoteTrackingCapacityNamesHeldArchivedCompletions(t *testing.T) {
	s := newTestStore(t)
	archived := Thread{ID: uuid.NewString(), Provider: "codex"}
	live := Thread{ID: uuid.NewString(), Provider: "codex"}
	for _, thread := range []Thread{archived, live} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	register := func(threadID, state string) error {
		w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			return err
		}
		if state == "" {
			return nil
		}
		return s.ObserveRemoteWatch(w.ComputerID, w.RequestID, RemoteJob{ID: w.RequestID, SourceThreadID: threadID, State: state}, "", 0)
	}
	for range 3 {
		if err := register(archived.ID, "succeeded"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	for range 253 {
		if err := register(live.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	err := register(live.ID, "")
	want := "256 remote jobs are awaiting completion or reconnection. 3 of them are finished jobs held for archived conversations; unarchive those conversations to receive the results. Resolve those jobs before starting more."
	if err == nil || err.Error() != want {
		t.Fatalf("capacity refusal: %v", err)
	}
}

// A queued completion is delivered in two phases, as an answer is: the
// queue write marks the watch queued, and only the provider's echo
// confirms it. Until then the boot sweep lists it and an unconfirmed
// give-back takes it back; once confirmed, neither does. A requeue starts
// unconfirmed.
func TestRemoteCompletionDeliveryIsConfirmedByItsEcho(t *testing.T) {
	s, thread := remoteWatchTestThread(t)
	w := finishedRemoteWatch(t, s, thread)
	unconfirmed := func() []string {
		t.Helper()
		watches, err := s.UnconfirmedRemoteCompletions()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, watch := range watches {
			got = append(got, watch.RequestID)
		}
		return got
	}
	queueAndSend := func() {
		t.Helper()
		item := FlushQueueItem{ID: "queue:" + uuid.NewString(), ThreadID: thread, SendID: "remote-completion:" + w.ComputerID + ":" + w.RequestID, Message: "done"}
		if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteDispatchedFlushQueueItem(item.ID); err != nil {
			t.Fatal(err)
		}
	}

	queueAndSend()
	if got := unconfirmed(); !slices.Equal(got, []string{w.RequestID}) {
		t.Fatalf("unconfirmed after the queue write = %q, want the watch", got)
	}
	if confirmed, err := s.ConfirmRemoteCompletion(w.ComputerID, w.RequestID, 500); err != nil || !confirmed {
		t.Fatalf("ConfirmRemoteCompletion = %v, %v; want true", confirmed, err)
	}
	if confirmed, err := s.ConfirmRemoteCompletion(w.ComputerID, w.RequestID, 600); err != nil || confirmed {
		t.Fatalf("second ConfirmRemoteCompletion = %v, %v; want false", confirmed, err)
	}
	if got := unconfirmed(); len(got) != 0 {
		t.Fatalf("unconfirmed after the echo = %q, want none", got)
	}
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.Notification != "queued" || got.ConfirmedAt != 500 {
		t.Fatalf("a confirmed completion was given back: %q confirmed at %d", got.Notification, got.ConfirmedAt)
	}

	// A watch given back before its echo, then queued again, is
	// unconfirmed again.
	other := finishedRemoteWatch(t, s, thread)
	w = other
	queueAndSend()
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.Notification != "pending" {
		t.Fatalf("an unconfirmed completion's watch = %q, want pending", got.Notification)
	}
	queueAndSend()
	if got := unconfirmed(); !slices.Equal(got, []string{w.RequestID}) {
		t.Fatalf("unconfirmed after a requeue = %q, want the watch", got)
	}
}

// A completion given back with a pace is due at the time the pace names
// and counts the attempt, read from the watch in the same transaction; one
// given up is dismissed with the reason and keeps its receipt; and one
// given back without a pace is due at once and keeps its count.
func TestRemoteCompletionGivenBackWithAPace(t *testing.T) {
	s, thread := remoteWatchTestThread(t)
	w := finishedRemoteWatch(t, s, thread)
	queue := func() {
		t.Helper()
		item := FlushQueueItem{ID: "queue:" + uuid.NewString(), ThreadID: thread, SendID: "remote-completion:" + w.ComputerID + ":" + w.RequestID, Message: "done"}
		if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteDispatchedFlushQueueItem(item.ID); err != nil {
			t.Fatal(err)
		}
	}
	due := func(at int64) bool {
		t.Helper()
		rows, err := s.ListRemoteWatches("", at, 256)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(rows, func(r RemoteWatch) bool { return r.RequestID == w.RequestID })
	}
	var seen []int64
	paced := func(attempts int64) CompletionReturn {
		seen = append(seen, attempts)
		return CompletionReturn{NextCheck: 10_000 * (attempts + 1), Attempts: attempts + 1}
	}

	queue()
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, paced); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.Notification != "pending" || got.NextCheck != 10_000 || got.WakeAttempts != 1 {
		t.Fatalf("paced give-back: %q due %d after %d attempts; want pending, 10000, 1", got.Notification, got.NextCheck, got.WakeAttempts)
	}
	if due(9_999) || !due(10_000) {
		t.Fatalf("paced completion due before its time or not at it")
	}
	queue()
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, paced); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.NextCheck != 20_000 || got.WakeAttempts != 2 || !slices.Equal(seen, []int64{0, 1}) {
		t.Fatalf("second paced give-back: due %d after %d attempts, pace saw %v", got.NextCheck, got.WakeAttempts, seen)
	}

	// Without a pace (a Stop) it is due at once and keeps its count.
	queue()
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.Notification != "pending" || got.WakeAttempts != 2 {
		t.Fatalf("unpaced give-back: %q after %d attempts", got.Notification, got.WakeAttempts)
	}

	queue()
	giveUp := func(int64) CompletionReturn { return CompletionReturn{Issue: "not delivered"} }
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, "", WithdrawUnconfirmed, giveUp); err != nil {
		t.Fatal(err)
	}
	got := mustRemoteWatch(t, s, w)
	if got.Notification != "dismissed" || got.WakeIssue != "not delivered" || got.Receipt.State != "succeeded" {
		t.Fatalf("given up: %q issue %q receipt %+v", got.Notification, got.WakeIssue, got.Receipt)
	}
	if due(1 << 62) {
		t.Fatal("a given-up completion is still due")
	}
}

func remoteWatchTestThread(t *testing.T) (*Store, string) {
	t.Helper()
	s := newTestStore(t)
	thread := Thread{ID: uuid.NewString(), Provider: "codex"}
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return s, thread.ID
}

func finishedRemoteWatch(t *testing.T, s *Store, threadID string) RemoteWatch {
	t.Helper()
	w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
	if _, err := s.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	done := RemoteJob{ID: w.RequestID, SourceThreadID: threadID, State: "succeeded"}
	if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, done, "", 0); err != nil {
		t.Fatal(err)
	}
	return w
}

func mustRemoteWatch(t *testing.T, s *Store, w RemoteWatch) RemoteWatch {
	t.Helper()
	got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A completion whose queue row is still there never reached the provider,
// so it goes back whatever its confirmation reads: a row queued before
// v149 was recorded as confirmed by the migration's backfill.
func TestWithdrawRemoteCompletionWithItsRowIgnoresTheConfirmation(t *testing.T) {
	s, thread := remoteWatchTestThread(t)
	w := finishedRemoteWatch(t, s, thread)
	item := FlushQueueItem{ID: "queue:" + w.RequestID, ThreadID: thread, SendID: "remote-completion:" + w.ComputerID + ":" + w.RequestID, Message: "done"}
	if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmRemoteCompletion(w.ComputerID, w.RequestID, 500); err != nil {
		t.Fatal(err)
	}
	if err := s.WithdrawRemoteCompletion(w.ComputerID, w.RequestID, item.ID, WithdrawQueued, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRemoteWatch(t, s, w); got.Notification != "pending" || got.ConfirmedAt != 0 {
		t.Fatalf("a queued completion was kept: %q confirmed at %d", got.Notification, got.ConfirmedAt)
	}
}
