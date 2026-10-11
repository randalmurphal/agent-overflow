package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A notice is accepted once it is queued. Its session starts afterwards, so
// neither a slow start nor a held action lock keeps the sender waiting, and
// a start that fails is a row in the thread.

func noticeTestThread(t *testing.T, a *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

func noticeRows(t *testing.T, a *App, threadID, prefix string) []string {
	t.Helper()
	items, err := a.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows []string
	for _, item := range items {
		if item.Kind == "notification" && strings.HasPrefix(item.Summary, prefix) {
			rows = append(rows, item.Summary)
		}
	}
	return rows
}

// sendWithin runs one thread send and fails the test if it does not return
// in time, instead of hanging on a start that never finishes.
func sendWithin(t *testing.T, a *App, threadID, text string, limit time.Duration) string {
	t.Helper()
	type result struct {
		sendID string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		sendID, err := a.SendExternalThreadMessage(threadID, text)
		done <- result{sendID, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("SendExternalThreadMessage: %v", r.err)
		}
		return r.sendID
	case <-time.After(limit):
		t.Fatalf("SendExternalThreadMessage waited past %s", limit)
		return ""
	}
}

func TestAThreadSendIsAcceptedWhileItsSessionStarts(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	release := make(chan struct{})
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		<-release
		return nil
	}
	thread := noticeTestThread(t, a, "notice-slow-start")

	first := sendWithin(t, a, thread.ID, "deploy 1 is done", 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for starts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no session start began for the first send")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The start is under way and holds the thread's action lock.
	second := sendWithin(t, a, thread.ID, "deploy 2 is done", 5*time.Second)
	if !strings.HasPrefix(first, agentSendIDRoot) || !strings.HasPrefix(second, agentSendIDRoot) || first == second {
		t.Fatalf("send ids %q and %q", first, second)
	}
	rows := durableQueueRows(t, a, thread.ID)
	if len(rows) != 2 || rows[0].SendID != first || rows[1].SendID != second {
		t.Fatalf("queued during the start: %+v", rows)
	}
	close(release)
	a.waitAgentNoticeWork()
}

func TestANoticeWhoseSessionWillNotStartSaysSoInAStoppedThread(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	thread := noticeTestThread(t, a, "notice-start-fails")
	// A thread whose session was stopped drops a torn-down session's wire
	// frames; the start failure is not one of them.
	if err := a.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	sendID := sendWithin(t, a, thread.ID, "deploy 3 is done", 5*time.Second)
	a.waitAgentNoticeWork()
	want := "A message sent with agent-overflow thread send is queued, but the agent could not start: the provider binary is missing"
	if rows := threadErrorRows(t, a.store, thread.ID, want); len(rows) != 1 {
		t.Fatalf("error rows %q, want one %q", rows, want)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 || rows[0].SendID != sendID {
		t.Fatalf("the message did not stay queued for the next start: %+v", rows)
	}
}

// Nobody watched a start a notice made, so its failure notifies, and the
// thread's next start withdraws that notification. A start the person made
// themselves shows its failure where they are looking, and a start with no
// failure to withdraw sends nothing.
func TestANoticeWhoseSessionWillNotStartNotifiesUntilTheThreadStarts(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	recorder := &recordingNotificationSender{}
	a.osNotifications = recorder
	a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	var failing atomic.Bool
	failing.Store(true)
	a.startSessionFn = func(string) error {
		if failing.Load() {
			return errors.New("the provider binary is missing")
		}
		return nil
	}
	thread := testThread("notice-start-notifies")
	thread.WorkspacePath = t.TempDir()
	thread.Title = "Deploy watcher"
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	ctx := context.Background()

	if err := a.startSession(ctx, thread.ID); err == nil {
		t.Fatal("the stubbed start succeeded")
	}
	a.notifications.queue.Wait()
	if sends := recorder.snapshot(); len(sends) != 0 {
		t.Fatalf("a start the person made notified: %#v", sends)
	}

	sendWithin(t, a, thread.ID, "deploy 8 is done", 5*time.Second)
	a.waitAgentNoticeWork()
	a.notifications.queue.Wait()
	sends := recorder.snapshot()
	want := notify.Send{
		ID:     "session-start:" + thread.ID,
		Kind:   notify.KindError,
		Title:  "Deploy watcher",
		Body:   "The agent could not start. Open the thread to see why.",
		Target: notify.Target{Kind: notify.TargetThread, ThreadID: thread.ID, BackendID: "backend-under-test"},
	}
	// Silent is the screen's sound preference, not the mapping's.
	if len(sends) == 1 {
		want.Silent = sends[0].Silent
	}
	if len(sends) != 1 || sends[0] != want {
		t.Fatalf("sends %#v, want one %#v", sends, want)
	}

	failing.Store(false)
	if err := a.startSession(ctx, thread.ID); err != nil {
		t.Fatalf("startSession: %v", err)
	}
	if err := a.startSession(ctx, thread.ID); err != nil {
		t.Fatalf("startSession again: %v", err)
	}
	a.notifications.queue.Wait()
	sends = recorder.snapshot()
	retract := notify.Send{ID: want.ID, Kind: notify.KindError, Retract: true}
	if len(sends) != 2 || sends[1] != retract {
		t.Fatalf("sends %#v, want the failure then one %#v", sends, retract)
	}
}

func TestAThreadSendArchivedBeforeItsSessionStartsIsWithdrawn(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		return nil
	}
	thread := noticeTestThread(t, a, "notice-archived")

	// An archive holds the action lock while it flips the row, and the
	// start waits for it.
	unlock := a.threadLocks().Lock(thread.ID)
	sendWithin(t, a, thread.ID, "deploy 4 is done", 5*time.Second)
	if _, _, err := a.threadApplication().Archive(thread.ID); err != nil {
		unlock()
		t.Fatalf("Archive: %v", err)
	}
	unlock()
	a.waitAgentNoticeWork()

	if n := starts.Load(); n != 0 {
		t.Fatalf("started %d sessions for an archived thread", n)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 0 {
		t.Fatalf("withdrawn send still queued: %+v", rows)
	}
	want := "A message sent with agent-overflow thread send was not delivered: the thread was archived before the agent took it in."
	if rows := noticeRows(t, a, thread.ID, want); len(rows) != 1 {
		t.Fatalf("notices %q, want one %q", rows, want)
	}
}

// A revert clears the queue, then truncates the timeline, both under the
// thread's action lock. The notice for a cleared send is written after, so
// the truncation cannot take it, and the send's row goes with its notice,
// so a second clear before then does not note it twice.
func TestAClearedQueueTellsTheThreadItsSendWasNotDelivered(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := noticeTestThread(t, a, "notice-cleared")
	sendID := sendWithin(t, a, thread.ID, "deploy 5 is done", 5*time.Second)
	sendWithin(t, a, thread.ID, "deploy 5b is done", 5*time.Second)
	a.waitAgentNoticeWork()
	if _, err := a.RegisterQueueItem(context.Background(), thread.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	want := "A message sent with agent-overflow thread send was not delivered: the thread's queue was cleared before the agent took it in."
	unlock := a.threadLocks().Lock(thread.ID)
	for range 2 {
		if err := a.StopSession(thread.ID); err != nil {
			unlock()
			t.Fatalf("StopSession: %v", err)
		}
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 2 || !strings.HasPrefix(rows[0].SendID, agentSendIDRoot) || rows[0].SendID != sendID {
		unlock()
		t.Fatalf("cleared queue kept %+v, want only the two sends waiting for their notices", rows)
	}
	if _, _, err := a.store.DeleteConversationFromTurn(thread.ID, 0); err != nil {
		unlock()
		t.Fatalf("DeleteConversationFromTurn: %v", err)
	}
	unlock()
	a.waitAgentNoticeWork()
	if rows := noticeRows(t, a, thread.ID, want); len(rows) != 2 {
		t.Fatalf("notices %q, want two %q", rows, want)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 0 {
		t.Fatalf("queue after the notices: %+v", rows)
	}
}

// A cleared send whose notice shutdown keeps from being written stays
// queued, and the next boot writes its notice.
func TestAClearedSendShutdownKeptFromItsNoticeIsNotedAtBoot(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return nil }
	thread := noticeTestThread(t, a, "notice-cleared-at-quit")
	sendID := sendWithin(t, a, thread.ID, "deploy 11 is done", 5*time.Second)
	a.waitAgentNoticeWork()

	a.stopAgentNoticeWork()
	if err := a.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 || rows[0].SendID != sendID {
		t.Fatalf("queue after a clear at shutdown: %+v, want the send kept for the next boot", rows)
	}
	a.restoreDurableFlushQueueAtBoot()
	want := "A message sent with agent-overflow thread send was not delivered: the app quit before the agent took it in."
	if rows := noticeRows(t, a, thread.ID, want); len(rows) != 1 {
		t.Fatalf("notices %q, want one %q", rows, want)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 0 {
		t.Fatalf("queue after boot: %+v", rows)
	}
}

// Shutdown refuses a start a notice would begin after it has joined the
// ones in flight; the queued row is the next boot's.
func TestANoticeQueuedAfterTheStartsStoppedStartsNothing(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		return nil
	}
	thread := noticeTestThread(t, a, "notice-stopped")
	a.stopAgentNoticeWork()
	sendID := sendWithin(t, a, thread.ID, "deploy 6 is done", 5*time.Second)
	a.waitAgentNoticeWork()
	if n := starts.Load(); n != 0 {
		t.Fatalf("started %d sessions after the starts were stopped", n)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 || rows[0].SendID != sendID {
		t.Fatalf("queued row: %+v", rows)
	}
}

// A session someone else started while the notice waited for the lock
// already has the message; starting again would restart it.
func TestANoticeDoesNotRestartASessionStartedWhileItWaited(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		return nil
	}
	thread := noticeTestThread(t, a, "notice-started-meanwhile")
	unlock := a.threadLocks().Lock(thread.ID)
	sendWithin(t, a, thread.ID, "deploy 7 is done", 5*time.Second)
	a.sessionManager().put(thread.ID, session{Provider: thread.Provider, Token: "started-meanwhile"})
	unlock()
	a.waitAgentNoticeWork()
	a.sessionManager().take(thread.ID)
	if n := starts.Load(); n != 0 {
		t.Fatalf("started %d sessions over a live one", n)
	}
}

// A wake held by a start that failed does not stall its loop: each later
// fire tries the start again, no sooner than half the floor after the last
// try, and says nothing new while the start keeps failing. The fire whose
// start succeeds queues its own wake behind the held one, and the start
// withdraws the one notification the streak raised.
func TestALaterFireRetriesAStartThatFailed(t *testing.T) {
	t.Parallel()
	a, emitted := newAppForFlushQueueRPC(t)
	recorder := &recordingNotificationSender{}
	a.osNotifications = recorder
	a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	a.agentWakes.floorOverride = 2 * time.Second
	var failing atomic.Bool
	failing.Store(true)
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		n := starts.Add(1)
		if failing.Load() {
			// Each attempt fails differently, so a row a retry wrote would
			// show even over the same item id.
			return fmt.Errorf("the provider binary is missing (attempt %d)", n)
		}
		return nil
	}
	thread := noticeTestThread(t, a, "notice-retry")
	row, err := a.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-retry", ThreadID: thread.ID, Kind: store.AgentWakeLoop, Label: "board",
		IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("RegisterAgentWake: %v", err)
	}
	ctx := context.Background()
	fire := func(at int64) error {
		t.Helper()
		err := a.deliverAgentWake(ctx, row, at, fmt.Sprintf("fire %d", at))
		a.waitAgentNoticeWork()
		a.notifications.queue.Wait()
		return err
	}
	queued := func() []string {
		var ids []string
		for _, item := range durableQueueRows(t, a, thread.ID) {
			ids = append(ids, item.SendID)
		}
		return ids
	}
	// Error rows of a stopped thread share one item id, so the rows written
	// are counted on the wire rather than in the store.
	startFailedRows := func() int {
		n := 0
		for _, call := range emitted.snapshot() {
			data, err := json.Marshal(call.Data)
			if err != nil {
				t.Fatalf("marshal %s: %v", call.Channel, err)
			}
			if call.Channel == "provider:item_event" && strings.Contains(string(data), "woke this thread and its message is queued, but the agent could not start") {
				n++
			}
		}
		return n
	}

	if err := fire(1); err != nil {
		t.Fatalf("first fire: %v", err)
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("starts after the first fire = %d, want 1", n)
	}
	// Too soon after the failure: the fire is skipped without a start.
	if err := fire(2); !errors.Is(err, store.ErrAgentWakeStillQueued) {
		t.Fatalf("fire inside the retry gap: %v, want ErrAgentWakeStillQueued", err)
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("starts inside the retry gap = %d, want 1", n)
	}
	time.Sleep(a.agentWakeFloor() / 2)
	if err := fire(3); !errors.Is(err, store.ErrAgentWakeStillQueued) {
		t.Fatalf("fire whose retry failed: %v, want ErrAgentWakeStillQueued", err)
	}
	if n := starts.Load(); n != 2 {
		t.Fatalf("starts after a due retry = %d, want 2", n)
	}
	if n := startFailedRows(); n != 1 {
		t.Fatalf("start failure rows written = %d, want one for the streak", n)
	}
	if sends := recorder.snapshot(); len(sends) != 1 || sends[0].Retract {
		t.Fatalf("sends %#v, want one start failure for the streak", sends)
	}
	if got, want := queued(), []string{agentWakeSendID(row.ID, 1)}; !slices.Equal(got, want) {
		t.Fatalf("queued %q, want %q", got, want)
	}

	failing.Store(false)
	time.Sleep(a.agentWakeFloor() / 2)
	if err := fire(4); err != nil {
		t.Fatalf("fire whose retry started the session: %v", err)
	}
	if got, want := queued(), []string{agentWakeSendID(row.ID, 1), agentWakeSendID(row.ID, 4)}; !slices.Equal(got, want) {
		t.Fatalf("queued %q, want the held wake and the new one %q", got, want)
	}
	sends := recorder.snapshot()
	retract := notify.Send{ID: "session-start:" + thread.ID, Kind: notify.KindError, Retract: true}
	if len(sends) != 2 || sends[1] != retract {
		t.Fatalf("sends %#v, want the failure then %#v", sends, retract)
	}
	if n := startFailedRows(); n != 1 {
		t.Fatalf("start failure rows written after the start = %d", n)
	}
}

// A notice whose check before the start fails to read is told as that read
// failing, not as the agent failing to start, and nothing is started. It
// still notifies: nobody is reading the row, and the message waits.
func TestANoticeWhoseCheckCannotReadSaysWhichRead(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	recorder := &recordingNotificationSender{}
	a.osNotifications = recorder
	a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
	var starts atomic.Int32
	a.startSessionFn = func(string) error {
		starts.Add(1)
		return nil
	}
	thread := noticeTestThread(t, a, "notice-check-fails")
	if err := a.queueAgentNotice(agentNotice{
		threadID:    thread.ID,
		sendID:      agentSendIDRoot + "check",
		prepare:     func() (string, bool, error) { return "deploy 12 is done", true, nil },
		keep:        func() (bool, error) { return false, errors.New("read the thread: database is locked") },
		startFailed: func(err error) string { return "the agent could not start: " + err.Error() },
	}); err != nil {
		t.Fatalf("queueAgentNotice: %v", err)
	}
	a.waitAgentNoticeWork()
	a.notifications.queue.Wait()
	if n := starts.Load(); n != 0 {
		t.Fatalf("started %d sessions after the check failed", n)
	}
	want := "A message is queued for this thread, but the agent was not started for it: the app could not check that the message is still wanted (read the thread: database is locked)."
	if rows := threadErrorRows(t, a.store, thread.ID, ""); len(rows) != 1 || rows[0] != want {
		t.Fatalf("error rows %q, want only %q", rows, want)
	}
	if sends := recorder.snapshot(); len(sends) != 1 || sends[0].ID != "session-start:"+thread.ID || sends[0].Retract {
		t.Fatalf("sends %#v, want the start failure", sends)
	}
	if rows := durableQueueRows(t, a, thread.ID); len(rows) != 1 {
		t.Fatalf("queued %+v, want the message kept", rows)
	}
}

// heldNoticeFixture is a thread whose session will not start, so whatever
// the app queues for it is held for the next start.
func heldNoticeFixture(t *testing.T, id string) (*App, store.Thread) {
	t.Helper()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	return a, noticeTestThread(t, a, id)
}

// holdAgentWake registers one loop or watch on thread and delivers a wake of
// it, which the failing start leaves queued.
func holdAgentWake(t *testing.T, a *App, thread store.Thread, kind, label string) store.AgentWake {
	t.Helper()
	row, err := a.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-" + label, ThreadID: thread.ID, Kind: kind, Label: label, Command: "true",
		IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatalf("RegisterAgentWake %s: %v", label, err)
	}
	if err := a.deliverAgentWake(context.Background(), row, 1, label+" fired"); err != nil {
		t.Fatalf("deliverAgentWake %s: %v", label, err)
	}
	a.waitAgentNoticeWork()
	return row
}

// queuedSendIDs is what the thread's queue holds, durable rows and in-memory
// entries, which a start would dispatch.
func queuedSendIDs(t *testing.T, a *App, threadID string) (durable, memory []string) {
	t.Helper()
	for _, row := range durableQueueRows(t, a, threadID) {
		durable = append(durable, row.SendID)
	}
	items, err := a.GetQueueState(threadID)
	if err != nil {
		t.Fatalf("GetQueueState: %v", err)
	}
	for _, item := range items {
		memory = append(memory, item.SendID)
	}
	return durable, memory
}

// Archiving a thread whose session is stopped takes back what the app queued
// for it, as archiving a live one does, so a later start, whoever makes it,
// delivers none of it. The person's own queued message stays theirs.
func TestArchivingAStoppedThreadWithdrawsWhatTheAppQueued(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "notice-archive-stopped")
	holdAgentWake(t, a, thread, store.AgentWakeLoop, "board")
	sendWithin(t, a, thread.ID, "deploy 13 is done", 5*time.Second)
	a.waitAgentNoticeWork()
	if _, err := a.RegisterQueueItem(context.Background(), thread.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if durable, _ := queuedSendIDs(t, a, thread.ID); len(durable) != 3 {
		t.Fatalf("held before the archive: %q, want the wake, the send and the person's message", durable)
	}

	if err := a.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	a.waitAgentNoticeWork()
	durable, memory := queuedSendIDs(t, a, thread.ID)
	if !slices.Equal(durable, []string{"person-send"}) || !slices.Equal(memory, []string{"person-send"}) {
		t.Fatalf("queued after the archive: durable %q, in memory %q, want only the person's message", durable, memory)
	}
	want := "A message sent with agent-overflow thread send was not delivered: the thread was archived before the agent took it in."
	if rows := noticeRows(t, a, thread.ID, want); len(rows) != 1 {
		t.Fatalf("notices %q, want one %q", rows, want)
	}
}

// Cancelling a loop or watch takes back its wake a failed start left queued,
// whichever door cancels it (the tool, a demotion, a move), and leaves other
// rows' wakes alone.
func TestCancellingAStoppedThreadsLoopOrWatchWithdrawsItsHeldWake(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "notice-cancel-stopped")
	if _, _, err := a.store.PromoteStandingAgent(thread.ID, t.TempDir(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	board := holdAgentWake(t, a, thread, store.AgentWakeLoop, "board")
	inbox := holdAgentWake(t, a, thread, store.AgentWakeLoop, "inbox")
	feed := holdAgentWake(t, a, thread, store.AgentWakeWatch, "feed")
	held := func() []string {
		durable, memory := queuedSendIDs(t, a, thread.ID)
		if !slices.Equal(durable, memory) {
			t.Fatalf("durable queue %q and in-memory queue %q differ", durable, memory)
		}
		return durable
	}
	if got := held(); len(got) != 3 {
		t.Fatalf("held: %q, want one wake of each", got)
	}

	if _, cancelled, err := (threadToolsApp{app: a}).CancelWake(context.Background(), threadtools.Caller{ThreadID: thread.ID}, "board", ""); err != nil || !cancelled {
		t.Fatalf("CancelWake: %v, %v", cancelled, err)
	}
	if got, want := held(), []string{agentWakeSendID(inbox.ID, 1), agentWakeSendID(feed.ID, 1)}; !slices.Equal(got, want) {
		t.Fatalf("after cancelling %s: %q, want %q", board.Label, got, want)
	}
	if err := a.DemoteStandingAgent(thread.ID); err != nil {
		t.Fatalf("DemoteStandingAgent: %v", err)
	}
	if got, want := held(), []string{agentWakeSendID(inbox.ID, 1)}; !slices.Equal(got, want) {
		t.Fatalf("after demoting away the watch: %q, want %q", got, want)
	}
	if err := a.stopThreadRequestsForMovedThread(context.Background(), thread.ID, ""); err != nil {
		t.Fatalf("stopThreadRequestsForMovedThread: %v", err)
	}
	if got := held(); len(got) != 0 {
		t.Fatalf("after moving the thread away: %q, want nothing queued", got)
	}
}

// A fire that retries the start for its row's held wake starts it as that
// wake's: when the wake is no longer wanted by then, the wake queued is the
// one withdrawn, not the fire's own, which was never queued.
func TestARetryForAWakeNoLongerWantedWithdrawsTheHeldWake(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "notice-retry-refused")
	a.agentWakes.floorOverride = 2 * time.Second
	row := holdAgentWake(t, a, thread, store.AgentWakeLoop, "board")
	time.Sleep(a.agentWakeFloor() / 2)
	// The cancel lands after this fire's own check and before its retry,
	// and the cancel's own withdrawal has not run yet.
	if _, _, err := a.store.CancelAgentWake(thread.ID, row.Label, "", agentWakeOutcomeCancelled); err != nil {
		t.Fatalf("CancelAgentWake: %v", err)
	}
	notice, _ := a.agentWakeNotice(row, agentWakeSendID(row.ID, 2), "board fired again")
	if err := a.retryAgentWakeStart(row, notice); !errors.Is(err, store.ErrAgentWakeStillQueued) {
		t.Fatalf("retryAgentWakeStart: %v, want ErrAgentWakeStillQueued", err)
	}
	a.waitAgentNoticeWork()
	if durable, memory := queuedSendIDs(t, a, thread.ID); len(durable) != 0 || len(memory) != 0 {
		t.Fatalf("queued after the refused retry: durable %q, in memory %q, want the held wake withdrawn", durable, memory)
	}
}

// A message a withdrawal could not take back would reach the agent at the
// next start, so the archive, the demotion and the cancel tool report it
// rather than return as if it were gone.
func TestAWithdrawalThatFailsIsReported(t *testing.T) {
	t.Parallel()
	a, thread := heldNoticeFixture(t, "notice-withdraw-fails")
	if _, _, err := a.store.PromoteStandingAgent(thread.ID, t.TempDir(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	holdAgentWake(t, a, thread, store.AgentWakeLoop, "board")
	holdAgentWake(t, a, thread, store.AgentWakeWatch, "feed")
	// Send admission refuses every removal from here on, as it does once
	// shutdown has begun.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.appCtx = ctx

	if _, _, err := (threadToolsApp{app: a}).CancelWake(context.Background(), threadtools.Caller{ThreadID: thread.ID}, "board", ""); err == nil ||
		!strings.Contains(err.Error(), `the loop "board" is cancelled, but its wake already queued could not be taken back`) {
		t.Fatalf("CancelWake: %v, want the wake left queued reported", err)
	}
	if err := a.DemoteStandingAgent(thread.ID); err == nil || !strings.Contains(err.Error(), "withdraw agent-wake:wake-feed:1") {
		t.Fatalf("DemoteStandingAgent: %v, want the wake left queued reported", err)
	}
	if err := a.stopArchivedThreadWork(context.Background(), thread.ID); err == nil || !strings.Contains(err.Error(), "withdraw agent-wake:wake-board:1") {
		t.Fatalf("stopArchivedThreadWork: %v, want the wakes left queued reported", err)
	}
}

// A start-failure notification outlives a restart, so the mark that
// withdraws it does too: the first start that succeeds after the restart
// withdraws it, once, and a later boot finds nothing left to withdraw.
func TestAStartFailureRaisedBeforeARestartIsWithdrawnAfterIt(t *testing.T) {
	t.Parallel()
	_, dbPath := newTestAppWithStorePath(t)
	boot := func(startErr error) (*App, *recordingNotificationSender) {
		t.Helper()
		a := newTestAppAtStorePath(t, dbPath)
		rec := &emitRecorder{}
		a.testEmitHook = rec.capture
		a.triage = triage.NewRouter(a.store, rec.captureChannel)
		a.configureTriageQueueCallbacks()
		recorder := &recordingNotificationSender{}
		a.osNotifications = recorder
		a.storeIdentity.Store(&store.Identity{BackendID: "backend-under-test"})
		a.startSessionFn = func(string) error { return startErr }
		if err := a.loadSessionStartFailures(); err != nil {
			t.Fatalf("loadSessionStartFailures: %v", err)
		}
		return a, recorder
	}
	ctx := context.Background()
	retract := notify.Send{ID: "session-start:notice-restart", Kind: notify.KindError, Retract: true}

	before, raised := boot(errors.New("the provider binary is missing"))
	thread := noticeTestThread(t, before, "notice-restart")
	sendWithin(t, before, thread.ID, "deploy 21 is done", 5*time.Second)
	before.waitAgentNoticeWork()
	before.notifications.queue.Wait()
	if sends := raised.snapshot(); len(sends) != 1 || sends[0].Retract || sends[0].ID != retract.ID {
		t.Fatalf("before the restart: %#v, want the failure raised once", sends)
	}

	after, withdrawn := boot(nil)
	for range 2 {
		if err := after.startSession(ctx, thread.ID); err != nil {
			t.Fatalf("startSession: %v", err)
		}
	}
	after.notifications.queue.Wait()
	if sends := withdrawn.snapshot(); len(sends) != 1 || sends[0] != retract {
		t.Fatalf("after the restart: %#v, want one %#v", sends, retract)
	}

	again, quiet := boot(nil)
	if err := again.startSession(ctx, thread.ID); err != nil {
		t.Fatalf("startSession after a second restart: %v", err)
	}
	again.notifications.queue.Wait()
	if sends := quiet.snapshot(); len(sends) != 0 {
		t.Fatalf("after a second restart: %#v, want nothing left to withdraw", sends)
	}
}
