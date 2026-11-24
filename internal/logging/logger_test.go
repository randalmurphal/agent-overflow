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

// Each entry is roughly 80-120 bytes of JSON: at maxBytes=100 entries
// accumulate before a rotation, at maxBytes=50 every entry rotates. Either
// way the current file is fresh (under maxBytes) and at most three backups
// are kept.
func TestRotationTriggersAtMaxBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxBytes int64
		entries  int
		message  string
		present  []string
		absent   []string
	}{
		{"triggers at max bytes", 100, 10, "padding message", []string{".1"}, nil},
		{"keeps three backups", 50, 20, "trigger rotation", []string{".1", ".2", ".3"}, []string{".4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.log")
			lg, err := NewLogger(path, tc.maxBytes)
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			defer lg.Close()
			for i := 0; i < tc.entries; i++ {
				if err := lg.Log(LogEntry{Level: "info", Component: "test", Message: tc.message}); err != nil {
					t.Fatalf("Log %d: %v", i, err)
				}
			}
			for _, suffix := range tc.present {
				if _, err := os.Stat(path + suffix); err != nil {
					t.Errorf("expected %s%s to exist: %v", path, suffix, err)
				}
			}
			for _, suffix := range tc.absent {
				if _, err := os.Stat(path + suffix); err == nil {
					t.Errorf("expected %s%s to not exist, but it does", path, suffix)
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat current: %v", err)
			}
			if info.Size() >= tc.maxBytes {
				t.Errorf("current file size = %d, expected < %d after rotation", info.Size(), tc.maxBytes)
			}
		})
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

// A rename of a current file that is not at the logger's path fails. If the
// path can be opened the logger reopens it and stays usable; if it cannot,
// the logger holds no file and says both failed.
func TestRotateReopensLoggerWhenRenameFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     []string
		wantErr  string
		reopened bool
	}{
		{"reopens", []string{"missing.log"}, "rename current to .1", true},
		{"reopen fails too", []string{"missing", "test.log"}, "rename current and reopen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file, err := os.OpenFile(filepath.Join(dir, "actual.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer file.Close()
			path := filepath.Join(append([]string{dir}, tc.path...)...)
			lg := &Logger{file: file, maxBytes: 1, path: path}
			defer lg.Close()

			err = lg.rotate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rotate error = %v, want one containing %q", err, tc.wantErr)
			}
			if (lg.file != nil) != tc.reopened {
				t.Fatalf("logger holds a file = %v, want %v", lg.file != nil, tc.reopened)
			}
			if _, statErr := os.Stat(path); (statErr == nil) != tc.reopened {
				t.Fatalf("log file at %s exists = %v, want %v", path, statErr == nil, tc.reopened)
			}
		})
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
