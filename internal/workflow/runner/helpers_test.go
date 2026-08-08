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
	phase := def.Phase{ID: "build", Prompt: "Goal: {{goal}}", Access: def.AccessWrite, Inputs: map[string]def.Variable{
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
		"- status question: a decision only a human can make; question must be a non-empty string; outputs and reason must both be null.\n" +
		"- status stuck: you cannot proceed and retrying will not change that; reason must be a non-empty string; outputs and question must both be null.\n"
	header := "Goal: ship\n\n<workflow-system-instructions>\n" +
		"Write a concise narrative of the work performed, decisions made, and validation results to this file:\n" + narrative +
		"\nThe narrative is for human inspection and is not part of the control envelope.\n" +
		// The workspace default is system-owned and unconditional: a phase that
		// switches branches on its own moves the ground under every later phase
		// of the same call tree, which shares one branch down the stack.
		"Work only in this workspace on its current branch; do not switch branches, merge, or push unless your prompt says to.\n" +
		// Nothing in the engine commits, and everything downstream — a later
		// phase, a unit worktree cut, a join's merge — reads the branch rather
		// than the checkout, which a done join then removes. A writing element
		// that is not told this rests on work nothing can see.
		"Leave your work committed on this branch before you finish: later phases, worktree cuts, and fan-out merges read this branch's commits, never its working tree. Leave nothing uncommitted unless your prompt says otherwise.\n"
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
	narrative := filepath.Join(t.TempDir(), "narrative.md")
	prompt, err := BuildTakeoverFinalizePrompt(narrative, def.AccessWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Do not redo the original phase") || !strings.Contains(prompt, "final workflow control envelope") {
		t.Fatalf("finalize prompt = %q", prompt)
	}
	if !strings.Contains(prompt, narrative) {
		t.Fatalf("write-access finalize prompt dropped the narrative path: %q", prompt)
	}
	// A takeover steers the phase's own session, which keeps the runtime mode the
	// declaration mapped to — so a read-only phase's finalize turn cannot write
	// the file either, and must not be told to.
	readOnly, err := BuildTakeoverFinalizePrompt(narrative, def.AccessReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readOnly, narrative) {
		t.Fatalf("read-only finalize prompt named a file it cannot write: %q", readOnly)
	}
}

// A read-only element runs in a session that denies every file write, so the
// suffix must ask for the narrative as a message. Instructing it to write the
// file is an instruction it cannot follow — the defect that left every read-only
// run's attempt directory empty.
func TestPromptSuffixAsksReadOnlyElementsForAMessage(t *testing.T) {
	narrative := filepath.Join(t.TempDir(), "narrative.md")
	const readOnlyInstruction = "You run read-only and cannot write files, so send your narrative as a message instead: " +
		"a concise account of the work performed, decisions made, and validation results, " +
		"as the message immediately before your final envelope.\n" +
		"The narrative is for human inspection and is not part of the control envelope.\n"

	// Unset access is read-only (def.DefaultAccess), so both spellings take the
	// message form — the default must not fall through to the file instruction.
	for _, access := range []def.Access{def.AccessReadOnly, ""} {
		suffix, err := PromptSuffix(narrative, access, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(suffix, readOnlyInstruction) {
			t.Fatalf("PromptSuffix(access=%q) =\n%s\nwant it to contain:\n%s", access, suffix, readOnlyInstruction)
		}
		if strings.Contains(suffix, narrative) {
			t.Fatalf("PromptSuffix(access=%q) named a file the session cannot write:\n%s", access, suffix)
		}
		// A read-only element has nothing to commit, so the commit default is
		// another instruction it could not follow.
		if strings.Contains(suffix, "Leave your work committed") {
			t.Fatalf("PromptSuffix(access=%q) told a read-only element to commit:\n%s", access, suffix)
		}
	}

	writing, err := PromptSuffix(narrative, def.AccessWrite, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(writing, "results to this file:\n"+narrative+"\n") {
		t.Fatalf("write-access suffix lost the file instruction:\n%s", writing)
	}
	if strings.Contains(writing, "cannot write files") {
		t.Fatalf("write-access suffix took the read-only form:\n%s", writing)
	}
	if !strings.Contains(writing, "Leave your work committed on this branch before you finish") {
		t.Fatalf("write-access suffix lost the commit default:\n%s", writing)
	}

	// The path is validated on both branches: the runner writes there either way.
	if _, err := PromptSuffix("relative/narrative.md", def.AccessReadOnly, nil); err == nil {
		t.Fatal("PromptSuffix accepted a relative narrative path for a read-only element")
	}
}

// A unit and a join carry their own access declaration, so the suffix each one
// gets has to come from the unit — not from the phase, which for a fan-out is
// forbidden to declare access at all.
func TestBuildUnitPromptSuffixFollowsTheUnitAccess(t *testing.T) {
	narrative := "/data/workflow-runs/item/port.1/units/port-0.1/narrative.md"
	readOnly, err := BuildUnitPrompt(
		def.Unit{ID: "port-0", Provider: "claude", Prompt: "look"}, nil, nil, narrative, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOnly, "cannot write files") || strings.Contains(readOnly, narrative) {
		t.Fatalf("read-only unit prompt = %q", readOnly)
	}
	writing, err := BuildUnitPrompt(
		def.Unit{ID: "port-0", Provider: "claude", Prompt: "port", Access: def.AccessWrite},
		nil, nil, narrative, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(writing, narrative) || strings.Contains(writing, "cannot write files") {
		t.Fatalf("writing unit prompt = %q", writing)
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
	work := def.Unit{
		ID: "port-0", Provider: "claude", Model: "sonnet",
		Prompt: "port {{section.path}}", Access: def.AccessWrite,
	}
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
