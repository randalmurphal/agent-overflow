package main

import (
	"context"
	"fmt"
	"strings"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/memory"
)

// `agent-overflow memory add` / `memory list` — the CLI half of campaign memory
// (Packet L).
//
// Neither verb takes a grant. Recording what the work learned is part of doing
// the work, exactly as returning an envelope is: a phase that can run at all can
// say what it found out, and a `grants:` line standing between an element and
// its own campaign's memory would mean every workflow that forgot one silently
// relearns everything each wave. The AUTHORITY that does apply is row-level and
// enforced here: a phase writes into the tree of the run it is a phase of, and
// into no other.
//
// These are LocalOnly for the same reason `run narrative` is — they read and
// write a file under this machine's app-managed config root.

// WorkflowAgentMemoryInput is one recorded note. There is deliberately no
// provenance or timestamp field: those are the system's answer to "who wrote
// this and when", and a shape that could carry a supplied one is a shape that
// could be lied to.
type WorkflowAgentMemoryInput struct {
	// ItemID names the run to attribute the note to. Optional for a phase
	// session, which is already a phase of one; required for an interactive
	// session, which is not.
	ItemID string   `json:"itemId,omitempty"`
	Kind   string   `json:"kind"`
	Text   string   `json:"text"`
	Files  []string `json:"files,omitempty"`
}

// WorkflowAgentMemoryResult reports where the note landed. The path is returned
// so a caller can read the log it just wrote to without being told the layout.
type WorkflowAgentMemoryResult struct {
	ItemID string `json:"itemId"`
	RootID string `json:"rootId"`
	Kind   string `json:"kind"`
	Wave   int    `json:"wave"`
	Path   string `json:"path"`
}

// WorkflowAgentMemoryListInput selects what a read returns.
type WorkflowAgentMemoryListInput struct {
	ItemID string `json:"itemId,omitempty"`
	// Kind narrows to one of the four; empty returns every kind.
	Kind string `json:"kind,omitempty"`
}

// WorkflowAgentMemoryLog is one tree's notes, oldest last — the order they were
// written, which is the order the log holds them in.
type WorkflowAgentMemoryLog struct {
	ItemID string        `json:"itemId"`
	RootID string        `json:"rootId"`
	Path   string        `json:"path"`
	Notes  []memory.Note `json:"notes"`
	// Total is how many notes the tree holds before Kind narrowed them, so a
	// filtered read still states the size of what it read from.
	Total int `json:"total"`
	// Skipped counts lines the log holds that are not readable notes — a torn
	// final line from a crash. Reported rather than hidden: a reader deciding
	// whether the memory is complete needs to know one was lost.
	Skipped int `json:"skipped"`
}

// WorkflowAgentAddMemory is `agent-overflow memory add`.
//
// LocalOnly: it appends to a file under this machine's app-managed config root.
func (a *App) WorkflowAgentAddMemory(ctx context.Context, input WorkflowAgentMemoryInput) (WorkflowAgentMemoryResult, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentMemoryResult{}, err
	}
	item, err := a.memoryScopedRun(scope, input.ItemID, "workflow memory add")
	if err != nil {
		return WorkflowAgentMemoryResult{}, err
	}
	provenance := memory.Provenance{}
	if scope.IsPhase() {
		provenance = a.workflowMemoryProvenance(scope.ThreadID, scope.PhaseID)
	}
	draft := memory.Draft{Kind: input.Kind, Text: input.Text, Files: input.Files}
	if _, err := a.recordWorkflowMemory(item, provenance, []memory.Draft{draft}); err != nil {
		return WorkflowAgentMemoryResult{}, describeMemoryFindings("workflow memory add", err)
	}
	tree, err := a.workflowMemoryTreeFor(item)
	if err != nil {
		return WorkflowAgentMemoryResult{}, err
	}
	return WorkflowAgentMemoryResult{
		ItemID: item.ID, RootID: tree.RootID, Kind: draft.Kind, Wave: tree.Wave, Path: tree.NotesPath,
	}, nil
}

// WorkflowAgentListMemory is `agent-overflow memory list`.
//
// LocalOnly: it reads a file under this machine's app-managed config root.
func (a *App) WorkflowAgentListMemory(ctx context.Context, input WorkflowAgentMemoryListInput) (WorkflowAgentMemoryLog, error) {
	scope, err := requireCallerScope(ctx)
	if err != nil {
		return WorkflowAgentMemoryLog{}, err
	}
	item, err := a.memoryScopedRun(scope, input.ItemID, "workflow memory list")
	if err != nil {
		return WorkflowAgentMemoryLog{}, err
	}
	tree, err := a.workflowMemoryTreeFor(item)
	if err != nil {
		return WorkflowAgentMemoryLog{}, err
	}
	kind := strings.TrimSpace(input.Kind)
	if kind != "" && !memory.KnownKind(kind) {
		return WorkflowAgentMemoryLog{}, fmt.Errorf(
			"workflow memory list: kind %q is not one of %s", kind, memory.KindList())
	}
	notes, skipped, err := memory.ReadNotes(tree.NotesPath)
	if err != nil {
		return WorkflowAgentMemoryLog{}, err
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
	return WorkflowAgentMemoryLog{
		ItemID: item.ID, RootID: tree.RootID, Path: tree.NotesPath,
		Notes: slicesx.OrEmpty(selected), Total: len(notes), Skipped: len(skipped),
	}, nil
}

// memoryScopedRun resolves the run whose tree a memory call touches, and refuses
// one the caller may not.
//
// A phase session that names no run gets its OWN — the run it is a phase of —
// which is what makes the verb usable from a prompt with nothing to substitute
// in. A phase that names one is held to the same rule every acting verb applies:
// it may name its own run, or one it started, and nothing else in the project.
// That is deliberately the ACTING rule rather than the reading one even for
// `memory list`: `introspect` widens reads to every run in the project, and a
// campaign's memory is not run state — reading another tree's notes is reading
// another campaign's lessons, which is not what introspection was granted for.
//
// An interactive session must name a run. It is a person at a keyboard whose
// thread is not part of any campaign, so there is no run to infer.
func (a *App) memoryScopedRun(scope transport.CallerScope, itemID, action string) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	// A phase's own run is resolved directly rather than through `scopedRun`,
	// which admits a phase only to the runs it STARTED: the run a phase is a
	// phase of is not one of those, and it is the one tree the phase must always
	// be able to write.
	if scope.IsPhase() && (itemID == "" || itemID == scope.ItemID) {
		item, err := a.store.GetWorkItem(scope.ItemID)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("%s: %w", action, err)
		}
		if item.ProjectID != scope.ProjectID {
			return store.WorkItem{}, fmt.Errorf("%s: run %s belongs to another project", action, item.ID)
		}
		return item, nil
	}
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf(
			"%s: a run id is required outside a workflow phase session", action)
	}
	return a.scopedRun(scope, itemID, action, false)
}
