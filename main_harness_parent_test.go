package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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

// runParentWatchChild is the child: once its parent is gone it must receive
// the SIGTERM a deliberate stop sends, and it says so in the marker file.
func runParentWatchChild() int {
	parent, err := strconv.Atoi(os.Getenv(parentWatchParentEnv))
	if err != nil {
		return 2
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGTERM)
	if err := watchParentExit(parent); err != nil {
		return 1
	}
	select {
	case <-stopped:
		if err := os.WriteFile(os.Getenv(parentWatchChildEnv), []byte("stopped\n"), 0o600); err != nil {
			return 1
		}
		return 0
	case <-time.After(30 * time.Second):
		return 1
	}
}

func TestExitWithParentRequiresHarnessAndAProcessID(t *testing.T) {
	flags, err := parseFlags([]string{"--harness", "--data-dir", "/tmp/x", "--exit-with-parent", "42"})
	if err != nil || flags.exitWithParent != 42 {
		t.Fatalf("parse --harness --exit-with-parent 42 = %+v, %v", flags.exitWithParent, err)
	}
	for _, args := range [][]string{
		{"--exit-with-parent", "42"},
		{"--soak", "--exit-with-parent", "42"},
		{"--harness", "--data-dir", "/tmp/x", "--exit-with-parent", "-1"},
	} {
		if _, err := parseFlags(args); err == nil || !strings.Contains(err.Error(), "--exit-with-parent") {
			t.Fatalf("parseFlags(%q) = %v, want a refusal naming --exit-with-parent", args, err)
		}
	}
}

// The watcher stops the backend as soon as the parent it was given is not
// its parent, including when that was already so at the start.
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
			calls := 0
			ppid := func() int {
				p := tc.parents[min(calls, len(tc.parents)-1)]
				calls++
				return p
			}
			stopped := false
			done := make(chan struct{})
			go func() {
				defer close(done)
				watchParent(7, ppid, func() { stopped = true }, time.Millisecond)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the watcher never stopped")
			}
			if !stopped || calls != tc.polls {
				t.Fatalf("stopped=%v after %d polls, want stopped after %d", stopped, calls, tc.polls)
			}
		})
	}
}

// A real orphan: the child's parent shell exits at once, and the child must
// stop itself through SIGTERM well inside its own deadline.
func TestAnOrphanedHarnessBackendStopsItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher's Job Object owns the tree on Windows")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	// $$ is the shell, the child's parent until the shell exits right after
	// starting it.
	shell := exec.Command("/bin/sh", "-c", `AO_TEST_PARENT_WATCH_PID=$$ "$0" -test.run='^$' >/dev/null 2>&1 &`, self)
	shell.Env = append(os.Environ(), parentWatchChildEnv+"="+marker)
	if err := shell.Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if body, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(body)) == "stopped" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the orphaned backend did not stop itself")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
