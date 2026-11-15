package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A rest stops every open request the thread made and every spawn it made,
// settled or not; nothing settled but a spawn, and nothing another thread
// made. The pages read oldest first and continue where the last one ended.
func TestListThreadRestRequestsReadsWhatARestStops(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"t-rest", "t-other", "t-open-child", "t-done-child"} {
		mustCreateThread(t, s, id)
	}
	insert := func(token, caller, kind, target string, at int64) {
		t.Helper()
		row := ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target,
			State: ThreadRequestAccepted, CreatedAt: at, UpdatedAt: at,
		}
		if kind == ThreadRequestRemind {
			row.DueAt, row.Notify = at+1000, true
		}
		if err := s.InsertThreadRequest(row); err != nil {
			t.Fatalf("insert %s: %v", token, err)
		}
	}
	settle := func(token string) {
		t.Helper()
		if _, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, AnswerKind: ThreadAnswerFinal, SettledAt: 900,
		}); err != nil {
			t.Fatalf("settle %s: %v", token, err)
		}
	}
	insert("done-spawn", "t-rest", ThreadRequestSpawn, "t-done-child", 100)
	insert("done-send", "t-rest", ThreadRequestSend, "t-other", 200)
	insert("open-send", "t-rest", ThreadRequestSend, "t-other", 300)
	insert("open-spawn", "t-rest", ThreadRequestSpawn, "t-open-child", 400)
	insert("open-remind", "t-rest", ThreadRequestRemind, "", 500)
	insert("elsewhere", "t-other", ThreadRequestSend, "t-rest", 600)
	settle("done-spawn")
	settle("done-send")

	var got []string
	var after ThreadRestCursor
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the pages never ended")
		}
		rows, err := s.ListThreadRestRequests("t-rest", after, 2)
		if err != nil {
			t.Fatalf("ListThreadRestRequests: %v", err)
		}
		for _, row := range rows {
			got = append(got, row.Token)
		}
		if len(rows) < 2 {
			break
		}
		after = after.After(rows[len(rows)-1])
	}
	if want := []string{"done-spawn", "open-send", "open-spawn", "open-remind"}; !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

// A rest drops each notification of the thread's remote commands the agent
// has not taken in, pending or queued and unconfirmed, and keeps one the
// provider confirmed and every other thread's.
func TestDismissUndeliveredRemoteWatchesKeepsWhatWasTakenIn(t *testing.T) {
	s := newTestStore(t)
	thread, other := Thread{ID: uuid.NewString(), Provider: "codex"}, Thread{ID: uuid.NewString(), Provider: "codex"}
	for _, row := range []Thread{thread, other} {
		if err := s.CreateThread(row); err != nil {
			t.Fatal(err)
		}
	}
	computer := uuid.NewString()
	watch := func(threadID string) RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: computer, RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "job"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	queue := func(w RemoteWatch) {
		t.Helper()
		if err := s.QueueRemoteCompletion(w.ComputerID, w.RequestID, FlushQueueItem{
			ID: "queue:" + w.RequestID, ThreadID: w.ThreadID, SendID: "remote-completion:" + w.RequestID, Message: "done",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, queued, confirmed, elsewhere := watch(thread.ID), watch(thread.ID), watch(thread.ID), watch(other.ID)
	queue(queued)
	queue(confirmed)
	if ok, err := s.ConfirmRemoteCompletion(confirmed.ComputerID, confirmed.RequestID, 1); err != nil || !ok {
		t.Fatalf("ConfirmRemoteCompletion: %v, %v", ok, err)
	}

	dropped, err := s.DismissUndeliveredRemoteWatches(thread.ID)
	if err != nil || dropped != 2 {
		t.Fatalf("DismissUndeliveredRemoteWatches = %d, %v; want 2", dropped, err)
	}
	for _, want := range []struct {
		watch        RemoteWatch
		notification string
	}{{pending, "dismissed"}, {queued, "dismissed"}, {confirmed, "queued"}, {elsewhere, "pending"}} {
		got, err := s.GetRemoteWatch(want.watch.ComputerID, want.watch.RequestID)
		if err != nil || got.Notification != want.notification {
			t.Errorf("watch %s = %q, %v; want %q", want.watch.RequestID, got.Notification, err, want.notification)
		}
	}
}
