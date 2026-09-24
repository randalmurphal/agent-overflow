//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/supervise"
)

// The update commands are tested as the launcher runs them: this test binary
// re-executed through main() with the command's argv, reports read from its
// stdout. The trial the trial command starts is this binary again, answered
// by TestMain with the real trial protocol around a stub boot. The real
// App boot is not run here: it would reach provider CLIs and the network
// through production wiring that only the harness isolates.

const (
	updateCommandArgsEnv = "AO_TEST_UPDATE_COMMAND_ARGS"
	updateTrialStubEnv   = "AO_TEST_UPDATE_TRIAL_STUB"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == supervise.UpdateTrialCommand && os.Getenv(updateTrialStubEnv) != "" {
		os.Exit(runTrialStub(os.Getenv(updateTrialStubEnv), os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runTrialStub is the trial's real protocol around a boot that reports one
// migration step, writes the database, and prepares or fails.
func runTrialStub(mode string, args []string) int {
	set := flag.NewFlagSet("stub", flag.ContinueOnError)
	dataDir := set.String("data-dir", "", "")
	if err := set.Parse(args); err != nil {
		return 2
	}
	root := filepath.Join(*dataDir, appdirs.DirName)
	return runTrialProtocol(func(observe func(startupprogress.Progress)) trialBackend {
		return &stubTrialBackend{mode: mode, root: root, observe: observe}
	})
}

type stubTrialBackend struct {
	mode    string
	root    string
	observe func(startupprogress.Progress)
}

func (b *stubTrialBackend) start(ctx context.Context) error {
	b.observe(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 1 of 1", UpdatedAt: 1, AliveAt: 1})
	// Progress relays keep only the newest report, so the step is held long
	// enough to be delivered before the next report supersedes it.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(b.root, "agent-overflow.db"), []byte("trial"), 0o600); err != nil {
		return err
	}
	if b.mode == "fail" {
		return errors.New("database schema 90 is newer than this build knows (88)")
	}
	return nil
}

func (b *stubTrialBackend) shutdown() {
	_ = os.WriteFile(filepath.Join(b.root, "stub-stopped"), []byte("stopped"), 0o600)
}

// TestUpdateCommandProcess is the re-executed command. It does nothing in an
// ordinary run.
func TestUpdateCommandProcess(t *testing.T) {
	raw := os.Getenv(updateCommandArgsEnv)
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		os.Exit(98)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	main()
	os.Exit(99)
}

type updateRun struct {
	events []supervise.UpdateEvent
	code   int
	stderr string
}

func (r updateRun) result(t *testing.T) supervise.UpdateEvent {
	t.Helper()
	if len(r.events) == 0 || r.events[len(r.events)-1].Type != supervise.UpdateEventResult {
		t.Fatalf("no result report: %+v\nstderr:\n%s", r.events, r.stderr)
	}
	return r.events[len(r.events)-1]
}

func (r updateRun) details() string {
	var details []string
	for _, event := range r.events {
		if event.Progress != nil {
			details = append(details, event.Progress.Detail)
		}
	}
	return strings.Join(details, "\n")
}

func runUpdateCommandProcess(t *testing.T, stub string, args ...string) updateRun {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestUpdateCommandProcess$")
	cmd.Env = append(os.Environ(), updateCommandArgsEnv+"="+string(encoded))
	if stub != "" {
		cmd.Env = append(cmd.Env, updateTrialStubEnv+"="+stub)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	run := updateRun{stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		run.code = exit.ExitCode()
	default:
		t.Fatalf("run %v: %v", args, err)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		event, ok, parseErr := supervise.ParseUpdateEvent(line)
		if parseErr != nil {
			t.Fatalf("stdout line %q: %v", line, parseErr)
		}
		if ok {
			run.events = append(run.events, event)
		} else if strings.TrimSpace(line) != "" {
			t.Fatalf("stdout carried a line that is not a report: %q", line)
		}
	}
	if len(run.events) > 0 && (run.events[0].Type != supervise.UpdateEventStarted || run.events[0].PID <= 0) {
		t.Fatalf("the first report is not the start: %+v", run.events[0])
	}
	return run
}

func TestUpdateCommandsRunThroughMain(t *testing.T) {
	kerneltest.IsolateSpawns(t)
	dataRoot := t.TempDir()
	dir := filepath.Join(dataRoot, appdirs.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "agent-overflow.db")
	writeDB := func(contents string) {
		t.Helper()
		if err := os.WriteFile(db, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readDB := func() string {
		t.Helper()
		data, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	writeDB("live")
	snapshot := func() {
		t.Helper()
		run := runUpdateCommandProcess(t, "", supervise.UpdateSnapshotCommand, "--id", "u1", "--data-dir", dataRoot)
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeOK || run.code != 0 {
			t.Fatalf("snapshot = %+v exit %d\n%s", result, run.code, run.stderr)
		}
	}

	t.Run("a failed trial is restored", func(t *testing.T) {
		snapshot()
		run := runUpdateCommandProcess(t, "fail", supervise.UpdateTrialRunCommand,
			"--id", "u1", "--to", "2.0.0", "--attempt", "1", "--data-dir", dataRoot)
		result := run.result(t)
		if result.Outcome != supervise.UpdateOutcomeRolledBack || run.code != 1 ||
			result.Reason != "database schema 90 is newer than this build knows (88)" {
			t.Fatalf("trial run = %+v exit %d\n%s", result, run.code, run.stderr)
		}
		if got := readDB(); got != "live" {
			t.Fatalf("database = %q, want the snapshot restored", got)
		}
	})

	t.Run("a prepared trial is stopped and kept", func(t *testing.T) {
		snapshot()
		run := runUpdateCommandProcess(t, "prepare", supervise.UpdateTrialRunCommand,
			"--id", "u1", "--to", "2.0.0", "--attempt", "1", "--data-dir", dataRoot)
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomePrepared || run.code != 0 {
			t.Fatalf("trial run = %+v exit %d\n%s", result, run.code, run.stderr)
		}
		if got := readDB(); got != "trial" {
			t.Fatalf("database = %q, want the trial's", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "stub-stopped")); err != nil {
			t.Fatalf("the trial was not shut down in order: %v", err)
		}
		if !strings.HasSuffix(run.details(), "Stopping the trial") {
			t.Fatalf("progress = %q", run.details())
		}
	})

	t.Run("restore and discard", func(t *testing.T) {
		run := runUpdateCommandProcess(t, "", supervise.UpdateRestoreCommand, "--id", "u1", "--data-dir", dataRoot, "--reason", "test")
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeOK {
			t.Fatalf("restore = %+v\n%s", result, run.stderr)
		}
		if got := readDB(); got != "live" {
			t.Fatalf("database = %q after restore", got)
		}
		run = runUpdateCommandProcess(t, "", supervise.UpdateDiscardCommand, "--id", "u1", "--data-dir", dataRoot)
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeOK {
			t.Fatalf("discard = %+v\n%s", result, run.stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "runtime", "app-update", "snapshot")); !os.IsNotExist(err) {
			t.Fatalf("the snapshot survived its discard: %v", err)
		}
	})

	t.Run("the snapshot waits for the previous backend", func(t *testing.T) {
		held, err := acquireBackendInstanceLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		released := make(chan struct{})
		go func() {
			time.Sleep(500 * time.Millisecond)
			held.file.Close()
			close(released)
		}()
		snapshot()
		select {
		case <-released:
		default:
			t.Fatal("the snapshot ran while another backend held the data root")
		}
	})

	t.Run("the space check runs beside the running backend", func(t *testing.T) {
		held, err := acquireBackendInstanceLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.file.Close()
		run := runUpdateCommandProcess(t, "", supervise.UpdateSpaceCommand, "--id", "u1", "--data-dir", dataRoot)
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeOK || run.code != 0 {
			t.Fatalf("space = %+v exit %d\n%s", result, run.code, run.stderr)
		}
		run = runUpdateCommandProcess(t, "", supervise.UpdateSpaceCommand, "--id", "u1", "--data-dir", dataRoot, "--host-free", "1")
		if result := run.result(t); result.Outcome != supervise.UpdateOutcomeRefused || run.code != 1 ||
			!strings.Contains(result.Reason, "Free at least") {
			t.Fatalf("space on a full host drive = %+v exit %d\n%s", result, run.code, run.stderr)
		}
	})

	t.Run("bad argv", func(t *testing.T) {
		run := runUpdateCommandProcess(t, "", supervise.UpdateSnapshotCommand, "--data-dir", dataRoot)
		if run.code != 2 || len(run.events) != 0 {
			t.Fatalf("missing --id = exit %d, %+v", run.code, run.events)
		}
	})
}

func TestParseUpdateCommandFlags(t *testing.T) {
	flags, err := parseUpdateCommandFlags(supervise.UpdateSnapshotCommand, []string{"--id", "u1", "--host-free", "1024"})
	if err != nil || flags.id != "u1" || flags.hostFree == nil || *flags.hostFree != 1024 {
		t.Fatalf("snapshot flags = %+v, %v", flags, err)
	}
	if flags, err := parseUpdateCommandFlags(supervise.UpdateSnapshotCommand, []string{"--id", "u1"}); err != nil || flags.hostFree != nil {
		t.Fatalf("no host bound = %+v, %v", flags, err)
	}
	for _, bad := range [][]string{
		{"--id", ""},
		{"--id", "u1", "extra"},
		{"--nope"},
	} {
		if _, err := parseUpdateCommandFlags(supervise.UpdateSnapshotCommand, bad); err == nil {
			t.Errorf("snapshot accepted %q", bad)
		}
	}
	if _, err := parseUpdateCommandFlags(supervise.UpdateTrialRunCommand, []string{"--id", "u1", "--to", "../x", "--attempt", "1"}); err == nil {
		t.Error("trial run accepted a version that is a path")
	}
	if _, err := parseUpdateCommandFlags(supervise.UpdateTrialRunCommand, []string{"--id", "u1", "--to", "2.0.0"}); err == nil {
		t.Error("trial run accepted no attempt")
	}
	if _, err := parseUpdateCommandFlags(supervise.UpdateTrialCommand, nil); err != nil {
		t.Errorf("the trial needs no id: %v", err)
	}
}

func TestWaitForBackendInstanceLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent-overflow")
	held, err := acquireBackendInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitForBackendInstanceLock(context.Background(), root, 300*time.Millisecond); err == nil {
		t.Fatal("the lock was taken while held")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitForBackendInstanceLock(ctx, root, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait = %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		held.file.Close()
	}()
	next, err := waitForBackendInstanceLock(context.Background(), root, 10*time.Second)
	if err != nil {
		t.Fatalf("the wait did not take a released lock: %v", err)
	}
	next.releaseForTest(t)
}
