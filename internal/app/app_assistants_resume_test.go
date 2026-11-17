package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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

// livePIDs lists the process ids recorded in path that are still alive.
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
		if syscall.Kill(pid, 0) == nil && !zombie(pid) {
			live = append(live, pid)
		}
	}
	return live
}

// zombie reports whether pid has exited and waits to be reaped.
func zombie(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}
