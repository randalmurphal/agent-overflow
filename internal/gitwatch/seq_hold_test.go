package gitwatch

import (
	"context"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
)

// publishDuringHold takes a hold on dir and answers a Publish of a status
// unlike the published one during it, returning the answer and the hold's
// release.
func publishDuringHold(t *testing.T, mgr *Manager, dir string, sub *Subscription) (gitops.GitStatus, func()) {
	t.Helper()
	resume := mgr.Suppress(dir)
	t.Cleanup(resume)
	read := gitops.GitStatus{IsRepo: true, Branch: "feature", FileCount: 3}
	got, err := mgr.Publish(dir, func() (gitops.GitStatus, error) { return read, nil })
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if read.Seq = got.Seq; got != read || got.Seq <= sub.Initial().Seq {
		t.Fatalf("Publish during the hold answered %+v, want its own read at a seq above %d", got, sub.Initial().Seq)
	}
	return got, resume
}

// A Publish during a hold answers with its own read, stamped with its
// seq, and publishes nothing: no broadcast, and a new subscriber gets the
// status published before the hold.
func TestPublishDuringAHoldAnswersItsOwnRead(t *testing.T) {
	t.Parallel()
	mgr, _, dir, sub := newSeqWatch(t)
	publishDuringHold(t, mgr, dir, sub)
	expectNoUpdate(t, sub, 100*time.Millisecond)
	next, err := mgr.Subscribe(t.Context(), dir)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	defer next.Close()
	if got := next.Initial(); got != sub.Initial() {
		t.Fatalf("snapshot during the hold %+v, want the status published before it %+v", got, sub.Initial())
	}
}

// After a Publish answered during a hold, the resume's refresh is broadcast
// although its content equals the status published before the hold: the
// client holding the answer must learn it no longer applies. That
// broadcast clears the mark, so the next equal refresh broadcasts nothing.
func TestResumeAfterAnAnswerDuringAHoldBroadcastsEqualContent(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)
	answer, resume := publishDuringHold(t, mgr, dir, sub)
	resume()

	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	got := recvWithin(t, sub, 3*time.Second)
	if !got.Equal(sub.Initial()) || got.Seq <= answer.Seq {
		t.Fatalf("broadcast after resume %+v, want the pre-hold content at a seq above the answer's %d", got, answer.Seq)
	}

	mgr.RequestRefresh(dir)
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	expectNoUpdate(t, sub, 100*time.Millisecond)
}

// A hold during which no answer was handed out leaves every client with a
// published status, so the resume's equal refresh broadcasts nothing.
func TestResumeWithoutAnAnswerBroadcastsNothingForEqualContent(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)
	mgr.Suppress(dir)()
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	expectNoUpdate(t, sub, 100*time.Millisecond)
}

// A refresh that began before the hold and finishes after it is broadcast,
// but its seq is below the answer's, so a client holding the answer ignores
// it: the mark stays until a broadcast passes the answer.
func TestRefreshStraddlingAHoldDoesNotClearTheAnswer(t *testing.T) {
	t.Parallel()
	mgr, refreshes, dir, sub := newSeqWatch(t)
	mgr.RequestRefresh(dir)
	refreshes.awaitEntered(t)
	answer, resume := publishDuringHold(t, mgr, dir, sub)
	resume()

	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	if straddler := recvWithin(t, sub, 3*time.Second); straddler.Seq >= answer.Seq {
		t.Fatalf("straddling refresh %+v, want a seq below the answer's %d", straddler, answer.Seq)
	}
	refreshes.awaitEntered(t)
	refreshes.finish(t, gitops.GitStatus{IsRepo: true, Branch: "main"})
	if got := recvWithin(t, sub, 3*time.Second); got.Seq <= answer.Seq {
		t.Fatalf("broadcast after the straddler %+v, want a seq above the answer's %d", got, answer.Seq)
	}
}

// A change a Publish published counts for missed-event detection only once
// the hold taken before its refresh ran is released: the watches of a
// workspace being deleted are not reinstalled, and the resume's refresh
// reinstalls them.
func TestOfferedChangeWaitsOutAHoldBeforeReinstalling(t *testing.T) {
	t.Parallel()
	refreshes := newGatedRead()
	mgr := NewManager(ManagerConfig{
		StatusFn: refreshes.fn(),
		FastStatusFn: func(context.Context, string) (gitops.GitStatus, error) {
			return gitops.GitStatus{IsRepo: true, Branch: "main"}, nil
		},
	})
	installs := &countingInstall{}
	mgr.installFn = installs.fn()
	t.Cleanup(mgr.Close)
	dir := makeRepoDir(t)
	sub, err := mgr.Subscribe(t.Context(), dir)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	// The run loop is held inside a requested refresh while the change is
	// published and the hold is taken.
	mgr.RequestRefresh(dir)
	refreshes.awaitEntered(t)
	changed := gitops.GitStatus{IsRepo: true, Branch: "main", FileCount: 4}
	if _, err := mgr.Publish(dir, func() (gitops.GitStatus, error) { return changed, nil }); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := recvWithin(t, sub, 3*time.Second); !got.Equal(changed) {
		t.Fatalf("broadcast %+v, want the published change", got)
	}
	resume := mgr.Suppress(dir)
	t.Cleanup(resume)
	refreshes.finish(t, changed)
	expectNoUpdate(t, sub, 300*time.Millisecond)
	if got := installs.count(); got != 1 {
		t.Fatalf("installs = %d during the hold, want 1", got)
	}

	resume()
	refreshes.awaitEntered(t)
	refreshes.finish(t, changed)
	waitFor(t, 3*time.Second, func() bool { return installs.count() >= 2 },
		"reinstall after the hold for the change the watches never reported")
}
