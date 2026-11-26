// Package procstate reads what the test process holds: heap in use,
// goroutines and open files, so a measurement can compare them before
// and after the work it measures. Open files are read from /proc/self/fd,
// so a test that reads them is skipped on a system without it. Standard
// library only.
package procstate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// State is the process at one moment.
type State struct {
	HeapInUse  uint64
	Goroutines int
	// Files is the target of each open descriptor, sorted; a deleted
	// file's ends in " (deleted)".
	Files []string
}

func (s State) String() string {
	return fmt.Sprintf("heap in use %.1f MB, %d goroutines, %d open files", float64(s.HeapInUse)/(1<<20), s.Goroutines, len(s.Files))
}

// DeletedBytes is the size of the deleted files the process holds open,
// such as SQLite's temporary files, which it deletes as it opens them.
func DeletedBytes(tb testing.TB) int64 {
	tb.Helper()
	entries := descriptors(tb)
	var n int64
	for _, e := range entries {
		fd := filepath.Join("/proc/self/fd", e.Name())
		target, err := os.Readlink(fd)
		if err != nil || !strings.HasSuffix(target, " (deleted)") {
			continue
		}
		if info, err := os.Stat(fd); err == nil {
			n += info.Size()
		}
	}
	return n
}

func descriptors(tb testing.TB) []os.DirEntry {
	tb.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		tb.Skipf("no /proc/self/fd to read the open files from: %v", err)
	}
	return entries
}

// Read is the process's state; collected, after a forced collection that
// returns freed memory to the system.
func Read(tb testing.TB, collected bool) State {
	tb.Helper()
	if collected {
		debug.FreeOSMemory()
	}
	entries := descriptors(tb)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var files []string
	for _, e := range entries {
		// The directory's own descriptor is gone by the time it is read.
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil {
			files = append(files, target)
		}
	}
	slices.Sort(files)
	return State{HeapInUse: m.HeapInuse, Goroutines: runtime.NumGoroutine(), Files: files}
}

// Settled is the collected state once its goroutines are no more than
// limit's, or after wait: database/sql ends a closed database's opener
// goroutine just after Close returns.
func Settled(tb testing.TB, limit State, wait time.Duration) State {
	tb.Helper()
	deadline := time.Now().Add(wait)
	for {
		s := Read(tb, true)
		if s.Goroutines <= limit.Goroutines || time.Now().After(deadline) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Added is the open files of s that base does not hold, a file open twice
// counted twice.
func (s State) Added(base State) []string {
	held := slices.Clone(base.Files)
	var out []string
	for _, f := range s.Files {
		if i := slices.Index(held, f); i >= 0 {
			held = slices.Delete(held, i, i+1)
			continue
		}
		out = append(out, f)
	}
	return out
}
