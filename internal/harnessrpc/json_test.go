package harnessrpc

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHarnessSeedRefusesBadDocuments: a seed document that is not one
// strict, well-formed spec is refused with an error that says what is
// wrong and, where it applies, where.
func TestHarnessSeedRefusesBadDocuments(t *testing.T) {
	h, _ := newHarnessTestHost(t)
	for _, tc := range []struct {
		name, doc string
		want      []string
	}{
		// The reason the RPC takes raw JSON. A mistyped key used to seed
		// nothing and return success; the caller's assertions then failed
		// several steps later, describing a missing thread rather than the
		// typo that caused it. The error names the field and says WHERE,
		// which is the whole point in a generated spec.
		{"unknown field", `{"projects":[{"name":"a","treads":[]}]}`, []string{"treads", "line "}},
		// The same document format's scenario half has always been strict
		// and positioned; the seed half matches.
		{"syntax error", "{\n  \"projects\": [\n    {\"name\": }\n  ]\n}", []string{"seed spec", "line 3", "column ", "byte offset "}},
		{"empty", "   ", []string{"empty"}},
		// Obeying only the first of two documents in one frame would
		// silently drop half of what the caller wrote.
		{"two documents", `{"projects":[]} {"projects":[]}`, []string{"trailing content"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.HarnessSeed(json.RawMessage(tc.doc))
			if err == nil {
				t.Fatal("HarnessSeed accepted the document")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q omits %q", err, want)
				}
			}
		})
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
