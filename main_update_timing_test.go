//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/supervise"
)

var phaseLineRE = regexp.MustCompile(`: phase=(\S+) duration=(\S+)`)

// phaseCounts counts the phase lines in text by phase, and those that
// failed, failing the test on a duration that does not parse.
func phaseCounts(t *testing.T, text string) (timed, failed map[string]int) {
	t.Helper()
	timed, failed = map[string]int{}, map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		m := phaseLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, err := time.ParseDuration(m[2]); err != nil {
			t.Errorf("phase line %q: %v", line, err)
		}
		timed[m[1]]++
		if strings.Contains(line, " failed: ") {
			failed[m[1]]++
		}
	}
	return timed, failed
}

func wantPhasesOnce(t *testing.T, text string, names ...string) {
	t.Helper()
	timed, _ := phaseCounts(t, text)
	for _, name := range names {
		if timed[name] != 1 {
			t.Errorf("phase %s logged %d times, want once\nlog:\n%s", name, timed[name], text)
		}
	}
}

// The WSL commands as the launcher runs them: each logs its phases on its
// stderr, which the launcher copies into its log, and so does the trial
// they start, including its shutdown.
func TestUpdateCommandsLogTheirPhasesThroughMain(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	dataRoot := t.TempDir()
	dir := filepath.Join(dataRoot, appdirs.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-overflow.db"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() {
		t.Helper()
		run := runUpdateCommandProcess(t, "", supervise.UpdateSnapshotCommand, "--id", "u1", "--data-dir", dataRoot)
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeOK {
			t.Fatalf("snapshot = %+v\n%s", result, run.stderr)
		}
		// The stub database is not SQLite, so the schema read fails and
		// says so with its duration.
		wantPhasesOnce(t, run.stderr, "command", "snapshot.lock", "snapshot.prepare", "snapshot.schema",
			"snapshot.clear", "snapshot.space", "snapshot.copy", "snapshot.sync")
		if _, failed := phaseCounts(t, run.stderr); failed["snapshot.schema"] != 1 || failed["command"] != 0 {
			t.Fatalf("failed phases = %v\n%s", failed, run.stderr)
		}
	}

	snapshot()
	run := runUpdateCommandProcess(t, "prepare", supervise.UpdateTrialRunCommand,
		"--id", "u1", "--to", "2.0.0", "--attempt", "1", "--data-dir", dataRoot)
	if result := run.result(t); result.Outcome != supervise.UpdateOutcomePrepared {
		t.Fatalf("trial run = %+v\n%s", result, run.stderr)
	}
	wantPhasesOnce(t, run.stderr, "command", "trial.lock", "trial.prepare", "trial.check",
		"trial.start", "trial.boot", "trial.stop", "shutdown")
	if !strings.Contains(run.stderr, "update trial: phase=shutdown duration=") {
		t.Fatalf("the trial did not time its own shutdown:\n%s", run.stderr)
	}

	snapshot()
	run = runUpdateCommandProcess(t, "fail", supervise.UpdateTrialRunCommand,
		"--id", "u1", "--to", "2.0.0", "--attempt", "1", "--data-dir", dataRoot)
	if result := run.result(t); result.Outcome != supervise.UpdateOutcomeRolledBack {
		t.Fatalf("trial run = %+v\n%s", result, run.stderr)
	}
	// A trial that reports its failure is stopped, then restored.
	wantPhasesOnce(t, run.stderr, "command", "trial.start", "trial.boot", "trial.stop", "shutdown",
		"restore.remove", "restore.copy", "restore.sync")
	if _, failed := phaseCounts(t, run.stderr); failed["trial.boot"] != 1 || failed["command"] != 1 || len(failed) != 2 {
		t.Fatalf("failed %v; want the boot and the command\n%s", failed, run.stderr)
	}
}

// The desktop helper runs every step in its own process: its lock wait and
// the update's summary are in its log beside the steps' phases.
func TestDesktopHelperLogsItsPhases(t *testing.T) {
	const id = "0123456789abcdef"
	r := newDesktopApplyRig(t)
	r.desktopUpdateFixture(id, 0)
	var mu sync.Mutex
	var logged []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	applier := r.applier(desktopApplyFlags{id: id, relaunch: []string{"--data-dir", r.root}}, "prepare",
		func(u *supervise.DesktopUpdate) { u.Logf = logf })
	applier.logf = logf
	applier.run(t.Context())

	mu.Lock()
	text := strings.Join(logged, "\n")
	mu.Unlock()
	wantPhasesOnce(t, text, "lock", "snapshot.copy", "trial.start", "trial.boot", "trial.stop", "settle", "publish")
	if !strings.Contains(text, "updater: update "+id+": phase=lock duration=") {
		t.Fatalf("the helper's lock wait is not logged as its own:\n%s", text)
	}
	summary := regexp.MustCompile(`updater: update `+id+`: summary total=\S+ phases=(\S+) outcome=committed`).FindAllStringSubmatch(text, -1)
	if len(summary) != 1 || !regexp.MustCompile(`^snapshot:[^,]+,record-attempt:[^,]+,trial\.1:[^,]+,settle:[^,]+,publish:[^,]+$`).MatchString(summary[0][1]) {
		t.Fatalf("summary = %q\nlog:\n%s", summary, text)
	}
}

// The new version's boot that finishes an update says how long it took
// from its process's start; a boot that finishes none says nothing.
func TestUpdateFirstBootLogsItsDuration(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	logUpdateFirstBoot(logf, "")
	if len(logged) != 0 {
		t.Fatalf("a boot that finishes no update logged %q", logged)
	}
	logUpdateFirstBoot(logf, "2.0.0")
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "updater: update to 2.0.0: phase=first-boot duration=") {
		t.Fatalf("logged %q", logged)
	}
	wantPhasesOnce(t, logged[0], "first-boot")
}

// The relaunched app waits for the helper that started it; the wait is
// timed, and one that runs out says so.
func TestWaitForUpdateHelperLogsTheWait(t *testing.T) {
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	helper, err := supervise.ProcessRefOf(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	waitForUpdateHelper(helper, 100*time.Millisecond, logf)
	if timed, failed := phaseCounts(t, strings.Join(logged, "\n")); timed["wait-helper"] != 1 || failed["wait-helper"] != 1 {
		t.Fatalf("logged %q; want the wait logged once as failed", logged)
	}
	if err := sleeper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = sleeper.Wait()
	logged = nil
	waitForUpdateHelper(helper, 10*time.Second, logf)
	if timed, failed := phaseCounts(t, strings.Join(logged, "\n")); timed["wait-helper"] != 1 || failed["wait-helper"] != 0 {
		t.Fatalf("logged %q; want the wait logged once", logged)
	}
}
