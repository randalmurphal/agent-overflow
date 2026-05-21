package logging

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLogWritesNDJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	entries := []LogEntry{
		{Level: "info", Component: "store", Message: "opened"},
		{Level: "warn", Component: "provider", Message: "slow response"},
		{Level: "error", Component: "triage", Message: "unknown type"},
	}
	for _, e := range entries {
		if err := lg.Log(e); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	for i, line := range lines {
		var got LogEntry
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", i, err)
		}
		if got.Level != entries[i].Level {
			t.Errorf("line %d: level = %q, want %q", i, got.Level, entries[i].Level)
		}
		if got.Component != entries[i].Component {
			t.Errorf("line %d: component = %q, want %q", i, got.Component, entries[i].Component)
		}
		if got.Message != entries[i].Message {
			t.Errorf("line %d: msg = %q, want %q", i, got.Message, entries[i].Message)
		}
	}
}

func TestNewLoggerUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	assertMode(t, dir, 0o700)
	assertMode(t, path, 0o600)
}

func TestLogSetsTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	if err := lg.Log(LogEntry{Level: "info", Component: "test", Message: "hello"}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var got LogEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Timestamp == "" {
		t.Fatal("expected timestamp to be set, got empty string")
	}
	// Sanity check: it should contain a T (RFC3339 separator).
	if !strings.Contains(got.Timestamp, "T") {
		t.Errorf("timestamp %q does not look like RFC3339", got.Timestamp)
	}
}

func TestRotationTriggersAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	lg, err := NewLogger(path, 100)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	// Each entry is roughly 80-120 bytes of JSON. Write enough to trigger rotation.
	for i := 0; i < 10; i++ {
		if err := lg.Log(LogEntry{Level: "info", Component: "test", Message: "padding message"}); err != nil {
			t.Fatalf("Log %d: %v", i, err)
		}
	}

	// Verify .1 backup exists.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected %s.1 to exist: %v", path, err)
	}

	// Current file should be smaller than maxBytes (fresh after rotation).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	if info.Size() >= 100 {
		t.Errorf("current file size = %d, expected < 100 after rotation", info.Size())
	}
}

func TestRotationKeepsThreeBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	// Use a very small maxBytes so each write triggers rotation.
	lg, err := NewLogger(path, 50)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	// Write many entries to trigger multiple rotations (each entry is ~80+ bytes,
	// so every entry triggers a rotation at maxBytes=50).
	for i := 0; i < 20; i++ {
		if err := lg.Log(LogEntry{Level: "info", Component: "test", Message: "trigger rotation"}); err != nil {
			t.Fatalf("Log %d: %v", i, err)
		}
	}

	// .1, .2, .3 should exist.
	for _, suffix := range []string{".1", ".2", ".3"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected %s%s to exist: %v", path, suffix, err)
		}
	}
	// .4 should NOT exist.
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("expected .4 to not exist, but it does")
	}
}

func TestNewLoggerCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "deep.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger with nested path: %v", err)
	}
	defer lg.Close()

	if err := lg.Log(LogEntry{Level: "info", Component: "test", Message: "deep"}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected log file at nested path: %v", err)
	}
}

func TestCloseClosesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Log after close should return an error, not panic.
	err = lg.Log(LogEntry{Level: "info", Component: "test", Message: "after close"})
	if err == nil {
		t.Fatal("expected error logging after Close, got nil")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %q, expected it to mention 'closed'", err.Error())
	}

	// Double close should be safe.
	if err := lg.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}

func TestConcurrentLogging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	const goroutines = 10
	const entriesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < entriesPerGoroutine; i++ {
				if err := lg.Log(LogEntry{Level: "info", Component: "concurrent", Message: "entry"}); err != nil {
					t.Errorf("Log: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	lineCount := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineCount++
		var entry LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", lineCount, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	expected := goroutines * entriesPerGoroutine
	if lineCount != expected {
		t.Errorf("line count = %d, want %d", lineCount, expected)
	}
}

func TestNewLoggerUsesDefaultMaxBytesAndExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.log")
	if err := os.WriteFile(path, []byte("seed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	if lg.maxBytes != defaultMaxBytes {
		t.Fatalf("maxBytes = %d, want %d", lg.maxBytes, defaultMaxBytes)
	}
	if lg.written != 4 {
		t.Fatalf("written = %d, want 4", lg.written)
	}
}

func TestNewLoggerReturnsDirectoryCreationError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := NewLogger(filepath.Join(blocker, "child.log"), 0)
	if err == nil {
		t.Fatal("expected directory creation error")
	}
	if !strings.Contains(err.Error(), "create parent dirs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLogReturnsMarshalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marshal.log")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	err = lg.Log(LogEntry{
		Level:     "info",
		Component: "test",
		Message:   "bad data",
		Data:      func() {},
	})
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "marshal entry") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRotateReopensLoggerWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	actualPath := filepath.Join(dir, "actual.log")
	file, err := os.OpenFile(actualPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer file.Close()

	lg := &Logger{
		file:     file,
		maxBytes: 1,
		path:     filepath.Join(dir, "missing.log"),
	}

	err = lg.rotate()
	if err == nil {
		t.Fatal("expected rotate error")
	}
	if !strings.Contains(err.Error(), "rename current to .1") {
		t.Fatalf("unexpected error: %v", err)
	}
	if lg.file == nil {
		t.Fatal("logger should reopen a usable file after rename failure")
	}
	defer lg.Close()

	if _, statErr := os.Stat(filepath.Join(dir, "missing.log")); statErr != nil {
		t.Fatalf("expected reopened log file: %v", statErr)
	}
}

func TestRotateReturnsErrorWhenRenameAndReopenFail(t *testing.T) {
	dir := t.TempDir()
	actualPath := filepath.Join(dir, "actual.log")
	file, err := os.OpenFile(actualPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer file.Close()

	lg := &Logger{
		file:     file,
		maxBytes: 1,
		path:     filepath.Join(dir, "missing", "test.log"),
	}

	err = lg.rotate()
	if err == nil {
		t.Fatal("expected rotate error")
	}
	if !strings.Contains(err.Error(), "rename current and reopen") {
		t.Fatalf("unexpected error: %v", err)
	}
	if lg.file != nil {
		t.Fatal("logger file should be nil when reopen fails")
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
