package replay

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewWriterCreatesParentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deeper")
	path := filepath.Join(dir, "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestWriterWriteAppendsNDJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	rec1, _ := NewRecord(time.Unix(0, 1_000_000), "thread-a", "turn:start", map[string]any{"index": 0})
	rec2, _ := NewRecord(time.Unix(0, 2_000_000), "thread-a", "item:persisted", map[string]any{"id": "abc"})

	if err := w.Write(rec1); err != nil {
		t.Fatalf("Write rec1: %v", err)
	}
	if err := w.Write(rec2); err != nil {
		t.Fatalf("Write rec2: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), contents)
	}

	var parsed1, parsed2 Record
	if err := json.Unmarshal([]byte(lines[0]), &parsed1); err != nil {
		t.Fatalf("parse line 1: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &parsed2); err != nil {
		t.Fatalf("parse line 2: %v", err)
	}
	if parsed1.Kind != "turn:start" {
		t.Errorf("line 1 kind = %q, want turn:start", parsed1.Kind)
	}
	if parsed2.Kind != "item:persisted" {
		t.Errorf("line 2 kind = %q, want item:persisted", parsed2.Kind)
	}
	if parsed1.ThreadID != "thread-a" {
		t.Errorf("line 1 threadId = %q, want thread-a", parsed1.ThreadID)
	}
	if parsed1.Timestamp != 1 {
		t.Errorf("line 1 ts = %d, want 1", parsed1.Timestamp)
	}
}

func TestWriterReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")

	w1, err := NewWriter(path, WriterConfig{FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	rec1, _ := NewRecord(time.Unix(0, 1_000_000), "thread-a", "a", nil)
	if err := w1.Write(rec1); err != nil {
		t.Fatalf("Write rec1: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close w1: %v", err)
	}

	w2, err := NewWriter(path, WriterConfig{FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter 2: %v", err)
	}
	defer w2.Close()
	rec2, _ := NewRecord(time.Unix(0, 2_000_000), "thread-a", "b", nil)
	if err := w2.Write(rec2); err != nil {
		t.Fatalf("Write rec2: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("reopen should append, got %d lines: %q", len(lines), contents)
	}
}

func TestWriterRotateAtThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	// Rotation threshold chosen small so a handful of records trips it.
	w, err := NewWriter(path, WriterConfig{MaxBytes: 256, FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Each record adds roughly 120-140 bytes; writing 4 guarantees rotation.
	for i := 0; i < 4; i++ {
		rec, err := NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "thread-a", "kind", map[string]any{
			"index":  i,
			"filler": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		})
		if err != nil {
			t.Fatalf("NewRecord: %v", err)
		}
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// We expect at least the .1 backup to exist.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation did not produce .1 backup: %v", err)
	}
	// The current file should be smaller than the threshold (contains only
	// the records written after rotation).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	if info.Size() >= 256 {
		t.Errorf("current size %d >= threshold 256; rotation didn't reset", info.Size())
	}
}

func TestWriterRotateKeepsAtMostThreeBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{MaxBytes: 80, FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Write a lot of records to drive several rotations. Each record is
	// well under 80 bytes so rotation triggers every 1-2 writes.
	for i := 0; i < 20; i++ {
		rec, err := NewRecord(time.Unix(0, int64(i)), "thread-a", "k", map[string]any{"i": i})
		if err != nil {
			t.Fatalf("NewRecord: %v", err)
		}
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	// .1/.2/.3 may exist; .4 must not.
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("unexpected .4 backup present: %v", err)
	}
}

func TestWriterWriteAfterCloseReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec, _ := NewRecord(time.Now(), "thread-a", "k", nil)
	if err := w.Write(rec); err == nil {
		t.Fatal("Write after Close returned nil, want error")
	}
}

func TestWriterLastAccessAdvancesOnWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	before := w.LastAccess()
	time.Sleep(5 * time.Millisecond)
	rec, _ := NewRecord(time.Now(), "thread-a", "k", nil)
	if err := w.Write(rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := w.LastAccess()
	if !after.After(before) {
		t.Errorf("LastAccess did not advance: before=%v after=%v", before, after)
	}
}

func TestRecordJSONShape(t *testing.T) {
	rec, err := NewRecord(time.Unix(0, 123*int64(time.Millisecond)), "t-1", "diff", map[string]any{"file": "a.go"})
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if parsed["ts"].(float64) != 123 {
		t.Errorf("ts = %v, want 123", parsed["ts"])
	}
	if parsed["threadId"] != "t-1" {
		t.Errorf("threadId = %v, want t-1", parsed["threadId"])
	}
	if parsed["kind"] != "diff" {
		t.Errorf("kind = %v, want diff", parsed["kind"])
	}
	if _, ok := parsed["data"]; !ok {
		t.Error("data key missing")
	}
}

func TestRecordRawMessagePassthrough(t *testing.T) {
	raw := json.RawMessage(`{"preformed":"value"}`)
	rec, err := NewRecord(time.Unix(0, 0), "t", "k", raw)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if string(rec.Data) != string(raw) {
		t.Errorf("data = %s, want %s (no double encoding)", rec.Data, raw)
	}
}

func TestRecordZeroTimeFilled(t *testing.T) {
	rec, err := NewRecord(time.Time{}, "t", "k", nil)
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	if rec.Timestamp == 0 {
		t.Error("zero time should have been replaced with now()")
	}
}

// TestWriterContentIsLineDelimited ensures bufio.Scanner can read back every
// write as a discrete JSON document.
func TestWriterContentIsLineDelimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-a.jsonl")
	w, err := NewWriter(path, WriterConfig{FsyncEvery: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	for i := 0; i < 10; i++ {
		rec, _ := NewRecord(time.Unix(0, int64(i)*int64(time.Millisecond)), "t", "k", map[string]int{"i": i})
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var r Record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatalf("parse line %d: %v (%q)", count, err, scanner.Text())
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	if count != 10 {
		t.Errorf("parsed %d lines, want 10", count)
	}
}
