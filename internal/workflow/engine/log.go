package engine

import (
	"fmt"
	"log"
	"strings"
)

// The run-lifecycle record: what the engine says about its own decisions, for a
// human reconstructing them afterwards. It is deliberately NOT the durable
// account of anything actionable — a park's cause lives on the attempt row, a
// refresh's evidence is the re-frozen snapshot — because a log is the one
// record nobody can act on programmatically. What it adds is the sequence, and
// the handful of decisions that leave no row of their own.

// logEvent writes one run-lifecycle line. A nil sink falls back to the standard
// logger rather than dropping the line: these records existed as log.Printf
// before the sink did, and an engine constructed without one — every test, a
// boot whose log file could not be opened — must not go quiet.
func (e *Engine) logEvent(event LogEvent) {
	if e.log != nil {
		e.log.LogEngineEvent(event)
		return
	}
	log.Printf("workflow %s: %s", event.Event, renderLogEvent(event))
}

// renderLogEvent is the one-line form the fallback logger writes. Fields are
// omitted when empty so a rebuild note does not carry three empty coordinates.
func renderLogEvent(event LogEvent) string {
	parts := make([]string, 0, 8)
	if event.ItemID != "" {
		parts = append(parts, "run="+event.ItemID)
	}
	if event.PhaseID != "" {
		phase := event.PhaseID
		if event.Attempt > 0 {
			phase = fmt.Sprintf("%s/%d", phase, event.Attempt)
		}
		parts = append(parts, "phase="+phase)
	}
	if event.State != "" {
		parts = append(parts, "state="+string(event.State))
	}
	if event.Reason != "" {
		parts = append(parts, "reason="+string(event.Reason))
	}
	if event.ThreadID != "" {
		parts = append(parts, "thread="+event.ThreadID)
	}
	if event.Message != "" {
		parts = append(parts, event.Message)
	}
	return strings.Join(parts, " ")
}

// noteResume records one resume, from the branch that is actually taking it.
//
// The shapes do materially different things — continuing the parked attempt on
// its own session, repairing a fan-out, re-linking a call's child, re-entering
// the phase from its inputs — so a line that said only "resumed" would answer
// none of the questions this record is read for, and a line that stated the
// REQUEST would answer them wrongly: the branch is not decided by the verb.
// `ContinuableReason` only routes a bare resume INTO the continuation path, and
// that path still re-enters fresh where there is nothing to continue (an attempt
// that ran no provider session, a session since deleted, a call whose child was
// never created, a run that froze no definition at all).
//
// So every branch emits exactly once, from where it knows what it is, and each
// emits BEFORE doing its work — the property the single up-front line had, kept
// per branch, so a resume that fails partway still says what it was doing. Only
// a resume that fails while DECIDING (a store read, a missing attempt row) logs
// nothing, and it changed nothing either.
//
// Emitting before the work is what makes the WORDING part of the contract: a
// note that names a live provider session says it is DISPATCHING onto it, never
// that a turn is running on it. "continuing the parked attempt on its own
// session" read as a completed fact for work that had not started yet, and an
// operator watching a run that then wedged had a log line saying the opposite of
// what happened (incident 2026-08-15). `LogEventRunnerStart` is the other half:
// the dispatch states the intent, and the start states the outcome, so silence
// between the two is itself the finding.
//
// The coordinates are the PARKED attempt's: they are what the note is about, and
// a fresh entry has not created its own row yet.
func (e *Engine) noteResume(item *runtimeItem, note string) {
	e.noteHumanVerb(LogEventResume, item, "", note)
}

// noteHumanVerb is the one construction of an operator-verb line — a resume, an
// answer, a takeover finalize. It exists so the coordinate rule above holds for
// every verb rather than for the one that happened to be written first: the
// PARKED attempt's phase and try, the park reason the verb is acting on, and
// (where the verb resolved one) the provider session it is about to dispatch
// onto.
func (e *Engine) noteHumanVerb(event string, item *runtimeItem, threadID, note string) {
	e.logEvent(LogEvent{
		Event: event, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: item.phaseID, Attempt: item.attempt, Reason: Reason(item.item.Reason),
		ThreadID: threadID, Message: note,
	})
}

// freshEntryNote is the note every re-entry shares, named for the phase when the
// human named one and stating a definition re-read where it happens.
func freshEntryNote(targetPhase string, refreshDefinition bool) string {
	note := "fresh entry into the parked phase"
	if targetPhase != "" {
		note = "fresh entry into phase " + targetPhase
	}
	if refreshDefinition {
		note += ", re-reading the definition from disk"
	}
	return note
}
