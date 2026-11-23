package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tieStandIns ties the stand-ins that runs in t start, the fake go command
// and its children, to t, and returns the directory to name in
// standInsEnv. Each stand-in records its pid and its parent's there as it
// starts (standIn), and t holds a lock on a file there until it ends. A
// stand-in ends once it can take the lock, which t's cleanup frees, as the
// kernel does when the test binary dies however it dies, and one that
// starts after that ends at once. The cleanup then fails t with any stand-in
// or parent of one still running, and kills it.
func tieStandIns(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock, err := os.Create(filepath.Join(dir, standInLock))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Errorf("free the stand-ins' lock: %v", err)
		}
		endStandIns(t, dir)
	})
	return dir
}

// standInLock is the file in a tieStandIns directory that the test locks.
const standInLock = "test"

// standInGrace is how long the stand-ins of a test that ended have to be
// gone before endStandIns kills them.
const standInGrace = 10 * time.Second

// standIn records this process in the standInsEnv directory, if there is
// one, and reports whether the test it stands in for has ended.
func standIn() (ended bool) {
	dir := os.Getenv(standInsEnv)
	if dir == "" {
		return false
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "record a stand-in:", err)
		return true
	}
	return testEnded(dir)
}

// testEnded reports whether the test that tied this stand-in to it has
// ended: its lock on dir's file is free, or the file is gone.
func testEnded(dir string) bool {
	lock, err := os.Open(filepath.Join(dir, standInLock))
	if err != nil {
		return true
	}
	defer lock.Close()
	return syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil
}

// nextSignal waits for a signal on signals, and reports false instead once
// the test this stand-in is tied to has ended.
func nextSignal(signals <-chan os.Signal) (os.Signal, bool) {
	dir := os.Getenv(standInsEnv)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case sig := <-signals:
			return sig, true
		case <-tick.C:
			if dir != "" && testEnded(dir) {
				return nil, false
			}
		}
	}
}

// endStandIns waits for every stand-in recorded in dir, and the parent of
// each, to be gone, and fails t with those still running after
// standInGrace, which it kills.
func endStandIns(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(standInGrace)
	for {
		left := standInsRunning(t, dir)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("processes the test's runs started are still running %v after it ended, and are killed: %v", standInGrace, left)
			for _, pid := range left {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			deadline = time.Now().Add(standInGrace)
			for len(standInsRunning(t, dir)) > 0 {
				if time.Now().After(deadline) {
					t.Errorf("processes the test's runs started outlived SIGKILL: %v", standInsRunning(t, dir))
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// standInsRunning lists the stand-ins recorded in dir and their parents
// that are still running, the test binary aside.
func standInsRunning(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{os.Getpid(): true, 1: true}
	var running []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		parent, err := strconv.Atoi(readFile(t, filepath.Join(dir, entry.Name())))
		if err != nil {
			t.Fatalf("stand-in %d recorded its parent as %v", pid, err)
		}
		for _, p := range []int{pid, parent} {
			if !seen[p] && syscall.Kill(p, 0) == nil {
				running = append(running, p)
			}
			seen[p] = true
		}
	}
	return running
}

// recorded reads what the children recorded in dir: each one's pid, and
// whether it recorded that it exited.
func recorded(t *testing.T, dir string) map[int]bool {
	t.Helper()
	started, err := filepath.Glob(filepath.Join(dir, "*.started"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, path := range started {
		base := strings.TrimSuffix(path, ".started")
		pid, err := strconv.Atoi(filepath.Base(base))
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(base + ".exited")
		got[pid] = err == nil
	}
	return got
}

// checkChildrenGone checks that at least least children recorded
// themselves in dir, that each is gone, and, when exited, that each
// recorded that it exited after its SIGTERM.
func checkChildrenGone(t *testing.T, dir string, least int, exited bool) {
	t.Helper()
	got := recorded(t, dir)
	if len(got) < least {
		t.Fatalf("%d children started; want at least %d", len(got), least)
	}
	for pid, done := range got {
		if exited && !done {
			t.Errorf("child %d had not exited after its SIGTERM", pid)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("child %d is still running (%v)", pid, err)
		}
	}
}

// childrenPerRun is how many children startChildren starts.
func childrenPerRun() int {
	if runtime.GOOS == "linux" {
		return 2
	}
	return 1
}
