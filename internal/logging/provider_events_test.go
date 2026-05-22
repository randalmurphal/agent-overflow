package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogProviderEventWritesExpectedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-events.ndjson")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	if err := lg.LogProviderEvent(ProviderEventEntry{
		ThreadID:  "thread-1",
		Direction: "out",
		Provider:  "codex",
		Data:      `{"jsonrpc":"2.0"}`,
	}); err != nil {
		t.Fatalf("LogProviderEvent: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var got ProviderEventEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ThreadID != "thread-1" {
		t.Fatalf("ThreadID = %q, want %q", got.ThreadID, "thread-1")
	}
	if got.Direction != "out" {
		t.Fatalf("Direction = %q, want %q", got.Direction, "out")
	}
	if got.Provider != "codex" {
		t.Fatalf("Provider = %q, want %q", got.Provider, "codex")
	}
	if got.Data != `{"jsonrpc":"2.0"}` {
		t.Fatalf("Data = %q, want raw payload", got.Data)
	}
	if got.Timestamp == "" {
		t.Fatal("expected timestamp to be populated")
	}
}

func TestPruneOlderThanRemovesAgedFiles(t *testing.T) {
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	now := time.Now()
	cutoff := now.Add(-7 * 24 * time.Hour)

	// Mirror NewProviderEventLogger's local-time format so the active-
	// file stub matches what the logger would have written.
	todayStub := now.Format("2006-01-02")
	activeFile := "provider-events-" + todayStub + ".ndjson"

	files := []struct {
		name      string
		mtime     time.Time
		wantGone  bool
		describe  string
		writeData []byte
	}{
		{
			name:     "provider-events-2026-04-01.ndjson",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: true,
			describe: "old day, current file: removed",
		},
		{
			name:     "provider-events-2026-04-01.ndjson.1",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: true,
			describe: "old day, rotation backup: removed",
		},
		{
			name:     "provider-events-2026-04-15.ndjson.3",
			mtime:    now.Add(-1 * time.Hour),
			wantGone: false,
			describe: "old day name but recent mtime: kept",
		},
		{
			name:     activeFile,
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: false,
			describe: "today's active file with old mtime: kept (clock skew guard)",
		},
		{
			name:     activeFile + ".1",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: true,
			describe: "today's rotation backup with old mtime: removed",
		},
		{
			name:     "unrelated.log",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: false,
			describe: "unrelated file: never touched",
		},
		{
			name:     "provider-events-not-a-date.ndjson",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: false,
			describe: "malformed date stub: doesn't match pattern, untouched",
		},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", f.name, err)
		}
		if err := os.Chtimes(path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.name, err)
		}
	}

	got, err := PruneOlderThan(baseDir, now, cutoff)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}

	wantRemoved := 0
	for _, f := range files {
		if f.wantGone {
			wantRemoved++
		}
	}
	if got != wantRemoved {
		t.Fatalf("PruneOlderThan returned count = %d, want %d", got, wantRemoved)
	}

	for _, f := range files {
		_, err := os.Stat(filepath.Join(dir, f.name))
		switch {
		case f.wantGone && err == nil:
			t.Errorf("%s (%s): still present, expected removal", f.name, f.describe)
		case !f.wantGone && err != nil:
			t.Errorf("%s (%s): missing, expected preservation: %v", f.name, f.describe, err)
		}
	}
}

func TestPruneOlderThanMissingDir(t *testing.T) {
	// baseDir exists but logs/ subdirectory does not — exercise the
	// os.IsNotExist branch through the joined path.
	baseDir := t.TempDir()
	now := time.Now()
	n, err := PruneOlderThan(baseDir, now, now)
	if err != nil {
		t.Fatalf("missing logs dir should be (0, nil): %v", err)
	}
	if n != 0 {
		t.Fatalf("missing logs dir count = %d, want 0", n)
	}
}

func TestProviderEventLoggingEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty", value: "", want: false},
		{name: "provider", value: "provider", want: true},
		{name: "all", value: "all", want: true},
		{name: "multiple includes provider", value: "rpc,provider,background", want: true},
		{name: "multiple excludes provider", value: "rpc,background", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerEventLoggingEnabled(tt.value); got != tt.want {
				t.Fatalf("providerEventLoggingEnabled(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
