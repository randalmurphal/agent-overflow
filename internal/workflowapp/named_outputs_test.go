package workflowapp

import (
	"encoding/json"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// outputsSnapshot is a frozen run snapshot whose workflow declares report
// (required) and notes (optional), both from phase work.
func outputsSnapshot(t *testing.T) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{
		ID: "campaign",
		Outputs: map[string]def.WorkflowOutput{
			"report": {From: "work.report"},
			"notes":  {From: "work.notes", Optional: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func workAttempt(attempt int, status, envelope string) store.WorkItemPhaseTimeline {
	return store.WorkItemPhaseTimeline{PhaseID: "work", Attempt: attempt, Status: status, OutputEnvelope: json.RawMessage(envelope)}
}

func namedOutputs(t *testing.T, phases ...store.WorkItemPhaseTimeline) map[string]any {
	t.Helper()
	outputs, err := NamedOutputs(outputsSnapshot(t), phases)
	if err != nil {
		t.Fatal(err)
	}
	return outputs
}

// A retry that withdrew an optional output shows only the retry's values,
// and the run view agrees with what the engine hands later phases.
func TestNamedOutputsTakeEachPhaseFromOneAttempt(t *testing.T) {
	phases := []store.WorkItemPhaseTimeline{
		workAttempt(1, "completed", `{"status":"done","outputs":{"report":"first.md","notes":"draft"}}`),
		workAttempt(2, "completed", `{"status":"done","outputs":{"report":"second.md","notes":null}}`),
	}
	got := namedOutputs(t, phases...)
	if want := map[string]any{"report": "second.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs = %v, want %v", got, want)
	}

	chosen, err := engine.SelectOutputs(phases, timelineAttempt, engine.Completed)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{}
	if err := engine.BindOutputs(vars, chosen); err != nil {
		t.Fatal(err)
	}
	fromEngine := map[string]any{}
	for name, from := range map[string]string{"report": "work.report", "notes": "work.notes"} {
		if value, ok := def.LookupVariable(vars, from); ok {
			fromEngine[name] = value
		}
	}
	if !reflect.DeepEqual(got, fromEngine) {
		t.Fatalf("the run view shows %v, the engine hands later phases %v", got, fromEngine)
	}
}

// A run that ended failed at a gate still shows the outputs of the attempt
// the gate failed, as the run view did before.
func TestNamedOutputsShowAFailedGateAttempt(t *testing.T) {
	got := namedOutputs(t, workAttempt(1, "failed", `{"status":"done","outputs":{"report":"rejected.md","notes":"why"}}`))
	if want := map[string]any{"report": "rejected.md", "notes": "why"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs = %v, want %v", got, want)
	}
}

// A final attempt that failed at its gate replaces an earlier completed
// attempt of its phase whole: none of the earlier values remain.
func TestNamedOutputsTakeAFailedFinalAttemptInsteadOfAnEarlierOne(t *testing.T) {
	got := namedOutputs(t,
		workAttempt(1, "completed", `{"status":"done","outputs":{"report":"first.md","notes":"draft"}}`),
		workAttempt(2, "failed", `{"status":"done","outputs":{"report":"rejected.md"}}`),
	)
	if want := map[string]any{"report": "rejected.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs = %v, want %v", got, want)
	}
}
