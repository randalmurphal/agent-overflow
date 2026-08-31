package harnessrpc

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHarnessSeedRefusesUnknownFields is the reason the RPC takes raw
// JSON. A mistyped key used to seed nothing and return success — the
// caller's assertions then failed several steps later, describing a
// missing thread rather than the typo that caused it.
func TestHarnessSeedRefusesUnknownFields(t *testing.T) {
	h, _ := newHarnessTestHost(t)

	_, err := h.HarnessSeed(json.RawMessage(`{"projects":[{"name":"a","treads":[]}]}`))
	if err == nil {
		t.Fatal("HarnessSeed accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "treads") {
		t.Errorf("error %q does not name the offending field", err)
	}
	// And it says WHERE, which is the whole point in a generated spec.
	if !strings.Contains(err.Error(), "line ") {
		t.Errorf("error %q carries no position", err)
	}
}

// TestHarnessSeedReportsSyntaxPosition: the same document format's
// scenario half has always been strict and positioned; the seed half now
// matches.
func TestHarnessSeedReportsSyntaxPosition(t *testing.T) {
	h, _ := newHarnessTestHost(t)

	_, err := h.HarnessSeed(json.RawMessage("{\n  \"projects\": [\n    {\"name\": }\n  ]\n}"))
	if err == nil {
		t.Fatal("HarnessSeed accepted malformed JSON")
	}
	for _, want := range []string{"seed spec", "line 3", "column ", "byte offset "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q omits %q", err, want)
		}
	}
}

func TestHarnessSeedRefusesEmptyAndTrailingDocuments(t *testing.T) {
	h, _ := newHarnessTestHost(t)

	if _, err := h.HarnessSeed(json.RawMessage("   ")); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty spec: err = %v, want an emptiness refusal", err)
	}
	// Two documents in one frame: obeying only the first would silently
	// drop half of what the caller wrote.
	_, err := h.HarnessSeed(json.RawMessage(`{"projects":[]} {"projects":[]}`))
	if err == nil || !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("two documents: err = %v, want a trailing-content refusal", err)
	}
}

// TestHarnessSeedStillAcceptsAValidSpec: strictness must not have made
// the ordinary path harder — every legal field still decodes.
func TestHarnessSeedStillAcceptsAValidSpec(t *testing.T) {
	h, _ := newHarnessTestHost(t)

	result, err := h.HarnessSeed(json.RawMessage(
		`{"projects":[{"name":"strict","repo":{},"threads":[{"title":"T","provider":"claude"}]}]}`,
	))
	if err != nil {
		t.Fatalf("HarnessSeed on a valid spec: %v", err)
	}
	if len(result.Projects) != 1 || len(result.Projects[0].ThreadIDs) != 1 {
		t.Fatalf("seed result = %+v, want one project with one thread", result)
	}
}

func TestJSONPositionSuffix(t *testing.T) {
	raw := []byte("{\n  \"a\": 1,\n  bad\n}")
	// Offsets are 1-based-past-the-character, matching json.SyntaxError.
	if got := jsonPositionSuffix(raw, 15); !strings.Contains(got, "line 3") {
		t.Errorf("suffix = %q, want line 3", got)
	}
	// Unusable offsets render nothing, so a caller can append blind.
	if got := jsonPositionSuffix(raw, 0); got != "" {
		t.Errorf("offset 0 rendered %q", got)
	}
	if got := jsonPositionSuffix(raw, int64(len(raw))+50); got != "" {
		t.Errorf("out-of-range offset rendered %q", got)
	}
}

// TestRequireValidJSONPositionsTheFailure covers the forward-verbatim
// surfaces (ui-query spec + reply, HarnessEmit payload) that validate
// without decoding.
func TestRequireValidJSONPositionsTheFailure(t *testing.T) {
	if err := requireValidJSON("spec", []byte(`{"v":1}`)); err != nil {
		t.Fatalf("valid JSON refused: %v", err)
	}
	err := requireValidJSON("spec", []byte("{\n\"v\": }"))
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
	for _, want := range []string{"spec", "line 2", "byte offset "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q omits %q", err, want)
		}
	}
}
