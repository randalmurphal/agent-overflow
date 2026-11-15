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

// A caller's spawn of a thread is its spawn link here, lineage included and
// a superseded spawn excluded, and every spawn row it holds for a thread on
// a paired computer; any request naming the thread links the caller to it.
func TestCallerSpawnsOfReadsTheSpawnLink(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"t-caller", "t-other", "t-worker", "t-old", "t-sent"} {
		mustCreateThread(t, s, id)
	}
	insert := func(token, caller, kind, target, computer string) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, TargetComputerID: computer,
			State: ThreadRequestAccepted, CreatedAt: 100, UpdatedAt: 100,
		}); err != nil {
			t.Fatalf("insert %s: %v", token, err)
		}
	}
	insert("lineage", "t-caller", ThreadRequestSpawn, "t-worker", "")
	insert("superseded", "t-caller", ThreadRequestSpawn, "t-old", "")
	insert("remote", "t-caller", ThreadRequestSpawn, "t-remote", "computer-b")
	insert("sent", "t-caller", ThreadRequestSend, "t-sent", "")
	insert("elsewhere", "t-other", ThreadRequestSpawn, "t-sent", "")
	if _, err := s.db.Exec(clearLineagePayloadSQL + ` AND token = 'lineage'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE thread_requests SET superseded = 1 WHERE token = 'superseded'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		computer, target string
		want             []string
	}{
		{"", "t-worker", []string{"lineage"}},
		{"", "t-old", nil},
		{"computer-b", "t-remote", []string{"remote"}},
		{"", "t-remote", nil},
		{"", "t-sent", nil},
	} {
		rows, err := s.CallerSpawnsOf("t-caller", tc.computer, tc.target)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range rows {
			got = append(got, row.Token)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("CallerSpawnsOf(%q, %q) = %q, want %q", tc.computer, tc.target, got, tc.want)
		}
	}
	for target, want := range map[string]bool{"t-sent": true, "t-worker": true, "t-other": false} {
		if got, err := s.CallerRequestedOf("t-caller", target); err != nil || got != want {
			t.Errorf("CallerRequestedOf(%s) = %v, %v; want %v", target, got, err, want)
		}
	}
}

// A rest pauses the root runs the thread started and that have not ended:
// not a run a person bound to it, one a phase started, a run that ended, or
// another thread's. A called run is never bound.
func TestListThreadStartedWorkItemsReadsTheRunsARestPauses(t *testing.T) {
	s := newTestStore(t)
	create := func(id, state, source, sourceRef, origin string, at int64) {
		t.Helper()
		item := testWorkItem(id, "project-a", state, at)
		item.Source, item.SourceRef, item.OriginThreadID = source, sourceRef, origin
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	create("started", "running", "agent", "", "t-rest", 10)
	create("parked", "needs-human", "agent", "", "t-rest", 20)
	create("bound-by-a-person", "running", "manual", "", "t-rest", 30)
	create("phase-started", "running", "agent", "phase:x", "t-rest", 40)
	create("ended", "done", "agent", "", "t-rest", 60)
	create("elsewhere", "running", "agent", "", "t-other", 70)

	runs, err := s.ListThreadStartedWorkItems("t-rest", "agent")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, run := range runs {
		got = append(got, run.ID)
	}
	if want := []string{"started", "parked"}; !slices.Equal(got, want) {
		t.Fatalf("runs = %q, want %q", got, want)
	}
}

// A thread works in a run while a running run holds it as a phase or a
// unit; one that ended does not hold it.
func TestThreadRunningWorkflowRunNamesTheRunAThreadWorksIn(t *testing.T) {
	s := newTestStore(t)
	for _, item := range []WorkItem{testWorkItem("live", "project-a", "running", 10), testWorkItem("over", "project-a", "done", 20)} {
		if err := s.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []WorkItemPhase{
		{ItemID: "live", PhaseID: "run", Attempt: 1, ThreadID: "t-member", Status: "running"},
		{ItemID: "over", PhaseID: "run", Attempt: 1, ThreadID: "t-former", Status: "completed"},
	} {
		if err := s.CreateWorkItemPhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	for thread, want := range map[string]string{"t-member": "live", "t-former": "", "t-none": ""} {
		if got, err := s.ThreadRunningWorkflowRun(thread); err != nil || got != want {
			t.Errorf("ThreadRunningWorkflowRun(%s) = %q, %v; want %q", thread, got, err, want)
		}
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
