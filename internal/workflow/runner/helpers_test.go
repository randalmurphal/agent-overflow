package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestNarrativePath(t *testing.T) {
	got, err := NarrativePath(t.TempDir(), "item", "build", 2)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := filepath.Join("workflow-runs", "item", "build.2", "narrative.md")
	if !filepath.IsAbs(got) || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("NarrativePath = %q, want absolute path ending %q", got, wantSuffix)
	}
	if _, err := NarrativePath("", "item", "build", 2); err == nil {
		t.Fatal("NarrativePath accepted an empty root")
	}
}

func TestBuildPromptSuffixShape(t *testing.T) {
	phase := def.Phase{ID: "build", Prompt: "Goal: {{goal}}", Inputs: map[string]def.Variable{
		"goal": {Schema: def.JSONSchema{Type: "string"}},
	}}
	narrative := filepath.Join(t.TempDir(), "narrative.md")
	got, err := BuildPrompt(phase, map[string]any{"goal": "ship"}, narrative, &engine.Feedback{
		Note: "address review", Values: map[string]any{"review.ok": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The envelope rules must reach the model verbatim: def.ValidateEnvelope
	// enforces them and cannot express them in the schema, so a silent drop
	// here costs every phase its one envelope retry.
	const envelopeRules = "Your final message must satisfy the attached schema; status must be done, question, or stuck.\n" +
		"Exactly one branch may be populated, and the other two fields must be null:\n" +
		"- status done: outputs must be non-null; question and reason must both be null.\n" +
		"- status question: question must be a non-empty string; outputs and reason must both be null.\n" +
		"- status stuck: reason must be a non-empty string; outputs and question must both be null.\n"
	header := "Goal: ship\n\n<workflow-system-instructions>\n" +
		"Write a concise narrative of the work performed, decisions made, and validation results to this file:\n" + narrative +
		"\nThe narrative is for human inspection and is not part of the control envelope.\n"
	want := header +
		"<workflow-feedback>\nNote:\naddress review\nValues:\n```json\n{\n  \"review.ok\": false\n}\n```\n</workflow-feedback>\n" +
		envelopeRules +
		"</workflow-system-instructions>"
	if got != want {
		t.Fatalf("BuildPrompt() =\n%s\nwant:\n%s", got, want)
	}
	withoutFeedback, err := BuildPrompt(phase, map[string]any{"goal": "ship"}, narrative, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantWithoutFeedback := header + envelopeRules + "</workflow-system-instructions>"
	if withoutFeedback != wantWithoutFeedback {
		t.Fatalf("BuildPrompt() without feedback =\n%s\nwant:\n%s", withoutFeedback, wantWithoutFeedback)
	}
}

func TestOutcomeFromEnvelope(t *testing.T) {
	for status, want := range map[string]engine.OutcomeKind{
		"done": engine.OutcomeDone, "question": engine.OutcomeQuestion, "stuck": engine.OutcomeStuck,
	} {
		payload := json.RawMessage(`{"status":"` + status + `"}`)
		got, err := OutcomeFromEnvelope(payload)
		if err != nil || got.Kind != want || string(got.Envelope) != string(payload) {
			t.Fatalf("OutcomeFromEnvelope(%s) = %+v, %v", status, got, err)
		}
	}
	if _, err := OutcomeFromEnvelope(json.RawMessage(`{"status":"other"}`)); err == nil {
		t.Fatal("unknown envelope status succeeded")
	}
}

func TestBuildTakeoverFinalizePrompt(t *testing.T) {
	prompt, err := BuildTakeoverFinalizePrompt(filepath.Join(t.TempDir(), "narrative.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Do not redo the original phase") || !strings.Contains(prompt, "final workflow control envelope") {
		t.Fatalf("finalize prompt = %q", prompt)
	}
}

func TestRetryMessage(t *testing.T) {
	got := RetryMessage([]def.EnvelopeFinding{{Path: "$.outputs.ok", Message: "property is required"}})
	if !strings.Contains(got, "- $.outputs.ok: property is required") {
		t.Fatalf("RetryMessage = %q", got)
	}
	if got := RetryMessage(nil); !strings.Contains(got, "structured output was absent") {
		t.Fatalf("absent RetryMessage = %q", got)
	}
}
