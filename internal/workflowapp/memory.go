package workflowapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/memory"
)

// AddMemory records one validated note in the authorized run tree.
func (s *Service) AddMemory(ctx context.Context, input MemoryInput) (MemoryResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return MemoryResult{}, err
	}
	item, err := s.memoryScopedRun(scope, input.ItemID, "workflow memory add")
	if err != nil {
		return MemoryResult{}, err
	}
	provenance := memory.Provenance{}
	if scope.IsPhase() && s.deps.MemoryProvenance != nil {
		provenance = s.deps.MemoryProvenance(scope.ThreadID, scope.PhaseID)
	}
	draft := memory.Draft{Kind: input.Kind, Text: input.Text, Files: input.Files}
	if s.deps.RecordMemory == nil {
		return MemoryResult{}, errors.New("workflow memory add: writer unavailable")
	}
	if _, err := s.deps.RecordMemory(item, provenance, []memory.Draft{draft}); err != nil {
		return MemoryResult{}, describeMemoryFindings("workflow memory add", err)
	}
	tree, err := s.memoryTree(item)
	if err != nil {
		return MemoryResult{}, err
	}
	return MemoryResult{
		ItemID: item.ID, RootID: tree.RootID, Kind: draft.Kind, Wave: tree.Wave, Path: tree.NotesPath,
	}, nil
}

// ListMemory reads one authorized run tree's append-only note log.
func (s *Service) ListMemory(ctx context.Context, input MemoryListInput) (MemoryLog, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return MemoryLog{}, err
	}
	item, err := s.memoryScopedRun(scope, input.ItemID, "workflow memory list")
	if err != nil {
		return MemoryLog{}, err
	}
	tree, err := s.memoryTree(item)
	if err != nil {
		return MemoryLog{}, err
	}
	kind := strings.TrimSpace(input.Kind)
	if kind != "" && !memory.KnownKind(kind) {
		return MemoryLog{}, fmt.Errorf("workflow memory list: kind %q is not one of %s", kind, memory.KindList())
	}
	notes, skipped, err := memory.ReadNotes(tree.NotesPath)
	if err != nil {
		return MemoryLog{}, err
	}
	selected := notes
	if kind != "" {
		selected = make([]memory.Note, 0, len(notes))
		for _, note := range notes {
			if note.Kind == kind {
				selected = append(selected, note)
			}
		}
	}
	return MemoryLog{
		ItemID: item.ID, RootID: tree.RootID, Path: tree.NotesPath,
		Notes: slicesx.OrEmpty(selected), Total: len(notes), Skipped: len(skipped),
	}, nil
}

func (s *Service) memoryScopedRun(scope transport.CallerScope, itemID, action string) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	if scope.IsPhase() && (itemID == "" || itemID == scope.ItemID) {
		database, err := s.store()
		if err != nil {
			return store.WorkItem{}, err
		}
		item, err := database.GetWorkItemSummary(scope.ItemID)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("%s: %w", action, err)
		}
		if item.ProjectID != scope.ProjectID {
			return store.WorkItem{}, fmt.Errorf("%s: run %s belongs to another project", action, item.ID)
		}
		return item, nil
	}
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("%s: a run id is required outside a workflow phase session", action)
	}
	return s.scopedRun(scope, itemID, action, false)
}

func (s *Service) memoryTree(item store.WorkItem) (MemoryTree, error) {
	if s == nil || s.deps.MemoryTree == nil {
		return MemoryTree{}, errors.New("workflow memory tree: resolver unavailable")
	}
	return s.deps.MemoryTree(item)
}

func describeMemoryFindings(action string, err error) error {
	var validation *memory.ValidationError
	if !errors.As(err, &validation) {
		return err
	}
	parts := make([]string, 0, len(validation.Findings))
	for _, finding := range validation.Findings {
		parts = append(parts, strings.TrimPrefix(finding.Path, ".")+" "+finding.Message)
	}
	return fmt.Errorf("%s: %s", action, strings.Join(parts, "; "))
}
