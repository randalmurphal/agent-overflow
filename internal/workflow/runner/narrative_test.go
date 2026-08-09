package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// The recovered narrative is marked as recovered and carries the session's LAST
// prose, which is the account a phase writes just before it closes out.
func TestRecoverNarrativeTakesTheFinalProse(t *testing.T) {
	got, ok := RecoverNarrative(
		[]string{"first I looked around", "  ", "then I checked the callers and found two"},
		json.RawMessage(`{"status":"done","outputs":{"report":"ok"}}`),
	)
	if !ok {
		t.Fatal("RecoverNarrative reported nothing to write")
	}
	if !strings.HasPrefix(got, RecoveredNarrativeHeader+"\n\n") {
		t.Fatalf("recovered narrative is not marked as recovered:\n%s", got)
	}
	if !strings.HasSuffix(got, "then I checked the callers and found two\n") {
		t.Fatalf("recovered narrative did not end with the final prose:\n%s", got)
	}
	if strings.Contains(got, "first I looked around") {
		t.Fatalf("recovered narrative concatenated earlier turns:\n%s", got)
	}
}

// A provider whose structured output IS its final message (Codex) must not have
// that JSON recovered as the narrative. The comparison is on the decoded
// document, so neither indentation nor key order can make the match miss.
func TestRecoverNarrativeSkipsTheEnvelopeItself(t *testing.T) {
	envelope := json.RawMessage(`{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`)
	got, ok := RecoverNarrative([]string{
		"the report is in outputs",
		"{\n  \"question\": null,\n  \"reason\": null,\n  \"outputs\": {\"report\": \"ok\"},\n  \"status\": \"done\"\n}",
	}, envelope)
	if !ok {
		t.Fatal("RecoverNarrative reported nothing to write")
	}
	if !strings.Contains(got, "the report is in outputs") {
		t.Fatalf("recovered narrative did not fall back past the envelope echo:\n%s", got)
	}
	if strings.Contains(got, `"status"`) {
		t.Fatalf("recovered narrative carried the envelope:\n%s", got)
	}
}

// Prose that merely happens to be JSON, but is not this turn's envelope, is
// still prose: the rule is identity with the envelope, never "looks like JSON".
func TestRecoverNarrativeKeepsJSONThatIsNotTheEnvelope(t *testing.T) {
	got, ok := RecoverNarrative(
		[]string{`{"finding":"the schema is wrong"}`},
		json.RawMessage(`{"status":"done","outputs":{}}`),
	)
	if !ok || !strings.Contains(got, "the schema is wrong") {
		t.Fatalf("RecoverNarrative(%v) = %q, %v", "json prose", got, ok)
	}
}

// Absence stays absence. A session that said nothing gets no file rather than a
// file that says nothing, so a reader can tell the two apart.
func TestRecoverNarrativeReportsNothingToWrite(t *testing.T) {
	envelope := json.RawMessage(`{"status":"stuck","reason":"blocked"}`)
	for name, texts := range map[string][]string{
		"no texts":      nil,
		"blank texts":   {"", "   \n\t"},
		"only envelope": {`{"status":"stuck","reason":"blocked"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := RecoverNarrative(texts, envelope); ok {
				t.Fatalf("RecoverNarrative recovered %q from nothing", got)
			}
		})
	}
}

// An outcome with no envelope (a takeover finalize whose payload the caller does
// not hold) must not make every text look like an envelope echo.
func TestRecoverNarrativeWithoutAnEnvelope(t *testing.T) {
	got, ok := RecoverNarrative([]string{"I reviewed the steered work and it holds"}, nil)
	if !ok || !strings.Contains(got, "I reviewed the steered work and it holds") {
		t.Fatalf("RecoverNarrative with no envelope = %q, %v", got, ok)
	}
}

// Codex applies a turn's outputSchema to EVERY assistant message, so falling
// back past the accepted envelope lands on more envelope JSON. Recovering that
// blob would be worse than recovering nothing: what gets lifted is the account
// the document carries, never its raw JSON.
func TestRecoverNarrativeLiftsTheAccountOutOfAnEnvelopeShapedCandidate(t *testing.T) {
	accepted := json.RawMessage(`{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`)
	for name, testCase := range map[string]struct {
		texts []string
		want  string
	}{
		"narrative field": {
			[]string{`{"status":"done","outputs":null,"narrative":"I surveyed the callers and found two"}`},
			"I surveyed the callers and found two",
		},
		"reason fallback": {
			[]string{`{"status":"stuck","reason":"the resolver has no binding for this project"}`},
			"the resolver has no binding for this project",
		},
		"skips past an accountless envelope": {
			[]string{"I surveyed the callers", `{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null,"narrative":null}`},
			"I surveyed the callers",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := RecoverNarrative(testCase.texts, accepted)
			if !ok {
				t.Fatal("RecoverNarrative reported nothing to write")
			}
			if !strings.HasPrefix(got, RecoveredNarrativeHeader+"\n\n") {
				t.Fatalf("lifted account is not marked as recovered:\n%s", got)
			}
			if !strings.Contains(got, testCase.want) {
				t.Fatalf("recovered %q, want it to carry %q", got, testCase.want)
			}
			if strings.Contains(got, `"status"`) {
				t.Fatalf("recovered narrative carried raw envelope JSON:\n%s", got)
			}
		})
	}

	// An envelope-shaped candidate with no account at all is skipped exactly
	// like the echo, so absence still stays absence.
	if got, ok := RecoverNarrative([]string{`{"status":"done","outputs":{"report":"ok"}}`}, accepted); ok {
		t.Fatalf("RecoverNarrative recovered %q from an accountless envelope", got)
	}
}
