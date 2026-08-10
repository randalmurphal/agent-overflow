package engine

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// A workflow output is synthesized when the run completes, and until `optional:`
// existed a name with no value killed the tree at the exact transition that
// meant it had succeeded (incident D-C1): a campaign's per-wave handoff was
// produced by a phase the completing route did not run through, and the whole
// call chain parked `wiring-error` on a `done` child.

// handoffChild declares two deliverables — one its work phase always produces
// and one it only sometimes does — so a completion that omits the second is the
// case under test.
func handoffChild(handoff def.WorkflowOutput) def.Workflow {
	work := agentPhase("work", nil, []def.Route{{To: "done"}})
	work.Outputs["notes"] = def.Variable{Schema: def.JSONSchema{Type: "string"}, Optional: true}
	return def.Workflow{
		ID: "child", Phases: []def.Phase{work},
		Outputs: map[string]def.WorkflowOutput{
			"verdict": {From: "work.ok"},
			"handoff": handoff,
		},
	}
}

// settleChild drives a caller to its call phase and completes the child with
// `envelope`, returning the envelope the CALL PHASE ended up with.
func settleChild(t *testing.T, handoff def.WorkflowOutput, envelope json.RawMessage) (*testHarness, string) {
	t.Helper()
	h := newCallHarness(t, map[string]def.Workflow{
		"caller": callerWorkflow("child"),
		"child":  handoffChild(handoff),
	}, nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	h.runner.complete(t, child.ID, Outcome{Kind: OutcomeDone, Envelope: envelope})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	return h, parent
}

// TestOptionalWorkflowOutputIsOmittedRatherThanFailingTheRun — the D-C1 shape.
// The child completes, the optional deliverable has no value, and the name is
// simply absent from the call phase's envelope: the same shape an absent
// optional call argument takes (D45), so the parent's optional input sees the
// "not supplied" a direct start would have produced.
func TestOptionalWorkflowOutputIsOmittedRatherThanFailingTheRun(t *testing.T) {
	h, parent := settleChild(t,
		def.WorkflowOutput{From: "work.notes", Optional: true},
		doneEnvelope(true))

	// The caller routed on `audit.verdict` and moved on, which is the whole
	// claim: a completing child no longer kills the run that called it.
	requireItemState(t, h.store, parent, StateRunning, "")
	envelope := decodeEnvelope(t, h.phaseAttempt(t, parent, "audit", 1).OutputEnvelope)
	if envelope.Status != "done" || envelope.Outputs["verdict"] != true {
		t.Fatalf("call phase envelope = %+v, want the child's produced outputs", envelope)
	}
	// Omitted, not nulled: a present key holding null is a value the parent's
	// gate would route on.
	if _, present := envelope.Outputs["handoff"]; present {
		t.Fatalf("absent optional output was materialized: %+v", envelope.Outputs)
	}
	if h.runner.startFor(t, RunKey{ItemID: parent, PhaseID: "report", Attempt: 1}).Key.PhaseID != "report" {
		t.Fatal("the caller did not advance past its call phase")
	}
}

// TestOptionalWorkflowOutputFlowsWhenProduced — declaring it optional changes
// nothing about the run that DOES produce it.
func TestOptionalWorkflowOutputFlowsWhenProduced(t *testing.T) {
	h, parent := settleChild(t,
		def.WorkflowOutput{From: "work.notes", Optional: true},
		json.RawMessage(`{"status":"done","outputs":{"ok":true,"notes":"wave 2 handed off"}}`))

	envelope := decodeEnvelope(t, h.phaseAttempt(t, parent, "audit", 1).OutputEnvelope)
	if envelope.Outputs["handoff"] != "wave 2 handed off" {
		t.Fatalf("produced optional output = %v, want the child's value", envelope.Outputs["handoff"])
	}
}

// TestRequiredWorkflowOutputStillFailsWhenAbsent — D44 strictness is untouched
// where nobody opted out. A caller's gate routes on these names, so a required
// deliverable with no value is still a `wiring-error` park naming what is
// missing and where it should have come from.
func TestRequiredWorkflowOutputStillFailsWhenAbsent(t *testing.T) {
	h, parent := settleChild(t,
		def.WorkflowOutput{From: "work.notes"},
		doneEnvelope(true))

	requireItemState(t, h.store, parent, StateNeedsHuman, ReasonWiringError)
	requireParkCause(t, h.phaseAttempt(t, parent, "audit", 1),
		"completed without its declared outputs", "handoff (work.notes)")
}
