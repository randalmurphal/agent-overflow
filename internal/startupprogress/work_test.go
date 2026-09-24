package startupprogress

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadProcessWorkCountsCPUAndStorage: the platform sampler sees this
// process's CPU time grow while it computes and its storage I/O grow by
// what it writes to a file. On Linux the I/O comes from /proc/self/io.
func TestReadProcessWorkCountsCPUAndStorage(t *testing.T) {
	before, err := ReadProcessWork()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	// 100 ms of CPU within 1 s of computing needs a tenth of a core; a
	// sampler that counted only kernel or system time would take seconds.
	var mid ProcessWork
	for giveUp := time.Now().Add(time.Second); ; {
		spinCPU(20 * time.Millisecond)
		if mid, err = ReadProcessWork(); err != nil {
			t.Fatalf("sample: %v", err)
		}
		if mid.CPU-before.CPU >= 100*time.Millisecond {
			break
		}
		if time.Now().After(giveUp) {
			t.Fatalf("CPU time grew %s in 1 s of computing, want at least 100ms", mid.CPU-before.CPU)
		}
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := ReadProcessWork()
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if runtime.GOOS == "linux" && after.IOSource != "/proc/self/io" {
		t.Fatalf("I/O came from %q on Linux, want /proc/self/io", after.IOSource)
	}
	if after.IOSource != mid.IOSource || after.IO-mid.IO < 1<<20 {
		t.Fatalf("I/O went from %d (%s) to %d (%s) across a 1 MiB write", mid.IO, mid.IOSource, after.IO, after.IOSource)
	}
}

// spinCPU keeps one goroutine computing for d without touching storage.
func spinCPU(d time.Duration) {
	x := uint64(1)
	for stop := time.Now().Add(d); time.Now().Before(stop); {
		for range 10_000 {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	spinSink.Store(x)
}

var spinSink atomic.Uint64

// TestWorkProgressedAtTheThresholds: a tick counts as progress at a
// twentieth of a core or 64 KiB/s of I/O over its interval, and I/O from
// different counters is never compared.
func TestWorkProgressedAtTheThresholds(t *testing.T) {
	const interval = 2 * time.Second
	minCPU := interval / WorkCPUShare
	minIO := int64(WorkIOPerSecond * interval.Seconds())
	base := ProcessWork{CPU: time.Second, IO: 1 << 30, IOSource: "a"}
	for _, c := range []struct {
		name string
		cur  ProcessWork
		want bool
	}{
		{"idle", base, false},
		{"CPU under", ProcessWork{CPU: base.CPU + minCPU - 1, IO: base.IO, IOSource: "a"}, false},
		{"CPU at", ProcessWork{CPU: base.CPU + minCPU, IO: base.IO, IOSource: "a"}, true},
		{"I/O under", ProcessWork{CPU: base.CPU, IO: base.IO + minIO - 1, IOSource: "a"}, false},
		{"I/O at", ProcessWork{CPU: base.CPU, IO: base.IO + minIO, IOSource: "a"}, true},
		{"I/O from another counter", ProcessWork{CPU: base.CPU, IO: base.IO + 10*minIO, IOSource: "b"}, false},
	} {
		if got := WorkProgressed(base, c.cur, interval); got != c.want {
			t.Errorf("%s: WorkProgressed = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSamplerCountsWatchedFilesAndWork: a watched file's size change is
// progress, including its appearance; an unreadable file is no evidence and
// is logged once; the first work sample and the one after a failed read are
// baselines.
func TestSamplerCountsWatchedFilesAndWork(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "db")
	wal := filepath.Join(dir, "db-wal")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var denied atomic.Bool
	work := ProcessWork{IOSource: "a"}
	var workErr error
	var logs []string
	s := NewSampler(SamplerOptions{
		Interval: time.Second,
		Stat: func(path string) (fs.FileInfo, error) {
			if path == db && denied.Load() {
				return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
			}
			return os.Stat(path)
		},
		ReadWork: func() (ProcessWork, error) { return work, workErr },
		Logf:     func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	s.Watch(db, wal)

	step := func(what string, want bool) {
		t.Helper()
		if got := s.Sample(); got != want {
			t.Fatalf("%s: Sample = %v, want %v", what, got, want)
		}
	}
	step("the first sample", false)
	step("nothing", false)
	if err := os.WriteFile(db, []byte("xy"), 0o600); err != nil {
		t.Fatal(err)
	}
	step("the database grew", true)
	step("nothing after growth", false)
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	step("the WAL appeared", true)

	denied.Store(true)
	step("an unreadable database", false)
	step("still unreadable", false)
	denied.Store(false)
	step("readable again at its last known size", false)

	work.CPU += time.Second / WorkCPUShare
	step("CPU work", true)
	workErr = errors.New("counters gone")
	work.CPU += time.Hour
	step("a failed work read", false)
	step("another failed work read", false)
	workErr = nil
	step("the work read after a failure is a baseline", false)
	work.IO += WorkIOPerSecond
	step("I/O work", true)

	var statLogs, workLogs int
	for _, l := range logs {
		switch {
		case strings.Contains(l, "cannot read "+db):
			statLogs++
		case strings.Contains(l, "counters gone"):
			workLogs++
		}
	}
	if statLogs != 1 || workLogs != 1 {
		t.Fatalf("logged %d stat and %d work failures, want one each: %q", statLogs, workLogs, logs)
	}
}
