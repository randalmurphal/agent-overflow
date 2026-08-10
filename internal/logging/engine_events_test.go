package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogEngineEventWritesExpectedShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.ndjson")
	lg, err := NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer lg.Close()

	if err := lg.LogEngineEvent(EngineEventEntry{
		Event: "park", ItemID: "run-1", ProjectID: "project-1",
		PhaseID: "implement", Attempt: 2, State: "needs-human", Reason: "setup-failed",
		Message: `provision worktree: branch "ao/wave-3" already exists`,
	}); err != nil {
		t.Fatalf("LogEngineEvent: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got EngineEventEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got.Timestamp = ""
	want := EngineEventEntry{
		Event: "park", ItemID: "run-1", ProjectID: "project-1",
		PhaseID: "implement", Attempt: 2, State: "needs-human", Reason: "setup-failed",
		Message: `provision worktree: branch "ao/wave-3" already exists`,
	}
	if got != want {
		t.Fatalf("entry = %+v, want %+v", got, want)
	}

	// RFC3339Nano: a tree coming down settles several records inside one
	// millisecond, and their order is what reconstructs the teardown.
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	timestamp, _ := raw["ts"].(string)
	if !strings.Contains(timestamp, ".") {
		t.Fatalf("ts = %q, want sub-second precision", timestamp)
	}
}

// The provider-event logger is an opt-in; this one is not. A run parks once,
// and there is no second chance to have enabled the log beforehand.
func TestNewEngineEventLoggerNeedsNoEnvGate(t *testing.T) {
	t.Setenv("AGENT_OVERFLOW_DEBUG", "")
	baseDir := t.TempDir()

	providerLogger, err := NewProviderEventLogger(baseDir)
	if err != nil {
		t.Fatalf("NewProviderEventLogger: %v", err)
	}
	if providerLogger != nil {
		providerLogger.Close()
		t.Fatal("the provider-event logger opened without its env gate")
	}

	engineLogger, err := NewEngineEventLogger(baseDir)
	if err != nil {
		t.Fatalf("NewEngineEventLogger: %v", err)
	}
	if engineLogger == nil {
		t.Fatal("the engine logger declined to open")
	}
	defer engineLogger.Close()
	if err := engineLogger.LogEngineEvent(EngineEventEntry{Event: "park", ItemID: "run-1"}); err != nil {
		t.Fatalf("LogEngineEvent: %v", err)
	}

	// It lands where PruneOlderThan looks for it, under the name that names it.
	entries, err := os.ReadDir(filepath.Join(baseDir, "logs"))
	if err != nil {
		t.Fatalf("read logs dir: %v", err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "engine-") ||
		!strings.HasSuffix(entries[0].Name(), ".ndjson") {
		t.Fatalf("logs dir = %v, want one engine-YYYY-MM-DD.ndjson", entries)
	}
}
