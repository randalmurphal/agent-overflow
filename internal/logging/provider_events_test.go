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
		Data:      json.RawMessage(`{"jsonrpc":"2.0"}`),
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
	if string(got.Data) != `{"jsonrpc":"2.0"}` {
		t.Fatalf("Data = %q, want raw payload", got.Data)
	}
	if got.Timestamp == "" {
		t.Fatal("expected timestamp to be populated")
	}
}

// TestLogProviderEventEmbedsRawJSON pins the alloc contract: a valid
// JSON payload lands in the line as a raw value (no string escaping),
// and a multi-line payload is compacted so it cannot break NDJSON
// framing.
func TestLogProviderEventEmbedsRawJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := lg.LogProviderEvent(ProviderEventEntry{
		ThreadID: "t", Direction: "in", Provider: "claude",
		Data: json.RawMessage("{\n  \"a\": 1\n}"),
	}); err != nil {
		t.Fatalf("LogProviderEvent: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if strings.Contains(line, "\n") {
		t.Fatalf("multi-line payload broke NDJSON framing: %q", line)
	}
	if !strings.Contains(line, `"data":{"a":1}`) {
		t.Fatalf("payload not embedded as compacted raw JSON: %q", line)
	}
}

// TestLogProviderEventQuotesNonJSON pins the fallback: a payload that
// is not valid JSON still logs, as the old quoted-string form.
func TestLogProviderEventQuotesNonJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := lg.LogProviderEvent(ProviderEventEntry{
		ThreadID: "t", Direction: "in", Provider: "claude",
		Data: json.RawMessage("not json at all"),
	}); err != nil {
		t.Fatalf("LogProviderEvent: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got ProviderEventEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var s string
	if err := json.Unmarshal(got.Data, &s); err != nil || s != "not json at all" {
		t.Fatalf("fallback = %q (err %v), want quoted original", got.Data, err)
	}
}
