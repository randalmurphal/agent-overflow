// Package observability_test contains cross-package integration tests that
// exercise the disabled OpenTelemetry provider and replay sub-package through
// their caller-facing APIs. Live turn spans and counters are tested where the
// app records them, in internal/triage/telemetry_test.go.
package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	obsotel "agent-overflow/internal/observability/otel"
	"agent-overflow/internal/observability/replay"
)

// --- OTel integration tests -----------------------------------------------

func TestObs_TracingDisabledEmitsNoSpans(t *testing.T) {
	ctx := context.Background()
	provider, err := obsotel.NewProvider(ctx, obsotel.Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	_, span := provider.Tracer().Start(ctx, "should-be-noop")
	if span.IsRecording() {
		t.Error("disabled provider returned a recording span")
	}
	span.End()
	if provider.Enabled() {
		t.Error("Enabled() returned true for disabled provider")
	}
}

// --- Replay integration tests ---------------------------------------------

func TestObs_ReplayWriterRotatesAt100MB(t *testing.T) {
	// We don't want to actually write 100MB in a unit test, so we use a
	// tiny MaxBytes threshold that exercises the same rotate() path.
	path := filepath.Join(t.TempDir(), "t-rotate.jsonl")
	w, err := replay.NewWriter(path, replay.WriterConfig{MaxBytes: 200, FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// Write enough records to trigger at least two rotations. Record JSON
	// is ~100 bytes so 2-3 records per rotation round-trip.
	for i := 0; i < 10; i++ {
		rec, _ := replay.NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "t", "kind",
			map[string]any{"i": i, "pad": strings.Repeat("x", 20)})
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// .1 must exist — we know rotation happened at least once.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf(".1 backup missing: %v", err)
	}
	// .2 should also exist given 10 records.
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Errorf(".2 backup missing: %v", err)
	}
	// current file is smaller than the threshold after rotation reset.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	if info.Size() >= 200 {
		t.Errorf("current size = %d, want < 200 after rotation", info.Size())
	}
}

func TestObs_ReplayRotationPreservesOrder(t *testing.T) {
	// Write 1, 2, 3 in order with a threshold that rotates after each full
	// record; verify the final layout puts the most recent record in the
	// current file and the older ones in a backup.
	path := filepath.Join(t.TempDir(), "t-order.jsonl")
	w, err := replay.NewWriter(path, replay.WriterConfig{MaxBytes: 80, FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	for i := 1; i <= 3; i++ {
		rec, _ := replay.NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "t", "k", map[string]int{"i": i})
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// Gather all records across current + .1 + .2 files and verify "i" field
	// is strictly increasing. Rotation renames old files, so:
	//   older (i=1) lives in .2 or is already dropped (we only keep .1/.2/.3)
	//   .1 holds the next-most-recent
	//   current holds the most recent
	files := []string{path + ".2", path + ".1", path}
	allIndexes := []int{}
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			// Missing backups are fine; early rotations may not have filled them.
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec replay.Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			var data map[string]int
			if err := json.Unmarshal(rec.Data, &data); err != nil {
				t.Fatalf("parse data %q: %v", rec.Data, err)
			}
			allIndexes = append(allIndexes, data["i"])
		}
	}
	if len(allIndexes) == 0 {
		t.Fatal("no records found across rotation files")
	}
	for i := 1; i < len(allIndexes); i++ {
		if allIndexes[i] < allIndexes[i-1] {
			t.Errorf("records out of order: %v", allIndexes)
		}
	}
	// Current file should contain the latest record (i=3).
	currentContent, _ := os.ReadFile(path)
	if !strings.Contains(string(currentContent), `"i":3`) {
		t.Errorf("current file missing i=3 record: %q", currentContent)
	}
}

func TestObs_ReplayManagerGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	m := replay.NewManager(replay.ManagerConfig{
		RootDir:     dir,
		QueueSize:   128,
		IdleTimeout: 10 * time.Second,
		Enabled:     true,
	})

	// Enqueue a moderate burst, which the queue holds whole.
	const burst = 50
	for i := 0; i < burst; i++ {
		rec, _ := replay.NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "shut", "k", map[string]int{"i": i})
		if !m.Enqueue(rec) {
			t.Fatalf("Enqueue %d refused", i)
		}
	}

	// Shutdown drains the queue before it closes the writers, and its
	// Close syncs, so nothing bounds it but go test's own timeout.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Post-shutdown: every buffered record is on disk.
	content, err := os.ReadFile(filepath.Join(dir, "shut.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if lines := strings.Count(string(content), "\n"); lines != burst {
		t.Errorf("%d records on disk after graceful shutdown, want %d", lines, burst)
	}
}

func TestObs_ReplayRespectsThreadScope(t *testing.T) {
	dir := t.TempDir()
	m := replay.NewManager(replay.ManagerConfig{
		RootDir:     dir,
		QueueSize:   16,
		IdleTimeout: 10 * time.Second,
		Enabled:     true,
	})
	t.Cleanup(func() {
		_ = m.Shutdown(context.Background())
	})

	for _, id := range []string{"A", "B"} {
		rec, _ := replay.NewRecord(time.Now(), id, "k", map[string]string{"id": id})
		if !m.Enqueue(rec) {
			t.Fatalf("Enqueue %s returned false", id)
		}
	}
	waitForFile(t, m, filepath.Join(dir, "A.jsonl"))
	waitForFile(t, m, filepath.Join(dir, "B.jsonl"))

	contentA, err := os.ReadFile(filepath.Join(dir, "A.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile A: %v", err)
	}
	contentB, err := os.ReadFile(filepath.Join(dir, "B.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile B: %v", err)
	}
	if strings.Contains(string(contentA), `"id":"B"`) {
		t.Error("A.jsonl contains B's record")
	}
	if strings.Contains(string(contentB), `"id":"A"`) {
		t.Error("B.jsonl contains A's record")
	}
}

func TestObs_ReplayRotationFilenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.jsonl")
	w, err := replay.NewWriter(path, replay.WriterConfig{MaxBytes: 80, FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 20; i++ {
		rec, _ := replay.NewRecord(time.Unix(0, int64(i)), "t", "k", map[string]int{"i": i})
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// At most .1/.2/.3 must be present; .4+ must not exist.
	for _, suffix := range []string{".1", ".2", ".3"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("%s missing: %v", suffix, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected .4 backup: %v", err)
	}
}

// waitForFile waits until every record enqueued so far is written, then
// requires the file. A record counts as written once its Write, Sync
// included, returns, so nothing bounds the wait but go test's own timeout.
func waitForFile(t *testing.T, m *replay.Manager, path string) {
	t.Helper()
	if err := m.WaitForDrain(context.Background()); err != nil {
		t.Fatalf("WaitForDrain: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file %s missing after the drain: %v", path, err)
	}
}
