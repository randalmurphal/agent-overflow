package workflowapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/untrustedtext"
	"agent-overflow/internal/workflow/def"
)

const (
	maxDigestOutputs         = 8
	maxDigestValueRunes      = 200
	workflowSessionContinued = "continued"
)

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

func workflowOutputDigest(outputs map[string]json.RawMessage) ([]OutputDigest, int) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := make([]OutputDigest, 0, min(len(names), maxDigestOutputs))
	for index, name := range names {
		if index == maxDigestOutputs {
			return digest, len(names) - index
		}
		digest = append(digest, OutputDigest{
			Name: name, Value: untrustedtext.Truncate(workflowOutputText(outputs[name]), maxDigestValueRunes),
		})
	}
	return digest, 0
}

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

func workflowAttachLatestDigests(itemID string, attempts []PhaseAttempt, timeline []store.WorkItemPhaseTimeline) error {
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

// PhaseAttempts projects one run's persisted attempt provenance. Root exposes
// it only as part of existing wire DTOs and integration-test seams.
func (s *Service) PhaseAttempts(itemID string) ([]PhaseAttempt, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	rows, err := database.ListWorkItemPhaseProvenance(itemID)
	if err != nil {
		return nil, err
	}
	attempts := make([]PhaseAttempt, 0, len(rows))
	sessions := make(map[string]bool, len(rows))
	for _, row := range rows {
		attempt := PhaseAttempt{
			PhaseID: row.PhaseID, Attempt: row.Attempt, Status: row.Status,
			Cause: row.ParkCause, Provider: row.Provider, Model: row.Model, Effort: row.Effort,
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

func selectWorkflowAttempt(itemID, phaseID string, attempt int, attempts []PhaseAttempt) (PhaseAttempt, error) {
	var selected PhaseAttempt
	found, phaseSeen := false, false
	for _, candidate := range attempts {
		if candidate.PhaseID != phaseID {
			continue
		}
		phaseSeen = true
		if attempt != 0 && candidate.Attempt != attempt {
			continue
		}
		if !found || candidate.Attempt > selected.Attempt {
			selected, found = candidate, true
		}
	}
	if found {
		return selected, nil
	}
	if phaseSeen {
		return PhaseAttempt{}, fmt.Errorf(
			"workflow run %s: phase %q has no attempt %d; it has %s",
			itemID, phaseID, attempt, describeWorkflowAttempts(phaseID, attempts))
	}
	return PhaseAttempt{}, fmt.Errorf(
		"workflow run %s has no phase %q; it has %s", itemID, phaseID, describeWorkflowPhases(attempts))
}

func describeWorkflowPhases(attempts []PhaseAttempt) string {
	seen := make(map[string]struct{}, len(attempts))
	ids := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if _, repeated := seen[attempt.PhaseID]; repeated {
			continue
		}
		seen[attempt.PhaseID] = struct{}{}
		ids = append(ids, attempt.PhaseID)
	}
	if len(ids) == 0 {
		return "no phase attempts at all"
	}
	sort.Strings(ids)
	return "phases " + strings.Join(ids, ", ")
}

func describeWorkflowAttempts(phaseID string, attempts []PhaseAttempt) string {
	numbers := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.PhaseID == phaseID {
			numbers = append(numbers, fmt.Sprintf("%d", attempt.Attempt))
		}
	}
	if len(numbers) == 0 {
		return "none"
	}
	return "attempts " + strings.Join(numbers, ", ")
}
