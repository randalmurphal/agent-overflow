//go:build !windows

package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"
	"agent-overflow/internal/testutil/helperproc"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v4/process"
)

// A composer shell command ends with the backend: its group is recorded for
// the orphan reaper, whose sidecar kills the group when the backend dies and
// whose registry the next launch sweeps.

// waitShellOutput waits for the thread's running command to print marker.
func waitShellOutput(t *testing.T, app *App, threadID, marker string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs, err := app.GetThreadShellCommands(threadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && strings.Contains(runs[0].Output, marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never printed %q: %+v", marker, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitOrphanRecord waits for the registry to hold one record and returns it.
// The record follows the process's start, which its first output can beat.
func waitOrphanRecord(t *testing.T, registry *orphanreaper.Registry) orphanreaper.Record {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		records, err := registry.Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 {
			return records[0]
		}
		if len(records) > 1 || time.Now().After(deadline) {
			t.Fatalf("the registry holds %+v, want one command's group", records)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// groupRunning reports whether any process of group pgid can still run.
func groupRunning(pgid int) bool {
	return !errors.Is(procutil.SignalGroup(pgid, 0), os.ErrProcessDone)
}

func TestAShellCommandIsRecordedForTheOrphanReaperWhileItRuns(t *testing.T) {
	for _, stop := range []bool{false, true} {
		app, rec := newShellCommandApp(t)
		registry := orphanreaper.NewRegistry(filepath.Join(t.TempDir(), "orphan-registry.json"))
		app.orphanRegistry = registry
		thread, _ := newMixedTurnThread(t, app, fmt.Sprintf("thread-shell-orphan-%v", stop))
		runID := uuid.NewString()
		command := "echo started; sleep 60"
		if !stop {
			command = "echo started"
		}
		if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
			RunID: runID, Command: command,
		}); err != nil {
			t.Fatal(err)
		}
		if stop {
			waitShellOutput(t, app, thread.ID, "started")
			record := waitOrphanRecord(t, registry)
			if record.Owner != "shell command" || record.PGID != record.PID || record.ParentPID != os.Getpid() || !groupRunning(record.PGID) {
				t.Fatalf("record %+v, want the running group of run %s", record, runID)
			}
			if leader, err := orphanreaper.Identify(record.PID); err != nil || record.Identity == nil || *record.Identity != leader {
				t.Fatalf("record %+v carries no identity that names the leader %+v %v", record, leader, err)
			}
			if err := app.StopThreadShellCommand(thread.ID, runID); err != nil {
				t.Fatal(err)
			}
		}
		waitShellFinished(t, rec, runID)
		if records, err := registry.Load(); err != nil || len(records) != 0 {
			t.Fatalf("stop=%v: after the command ended the registry holds %+v %v", stop, records, err)
		}
	}
}

// The environment that makes TestShellCommandBackendHelper the backend that
// dies: the data directory its orphan reaper keeps the registry in, and the
// command it runs, which prints "started".
const (
	shellBackendDataEnv    = "SHELL_ORPHAN_TEST_DATA"
	shellBackendCommandEnv = "SHELL_ORPHAN_TEST_COMMAND"
)

// TestShellCommandBackendHelper is not a test of its own: the test below runs
// this binary as a backend that starts a command and is then killed. It
// starts the orphan reaper as Startup does.
func TestShellCommandBackendHelper(t *testing.T) {
	dataDir := os.Getenv(shellBackendDataEnv)
	if dataDir == "" {
		t.Skip("runs only as the backend TestABackendsDeathEndsItsShellCommand kills")
	}
	app, _ := newShellCommandApp(t)
	if err := app.startOrphanReaper(dataDir); err != nil {
		t.Fatal(err)
	}
	thread, _ := newMixedTurnThread(t, app, "thread-shell-dying-backend")
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: uuid.NewString(), Command: os.Getenv(shellBackendCommandEnv),
	}); err != nil {
		t.Fatal(err)
	}
	waitShellOutput(t, app, thread.ID, "started")
	// The sidecar is told of the group before the record is written.
	record := waitOrphanRecord(t, app.orphanRegistry)
	fmt.Printf("ready %d\n", record.PGID)
	helperproc.Hold()
}

// sidecarOf returns the orphan reaper sidecar the backend started.
func sidecarOf(t *testing.T, backend int) int {
	t.Helper()
	parent, err := process.NewProcess(int32(backend))
	if err != nil {
		t.Fatal(err)
	}
	children, err := parent.Children()
	if err != nil {
		t.Fatalf("list the backend's children: %v", err)
	}
	for _, child := range children {
		if args, err := child.CmdlineSlice(); err == nil && slices.Contains(args, orphanreaper.SidecarName) {
			// A signal to the backend's group must not end the sidecar.
			if pgid, err := syscall.Getpgid(int(child.Pid)); err != nil || pgid != int(child.Pid) {
				t.Fatalf("the sidecar is in group %d %v, want its own", pgid, err)
			}
			return int(child.Pid)
		}
	}
	t.Fatal("the backend started no orphan reaper sidecar")
	return 0
}

// A backend killed while its command runs leaves the command no way to be
// stopped from inside. Every process of the command's group ends with the
// backend, including one its shell started: a signal to the shell alone
// would leave `sleep` running. A group whose sidecar died with the backend is
// reaped by the next boot's sweep from the record the backend left, under
// whatever process the orphan was reparented to.
func TestABackendsDeathEndsItsShellCommand(t *testing.T) {
	for _, c := range []struct {
		name        string
		command     string
		sidecarDies bool
	}{
		{"the whole group ends with the backend", `echo started; sleep 600; echo after`, false},
		{"a group that ignores SIGTERM ends with the backend", `trap '' TERM; echo started; sleep 600; echo after`, false},
		{"the next boot reaps the group", `echo started; sleep 600; echo after`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			backend := exec.Command(os.Args[0], "-test.run=^TestShellCommandBackendHelper$")
			backend.Env = append(os.Environ(),
				shellBackendDataEnv+"="+dataDir, shellBackendCommandEnv+"="+c.command)
			stdout, err := backend.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			helperproc.Tie(t, backend)
			if err := backend.Start(); err != nil {
				t.Fatal(err)
			}
			pgid := 0
			scanner := bufio.NewScanner(stdout)
			for pgid == 0 && scanner.Scan() {
				if rest, ok := strings.CutPrefix(scanner.Text(), "ready "); ok {
					pgid, _ = strconv.Atoi(rest)
				}
			}
			if pgid == 0 {
				_ = backend.Process.Kill()
				_ = backend.Wait()
				t.Fatalf("the backend never started its command: %v", scanner.Err())
			}
			t.Cleanup(func() { _ = procutil.SignalGroup(pgid, syscall.SIGKILL) })
			if c.sidecarDies {
				sidecar := sidecarOf(t, backend.Process.Pid)
				if err := syscall.Kill(sidecar, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = backend.Wait()

			registry := orphanreaper.NewRegistry(filepath.Join(dataDir, "orphan-registry.json"))
			if c.sidecarDies {
				if !groupRunning(pgid) {
					t.Fatal("the group ended with the backend; this case needs one that survives it")
				}
				if err := orphanreaper.Sweep(registry, orphanreaper.SystemProcInfo(), 100*time.Millisecond); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(10 * time.Second)
			for groupRunning(pgid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if groupRunning(pgid) {
				t.Fatal("the command's group outlived the backend")
			}
			if c.sidecarDies {
				if records, err := registry.Load(); err != nil || len(records) != 0 {
					t.Fatalf("the sweep left %+v %v in the registry", records, err)
				}
			}
		})
	}
}
