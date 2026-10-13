package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

func fromSession(evt provider.ProviderEvent, token string) provider.ProviderEvent {
	evt.SessionToken = token
	return evt
}

func compactingOpen(r *Router, threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	return st != nil && st.compactingSinceSet
}

// Handle admits a provider frame only from the session MarkThreadActive
// last committed the thread to. A frame of an earlier session stays
// refused across every later transition, including a stop and a new
// start, because tokens are never reused.
func TestHandleAdmitsOnlyTheCommittedSessionsFrames(t *testing.T) {
	t.Parallel()
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	const threadID = "thread-session-gate"
	fireAt := time.Now().Add(30 * time.Minute).UnixMilli()
	job := provider.ScheduledJob{ID: "a", Cron: "0 9 * * 1", FireByUnixMs: time.Now().Add(6 * 24 * time.Hour).UnixMilli()}
	armed := func(token string) bool {
		t.Helper()
		if err := r.Handle(fromSession(wakeupEvent(t, threadID, fireAt), token)); err != nil {
			t.Fatalf("handle wakeup from %q: %v", token, err)
		}
		if err := r.Handle(fromSession(scheduledJobsEvent(t, threadID, job), token)); err != nil {
			t.Fatalf("handle jobs from %q: %v", token, err)
		}
		_, ok := r.PendingWakeupAt(threadID)
		r.ClearProcessSchedule(threadID)
		return ok
	}

	if armed("session-a") {
		t.Fatal("a frame reached a thread no session was committed to")
	}

	r.MarkThreadActive(threadID, "session-a")
	if !armed("session-a") {
		t.Fatal("the live session's frame was refused")
	}
	if !r.SessionCurrent(threadID, "session-a") {
		t.Fatal("SessionCurrent refused the live session")
	}

	r.MarkThreadActive(threadID, "session-b")
	if armed("session-a") {
		t.Fatal("a retired session's frame armed the replacement's schedule")
	}
	if r.SessionCurrent(threadID, "session-a") {
		t.Fatal("SessionCurrent admitted a retired session")
	}
	if err := r.Handle(fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{Active: true}, time.Now()), "session-a")); err != nil {
		t.Fatalf("handle compaction: %v", err)
	}
	if compactingOpen(r, threadID) {
		t.Fatal("a retired session's frame opened a compacting window on the replacement")
	}
	if !armed("session-b") {
		t.Fatal("the replacement's own frame was refused")
	}

	// Retired, then live again under a new session: neither earlier
	// session's frames come back.
	r.CleanupThread(threadID)
	if armed("session-b") {
		t.Fatal("a stopped thread admitted its last session's frame")
	}
	r.MarkThreadActive(threadID, "session-c")
	for _, old := range []string{"session-a", "session-b"} {
		if armed(old) {
			t.Fatalf("%s's frame was admitted after the thread went live again", old)
		}
	}
	if !armed("session-c") {
		t.Fatal("the new session's frame was refused")
	}

	// A host event carries no token: refused only while the thread is
	// stopped.
	if !armed("") {
		t.Fatal("a host event was refused on a live thread")
	}
	r.CleanupThread(threadID)
	if armed("") {
		t.Fatal("a host event reached a stopped thread through Handle")
	}
}

// A frame already past Handle's entry when a replacement commits cannot
// write the dead process's schedule or compacting window for the
// replacement: the handlers that write that state compare the token again
// under the lock they write under.
func TestProcessStateHandlersRefuseARetiredSessionsFrame(t *testing.T) {
	t.Parallel()
	const threadID = "thread-session-inflight"
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	r.MarkThreadActive(threadID, "session-a")
	wake := fromSession(wakeupEvent(t, threadID, time.Now().Add(30*time.Minute).UnixMilli()), "session-a")
	jobs := fromSession(scheduledJobsEvent(t, threadID, provider.ScheduledJob{ID: "a", Cron: "* * * * *", Recurring: true, FireByUnixMs: time.Now().Add(time.Minute).UnixMilli()}), "session-a")
	compacting := fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{Active: true}, time.Now()), "session-a")

	r.MarkThreadActive(threadID, "session-b")
	if err := r.handleSessionWakeup(wake); err != nil {
		t.Fatalf("handleSessionWakeup: %v", err)
	}
	if err := r.handleScheduledJobs(jobs); err != nil {
		t.Fatalf("handleScheduledJobs: %v", err)
	}
	if err := r.handleCompactionStatus(compacting); err != nil {
		t.Fatalf("handleCompactionStatus: %v", err)
	}
	if at, ok := r.PendingWakeupAt(threadID); ok {
		t.Fatalf("the replacement holds the retired session's schedule until %s", at)
	}
	if compactingOpen(r, threadID) {
		t.Fatal("the replacement holds the retired session's compacting window")
	}
}
