//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/workflow/engine"
)

// A tool phase's process tree must not survive its phase. These tests bind a
// command that backgrounds a child, so a kill that only reached the direct
// child would leave the grandchild running and fail here.

func TestWorkflowToolPhaseWatchdogKillsTheProcessGroup(t *testing.T) {
	fixture := newToolWorkflowFixture(t, toolLifecyclePhase)
	pidPath := filepath.Join(t.TempDir(), "pids")
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeSilentSleepScript(t, pidPath)},
	}, nil, "reliability:\n  watchdog: 100ms\n  backoff: [1ms]\n")
	item := fixture.start(t, "watchdog")

	// The command emits nothing, so the inactivity window elapses and the one
	// teardown path parks the run.
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonStalled)
	for _, pid := range waitForRecordedPIDs(t, pidPath) {
		waitForProcessDeath(t, pid)
	}
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if len(phases) != 1 || phases[0].Status != "parked" {
		t.Fatalf("phases = %+v", phases)
	}
	narrative := readFileForTest(t, phases[0].NarrativePath)
	if !strings.Contains(narrative, "killed after 100ms without output") {
		t.Fatalf("narrative did not record the watchdog kill:\n%s", narrative)
	}
}

func TestWorkflowToolPhaseCancelKillsTheProcessGroup(t *testing.T) {
	fixture := newToolWorkflowFixture(t, toolLifecyclePhase)
	pidPath := filepath.Join(t.TempDir(), "pids")
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeSilentSleepScript(t, pidPath)},
	}, nil, "reliability:\n  watchdog: 1h\n  backoff: [1ms]\n")
	item := fixture.start(t, "cancel")

	pids := waitForRecordedPIDs(t, pidPath)
	if err := fixture.app.WorkflowCancelItem(item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateCancelled, engine.ReasonInterrupted)
	for _, pid := range pids {
		waitForProcessDeath(t, pid)
	}
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	narrative := waitForNarrative(t, phases[0].NarrativePath)
	if !strings.Contains(narrative, "killed by workflow teardown") {
		t.Fatalf("narrative did not record the teardown kill:\n%s", narrative)
	}
}

const toolLifecyclePhase = `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`

// writeSilentSleepScript backgrounds a child, records both PIDs, and then
// blocks without ever writing to stdout or stderr.
func writeSilentSleepScript(t *testing.T, pidPath string) string {
	t.Helper()
	return writeExecutable(t, "sleep-quietly.sh", "#!/bin/sh\n"+
		"sleep 300 &\n"+
		"child=$!\n"+
		"printf '%d %d\\n' $$ $child > "+pidPath+"\n"+
		"wait $child\n")
}

func waitForRecordedPIDs(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			pids := make([]int, 0, len(fields))
			for _, field := range fields {
				pid, convErr := strconv.Atoi(field)
				if convErr != nil {
					t.Fatalf("pid file %q contains %q", path, data)
				}
				pids = append(pids, pid)
			}
			if len(pids) == 2 {
				return pids
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool phase command never recorded its process ids at %s", path)
	return nil
}

func waitForProcessDeath(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		// Signal 0 probes for existence. A reaped child of this test process
		// is gone entirely; an unrelated survivor answers.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d survived its workflow phase", pid)
}

// waitForNarrative tolerates the narrative being written by the reaping
// goroutine just after teardown transitioned the item.
func waitForNarrative(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tool phase narrative %s was never written", path)
	return ""
}
