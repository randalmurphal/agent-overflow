package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

func TestNewWriterUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "replay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "thread-a.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, err := NewWriter(path, WriterConfig{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	assertMode(t, dir, 0o700)
	assertMode(t, path, 0o600)
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

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
