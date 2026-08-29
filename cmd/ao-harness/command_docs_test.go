package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandDescriptorsDriveGroupHelp(t *testing.T) {
	for _, name := range []string{"scenario", "mock", "events", "record", "replay", "ui", "perf", "monitor", "compare"} {
		got, ok := commandByName(name)
		if !ok || len(got.children) == 0 {
			t.Fatalf("%s has no command descriptor children", name)
		}
	}
	text := referenceDoc()
	for _, name := range []string{"compare", "postmortem", "events", "from-thread", "idPrefix", "page-id", "Managed run example"} {
		if !strings.Contains(text, name) {
			t.Errorf("generated reference omits %q", name)
		}
	}
}

func TestCheckedInReferenceMatchesGeneratedReference(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "reference", "ao-harness.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checked-in reference: %v", err)
	}
	want := []byte(referenceDoc())
	if !bytes.Equal(data, want) {
		t.Fatalf("%s is stale; run `go generate ./cmd/ao-harness`", path)
	}
}

func TestWriteQueryJSONRequiresExplicitLargeOutputMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr}
	err := e.writeQueryJSON(json.RawMessage(`{"value":"large"}`), &queryOutputBudget{maxBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "--full or --file") {
		t.Fatalf("writeQueryJSON error = %v, want explicit escape hatch", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("refused output wrote %d stdout bytes", stdout.Len())
	}
}

func TestWriteQueryJSONFileModeWritesCompleteResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr, format: "json"}
	raw := json.RawMessage(`{"value":"complete"}`)
	if err := e.writeQueryJSON(raw, &queryOutputBudget{maxBytes: 1, file: path}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"complete"`)) {
		t.Fatalf("file lost complete result: %s", data)
	}
	var metadata map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &metadata); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if metadata["file"] != path || metadata["full"] != true {
		t.Fatalf("metadata = %#v", metadata)
	}
}
