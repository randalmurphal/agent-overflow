package itemmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeMeta(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func TestMarkImportedEmptyMeta(t *testing.T) {
	got, err := MarkImported("", "8f0a-uuid")
	if err != nil {
		t.Fatalf("MarkImported(\"\"): %v", err)
	}
	m := decodeMeta(t, got)
	if len(m) != 1 {
		t.Errorf("marked empty meta = %q, want single-key object", got)
	}
	if m[ImportSourceUUIDKey] != "8f0a-uuid" {
		t.Errorf("marked empty meta = %q, want %s=8f0a-uuid", got, ImportSourceUUIDKey)
	}
}

func TestMarkImportedPreservesOtherKeys(t *testing.T) {
	got, err := MarkImported(`{"toolName":"Edit","input":{"file_path":"a.go"}}`, "row-7")
	if err != nil {
		t.Fatalf("MarkImported: %v", err)
	}
	m := decodeMeta(t, got)
	if m["toolName"] != "Edit" {
		t.Errorf("toolName lost: %q", got)
	}
	input, ok := m["input"].(map[string]any)
	if !ok || input["file_path"] != "a.go" {
		t.Errorf("nested input lost: %q", got)
	}
	if m[ImportSourceUUIDKey] != "row-7" {
		t.Errorf("provenance missing: %q", got)
	}
}

// Re-marking the same row (a refresh re-walking a row it already
// imported) must land on the same bytes, not accumulate.
func TestMarkImportedIdempotent(t *testing.T) {
	once, err := MarkImported(`{"foo":"bar"}`, "row-1")
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}
	twice, err := MarkImported(once, "row-1")
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if twice != once {
		t.Errorf("second mark rewrote the meta: %q -> %q", once, twice)
	}
}

func TestMarkImportedRejectsMalformedMeta(t *testing.T) {
	if _, err := MarkImported(`{not json`, "row-1"); err == nil {
		t.Fatal("expected error for malformed meta — a dropped provenance stamp leaves an unrefreshable thread")
	}
}

func TestMarkImportedRejectsEmptySourceUUID(t *testing.T) {
	for _, uuid := range []string{"", "   "} {
		if _, err := MarkImported(`{"foo":"bar"}`, uuid); err == nil {
			t.Errorf("MarkImported(_, %q) = nil error, want rejection", uuid)
		}
	}
}

func TestMarkImportUnavailableEmptyMeta(t *testing.T) {
	got, err := MarkImportUnavailable("", "tool-output-gc")
	if err != nil {
		t.Fatalf("MarkImportUnavailable(\"\"): %v", err)
	}
	m := decodeMeta(t, got)
	if len(m) != 1 || m[ImportUnavailableKey] != "tool-output-gc" {
		t.Errorf("marked empty meta = %q, want single-key %s=tool-output-gc", got, ImportUnavailableKey)
	}
}

func TestMarkImportUnavailableMergesIntoExistingMeta(t *testing.T) {
	marked, err := MarkImported(`{"toolName":"Bash"}`, "row-3")
	if err != nil {
		t.Fatalf("MarkImported: %v", err)
	}
	got, err := MarkImportUnavailable(marked, "exec-detail")
	if err != nil {
		t.Fatalf("MarkImportUnavailable: %v", err)
	}
	m := decodeMeta(t, got)
	if m["toolName"] != "Bash" {
		t.Errorf("toolName lost: %q", got)
	}
	if m[ImportSourceUUIDKey] != "row-3" {
		t.Errorf("provenance lost through the unavailable stamp: %q", got)
	}
	if m[ImportUnavailableKey] != "exec-detail" {
		t.Errorf("reason missing: %q", got)
	}
}

func TestMarkImportUnavailableRejectsMalformedMeta(t *testing.T) {
	if _, err := MarkImportUnavailable(`{not json`, "exec-detail"); err == nil {
		t.Fatal("expected error for malformed meta")
	}
}

func TestMarkImportUnavailableRejectsEmptyReason(t *testing.T) {
	for _, reason := range []string{"", "  "} {
		if _, err := MarkImportUnavailable(`{"foo":"bar"}`, reason); err == nil {
			t.Errorf("MarkImportUnavailable(_, %q) = nil error, want rejection", reason)
		}
	}
}

// TestMarkImportedPreservesNumericFormatting pins the one way a marker
// stamp could corrupt data it has nothing to do with: decoding numbers
// as float64 re-encodes a 1000000-byte attachment size as `1e+06` and
// rounds anything past 2^53. Every marker writer in this package shares
// mergeKey, so this covers them all.
func TestMarkImportedPreservesNumericFormatting(t *testing.T) {
	raw := `{"size":1000000,"offset":9007199254740993,"ratio":0.125}`
	got, err := MarkImported(raw, "uuid-1")
	if err != nil {
		t.Fatalf("MarkImported: %v", err)
	}
	for _, want := range []string{`"size":1000000`, `"offset":9007199254740993`, `"ratio":0.125`} {
		if !strings.Contains(got, want) {
			t.Errorf("MarkImported rewrote a number: %q missing %q", got, want)
		}
	}
}
