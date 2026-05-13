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
