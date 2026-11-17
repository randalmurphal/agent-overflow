package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/notify"
	"agent-overflow/internal/store"
)

// A watch whose script keeps failing tells the person once per failure
// episode (docs/specs/standing-agents.md, "Wake supervisor").

const watchFailureBackend = "backend-under-test"

// watchFailureApp is an assistant's thread named "Mail agent" whose
// notifications are recorded, with no provider behind it.
func watchFailureApp(t *testing.T, a *App, id string) (store.Thread, *recordingNotificationSender) {
	t.Helper()
	stubNoticeStarts(a)
	recorder := &recordingNotificationSender{}
	a.osNotifications = recorder
	a.storeIdentity.Store(&store.Identity{BackendID: watchFailureBackend})
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	thread.Title = "Mail agent"
	if err := a.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	testAssistant(t, a, thread.ID)
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	return thread, recorder
}

// watchSends is what the recorder was sent about watches once the
// notification queue is idle. A caller polling while the supervisor may still
// queue one reads sentWatchNotices instead.
func watchSends(a *App, recorder *recordingNotificationSender) []notify.Send {
	a.notifications.queue.Wait()
	return sentWatchNotices(recorder)
}

func sentWatchNotices(recorder *recordingNotificationSender) []notify.Send {
	var out []notify.Send
	for _, send := range recorder.snapshot() {
		if strings.HasPrefix(send.ID, "watch:") {
			send.Silent = false
			out = append(out, send)
		}
	}
	return out
}

func watchRaised(row store.AgentWake, phrase string) notify.Send {
	return notify.Send{
		ID:     "watch:" + row.ID,
		Kind:   notify.KindError,
		Title:  "Mail agent",
		Body:   `The watch "` + row.Label + `" is failing: ` + phrase + ". Open the thread to see why.",
		Target: notify.Target{Kind: notify.TargetThread, ThreadID: row.ThreadID, BackendID: watchFailureBackend},
	}
}

func watchWithdrawn(row store.AgentWake) notify.Send {
	return notify.Send{ID: "watch:" + row.ID, Kind: notify.KindError, Retract: true}
}

func expectWatchSends(t *testing.T, a *App, recorder *recordingNotificationSender, want ...notify.Send) {
	t.Helper()
	got := watchSends(a, recorder)
	if len(got) != len(want) {
		t.Fatalf("watch notifications %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("watch notification %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func watchEpisodeOpen(a *App, wakeID string) bool {
	a.agentWakes.failures.mu.Lock()
	defer a.agentWakes.failures.mu.Unlock()
	_, open := a.agentWakes.failures.episodes[wakeID]
	return open
}

// runWatchOnce runs one interval run of the watch to its end, as its fire
// does.
func runWatchOnce(t *testing.T, a *App, wakeID string) {
	t.Helper()
	now := time.Now()
	if err := a.fireAgentWake(agentWake(t, a, wakeID), now, agentWakeFire{at: now.UnixMilli()}); err != nil {
		t.Fatalf("fire %s: %v", wakeID, err)
	}
	awaitNoAgentWakeRun(t, a, wakeID)
}

func failingWatch(t *testing.T, a *App, thread store.Thread, label string) (store.AgentWake, string) {
	t.Helper()
	dir := t.TempDir()
	row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: label,
		Command: `[ -f ok ] || { echo "token expired" >&2; exit 3; }`, Cwd: dir,
		IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	return row, dir
}

func mend(t *testing.T, dir string, ok bool) {
	t.Helper()
	path := filepath.Join(dir, "ok")
	if !ok {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The episode rule: a run that fails starts an episode; one that fails at
// least the window after its first failure raises the notice, once; the
// end of the episode withdraws it, once; a later episode notifies again.
// A process that could not start notifies at once.
func TestAWatchNotifiesOncePerFailureEpisode(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread, recorder := watchFailureApp(t, a, "watch-episode")
	row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "inbox",
		Command: "exit 3", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	exit := notify.WatchFailure{Cause: notify.WatchExitStatus, ExitCode: 3}
	t0 := time.Now()

	a.noteAgentWatchFailed(row, exit, t0)
	a.noteAgentWatchFailed(row, exit, t0.Add(4*time.Minute))
	expectWatchSends(t, a, recorder)
	a.noteAgentWatchFailed(row, exit, t0.Add(agentWatchFailureWindow+time.Second))
	raised := watchRaised(row, "exit status 3")
	expectWatchSends(t, a, recorder, raised)
	a.noteAgentWatchFailed(row, exit, t0.Add(6*time.Minute))
	a.noteAgentWatchFailed(row, exit, t0.Add(time.Hour))
	expectWatchSends(t, a, recorder, raised)

	a.endAgentWatchFailure(row.ID)
	a.endAgentWatchFailure(row.ID)
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row))

	t1 := t0.Add(2 * time.Hour)
	a.noteAgentWatchFailed(row, exit, t1)
	a.noteAgentWatchFailed(row, exit, t1.Add(agentWatchFailureWindow))
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row), raised)
}

// A run that fails once and is followed by a clean one never notifies: the
// clean run ended the episode, and the next failure starts a new one.
func TestAWatchThatFailsOnceThenRunsCleanDoesNotNotify(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread, recorder := watchFailureApp(t, a, "watch-once")
	row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "inbox",
		Command: "exit 3", IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	exit := notify.WatchFailure{Cause: notify.WatchExitStatus, ExitCode: 3}
	t0 := time.Now()
	a.noteAgentWatchFailed(row, exit, t0)
	a.endAgentWatchFailure(row.ID)
	a.noteAgentWatchFailed(row, exit, t0.Add(agentWatchFailureWindow+time.Minute))
	expectWatchSends(t, a, recorder)
	if marks, err := a.store.AgentWatchFailureNotices(); err != nil || len(marks) != 0 {
		t.Fatalf("marks %v, %v; want none", marks, err)
	}
}

// A watch whose process cannot start notifies at once, and only once.
func TestAWatchThatCannotStartNotifiesAtOnce(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread, recorder := watchFailureApp(t, a, "watch-not-started")
	row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "inbox",
		Command: "true", Cwd: filepath.Join(t.TempDir(), "gone"), IntervalSeconds: 3600, TimeoutSeconds: 60,
		NextAt: time.Now().Add(time.Hour).UnixMilli()})
	runWatchOnce(t, a, row.ID)
	raised := watchRaised(row, "could not start")
	expectWatchSends(t, a, recorder, raised)
	runWatchOnce(t, a, row.ID)
	expectWatchSends(t, a, recorder, raised)
	if marks, err := a.store.AgentWatchFailureNotices(); err != nil || len(marks) != 1 {
		t.Fatalf("marks %v, %v; want the raised one", marks, err)
	}
}

// An interval watch's runs, end to end: two failing runs past the window
// notify once with the exit status, a clean run withdraws it, and the next
// failing episode notifies again.
func TestAnIntervalWatchThatKeepsFailingNotifiesUntilACleanRun(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.agentWakes.failures.windowOverride = time.Nanosecond
	thread, recorder := watchFailureApp(t, a, "watch-interval")
	row, dir := failingWatch(t, a, thread, "inbox")

	runWatchOnce(t, a, row.ID)
	expectWatchSends(t, a, recorder)
	runWatchOnce(t, a, row.ID)
	raised := watchRaised(row, "exit status 3")
	expectWatchSends(t, a, recorder, raised)
	runWatchOnce(t, a, row.ID)
	expectWatchSends(t, a, recorder, raised)
	if got := agentWake(t, a, row.ID).LastOutcome; got != "error: exit status 3; stderr: token expired" {
		t.Fatalf("outcome %q: the row keeps the detail", got)
	}

	mend(t, dir, true)
	runWatchOnce(t, a, row.ID)
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row))
	if marks, err := a.store.AgentWatchFailureNotices(); err != nil || len(marks) != 0 {
		t.Fatalf("marks %v, %v; want none after the clean run", marks, err)
	}

	mend(t, dir, false)
	runWatchOnce(t, a, row.ID)
	runWatchOnce(t, a, row.ID)
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row), raised)
}

// A long-running watch in a restart loop notifies once, and the first line
// it prints ends the episode. A process that cannot start notifies at once.
func TestALongRunningWatchInARestartLoopNotifiesUntilItPrints(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.agentWakes.failures.windowOverride = time.Nanosecond
	thread, recorder := watchFailureApp(t, a, "watch-long")
	dir := t.TempDir()
	row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "events",
		Command: `[ -f ok ] && { echo up; exec sleep 3600; }; exit 1`, Cwd: dir, LongRunning: true})
	raised := watchRaised(row, "exit status 1")
	waitUntil(t, 15*time.Second, func() bool { return len(sentWatchNotices(recorder)) > 0 })
	expectWatchSends(t, a, recorder, raised)

	mend(t, dir, true)
	waitUntil(t, 15*time.Second, func() bool { return len(sentWatchNotices(recorder)) > 1 })
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row))
	if watchEpisodeOpen(a, row.ID) {
		t.Fatal("the line did not end the episode")
	}

	gone := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "feed",
		Command: "exec sleep 3600", Cwd: filepath.Join(dir, "gone"), LongRunning: true})
	waitUntil(t, 15*time.Second, func() bool { return len(sentWatchNotices(recorder)) > 2 })
	expectWatchSends(t, a, recorder, raised, watchWithdrawn(row), watchRaised(gone, "could not start"))
}

// A watch stopped by a cancel, a replacement, an archive or assistants
// being turned off has not failed, and the notice of its open episode is
// withdrawn.
func TestAStoppedWatchHasNotFailedAndItsNoticeIsWithdrawn(t *testing.T) {
	t.Parallel()
	notStarted := notify.WatchFailure{Cause: notify.WatchNotStarted}
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, a *App, row store.AgentWake)
	}{
		{"cancelled", func(t *testing.T, a *App, row store.AgentWake) {
			if _, cancelled, err := a.cancelAgentWake(row.ThreadID, row.Label, store.AgentWakeWatch); err != nil || !cancelled {
				t.Fatalf("cancel: %v %v", cancelled, err)
			}
		}},
		{"replaced", func(t *testing.T, a *App, row store.AgentWake) {
			row.Command = "exec sleep 3601"
			registerTestWake(t, a, row)
		}},
		{"archived", func(t *testing.T, a *App, row store.AgentWake) {
			work, err := a.archiveThreadLocked(row.ThreadID, "")
			if err != nil {
				t.Fatalf("archiveThreadLocked: %v", err)
			}
			if err := a.stopRestingThreadWork(work); err != nil {
				t.Fatalf("stopRestingThreadWork: %v", err)
			}
		}},
		{"assistants off", func(t *testing.T, a *App, row store.AgentWake) {
			a.registerSettingsBroadcast()
			setAssistants(t, a, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppForFlushQueueRPC(t)
			thread, recorder := watchFailureApp(t, a, "watch-stopped-"+strings.ReplaceAll(tc.name, " ", "-"))
			row := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "events",
				Command: "exec sleep 3600", LongRunning: true})
			waitUntil(t, 10*time.Second, func() bool { return len(a.agentWatchDirs(thread.ID)) == 1 })
			a.noteAgentWatchFailed(row, notStarted, time.Now())
			raised := watchRaised(row, "could not start")
			expectWatchSends(t, a, recorder, raised)

			a.agentWakes.mu.Lock()
			run := a.agentWakes.runs[row.ID]
			a.agentWakes.mu.Unlock()
			tc.stop(t, a, row)
			within(t, "the stopped process", run.done)

			expectWatchSends(t, a, recorder, raised, watchWithdrawn(row))
			if watchEpisodeOpen(a, row.ID) {
				t.Fatal("the stop was counted as a failure")
			}
			if marks, err := a.store.AgentWatchFailureNotices(); err != nil || len(marks) != 0 {
				t.Fatalf("marks %v, %v; want none", marks, err)
			}
		})
	}
}

// A notice raised before the app quit outlives the quit, which is not a
// failure: the next boot keeps the episode, a failing run after it does not
// notify again, and the clean run that ends it withdraws the notice, once.
func TestAWatchFailureNoticeRaisedBeforeARestartIsWithdrawnAfterIt(t *testing.T) {
	t.Parallel()
	_, dbPath := newTestAppWithStorePath(t)
	boot := func() (*App, *recordingNotificationSender) {
		t.Helper()
		a := newTestAppAtStorePath(t, dbPath)
		recorder := &recordingNotificationSender{}
		a.osNotifications = recorder
		a.storeIdentity.Store(&store.Identity{BackendID: watchFailureBackend})
		stubNoticeStarts(a)
		a.agentWakes.failures.windowOverride = time.Nanosecond
		a.recoverAgentWakesAtBoot()
		return a, recorder
	}

	before, raisedRec := boot()
	thread := testThread("watch-restart")
	thread.WorkspacePath = t.TempDir()
	thread.Title = "Mail agent"
	if err := before.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	testAssistant(t, before, thread.ID)
	row, dir := failingWatch(t, before, thread, "inbox")
	tail := registerTestWake(t, before, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail",
		Command: "exec sleep 3600", LongRunning: true})
	waitUntil(t, 10*time.Second, func() bool { return len(before.agentWatchDirs(thread.ID)) == 1 })
	runWatchOnce(t, before, row.ID)
	runWatchOnce(t, before, row.ID)
	raised := watchRaised(row, "exit status 3")
	expectWatchSends(t, before, raisedRec, raised)
	// The quit stops the long-running process: no failure of it.
	before.appCancel()
	before.threadRequestsWG.Wait()
	expectWatchSends(t, before, raisedRec, raised)
	if watchEpisodeOpen(before, tail.ID) {
		t.Fatal("the quit was counted as a failure")
	}

	after, withdrawnRec := boot()
	t.Cleanup(func() { after.stopAgentWakeRunsForThread(thread.ID) })
	runWatchOnce(t, after, row.ID)
	expectWatchSends(t, after, withdrawnRec)
	mend(t, dir, true)
	runWatchOnce(t, after, row.ID)
	expectWatchSends(t, after, withdrawnRec, watchWithdrawn(row))

	again, quiet := boot()
	t.Cleanup(func() { again.stopAgentWakeRunsForThread(thread.ID) })
	runWatchOnce(t, again, row.ID)
	expectWatchSends(t, again, quiet)
}
