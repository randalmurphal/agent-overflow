//go:build unix

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// parentWatchChildEnv makes the test binary a stand-in harness backend that
// watches the parent named in parentWatchParentEnv and records how it ends.
const (
	parentWatchChildEnv  = "AO_TEST_PARENT_WATCH_MARKER"
	parentWatchParentEnv = "AO_TEST_PARENT_WATCH_PID"
)

// childExitDeadline stands in for orphanExitDeadline in the child, so a
// case does not wait out the real shutdown bound.
const childExitDeadline = 2 * time.Second

// runParentWatchChild is the child. Like the headless shutdown, it logs the
// SIGTERM a deliberate stop sends before acting on it, then says it stopped
// in the marker file.
func runParentWatchChild() int {
	parent, err := strconv.Atoi(os.Getenv(parentWatchParentEnv))
	if err != nil {
		return 2
	}
	marker := os.Getenv(parentWatchChildEnv)
	if err := os.WriteFile(marker+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 1
	}
	orphanExitDeadline = childExitDeadline
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM)
	if err := watchParentExit(parent); err != nil {
		return 1
	}
	select {
	case <-stopped:
		log.Printf("received SIGTERM")
		if err := os.WriteFile(marker, []byte("stopped\n"), 0o600); err != nil {
			return 1
		}
		return 0
	case <-time.After(30 * time.Second):
		return 1
	}
}

// The watcher stops the backend as soon as the parent it was given is not
// its parent, including when that was already so at the start, and before
// it logs anything.
func TestWatchParentStopsOnceTheParentIsGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parents []int
		polls   int
	}{
		{"exits later", []int{7, 7, 7, 1}, 4},
		{"already gone", []int{1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			log.SetOutput(writerFunc(func(p []byte) (int, error) {
				events = append(events, "log")
				return len(p), nil
			}))
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			calls := 0
			ppid := func() int {
				p := tc.parents[min(calls, len(tc.parents)-1)]
				calls++
				return p
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				watchParent(7, ppid, func() { events = append(events, "stop") }, time.Millisecond)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the watcher never stopped")
			}
			if strings.Join(events, ",") != "stop,log" || calls != tc.polls {
				t.Fatalf("events %v after %d polls, want stop then log after %d", events, calls, tc.polls)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A real orphan whose output still flows stops itself through SIGTERM.
func TestAnOrphanedHarnessBackendStopsItself(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	marker, pid := startOrphan(t, devNull)
	waitGone(t, pid)
	if body, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(body)) != "stopped" {
		t.Fatalf("the orphan ended without its ordered stop: %q, %v", body, err)
	}
}

// An orphan whose stderr is a full pipe nobody reads blocks on its first
// log line, in the watcher and in the shutdown alike; it must still end.
func TestAnOrphanWhoseOutputNobodyReadsStillEnds(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	// The read end stays open and unread for the whole case.
	read := os.NewFile(uintptr(fds[0]), "stderr-read")
	t.Cleanup(func() { _ = read.Close() })
	fillPipe(t, fds[1])
	write := os.NewFile(uintptr(fds[1]), "stderr-write")
	t.Cleanup(func() { _ = write.Close() })
	marker, pid := startOrphan(t, write)
	waitGone(t, pid)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the orphan finished its ordered stop, so its stderr never blocked; the case proves nothing")
	}
}

// fillPipe writes to fd until the pipe holds not one more byte, then makes
// it blocking again, so the next write by anyone blocks.
func fillPipe(t *testing.T, fd int) {
	t.Helper()
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{4096, 1} {
		chunk := make([]byte, size)
		for {
			if _, err := syscall.Write(fd, chunk); err != nil {
				if errors.Is(err, syscall.EAGAIN) {
					break
				}
				t.Fatal(err)
			}
		}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
}

// startOrphan starts the child under a shell that exits at once, so the
// child is orphaned straight away, with stderr as its standard error. It
// returns the child's marker path and PID.
func startOrphan(t *testing.T, stderr *os.File) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	// $$ is the shell, the child's parent until the shell exits right after
	// starting it.
	shell := exec.Command("/bin/sh", "-c", `AO_TEST_PARENT_WATCH_PID=$$ "$0" -test.run='^$' >/dev/null 2>&3 &`, self)
	shell.Env = append(os.Environ(), parentWatchChildEnv+"="+marker)
	shell.ExtraFiles = []*os.File{stderr}
	if err := shell.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(marker + ".pid"); err == nil && len(body) > 0 {
			pid, err := strconv.Atoi(string(body))
			if err != nil {
				t.Fatal(err)
			}
			return marker, pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the orphan never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitGone waits well past the child's exit deadline for pid to be gone.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(childExitDeadline + 10*time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the orphaned backend %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
