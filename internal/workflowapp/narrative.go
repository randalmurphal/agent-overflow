package workflowapp

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

const maxWorkflowNarrativeBytes = 64 * 1024

// RunNarrative resolves one persisted attempt narrative by workflow
// coordinates rather than exposing the package's directory layout.
func (s *Service) RunNarrative(ctx context.Context, input NarrativeInput) (Narrative, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return Narrative{}, err
	}
	item, err := s.scopedRun(scope, input.ItemID, "workflow run narrative", true)
	if err != nil {
		return Narrative{}, err
	}
	phaseID := strings.TrimSpace(input.PhaseID)
	if phaseID == "" {
		return Narrative{}, fmt.Errorf("workflow run narrative %s: a phase id is required", item.ID)
	}
	attempts, err := s.PhaseAttempts(item.ID)
	if err != nil {
		return Narrative{}, err
	}
	selected, err := selectWorkflowAttempt(item.ID, phaseID, input.Attempt, attempts)
	if err != nil {
		return Narrative{}, err
	}
	narrative := Narrative{ItemID: item.ID, PhaseID: selected.PhaseID, Attempt: selected.Attempt}
	unitID := strings.TrimSpace(input.UnitID)
	if unitID == "" {
		narrative.Path, narrative.Present, err = workflowhost.NarrativeLookup(
			s.dataRoot(), item.ID, selected.PhaseID, selected.Attempt)
	} else {
		var unit store.WorkItemUnit
		unit, err = s.narrativeUnit(item.ID, selected.PhaseID, selected.Attempt, unitID)
		if err != nil {
			return Narrative{}, err
		}
		narrative.UnitID, narrative.UnitAttempt = unit.UnitID, unit.UnitAttempt
		narrative.Path, narrative.Present, err = workflowhost.UnitNarrativeLookup(
			s.dataRoot(), item.ID, selected.PhaseID, selected.Attempt, unit.UnitID, unit.UnitAttempt)
	}
	if err != nil || !narrative.Present {
		return narrative, err
	}
	if err := loadNarrativeContent(&narrative); err != nil {
		return Narrative{}, err
	}
	return narrative, nil
}

func (s *Service) narrativeUnit(itemID, phaseID string, attempt int, unitID string) (store.WorkItemUnit, error) {
	database, err := s.store()
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	unit, found, err := database.GetWorkItemUnit(itemID, phaseID, attempt, unitID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	if found {
		if unit.UnitAttempt < 1 {
			unit.UnitAttempt = 1
		}
		return unit, nil
	}
	units, err := database.ListWorkItemPhaseUnits(itemID, phaseID, attempt)
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

func loadNarrativeContent(narrative *Narrative) error {
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
