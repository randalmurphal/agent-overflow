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
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/orphanreaper"
	"agent-overflow/internal/procutil"

	"github.com/google/uuid"
)

// A composer shell command ends with the backend, by the rule provider
// processes follow: on Linux its leader gets SIGTERM when the backend dies,
// and on macOS its group is recorded for the orphan reaper, whose sidecar
// kills it when the backend dies and whose registry the next launch sweeps.

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
			if record.UUID != runID || record.PGID != record.PID || !groupRunning(record.PGID) {
				t.Fatalf("record %+v, want the running group of run %s", record, runID)
			}
			if record.CreateUnix == 0 || record.CreateUnix != orphanreaper.CaptureCreateUnix(record.PID) {
				t.Fatalf("record %+v carries no start time that names the leader", record)
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
// dies: the registry it records into and the command it runs.
const (
	shellBackendRegistryEnv = "SHELL_ORPHAN_TEST_REGISTRY"
	shellBackendCommandEnv  = "SHELL_ORPHAN_TEST_COMMAND"
)

// TestShellCommandBackendHelper is not a test of its own: the test below runs
// this binary as a backend that starts a command and is then killed.
func TestShellCommandBackendHelper(t *testing.T) {
	registryPath := os.Getenv(shellBackendRegistryEnv)
	if registryPath == "" {
		t.Skip("runs only as the backend TestABackendsDeathEndsItsShellCommand kills")
	}
	app, _ := newShellCommandApp(t)
	app.orphanRegistry = orphanreaper.NewRegistry(registryPath)
	thread, _ := newMixedTurnThread(t, app, "thread-shell-dying-backend")
	if _, err := app.RunThreadShellCommand(context.Background(), thread.ID, ShellCommandRequest{
		RunID: uuid.NewString(), Command: os.Getenv(shellBackendCommandEnv),
	}); err != nil {
		t.Fatal(err)
	}
	waitShellOutput(t, app, thread.ID, "started")
	waitOrphanRecord(t, app.orphanRegistry)
	fmt.Println("ready")
	select {}
}

// orphanedProcInfo is the production process table, with the parent of a
// process whose backend is gone read as init. macOS reparents an orphan to
// launchd, which the sweep checks for; Linux reparents it to the nearest
// subreaper, which the test cannot predict.
type orphanedProcInfo struct {
	orphanreaper.ProcInfo
	deadBackend int
}

func (info orphanedProcInfo) PPID(pid int) (int, bool) {
	ppid, ok := info.ProcInfo.PPID(pid)
	if ok && ppid != info.deadBackend {
		return 1, true
	}
	return ppid, ok
}

// A backend killed while its command runs leaves the command no way to be
// stopped from inside. On Linux the command's leader ends with the backend.
// A group whose leader survives that, as every group does on macOS, is
// reaped by the next boot's sweep from the record the backend left.
func TestABackendsDeathEndsItsShellCommand(t *testing.T) {
	for _, c := range []struct {
		name    string
		command string
		// endsWithBackend: the leader takes the parent-death signal.
		endsWithBackend bool
	}{
		{"leader ends with the backend", "echo started; exec sleep 600", runtime.GOOS == "linux"},
		{"next boot reaps the group", "trap '' TERM; echo started; sleep 600", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			registryPath := filepath.Join(t.TempDir(), "orphan-registry.json")
			backend := exec.Command(os.Args[0], "-test.run=^TestShellCommandBackendHelper$")
			// The backend's temporary files land where this test removes them.
			backend.Env = append(os.Environ(),
				shellBackendRegistryEnv+"="+registryPath, shellBackendCommandEnv+"="+c.command,
				"TMPDIR="+t.TempDir())
			stdout, err := backend.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Start(); err != nil {
				t.Fatal(err)
			}
			ready := false
			scanner := bufio.NewScanner(stdout)
			for !ready && scanner.Scan() {
				ready = scanner.Text() == "ready"
			}
			if !ready {
				_ = backend.Process.Kill()
				_ = backend.Wait()
				t.Fatalf("the backend never started its command: %v", scanner.Err())
			}
			registry := orphanreaper.NewRegistry(registryPath)
			records, err := registry.Load()
			if err != nil || len(records) != 1 {
				_ = backend.Process.Kill()
				t.Fatalf("the backend recorded %+v %v, want its command's group", records, err)
			}
			pgid := records[0].PGID
			t.Cleanup(func() { _ = procutil.SignalGroup(pgid, syscall.SIGKILL) })
			if err := backend.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = backend.Wait()

			if c.endsWithBackend {
				deadline := time.Now().Add(10 * time.Second)
				for groupRunning(pgid) && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if groupRunning(pgid) {
					t.Fatal("the command outlived the backend")
				}
				return
			}
			if !groupRunning(pgid) {
				t.Fatal("the group ended with the backend; this case needs one that survives it")
			}
			info := orphanedProcInfo{ProcInfo: orphanreaper.GopsutilProcInfo(), deadBackend: backend.Process.Pid}
			if err := orphanreaper.Sweep(registry, info, 100*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for groupRunning(pgid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if groupRunning(pgid) {
				t.Fatal("the sweep left the command's group running")
			}
			if records, err := registry.Load(); err != nil || len(records) != 0 {
				t.Fatalf("the sweep left %+v %v in the registry", records, err)
			}
		})
	}
}
