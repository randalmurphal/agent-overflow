package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestWorkItemPhaseAttemptsAndEffects(t *testing.T) {
	s := newTestStore(t)
	for _, phase := range []WorkItemPhase{
		{ItemID: "item", PhaseID: "build", Attempt: 1, ThreadID: "thread-deleted", InputEnvelope: json.RawMessage(`{"input":1}`), Status: "superseded", StartedAt: 10, EndedAt: 11},
		{ItemID: "item", PhaseID: "build", Attempt: 2, ThreadID: "thread-live", InputEnvelope: json.RawMessage(`{"input":2}`), NarrativePath: "/tmp/narrative.md", Status: "running", StartedAt: 20},
		{ItemID: "item", PhaseID: "review", Attempt: 1, Status: "running", StartedAt: 30},
	} {
		if err := s.CreateWorkItemPhase(phase); err != nil {
			t.Fatalf("create phase %s/%d: %v", phase.PhaseID, phase.Attempt, err)
		}
	}
	if err := s.CreateWorkItemPhase(WorkItemPhase{ItemID: "item", PhaseID: "build", Attempt: 2, Status: "running", StartedAt: 40}); err == nil {
		t.Fatal("duplicate phase attempt succeeded")
	}
	if err := s.AttachWorkItemPhaseRun("item", "review", 1, "workflow-thread", "/runs/review/narrative.md"); err != nil {
		t.Fatalf("attach phase run: %v", err)
	}

	output := json.RawMessage(`{"status":"done"}`)
	trace := json.RawMessage(`{"route":"review"}`)
	if err := s.CompleteWorkItemPhase("item", "build", 2, output, trace, "completed", 25); err != nil {
		t.Fatalf("complete phase: %v", err)
	}
	intervention := json.RawMessage(`{"kind":"takeover"}`)
	if err := s.UpdateWorkItemPhaseIntervention("item", "build", 2, intervention); err != nil {
		t.Fatalf("update intervention: %v", err)
	}
	current, err := s.GetCurrentWorkItemPhase("item", "build")
	if err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if current.Attempt != 2 || current.Status != "completed" || current.EndedAt != 25 ||
		string(current.OutputEnvelope) != string(output) || string(current.GateTrace) != string(trace) ||
		string(current.Intervention) != string(intervention) {
		t.Fatalf("current phase = %#v", current)
	}
	phases, err := s.ListWorkItemPhases("item")
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	if len(phases) != 3 || phases[0].Attempt != 1 || phases[1].Attempt != 2 || phases[2].PhaseID != "review" ||
		phases[2].ThreadID != "workflow-thread" || phases[2].NarrativePath != "/runs/review/narrative.md" {
		t.Fatalf("phase list = %#v", phases)
	}
	contexts, err := s.ListWorkItemPhaseContexts("item")
	if err != nil {
		t.Fatalf("list phase contexts: %v", err)
	}
	if len(contexts) != 3 || contexts[0].PhaseID != "build" || contexts[0].Attempt != 1 ||
		contexts[1].Status != "completed" || string(contexts[1].OutputEnvelope) != string(output) ||
		string(contexts[1].GateTrace) != string(trace) || string(contexts[1].Intervention) != string(intervention) {
		t.Fatalf("phase contexts = %#v", contexts)
	}
	timeline, err := s.ListWorkItemPhaseTimeline("item")
	if err != nil {
		t.Fatalf("list phase timeline: %v", err)
	}
	if len(timeline) != 3 || timeline[0].PhaseID != "build" || timeline[0].Attempt != 1 ||
		timeline[1].ThreadID != "thread-live" || string(timeline[1].OutputEnvelope) != string(output) ||
		timeline[1].Status != "completed" || timeline[1].StartedAt != 20 || timeline[1].EndedAt != 25 {
		t.Fatalf("phase timeline = %#v", timeline)
	}

	effect := WorkItemEffect{ItemID: "item", PhaseID: "build", Tool: "report", PayloadHash: "sha256", Payload: json.RawMessage(`{"issue":1}`), CreatedAt: 50}
	if err := s.RecordWorkItemEffect(effect); err != nil {
		t.Fatalf("record effect: %v", err)
	}
	duplicate := effect
	duplicate.Payload = json.RawMessage(`{"issue":2}`)
	duplicate.CreatedAt = 60
	if err := s.RecordWorkItemEffect(duplicate); err != nil {
		t.Fatalf("record duplicate effect: %v", err)
	}
	gotEffect, found, err := s.GetWorkItemEffect("item", "build", "report", "sha256")
	if err != nil || !found {
		t.Fatalf("get effect: found=%t err=%v", found, err)
	}
	if string(gotEffect.Payload) != string(effect.Payload) || gotEffect.CreatedAt != 50 {
		t.Fatalf("duplicate replaced original effect: %#v", gotEffect)
	}
	// A different payload hash is a different effect, and an unrecorded one is
	// a plain miss rather than an error — that distinction is what lets the
	// surface-and-skip check run on every invocation.
	if _, found, err := s.GetWorkItemEffect("item", "build", "report", "other-hash"); err != nil || found {
		t.Fatalf("unrecorded effect = found:%t err:%v, want false/nil", found, err)
	}
	if err := s.CompleteWorkItemPhase("missing", "build", 1, output, trace, "completed", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing phase completion error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateWorkItemPhaseIntervention("missing", "build", 1, intervention); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing phase intervention error = %v, want sql.ErrNoRows", err)
	}
	if err := s.AttachWorkItemPhaseRun("missing", "build", 1, "thread", "/tmp/narrative.md"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing phase attachment error = %v, want sql.ErrNoRows", err)
	}
}
