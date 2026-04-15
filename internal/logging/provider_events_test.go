package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
