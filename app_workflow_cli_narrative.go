package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflowhost"
)

// `agent-overflow run narrative` — the per-attempt account, by coordinate
// rather than by path.
//
// The narrative is the one human-readable record of what an element did, and it
// is the file a supervising agent reads most: the campaign this verb came out of
// opened twenty-seven of them, every one by hand-assembling
// `workflow-runs/<id>/<phase>.<n>/units/<unit>.<n>/narrative.md` and discovering
// the shape by trial. The path shape is ours, so naming it is our job.

// maxWorkflowNarrativeBytes bounds what one read returns. A narrative is
// model-authored prose with no ceiling of its own, and this answer lands in a
// reader's context window; a file past the cap is reported truncated with its
// real size, so the reader can decide to open it directly rather than being told
// a partial account is the whole one.
const maxWorkflowNarrativeBytes = 64 * 1024

// WorkflowAgentNarrativeInput names one attempt's account. Attempt defaults to
// the latest attempt of the phase, which is the one a parked run is resting on;
// UnitID selects a fan-out unit's account instead of the phase's, on the unit's
// current try — the try is the unit row's, not the caller's to guess.
type WorkflowAgentNarrativeInput struct {
	ItemID  string `json:"itemId"`
	PhaseID string `json:"phaseId"`
	Attempt int    `json:"attempt,omitempty"`
	UnitID  string `json:"unitId,omitempty"`
}

// WorkflowAgentNarrative is one resolved account. Path is populated whether or
// not the file exists: an absent narrative is an answer, and the answer has to
// say what was looked for.
type WorkflowAgentNarrative struct {
	ItemID      string `json:"itemId"`
	PhaseID     string `json:"phaseId"`
	Attempt     int    `json:"attempt"`
	UnitID      string `json:"unitId,omitempty"`
	UnitAttempt int    `json:"unitAttempt,omitempty"`
	Path        string `json:"path"`
	Present     bool   `json:"present"`
	// Bytes is the file's real size, so a truncated read still reports how much
	// account there is.
	Bytes     int64  `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Content   string `json:"content,omitempty"`
}

// WorkflowAgentRunNarrative is `agent-overflow run narrative`.
//
// LocalOnly: it reads a file out of this machine's app-managed run directory,
// like every other local-filesystem read on the agent surface.
func (a *App) WorkflowAgentRunNarrative(ctx context.Context, input WorkflowAgentNarrativeInput) (WorkflowAgentNarrative, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	item, err := a.scopedRun(scope, input.ItemID, "workflow run narrative", true)
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	phaseID := strings.TrimSpace(input.PhaseID)
	if phaseID == "" {
		return WorkflowAgentNarrative{}, fmt.Errorf(
			"workflow run narrative %s: a phase id is required", item.ID)
	}
	attempts, err := a.workflowAgentPhaseAttempts(item.ID)
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	selected, err := selectWorkflowAttempt(item.ID, phaseID, input.Attempt, attempts)
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	narrative := WorkflowAgentNarrative{
		ItemID: item.ID, PhaseID: selected.PhaseID, Attempt: selected.Attempt,
	}
	unitID := strings.TrimSpace(input.UnitID)
	if unitID == "" {
		narrative.Path, narrative.Present, err = workflowhost.NarrativeLookup(
			a.workflowDataRoot(), item.ID, selected.PhaseID, selected.Attempt)
	} else {
		var unit store.WorkItemUnit
		unit, err = a.workflowAgentNarrativeUnit(item.ID, selected.PhaseID, selected.Attempt, unitID)
		if err != nil {
			return WorkflowAgentNarrative{}, err
		}
		narrative.UnitID, narrative.UnitAttempt = unit.UnitID, unit.UnitAttempt
		narrative.Path, narrative.Present, err = workflowhost.UnitNarrativeLookup(
			a.workflowDataRoot(), item.ID, selected.PhaseID, selected.Attempt, unit.UnitID, unit.UnitAttempt)
	}
	if err != nil {
		return WorkflowAgentNarrative{}, err
	}
	if !narrative.Present {
		return narrative, nil
	}
	if err := loadWorkflowNarrativeContent(&narrative); err != nil {
		return WorkflowAgentNarrative{}, err
	}
	return narrative, nil
}

// workflowAgentNarrativeUnit resolves the unit row an account belongs to. The
// row is what carries the try number, and a unit id that is not in the attempt
// is refused with the ids that are: a fan-out's unit ids come from the workflow's
// expansion, so a caller reading them off a narrative path is guessing.
func (a *App) workflowAgentNarrativeUnit(itemID, phaseID string, attempt int, unitID string) (store.WorkItemUnit, error) {
	unit, found, err := a.store.GetWorkItemUnit(itemID, phaseID, attempt, unitID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	if found {
		if unit.UnitAttempt < 1 {
			// A unit row is born `pending` with no try yet; its first try is 1, and
			// that is the directory the runner will have written into.
			unit.UnitAttempt = 1
		}
		return unit, nil
	}
	units, err := a.store.ListWorkItemPhaseUnits(itemID, phaseID, attempt)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	if len(units) == 0 {
		return store.WorkItemUnit{}, fmt.Errorf(
			"workflow run %s: %s attempt %d expanded no units; it is not a fan-out", itemID, phaseID, attempt)
	}
	ids := make([]string, 0, len(units))
	for _, candidate := range units {
		ids = append(ids, candidate.UnitID)
	}
	return store.WorkItemUnit{}, fmt.Errorf(
		"workflow run %s: %s attempt %d has no unit %q; it has %s",
		itemID, phaseID, attempt, unitID, strings.Join(ids, ", "))
}

// loadWorkflowNarrativeContent fills in the account's size and content. The file is
// opened once and both facts come from that handle, so a narrative rewritten
// between a stat and a read cannot report one file's size with another's bytes.
func loadWorkflowNarrativeContent(narrative *WorkflowAgentNarrative) error {
	file, err := os.Open(narrative.Path)
	if err != nil {
		return fmt.Errorf("read narrative %q: %w", narrative.Path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("read narrative %q: %w", narrative.Path, err)
	}
	narrative.Bytes = info.Size()
	data, err := io.ReadAll(io.LimitReader(file, maxWorkflowNarrativeBytes))
	if err != nil {
		return fmt.Errorf("read narrative %q: %w", narrative.Path, err)
	}
	if narrative.Bytes > int64(len(data)) {
		narrative.Truncated = true
		data = trimPartialRune(data)
	}
	narrative.Content = string(data)
	return nil
}

// trimPartialRune drops the incomplete UTF-8 sequence a byte-bounded read can
// leave at the end, so a truncated narrative is still text rather than text
// ending in a replacement character.
func trimPartialRune(data []byte) []byte {
	for len(data) > 0 {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			return data
		}
		data = data[:len(data)-1]
	}
	return data
}
