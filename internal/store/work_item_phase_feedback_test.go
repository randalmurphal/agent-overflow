package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// `work_item_phases.feedback_delivered_at` (v64): the durable half of the
// engine's feedback delivery contract. 0 means the attempt's persisted feedback
// is still owed to a turn, and the redelivery read is what a later attempt of
// the same phase consults so an answer nothing rendered is not lost.

func TestUndeliveredPhaseFeedbackReadIsBoundedToOwedPriorAttempts(t *testing.T) {
	s := newTestStore(t)
	for _, phase := range []WorkItemPhase{
		// Rendered by a session that started, so no longer owed.
		{ItemID: "item", PhaseID: "build", Attempt: 1, InputEnvelope: json.RawMessage(`{"feedback":{"note":"rendered"}}`),
			FeedbackDeliveredAt: 15, Status: "completed", StartedAt: 10, EndedAt: 15},
		// The incident shape: persisted, never rendered.
		{ItemID: "item", PhaseID: "build", Attempt: 2, InputEnvelope: json.RawMessage(`{"feedback":{"note":"use the safe option"}}`),
			Status: "parked", StartedAt: 20, EndedAt: 21},
		// A park recorded before its attempt had any input: nothing to owe, and
		// the read must not pay to ship it.
		{ItemID: "item", PhaseID: "build", Attempt: 3, ParkCause: "workspace would not provision",
			Status: "parked", StartedAt: 30, EndedAt: 30},
		// Another phase entirely — feedback is owed by the attempt, and only the
		// same phase's next attempt can read it.
		{ItemID: "item", PhaseID: "review", Attempt: 1, InputEnvelope: json.RawMessage(`{"feedback":{"note":"other phase"}}`),
			Status: "parked", StartedAt: 40, EndedAt: 41},
	} {
		if err := s.CreateWorkItemPhase(phase); err != nil {
			t.Fatalf("create phase %s/%d: %v", phase.PhaseID, phase.Attempt, err)
		}
	}

	owed, err := s.ListUndeliveredWorkItemPhaseFeedback("item", "build", 4)
	if err != nil {
		t.Fatalf("list undelivered feedback: %v", err)
	}
	if len(owed) != 1 || owed[0].Attempt != 2 ||
		!strings.Contains(string(owed[0].InputEnvelope), "use the safe option") {
		t.Fatalf("owed = %#v, want only attempt 2's unrendered feedback", owed)
	}

	// The attempt bound is what keeps an attempt from redelivering its own note
	// to itself: composing attempt 2 sees nothing owed below it.
	below, err := s.ListUndeliveredWorkItemPhaseFeedback("item", "build", 2)
	if err != nil {
		t.Fatalf("list undelivered feedback below attempt 2: %v", err)
	}
	if len(below) != 0 {
		t.Fatalf("owed below attempt 2 = %#v, want none", below)
	}
}

func TestMarkWorkItemPhaseFeedbackDeliveredSettlesOneAttempt(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateWorkItemPhase(WorkItemPhase{
		ItemID: "item", PhaseID: "build", Attempt: 1,
		InputEnvelope: json.RawMessage(`{"feedback":{"note":"answer"}}`),
		Status:        "running", StartedAt: 10,
	}); err != nil {
		t.Fatalf("create phase: %v", err)
	}

	if err := s.MarkWorkItemPhaseFeedbackDelivered("item", "build", 1, 0); err == nil {
		t.Fatal("an unset delivery clock was accepted; 0 is the column's still-owed value")
	}
	if err := s.MarkWorkItemPhaseFeedbackDelivered("item", "build", 9, 55); err == nil {
		t.Fatal("stamping an attempt that does not exist reported success")
	}
	if err := s.MarkWorkItemPhaseFeedbackDelivered("item", "build", 1, 55); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	current, err := s.GetCurrentWorkItemPhase("item", "build")
	if err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if current.FeedbackDeliveredAt != 55 {
		t.Fatalf("feedback_delivered_at = %d, want 55", current.FeedbackDeliveredAt)
	}
	// The note stays on the row: only the obligation was settled, and the input
	// envelope is still the record of what the attempt ran with.
	if !strings.Contains(string(current.InputEnvelope), "answer") {
		t.Fatalf("input envelope = %s, want the feedback preserved", current.InputEnvelope)
	}
	owed, err := s.ListUndeliveredWorkItemPhaseFeedback("item", "build", 2)
	if err != nil {
		t.Fatalf("list undelivered feedback: %v", err)
	}
	if len(owed) != 0 {
		t.Fatalf("owed after the stamp = %#v, want none", owed)
	}
}

// A reopened attempt keeps its stamp. Reopening does not rewrite the attempt's
// input, so the note on the row is the one a session already rendered, and
// re-owing it would deliver it a second time to the very turn that read it.
func TestReopenWorkItemPhaseKeepsTheFeedbackStamp(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateWorkItemPhase(WorkItemPhase{
		ItemID: "item", PhaseID: "build", Attempt: 1,
		InputEnvelope: json.RawMessage(`{"feedback":{"note":"answer"}}`),
		Status:        "running", StartedAt: 10, FeedbackDeliveredAt: 12,
	}); err != nil {
		t.Fatalf("create phase: %v", err)
	}
	if err := s.CompleteWorkItemPhase("item", "build", 1, nil, nil, "parked", "unit failed", 0, 20); err != nil {
		t.Fatalf("complete phase: %v", err)
	}
	if err := s.ReopenWorkItemPhase("item", "build", 1); err != nil {
		t.Fatalf("reopen phase: %v", err)
	}
	current, err := s.GetCurrentWorkItemPhase("item", "build")
	if err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if current.FeedbackDeliveredAt != 12 || current.ParkCause != "" {
		t.Fatalf("reopened attempt = delivered %d cause %q, want the stamp kept and the cause cleared",
			current.FeedbackDeliveredAt, current.ParkCause)
	}
}

// THE BACKFILL. A bare ADD COLUMN leaves every historical row at 0, which reads
// as "this attempt's feedback is still owed" — so the next attempt of every
// phase any run ever entered would prepend feedback from a round that finished
// months ago. v64's second statement is what makes a pre-migration row
// delivered, and it has to hold for a settled attempt, a still-running one, and
// a row whose timestamps were never written.
func TestMigrationV64BackfillsHistoricalAttemptsAsDelivered(t *testing.T) {
	db := migrateThrough(t, 63)
	mustExec(t, db, `
		INSERT INTO work_item_phases (
			item_id, phase_id, attempt, thread_id, input_envelope, output_envelope,
			gate_trace, intervention, narrative_path, park_cause,
			provider_usage_scope_id, status, started_at, ended_at
		) VALUES
			('legacy', 'build', 1, '', '{"feedback":{"note":"ancient answer"}}', '', '', '', '', '', 0, 'parked', 1000, 1100),
			('legacy', 'build', 2, '', '{"feedback":{"note":"live answer"}}', '', '', '', '', '', 0, 'running', 1200, 0),
			('legacy', 'review', 1, '', '{"feedback":{"note":"clockless"}}', '', '', '', '', '', 0, 'parked', 0, 0)
	`)

	if err := applyMigration(db, migrationByVersion(t, 64)); err != nil {
		t.Fatalf("apply migration v64: %v", err)
	}

	for _, want := range []struct {
		phase     string
		attempt   int
		delivered int64
	}{
		{"build", 1, 1100}, // settled: its own ended_at
		{"build", 2, 1200}, // still running: its own started_at
		{"review", 1, 1},   // no clock at all: still delivered, structurally
	} {
		var delivered int64
		if err := db.QueryRow(
			`SELECT feedback_delivered_at FROM work_item_phases
			 WHERE item_id = 'legacy' AND phase_id = ? AND attempt = ?`,
			want.phase, want.attempt,
		).Scan(&delivered); err != nil {
			t.Fatalf("read %s/%d: %v", want.phase, want.attempt, err)
		}
		if delivered != want.delivered {
			t.Fatalf("%s/%d feedback_delivered_at = %d, want %d",
				want.phase, want.attempt, delivered, want.delivered)
		}
	}

	// The observable consequence: nothing historical is owed, so no future
	// attempt of these phases redelivers a note from before the feature existed.
	var owed int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM work_item_phases WHERE feedback_delivered_at = 0`,
	).Scan(&owed); err != nil {
		t.Fatalf("count owed rows: %v", err)
	}
	if owed != 0 {
		t.Fatalf("%d pre-migration attempts still owe feedback, want 0", owed)
	}

	// A row written AFTER the migration defaults back to owed, which is the
	// state the engine settles at its runner start.
	mustExec(t, db, `
		INSERT INTO work_item_phases (
			item_id, phase_id, attempt, thread_id, input_envelope, output_envelope,
			gate_trace, intervention, narrative_path, park_cause,
			provider_usage_scope_id, status, started_at, ended_at
		) VALUES ('legacy', 'build', 3, '', '{"feedback":{"note":"new"}}', '', '', '', '', '', 0, 'running', 2000, 0)
	`)
	var fresh int64
	if err := db.QueryRow(
		`SELECT feedback_delivered_at FROM work_item_phases
		 WHERE item_id = 'legacy' AND phase_id = 'build' AND attempt = 3`,
	).Scan(&fresh); err != nil {
		t.Fatalf("read post-migration row: %v", err)
	}
	if fresh != 0 {
		t.Fatalf("post-migration default = %d, want 0 (owed)", fresh)
	}
}
