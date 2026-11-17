//go:build !windows

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/store"
)

// Turning assistants on: orphan recovery completes before the sweep may
// serve their rows, a live run is never taken for an orphan, and a recovery
// that did not apply reruns nothing.

// gatedMarkerWatch is markerWatch whose run, after counting itself, waits
// for release to exist.
func gatedMarkerWatch(t *testing.T) (command, marker, release string) {
	t.Helper()
	dir := t.TempDir()
	marker, release = filepath.Join(dir, "runs"), filepath.Join(dir, "release")
	return "echo run >> '" + marker + "'; while [ ! -e '" + release + "' ]; do sleep 0.02; done", marker, release
}

func releaseWatch(t *testing.T, release string) {
	t.Helper()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// onResumeRead installs hook as what turning assistants on runs around its
// read of their loops and watches.
func onResumeRead(a *App, hook func(read bool)) {
	a.assistantsSwitch.mu.Lock()
	a.assistantsSwitch.resumeReadForTest = hook
	a.assistantsSwitch.mu.Unlock()
}

// The sweep passes as assistants come on, before resume reads their rows,
// and the watch it could start finishes before recovery proceeds: the
// overdue watch runs once, not once for the sweep and again as a recovered
// orphan.
func TestTurningAssistantsOnRunsAnOverdueWatchOnce(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-once")
	testAssistant(t, a, thread.ID)
	command, marker, release := gatedMarkerWatch(t)
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: time.Now().Add(-time.Minute).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	setAssistants(t, a, false)

	onResumeRead(a, func(read bool) {
		if !read {
			// Nothing is admitted until recovery is done.
			a.fireDueAgentWakes(time.Now())
			if agentWakeRunning(a, watch.ID) {
				t.Error("the sweep started the watch before resume recovered the rows")
			}
			return
		}
		releaseWatch(t, release)
		waitUntil(t, 10*time.Second, func() bool { return !agentWakeRunning(a, watch.ID) })
	})
	setAssistants(t, a, true)
	a.fireDueAgentWakes(time.Now())
	awaitNoAgentWakeRun(t, a, watch.ID)

	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("the overdue watch ran %d times, want once", runs)
	}
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed || row.LastOutcome == agentWakeOutcomeCutByQuit {
		t.Fatalf("the watch after assistants came on = %+v", row)
	}
}

// A run the supervisor has registered when assistants come on is live, not
// one a quit cut: resume leaves its row alone and runs nothing more.
func TestTurningAssistantsOnLeavesALiveRunAlone(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-live")
	testAssistant(t, a, thread.ID)
	command, marker, release := gatedMarkerWatch(t)
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	if err := a.fireAgentWake(watch, time.Now(), agentWakeFire{at: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("fire the watch: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return markerRuns(t, marker) == 1 })

	a.resumeAssistants()
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeRunning || row.LastOutcome == agentWakeOutcomeCutByQuit {
		t.Fatalf("resume rewrote a live run: %+v", row)
	}
	releaseWatch(t, release)
	awaitNoAgentWakeRun(t, a, watch.ID)
	a.fireDueAgentWakes(time.Now())
	awaitNoAgentWakeRun(t, a, watch.ID)
	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("the watch ran %d times, want once", runs)
	}
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed || row.LastOutcome != agentWakeOutcomeNoOutput {
		t.Fatalf("the watch after its run = %+v; want its own outcome", row)
	}
}

// A row a quit left running whose recovery does not apply, because it
// changed after resume read it, is not rerun.
func TestTurningAssistantsOnRerunsOnlyWhatItRecovered(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-applied")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	now := time.Now()
	watch := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: now.Add(time.Hour).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	setAssistants(t, a, false)
	// A run a quit cut while assistants were off.
	if _, claimed, err := a.store.ClaimAgentWake(watch, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	onResumeRead(a, func(read bool) {
		if !read {
			return
		}
		// The row ends before its recovery is written.
		if _, applied, err := a.store.FinishAgentWake(watch.ID, "finished elsewhere", "", 0, store.AgentWakeNoWake); err != nil || !applied {
			t.Errorf("finish the row: %v %v", applied, err)
		}
	})
	setAssistants(t, a, true)
	awaitNoAgentWakeRun(t, a, watch.ID)
	if runs := markerRuns(t, marker); runs != 0 {
		t.Fatalf("a row whose recovery did not apply ran %d times", runs)
	}
	if row := agentWake(t, a, watch.ID); row.State != store.AgentWakeArmed || row.LastOutcome != "finished elsewhere" {
		t.Fatalf("the row = %+v; want it as it ended", row)
	}
}

// Turned off again while turning on is recovering their rows: the pause is
// never released, so nothing of theirs starts, not even for a moment.
func TestAssistantsTurnedOffDuringResumeStartNothing(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-off-again")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	tail := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: command + "; exec sleep 60", LongRunning: true})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	waitUntil(t, 10*time.Second, func() bool { return markerRuns(t, marker) == 1 })
	setAssistants(t, a, false)
	awaitNoAgentWakeRun(t, a, tail.ID)

	var converged []bool
	a.assistantsSwitch.mu.Lock()
	a.assistantsSwitch.convergedForTest = func(paused bool) { converged = append(converged, paused) }
	a.assistantsSwitch.mu.Unlock()
	var resumed atomic.Bool
	onResumeRead(a, func(read bool) {
		if !read {
			switchAssistants(t, a, false)
			return
		}
		resumed.Store(true)
	})
	switchAssistants(t, a, true)
	waitUntil(t, 10*time.Second, resumed.Load)
	// The resume's convergence holds the switch's lock until it returns.
	a.assistantsSwitch.mu.Lock()
	defer a.assistantsSwitch.mu.Unlock()
	if len(converged) != 0 || !a.assistantsSwitch.paused || !a.assistantsPaused() {
		t.Fatalf("convergences %v, paused %v, pause %v; want it paused throughout", converged, a.assistantsSwitch.paused, a.assistantsPaused())
	}
	if runs := markerRuns(t, marker); runs != 1 || agentWakeRunning(a, tail.ID) {
		t.Fatalf("the paused watch's process started again (%d starts in all)", runs)
	}
}

// Off, on, off and on again in quick succession: the long-running watch
// ends with one process alive, the interval watch that came due runs once,
// and nothing is left paused or running.
func TestAssistantsToggledQuicklyLeaveNoDuplicateAndNothingStranded(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "toggle-quick")
	testAssistant(t, a, thread.ID)
	pids := filepath.Join(t.TempDir(), "pids")
	tail := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: "echo $$ >> '" + pids + "'; exec sleep 60", LongRunning: true})
	command, marker := markerWatch(t)
	poll := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: time.Now().Add(-time.Minute).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	waitUntil(t, 10*time.Second, func() bool { return agentWake(t, a, tail.ID).State == store.AgentWakeRunning })

	for _, on := range []bool{false, true, false, true} {
		switchAssistants(t, a, on)
		a.fireDueAgentWakes(time.Now())
	}
	awaitAssistants(t, a, true)
	waitUntil(t, 10*time.Second, func() bool { return !a.assistantsPaused() })
	a.fireDueAgentWakes(time.Now())
	awaitNoAgentWakeRun(t, a, poll.ID)

	if a.assistantsPaused() {
		t.Fatal("assistants are still paused")
	}
	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("the due watch ran %d times, want once", runs)
	}
	if row := agentWake(t, a, poll.ID); row.State != store.AgentWakeArmed {
		t.Fatalf("the due watch = %+v; want it armed", row)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return agentWakeRunning(a, tail.ID) && agentWake(t, a, tail.ID).State == store.AgentWakeRunning && len(livePIDs(t, pids)) == 1
	})
	if live := livePIDs(t, pids); len(live) != 1 || !agentWakeRunning(a, tail.ID) {
		t.Fatalf("the long-running watch has %d processes alive (supervised=%v), want one", len(live), agentWakeRunning(a, tail.ID))
	}
}

// livePIDs lists the process ids recorded in path that are still alive. A
// process is gone once signal 0 finds no such process or the platform's
// process table shows it exited (procutil.Exited); a signal that fails for
// another reason is an error, not an exit.
func livePIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var live []int
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("pid %q: %v", field, err)
		}
		switch err := syscall.Kill(pid, 0); {
		case errors.Is(err, syscall.ESRCH):
			continue
		case err != nil && !errors.Is(err, syscall.EPERM):
			t.Fatalf("signal 0 to %d: %v", pid, err)
		}
		if !procutil.Exited(pid) {
			live = append(live, pid)
		}
	}
	return live
}

// A long-running watch whose restart the pause refused is still registered
// when turning assistants on reads the rows. Resume either finds a
// supervisor that goes on or owns the replacement: once the refused one
// returns, exactly one process runs, supervised, with no further toggle.
func TestAWatchRefusedAsAssistantsComeBackOnKeepsOneProcess(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-refused-watch")
	testAssistant(t, a, thread.ID)
	dir := t.TempDir()
	pids, first := filepath.Join(dir, "pids"), filepath.Join(dir, "first")
	// The first process exits at once, so the supervisor restarts it.
	command := "if [ -e '" + first + "' ]; then echo $$ >> '" + pids + "'; exec sleep 60; fi; touch '" + first + "'"
	refused, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	a.agentWatchRefusedForTest = func(string) {
		if once.CompareAndSwap(false, true) {
			close(refused)
			<-release
		}
	}
	tail := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "tail", Command: command, LongRunning: true})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	waitUntil(t, 10*time.Second, func() bool {
		_, err := os.Stat(first)
		return err == nil
	})
	// Off and on again before an off convergence ran: the observer's pause
	// holds while the setting reads on, and the supervisor's restart meets it.
	a.store.PauseAssistants(true)
	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("the restart was not refused")
	}

	// The final on's convergence, while the refused supervisor has not
	// returned.
	a.convergeAssistants()
	close(release)

	waitUntil(t, 10*time.Second, func() bool { return len(livePIDs(t, pids)) == 1 && agentWakeRunning(a, tail.ID) })
	// Long enough for a second supervisor to start a second process.
	time.Sleep(2 * agentWakeRestartBase)
	if live := livePIDs(t, pids); len(live) != 1 || !agentWakeRunning(a, tail.ID) {
		t.Fatalf("the long-running watch has %d processes alive (supervised=%v), want one", len(live), agentWakeRunning(a, tail.ID))
	}
	if row := agentWake(t, a, tail.ID); row.State != store.AgentWakeRunning {
		t.Fatalf("the long-running watch = %+v; want it running", row)
	}
}

// Off, an on whose recovery is written and then turned off before the pause
// is released, off, and on again, with an interval watch a quit cut whose
// next fire is an hour away: the cut run is replayed once as soon as
// assistants are on, and only once.
func TestAnAbortedResumeKeepsTheReplayACutRunIsOwed(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "resume-aborted-replay")
	testAssistant(t, a, thread.ID)
	command, marker := markerWatch(t)
	now := time.Now()
	hour := now.Add(time.Hour).UnixMilli()
	poll := registerTestWake(t, a, store.AgentWake{ThreadID: thread.ID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: now.Add(-time.Minute).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(thread.ID) })
	setAssistants(t, a, false)
	// A run a quit cut while assistants were off: claimed, its next fire an
	// hour away, with no process.
	if _, claimed, err := a.store.ClaimAgentWake(poll, now.UnixMilli(), hour, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	var aborted atomic.Bool
	onResumeRead(a, func(read bool) {
		if read && aborted.CompareAndSwap(false, true) {
			switchAssistants(t, a, false)
		}
	})
	switchAssistants(t, a, true)
	waitUntil(t, 10*time.Second, aborted.Load)
	awaitAssistants(t, a, false)
	if row := agentWake(t, a, poll.ID); row.State != store.AgentWakeArmed || markerRuns(t, marker) != 0 {
		t.Fatalf("after the aborted resume: %+v, %d runs; want it recovered and not run", row, markerRuns(t, marker))
	}

	setAssistants(t, a, true)
	// The sweep the resume nudges.
	a.fireDueAgentWakes(time.Now())
	waitUntil(t, 10*time.Second, func() bool { return markerRuns(t, marker) == 1 })
	awaitNoAgentWakeRun(t, a, poll.ID)
	a.fireDueAgentWakes(time.Now())
	awaitNoAgentWakeRun(t, a, poll.ID)
	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("the cut run was replayed %d times, want once", runs)
	}
	if row := agentWake(t, a, poll.ID); row.State != store.AgentWakeArmed || row.NextAt <= time.Now().UnixMilli() {
		t.Fatalf("the watch after its replay = %+v; want it armed for its next fire", row)
	}
}

// cutWatch registers an interval watch whose run a quit cut: claimed, its
// next fire an hour away, with no process.
func cutWatch(t *testing.T, a *App, threadID string) (store.AgentWake, string) {
	t.Helper()
	command, marker := markerWatch(t)
	now := time.Now()
	poll := registerTestWake(t, a, store.AgentWake{ThreadID: threadID, Kind: store.AgentWakeWatch, Label: "poll", Command: command, IntervalSeconds: 3600, TimeoutSeconds: 60, NextAt: now.Add(-time.Minute).UnixMilli()})
	t.Cleanup(func() { a.stopAgentWakeRunsForThread(threadID) })
	if _, claimed, err := a.store.ClaimAgentWake(poll, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	return poll, marker
}

// awaitOneReplay waits for the cut watch's replay and checks that the sweep
// serves it no second time.
func awaitOneReplay(t *testing.T, a *App, poll store.AgentWake, marker string) store.AgentWake {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool { return markerRuns(t, marker) == 1 })
	awaitNoAgentWakeRun(t, a, poll.ID)
	a.fireDueAgentWakes(time.Now())
	awaitNoAgentWakeRun(t, a, poll.ID)
	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("the cut run was replayed %d times, want once", runs)
	}
	row := agentWake(t, a, poll.ID)
	if row.State != store.AgentWakeArmed || row.NextAt <= time.Now().UnixMilli() {
		t.Fatalf("the watch after its replay = %+v; want it armed for its next fire", row)
	}
	return row
}

// At boot, a run a quit cut within its interval is replayed at once, once,
// and stands for no missed fire.
func TestABootReplaysACutRunOnceCountingNothingMissed(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "boot-replay")
	testAssistant(t, a, thread.ID)
	poll, marker := cutWatch(t, a, thread.ID)

	a.recoverAgentWakesAtBoot()
	if row := awaitOneReplay(t, a, poll, marker); row.Missed != 0 {
		t.Fatalf("the replay counted %d missed fires, want none", row.Missed)
	}
}

// A boot that recovered a cut run and quit before replaying it leaves the
// replay owed: the next boot replays it once.
func TestABootThatQuitBeforeTheReplayLeavesItOwed(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	thread := noticeTestThread(t, a, "boot-replay-owed")
	testAssistant(t, a, thread.ID)
	poll, marker := cutWatch(t, a, thread.ID)
	// The first boot's recovery, then the quit.
	if _, applied, err := a.store.RecoverAgentWake(poll.ID, agentWakeOutcomeCutByQuit); err != nil || !applied {
		t.Fatalf("recover: %v %v", applied, err)
	}

	a.recoverAgentWakesAtBoot()
	awaitOneReplay(t, a, poll, marker)
}
