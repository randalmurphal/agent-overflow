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

// A unit try's files nest under its phase attempt so a run stays one tree, and
// the try number is in the directory name because a retried unit reuses its row
// but must not overwrite the previous try's narrative — that account of what the
// unit did is the evidence a human retries or drops on.
func TestUnitAttemptPathsNestUnderThePhaseAttempt(t *testing.T) {
	narrative, err := UnitNarrativePath("/data", "item", "port", 2, "port-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/data/workflow-runs/item/port.2/units/port-1.3/narrative.md"; narrative != want {
		t.Fatalf("unit narrative path = %q, want %q", narrative, want)
	}
	envelope, err := UnitEnvelopePath("/data", "item", "port", 2, "port-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/data/workflow-runs/item/port.2/units/port-1.3/envelope.json"; envelope != want {
		t.Fatalf("unit envelope path = %q, want %q", envelope, want)
	}
	dir, err := UnitAttemptDir("/data", "item", "port", 2, "port-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(narrative) != dir || filepath.Dir(envelope) != dir {
		t.Fatalf("unit files escaped the try directory %q", dir)
	}
	if first, err := UnitNarrativePath("/data", "item", "port", 2, "port-1", 1); err != nil || first == narrative {
		t.Fatalf("try 1 narrative = %q err=%v, want a path distinct from try 3", first, err)
	}
	for _, tc := range []struct {
		name        string
		unitID      string
		unitAttempt int
	}{
		{"blank unit id", "  ", 1},
		{"traversing unit id", "../escape", 1},
		{"separator in unit id", "port/1", 1},
		{"zero try", "port-1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if path, err := UnitNarrativePath("/data", "item", "port", 2, tc.unitID, tc.unitAttempt); err == nil {
				t.Fatalf("unusable unit identity produced %q", path)
			}
		})
	}
}

// A unit's prompt is built from the declarations its role gets, so the same
// builder renders a work unit's element binding and a join's reserved results
// without either being able to read the other's.
func TestBuildUnitPromptRendersRoleDeclarations(t *testing.T) {
	work := def.Unit{ID: "port-0", Provider: "claude", Model: "sonnet", Prompt: "port {{section.path}}"}
	prompt, err := BuildUnitPrompt(work,
		map[string]def.Variable{"section": {Schema: def.JSONSchema{
			Type:       "object",
			Properties: map[string]def.JSONSchema{"path": {Type: "string"}},
			Required:   []string{"path"},
		}}},
		map[string]any{"section": map[string]any{"path": "internal/a"}},
		"/data/workflow-runs/item/port.1/units/port-0.1/narrative.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, "port internal/a\n\n<workflow-system-instructions>") {
		t.Fatalf("work unit prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "/units/port-0.1/narrative.md") {
		t.Fatalf("work unit prompt does not name its own narrative: %q", prompt)
	}

	join := def.Unit{ID: "merge", Provider: "claude", Model: "sonnet", Prompt: "merge {{units}}"}
	joined, err := BuildUnitPrompt(join,
		def.JoinDeclarations(def.Phase{ID: "port"}),
		map[string]any{def.UnitsVariable: []any{
			map[string]any{"id": "port-0", "index": 0, "status": "done", "outputs": map[string]any{"file": "a.go"}},
		}},
		"/data/workflow-runs/item/port.1/units/merge.1/narrative.md",
		&engine.Feedback{Note: "prefer the second"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined, `"id":"port-0"`) || !strings.Contains(joined, `"file":"a.go"`) {
		t.Fatalf("join prompt did not render the units binding: %q", joined)
	}
	if !strings.Contains(joined, "prefer the second") {
		t.Fatalf("join prompt dropped its feedback: %q", joined)
	}

	// An undeclared reference fails the build rather than reaching a provider as
	// a literal `{{...}}` the model would try to interpret.
	if _, err := BuildUnitPrompt(work, nil, nil, "/data/n.md", nil); err == nil {
		t.Fatal("a unit prompt with an undeclared reference built successfully")
	}
}
