package supervise

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// The supervisor writes the copy of itself it then spawns with forks held
// off. An observer watches this process's descriptors for the staging temp
// file open for writing and then tries to take ForkLock for writing, as a
// fork would:
//
//   - The lock refused while the file was open is the evidence the guard
//     held. The test passes only after several such sightings, so a copy the
//     observer never caught proves nothing and is staged again.
//   - The lock taken while the file is still open, checked again with the lock
//     held, is a fork that could have inherited the writer: the failure.
//   - The lock taken after the file closed is inconclusive and ignored.
//
// Not parallel: no other test starts processes while this one reads the lock.
func TestTheSupervisorStagesItselfWithForksHeldOff(t *testing.T) {
	rig := newRig(t)
	config := rig.config()
	// Large enough that the copy and its sync span many observer passes.
	if err := os.WriteFile(config.SelfExecutable, bytes.Repeat([]byte("#!/bin/sh\n"), 1<<20), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisor, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	versionDir, err := rig.layout.VersionDir(config.SelfVersion)
	if err != nil {
		t.Fatal(err)
	}

	const wantSightings = 3
	// maxCopies bounds the test. A copy holds the file open for writing
	// through a 10 MiB write and its fsync, a window the observer passes
	// through many times even on one starved CPU, so a single copy normally
	// supplies every sighting. Missing them across this many copies means
	// the observer cannot see the window at all.
	const maxCopies = 100
	var (
		sightings atomic.Int64
		violation atomic.Pointer[string]
		inspect   atomic.Pointer[error]
	)
	stop := make(chan struct{})
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		for {
			select {
			case <-stop:
				return
			default:
			}
			path, open, err := stagingWriterOpen(rig.layout.VersionsDir())
			if err != nil {
				inspect.Store(&err)
				return
			}
			if !open {
				continue
			}
			if !syscall.ForkLock.TryLock() {
				sightings.Add(1)
				continue
			}
			_, stillOpen, err := stagingWriterOpen(rig.layout.VersionsDir())
			syscall.ForkLock.Unlock()
			if err != nil {
				inspect.Store(&err)
				return
			}
			if stillOpen {
				violation.Store(&path)
				return
			}
		}
	}()
	copies := 0
	for ; copies < maxCopies && sightings.Load() < wantSightings &&
		violation.Load() == nil && inspect.Load() == nil; copies++ {
		if err := supervisor.stageSelf(); err != nil {
			close(stop)
			<-observed
			t.Fatalf("stageSelf: %v", err)
		}
		if err := os.RemoveAll(versionDir); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-observed
	switch {
	case inspect.Load() != nil:
		t.Fatalf("inspecting this process's descriptors: %v", *inspect.Load())
	case violation.Load() != nil:
		t.Fatalf("a fork could start while %s was open for writing", *violation.Load())
	case sightings.Load() < wantSightings:
		t.Fatalf("%d copies ran and the observer caught the staged file open %d times, want %d",
			copies, sightings.Load(), wantSightings)
	}
}

// StageBinary itself takes no fork lock: the backend stages binaries another
// process runs, and a lock held across that copy would stall every launch in
// it. A fork holds ForkLock while StageBinary runs; it either finishes or
// blocks acquiring the lock, and the goroutine dump says which, so the
// outcome does not depend on how long anything takes.
func TestStageBinaryTakesNoForkLock(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	syscall.ForkLock.Lock()
	done := make(chan error, 1)
	go func() { done <- StageBinary(layout, "1.0.0", source) }()
	buf := make([]byte, 1<<20)
	for {
		select {
		case err := <-done:
			syscall.ForkLock.Unlock()
			if err != nil {
				t.Fatalf("StageBinary: %v", err)
			}
			return
		default:
		}
		if blockedOnForkLock(buf[:runtime.Stack(buf, true)]) {
			syscall.ForkLock.Unlock()
			<-done
			t.Fatal("StageBinary waited for ForkLock")
		}
		runtime.Gosched()
	}
}

// blockedOnForkLock reports a goroutine parked acquiring an RWMutex for
// reading from inside StageBinary.
func blockedOnForkLock(stacks []byte) bool {
	for _, stack := range strings.Split(string(stacks), "\n\n") {
		if strings.Contains(stack, "supervise.StageBinary") &&
			strings.Contains(stack, "sync.(*RWMutex).RLock") {
			return true
		}
	}
	return false
}

// stagingWriterOpen reports a descriptor of this process open for writing on
// a staging temp file under versionsDir. A descriptor that closes while it is
// read is skipped; any other failure to read /proc is an error.
func stagingWriterOpen(versionsDir string) (string, bool, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if !strings.HasPrefix(target, versionsDir+string(filepath.Separator)) ||
			!strings.HasPrefix(filepath.Base(target), ".staging-") {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		flags, err := fdinfoFlags(string(info))
		if err != nil {
			return "", false, fmt.Errorf("fd %s: %w", entry.Name(), err)
		}
		if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
			return target, true, nil
		}
	}
	return "", false, nil
}

func fdinfoFlags(info string) (uint64, error) {
	for _, line := range strings.Split(info, "\n") {
		if field, ok := strings.CutPrefix(line, "flags:"); ok {
			return strconv.ParseUint(strings.TrimSpace(field), 8, 64)
		}
	}
	return 0, errors.New("fdinfo has no flags line")
}
