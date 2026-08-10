package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"agent-overflow/internal/store"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/def"
)

// The per-attempt provenance `ao run status` carries (D38). A campaign agent
// reading a parked run has two questions the run row cannot answer: which
// attempt produced the outputs the gate consumed, and what each element
// actually ran with. Both are already persisted — the attempt rows carry
// status and gate trace, and the thread the attempt ran on carries the
// resolved provider/model/effort — so this is a projection, not new state.

// WorkflowAgentPhaseAttempt is one attempt of one phase. It is deliberately
// narrower than the phase row: no envelopes, no predicate trace, no narrative
// path. The decision fields are the gate's OUTCOME — which way the run went and
// which loop budgets it had spent — because that is what a reader deciding
// between `run resolve`, `run resume --phase`, and `run rerun` needs; the
// predicates that produced it are a debugging read, and they live in the app.
type WorkflowAgentPhaseAttempt struct {
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	// Provider, Model, and Effort are the settings the attempt's thread was
	// created with, empty for an attempt that has no thread — a tool-driver
	// phase runs a command, not a provider session.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Cause is why the ENGINE parked this attempt, in its own words — a
	// worktree that could not be cut, a phase missing from the snapshot, a
	// budget that ran out. It is empty for every attempt that rested on its
	// own envelope, and for the reasons that name their own cause
	// (`interrupted`, `paused`, `taken-over`). It is never model output.
	Cause string `json:"cause,omitempty"`
	// Session is "continued" when this attempt ran as the next turn of a session
	// an EARLIER attempt of the same phase started, and empty otherwise — which
	// is the same fact the two rows' shared thread id is, named so a reader does
	// not have to compare thread ids to see it. Three things produce it and they
	// are deliberately not distinguished: a loop route declaring
	// `session: continue`, an answered question, and a finalized takeover. All
	// three mean the same thing to anyone reading the run — this round remembers
	// the last one — and the definition says which edge asked for it.
	Session string `json:"session,omitempty"`
	// Decision, DecisionTarget, and ExhaustedLoops are absent until the attempt's
	// gate has been evaluated and persisted.
	Decision       string   `json:"decision,omitempty"`
	DecisionTarget string   `json:"decisionTarget,omitempty"`
	ExhaustedLoops []string `json:"exhaustedLoops,omitempty"`
	// Outputs is a bounded digest of the attempt's envelope outputs and
	// OutputOverflow how many it left out. Only `run inspect` populates them, and
	// only for the LATEST attempt of each phase: the digest exists so a reader
	// deciding a gate does not have to open every attempt, and `run inspect
	// --phase` returns that attempt's outputs whole instead. `run status` carries
	// neither — its projection is deliberately envelope-free.
	Outputs        []WorkflowAgentOutputDigest `json:"outputs,omitempty"`
	OutputOverflow int                         `json:"outputOverflow,omitempty"`
}

// WorkflowAgentOutputDigest is one envelope output rendered small enough that a
// whole run's worth fits in one read. The value is the output's text when it is
// a JSON string and its compact JSON otherwise, rune-capped with the shared
// truncation marker — a reader that needs the untruncated value asks for the
// attempt.
type WorkflowAgentOutputDigest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Digest budgets. A run's inspection names every phase attempt, so the per-
// attempt digest has to stay small enough that a twenty-attempt run is still one
// readable answer.
const (
	maxDigestOutputs    = 8
	maxDigestValueRunes = 200
)

// workflowSessionContinued is the one value WorkflowAgentPhaseAttempt.Session
// takes. A fresh attempt carries no value at all rather than "fresh": it is the
// default every attempt has had since before loop routes could ask for anything
// else, and a field that is present on every row of every run is bytes an agent
// pays for on every read.
const workflowSessionContinued = "continued"

// workflowAttemptOutputs decodes one attempt's envelope outputs. An attempt with
// no envelope, or one that rested on a question or a stuck reason, simply has
// none — that is an answer rather than an error. The size assertion is the read
// side of the contract every envelope was accepted under: a record past the cap
// is corrupt, and shipping it into an agent's context window unremarked is the
// one thing worse than refusing it.
func workflowAttemptOutputs(itemID, phaseID string, attempt int, payload json.RawMessage) (map[string]json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	if len(payload) > def.DefaultEnvelopeSizeCap {
		return nil, fmt.Errorf(
			"workflow run %s: envelope for %s attempt %d is %d bytes; maximum is %d",
			itemID, phaseID, attempt, len(payload), def.DefaultEnvelopeSizeCap)
	}
	var envelope struct {
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf(
			"workflow run %s: envelope for %s attempt %d is unreadable: %w", itemID, phaseID, attempt, err)
	}
	return envelope.Outputs, nil
}

// workflowOutputDigest projects an attempt's outputs into the bounded form, in a
// stable order, and reports how many it left out. The overflow count is returned
// rather than silently dropped: a digest that hides its own truncation is how a
// reader concludes an output does not exist.
func workflowOutputDigest(outputs map[string]json.RawMessage) ([]WorkflowAgentOutputDigest, int) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := make([]WorkflowAgentOutputDigest, 0, min(len(names), maxDigestOutputs))
	for index, name := range names {
		if index == maxDigestOutputs {
			return digest, len(names) - index
		}
		digest = append(digest, WorkflowAgentOutputDigest{
			Name:  name,
			Value: untrustedtext.Truncate(workflowOutputText(outputs[name]), maxDigestValueRunes),
		})
	}
	return digest, 0
}

// workflowOutputText renders one output value for a line. A JSON string becomes
// its text — the caller quotes it as untrusted data when it renders it, and a
// value that arrived already quoted would reach the reader double-quoted —
// while every other shape keeps its JSON, compacted so one value stays one line.
func workflowOutputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

// workflowAttachLatestDigests stamps each phase's LATEST attempt with the digest
// of its envelope outputs. Earlier attempts are left bare: they were superseded,
// and a run that retried a phase five times would otherwise answer with five
// digests of which only one is current.
func workflowAttachLatestDigests(
	itemID string, attempts []WorkflowAgentPhaseAttempt, timeline []store.WorkItemPhaseTimeline,
) error {
	latest := make(map[string]int, len(attempts))
	for _, attempt := range attempts {
		if attempt.Attempt > latest[attempt.PhaseID] {
			latest[attempt.PhaseID] = attempt.Attempt
		}
	}
	envelopes := make(map[string]json.RawMessage, len(timeline))
	for _, phase := range timeline {
		if latest[phase.PhaseID] == phase.Attempt {
			envelopes[phase.PhaseID] = phase.OutputEnvelope
		}
	}
	for index := range attempts {
		attempt := &attempts[index]
		if latest[attempt.PhaseID] != attempt.Attempt {
			continue
		}
		outputs, err := workflowAttemptOutputs(itemID, attempt.PhaseID, attempt.Attempt, envelopes[attempt.PhaseID])
		if err != nil {
			return err
		}
		attempt.Outputs, attempt.OutputOverflow = workflowOutputDigest(outputs)
	}
	return nil
}

// workflowAgentPhaseAttempts projects a run's phase history for the single-run
// status read. A gate trace that no longer decodes fails the read rather than
// reporting an attempt with no decision: "this attempt reached no gate" and
// "this attempt's record is corrupt" are different answers, and only one of
// them is a state a run can legitimately be in.
func (a *App) workflowAgentPhaseAttempts(itemID string) ([]WorkflowAgentPhaseAttempt, error) {
	rows, err := a.store.ListWorkItemPhaseProvenance(itemID)
	if err != nil {
		return nil, err
	}
	attempts := make([]WorkflowAgentPhaseAttempt, 0, len(rows))
	// The rows arrive oldest first, so the first attempt to claim a (phase,
	// thread) pair is the one that STARTED that session and every later one
	// continued it. No column records the mode: reusing the thread is what a
	// continuation IS, so the row already carries the evidence.
	sessions := make(map[string]bool, len(rows))
	for _, row := range rows {
		attempt := WorkflowAgentPhaseAttempt{
			PhaseID: row.PhaseID, Attempt: row.Attempt, Status: row.Status,
			Cause:    row.ParkCause,
			Provider: row.Provider, Model: row.Model, Effort: row.Effort,
		}
		if row.ThreadID != "" {
			key := row.PhaseID + "\x00" + row.ThreadID
			if sessions[key] {
				attempt.Session = workflowSessionContinued
			}
			sessions[key] = true
		}
		if len(row.GateTrace) > 0 {
			var trace def.GateTrace
			if err := json.Unmarshal(row.GateTrace, &trace); err != nil {
				return nil, fmt.Errorf(
					"workflow run status %s: gate trace for %s attempt %d is unreadable: %w",
					itemID, row.PhaseID, row.Attempt, err)
			}
			attempt.Decision = string(trace.Decision.Kind)
			attempt.DecisionTarget = trace.Decision.Target
			attempt.ExhaustedLoops = trace.ExhaustedLoops
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}
