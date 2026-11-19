package app

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/transport"
)

// remoteWatchesNudged takes the nudge waiting for the watch loop, if any.
func remoteWatchesNudged(a *App) bool {
	watches, _ := a.remoteWatchNudges.channels()
	return takeNudge(watches)
}

// remoteCancelsNudged takes the nudge waiting for the owed-cancel loop.
func remoteCancelsNudged(a *App) bool {
	_, cancel := a.remoteWatchNudges.channels()
	return takeNudge(cancel)
}

// A completion queued on a thread whose assistant moves on is withdrawn
// from that queue and given back to its watch, and the watcher is woken to
// deliver it to the successor.
func TestACompletionAHandOffWithdrawsWakesTheWatcher(t *testing.T) {
	t.Parallel()
	a, old := remoteJobAssistant(t)
	w := completedRemoteWatch(t, a, old)
	watchPass(t, a, w)
	if got := completionRows(t, a, old.ID, w); got != 1 {
		t.Fatalf("completion rows before the handoff = %d, want 1", got)
	}
	remoteWatchesNudged(a)
	successor, err := a.nextAssistantThread(t.Context(), old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a.waitAgentNoticeWork()
	saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil || saved.Notification != "pending" || saved.ThreadID != successor.ID {
		t.Fatalf("the watch after the handoff = %+v, %v; want pending on the successor", saved, err)
	}
	if !remoteWatchesNudged(a) {
		t.Fatal("the withdrawn completion did not wake the watcher")
	}
}

func takeNudge(nudge chan struct{}) bool {
	select {
	case <-nudge:
		return true
	default:
		return false
	}
}

// TestWorkForTheRemoteWatcherNudgesIt: the watcher sleeps until its next due
// row, so each write that adds a row or brings one forward wakes it: a
// command started, the completions an archive held unarchived by either
// path, a cancel owed, and an owed cancel settled.
func TestWorkForTheRemoteWatcherNudgesIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, receiver, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)

	// A start whose reply is lost leaves only the registered watch for the
	// watcher to find.
	receiver.lostReply.Store(true)
	remoteWatchesNudged(source)
	if _, err := source.AgentRemoteStart(ctx, input); err == nil {
		t.Fatal("a start whose reply was lost succeeded")
	}
	if !remoteWatchesNudged(source) {
		t.Fatal("a registered command did not wake the watcher")
	}
	// Its retry registers nothing new and records the receipt it reads.
	receiver.lostReply.Store(false)
	if _, err := source.AgentRemoteStart(ctx, input); err != nil {
		t.Fatal(err)
	}
	if !remoteWatchesNudged(source) {
		t.Fatal("an observed receipt did not wake the watcher")
	}

	for _, unarchive := range []struct {
		name string
		run  func() error
	}{
		{"UnarchiveThread", func() error { _, err := source.UnarchiveThread(scope.ThreadID); return err }},
		{"thread_update", func() error {
			archived := false
			_, _, err := source.applyThreadOrganizePatchLocked(ctx, scope.ThreadID, threadapp.OrganizePatch{Archived: &archived})
			return err
		}},
	} {
		if _, _, err := source.store.ArchiveThread(scope.ThreadID); err != nil {
			t.Fatal(err)
		}
		remoteWatchesNudged(source)
		if err := unarchive.run(); err != nil {
			t.Fatalf("%s: %v", unarchive.name, err)
		}
		if !remoteWatchesNudged(source) {
			t.Fatalf("%s did not wake the watcher for the completions the archive held", unarchive.name)
		}
	}

	refusal := errorsx.Public("remote_unavailable", "Not now.", nil)
	receiver.cancelRefusal.Store(&refusal)
	watch, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The test command may have finished already; the cancel is sent only for
	// one the watch still reads as running.
	watch.Receipt.State = "running"
	remoteCancelsNudged(source)
	if undelivered, err := source.cancelRemoteCommands(ctx, scope.ThreadID, []store.RemoteWatch{watch}, true); undelivered == nil || err != nil {
		t.Fatalf("cancel = %v, %v; want it owed", undelivered, err)
	}
	if !remoteCancelsNudged(source) {
		t.Fatal("an owed cancel did not wake its loop")
	}

	remoteWatchesNudged(source)
	source.settleOwedRemoteCancel(watch)
	if !remoteWatchesNudged(source) {
		t.Fatal("a settled cancel did not wake the watcher for its watch")
	}
}

// runRemoteWatchLoop starts runDueLoop in the calling synctest bubble over
// a schedule the test sets, and returns the pass count, the nudge channel
// and a stop that waits for the loop to end.
func runRemoteWatchLoop(t *testing.T, next func() store.ScheduledMoment) (*atomic.Int64, chan struct{}, func()) {
	t.Helper()
	var passes atomic.Int64
	nudge := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDueLoop(ctx, nudge, func() (store.ScheduledMoment, error) { return next(), nil },
			func(time.Time) { passes.Add(1) })
	}()
	return &passes, nudge, func() {
		cancel()
		<-done
	}
}

// TestRemoteWatchLoopDoesNothingWithoutCommands: with no command watched
// and no cancel owed, the watcher makes its boot pass and then waits for
// work instead of reading the store every two seconds.
func TestRemoteWatchLoopDoesNothingWithoutCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		passes, _, stop := runRemoteWatchLoop(t, func() store.ScheduledMoment { return store.ScheduledMoment{} })
		defer stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := passes.Load(); got != 1 {
			t.Fatalf("passes over an hour with no commands = %d, want the boot pass alone", got)
		}
	})
}

// TestRemoteWatchLoopSleepsUntilItsNextDueRow: a running command checked
// every five seconds costs one pass per check, and new work is taken at
// once on a nudge.
func TestRemoteWatchLoopSleepsUntilItsNextDueRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var due atomic.Int64
		due.Store(time.Now().Add(5 * time.Second).UnixMilli())
		passes, nudge, stop := runRemoteWatchLoop(t, func() store.ScheduledMoment {
			return store.ScheduledMoment{At: due.Load(), Set: true}
		})
		defer stop()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := passes.Load(); got != 1 {
			t.Fatalf("passes before the row was due = %d, want 1", got)
		}
		due.Store(time.Now().Add(time.Hour).UnixMilli())
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := passes.Load(); got != 2 {
			t.Fatalf("passes once the row came due = %d, want 2", got)
		}
		nudge <- struct{}{}
		synctest.Wait()
		if got := passes.Load(); got != 3 {
			t.Fatalf("passes after a nudge = %d, want 3", got)
		}
	})
}

// TestRemoteWatchLoopDoesNotSpinOnARowLeftDue: a row a pass could not
// reschedule stays due, and is checked again at the floor rather than in a
// loop.
func TestRemoteWatchLoopDoesNotSpinOnARowLeftDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		passes, _, stop := runRemoteWatchLoop(t, func() store.ScheduledMoment {
			return store.ScheduledMoment{At: time.Now().Add(-time.Second).UnixMilli(), Set: true}
		})
		defer stop()
		time.Sleep(10*time.Second - time.Millisecond)
		synctest.Wait()
		if got, want := passes.Load(), int64(10*time.Second/remoteWatchFloor); got != want {
			t.Fatalf("passes over ten seconds with a row left due = %d, want %d", got, want)
		}
	})
}
