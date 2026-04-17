package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerEnqueueWhenDisabledDrops(t *testing.T) {
	dir := t.TempDir()
	drops := atomic.Int64{}
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Millisecond,
		Enabled:      false,
		DropHook:     func() { drops.Add(1) },
	})
	defer m.Shutdown(context.Background())

	rec, _ := NewRecord(time.Now(), "t-1", "k", nil)
	if m.Enqueue(rec) {
		t.Fatal("Enqueue returned true while disabled")
	}
	// We do NOT count "rejected because disabled" as a drop — the event
	// never entered the pipeline, so DropHook should not fire.
	if got := drops.Load(); got != 0 {
		t.Errorf("drops = %d, want 0 for disabled-mode reject", got)
	}

	// No files should be created while disabled.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dir has %d entries, want 0 when disabled", len(entries))
	}
}

func TestManagerEnqueueWhenEnabledWrites(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second, // don't evict during the test
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	for i := 0; i < 3; i++ {
		rec, _ := NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "t-1", "turn:start", map[string]int{"i": i})
		if !m.Enqueue(rec) {
			t.Fatalf("Enqueue %d returned false", i)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	path := filepath.Join(dir, "t-1.jsonl")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), contents)
	}
}

func TestManagerDropsWhenQueueFullAndInvokesHook(t *testing.T) {
	dir := t.TempDir()
	drops := atomic.Int64{}

	// Build a writer that blocks forever on the first write. We do that by
	// pre-creating the file as a directory so os.OpenFile fails — Enqueue
	// still fills the queue, then subsequent Enqueue calls drop.
	// Easier: use a tiny queue with a slow producer so the queue fills fast.
	slow := make(chan struct{})
	t.Cleanup(func() { close(slow) })

	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    2,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
		DropHook:     func() { drops.Add(1) },
	})
	defer m.Shutdown(context.Background())

	// Block the drain loop by holding the writer mutex on the only active
	// file. We do this indirectly: start one legitimate write, then flood
	// the queue.
	// Strategy: write many records fast enough that the bounded queue
	// overflows. We rely on the reaper/drainer being sequential.
	const flood = 500
	first, _ := NewRecord(time.Unix(0, 0), "t-1", "k", nil)
	m.Enqueue(first) // warms the writer

	// Spin a tight loop pushing to a size-2 channel; at least some will drop.
	accepted := int64(0)
	for i := 0; i < flood; i++ {
		rec, _ := NewRecord(time.Unix(0, int64(i)*int64(time.Microsecond)), "t-1", "k", map[string]int{"i": i})
		if m.Enqueue(rec) {
			accepted++
		}
	}

	// Give the drainer a moment to process.
	time.Sleep(100 * time.Millisecond)

	if drops.Load() == 0 {
		t.Errorf("expected at least one drop (queue size 2 vs %d records)", flood)
	}
	if accepted+drops.Load() < int64(flood) {
		// Sanity: every record either accepted or dropped.
		t.Errorf("accepted=%d drops=%d flood=%d; some records disappeared",
			accepted, drops.Load(), flood)
	}
}

func TestManagerReaperClosesIdleWriters(t *testing.T) {
	dir := t.TempDir()
	// Short idle timeout so the test runs fast; reaperInterval stays 30s
	// but we trigger the reaper manually via evictIdle.
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  5 * time.Millisecond,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	rec, _ := NewRecord(time.Now(), "t-1", "k", nil)
	if !m.Enqueue(rec) {
		t.Fatal("Enqueue returned false")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	if m.openCount() != 1 {
		t.Fatalf("openCount = %d, want 1 after first write", m.openCount())
	}

	// Wait past the idle timeout, then trigger reaper directly (the
	// 30-second ticker would otherwise block the test).
	time.Sleep(20 * time.Millisecond)
	m.evictIdle()

	if m.openCount() != 0 {
		t.Errorf("openCount = %d, want 0 after idle eviction", m.openCount())
	}
}

func TestManagerSetEnabledToggle(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      false,
	})
	defer m.Shutdown(context.Background())

	rec1, _ := NewRecord(time.Unix(0, 1_000_000), "t-1", "k", nil)
	if m.Enqueue(rec1) {
		t.Fatal("Enqueue accepted while disabled")
	}

	m.SetEnabled(true)
	rec2, _ := NewRecord(time.Unix(0, 2_000_000), "t-1", "k", nil)
	if !m.Enqueue(rec2) {
		t.Fatal("Enqueue rejected after SetEnabled(true)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	path := filepath.Join(dir, "t-1.jsonl")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Only one record should land in the file (the one written while enabled).
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), contents)
	}

	m.SetEnabled(false)
	rec3, _ := NewRecord(time.Unix(0, 3_000_000), "t-1", "k", nil)
	if m.Enqueue(rec3) {
		t.Fatal("Enqueue accepted after SetEnabled(false)")
	}
}

func TestManagerShutdownClosesAllWriters(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})

	for _, id := range []string{"t-1", "t-2", "t-3"} {
		rec, _ := NewRecord(time.Now(), id, "k", nil)
		m.Enqueue(rec)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = m.waitForDrain(ctx)

	if got := m.openCount(); got != 3 {
		t.Fatalf("openCount before shutdown = %d, want 3", got)
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := m.openCount(); got != 0 {
		t.Errorf("openCount after shutdown = %d, want 0", got)
	}

	// Post-shutdown Enqueue must be a no-op that returns false.
	rec, _ := NewRecord(time.Now(), "t-4", "k", nil)
	if m.Enqueue(rec) {
		t.Error("Enqueue accepted after Shutdown")
	}

	// Second Shutdown must be a no-op.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned %v, want nil", err)
	}
}

func TestManagerShutdownHonoursContextTimeout(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})

	// Flood the queue so drain takes a moment.
	for i := 0; i < 100; i++ {
		rec, _ := NewRecord(time.Now(), "t", "k", map[string]int{"i": i})
		m.Enqueue(rec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// Shutdown may not complete its drain within 10ms but it must return.
	start := time.Now()
	_ = m.Shutdown(ctx)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v, want bounded by context", elapsed)
	}
}

func TestManagerEnqueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	rec, _ := NewRecord(time.Unix(0, 42*int64(time.Millisecond)), "thread-x", "diff", map[string]string{"file": "main.go"})
	if !m.Enqueue(rec) {
		t.Fatal("Enqueue: false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(dir, "thread-x.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var parsed Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(contents))), &parsed); err != nil {
		t.Fatalf("Unmarshal: %v (%q)", err, contents)
	}
	if parsed.ThreadID != "thread-x" {
		t.Errorf("ThreadID = %q, want thread-x", parsed.ThreadID)
	}
	if parsed.Kind != "diff" {
		t.Errorf("Kind = %q, want diff", parsed.Kind)
	}
	if parsed.Timestamp != 42 {
		t.Errorf("Timestamp = %d, want 42", parsed.Timestamp)
	}
	// Data should be the map we passed, decoded as a nested object.
	var payload map[string]string
	if err := json.Unmarshal(parsed.Data, &payload); err != nil {
		t.Fatalf("parse data: %v", err)
	}
	if payload["file"] != "main.go" {
		t.Errorf("data.file = %q, want main.go", payload["file"])
	}
}

func TestManagerConcurrentEnqueueAllWritten(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    1024,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	const total = 200
	done := make(chan struct{}, total)
	accepted := atomic.Int64{}
	for i := 0; i < total; i++ {
		go func(i int) {
			rec, _ := NewRecord(time.Unix(0, int64(i)*int64(time.Microsecond)), "tc", "k", map[string]int{"i": i})
			if m.Enqueue(rec) {
				accepted.Add(1)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < total; i++ {
		<-done
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(dir, "tc.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if int64(len(lines)) != accepted.Load() {
		t.Errorf("written lines = %d, accepted = %d (should match once inflight==0)", len(lines), accepted.Load())
	}
}

func TestNilManagerMethodsSafe(t *testing.T) {
	var m *Manager
	if m.Enabled() {
		t.Error("nil manager reports enabled")
	}
	if m.Enqueue(Record{}) {
		t.Error("nil manager accepted Enqueue")
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Errorf("nil manager Shutdown returned %v, want nil", err)
	}
	if err := m.RemoveThreadLog("tx"); err != nil {
		t.Errorf("nil manager RemoveThreadLog returned %v, want nil", err)
	}
}

// TestRemoveThreadLogRemovesFileAndClosesWriter covers the common path: a
// running manager has persisted events for a thread, then we ask it to
// drop the thread's log. The writer should close cleanly and the file
// should be gone.
func TestRemoveThreadLogRemovesFileAndClosesWriter(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	rec, _ := NewRecord(time.Now(), "t-remove", "k", nil)
	if !m.Enqueue(rec) {
		t.Fatal("Enqueue rejected while enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.waitForDrain(ctx); err != nil {
		t.Fatalf("waitForDrain: %v", err)
	}

	path := filepath.Join(dir, "t-remove.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replay file missing before removal: %v", err)
	}
	if got := m.openCount(); got != 1 {
		t.Fatalf("openCount before remove = %d, want 1", got)
	}

	if err := m.RemoveThreadLog("t-remove"); err != nil {
		t.Fatalf("RemoveThreadLog: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("replay file still present after removal: err=%v", err)
	}
	if got := m.openCount(); got != 0 {
		t.Errorf("openCount after remove = %d, want 0", got)
	}
}

// TestRemoveThreadLogRemovesRotatedBackups covers the case where the
// writer has rotated and left .1/.2/.3 files behind. All of them must be
// removed, not just the current file.
func TestRemoveThreadLogRemovesRotatedBackups(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      false, // we're going to plant files by hand
	})
	defer m.Shutdown(context.Background())

	// Simulate previous runs that left the main log plus rotated backups.
	// We don't need the writer involved for this branch — RemoveThreadLog
	// must work even when the writer was never opened in this process.
	base := filepath.Join(dir, "t-rotated.jsonl")
	for _, p := range []string{base, base + ".1", base + ".2", base + ".3"} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	if err := m.RemoveThreadLog("t-rotated"); err != nil {
		t.Fatalf("RemoveThreadLog: %v", err)
	}
	for _, p := range []string{base, base + ".1", base + ".2", base + ".3"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after removal: err=%v", p, err)
		}
	}
}

// TestRemoveThreadLogNoFileNoError covers the case where replay was never
// enabled for the thread (e.g. replay disabled at thread creation, or
// thread deleted before any event fired). Removing must succeed silently.
func TestRemoveThreadLogNoFileNoError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	if err := m.RemoveThreadLog("t-never-wrote"); err != nil {
		t.Errorf("RemoveThreadLog on missing file returned %v, want nil", err)
	}
}

// TestRemoveThreadLogEmptyThreadID silently succeeds. Guarding against a
// misuse that would otherwise blow away the entire replay root ("/.jsonl").
func TestRemoveThreadLogEmptyThreadID(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(ManagerConfig{
		RootDir:      dir,
		QueueSize:    16,
		WriterConfig: WriterConfig{FsyncEvery: 1},
		IdleTimeout:  100 * time.Second,
		Enabled:      true,
	})
	defer m.Shutdown(context.Background())

	if err := m.RemoveThreadLog(""); err != nil {
		t.Errorf("RemoveThreadLog('') returned %v, want nil", err)
	}
}
