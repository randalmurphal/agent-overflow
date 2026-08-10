package main

import (
	"encoding/json"
	"log"
	"slices"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// The app-resolved half of prompt assembly (Packet O). Two of the blocks every
// element's prompt carries are facts about the run TREE rather than about the
// element: the campaign-memory digest, which is keyed on the tree's root, and
// the goal chain, which IS the tree's call linkage. Both need the same ancestry
// walk, so they are resolved together — walking twice per element would pay for
// the same parent rows once each.
//
// Everything here is best effort. A failure to resolve is LOGGED and yields the
// blocks it could build rather than failing the attempt: the goal chain and the
// memory digest are context, not contract, and an element that runs without
// them does the work with less to go on while an element that never starts does
// none.

// workflowPromptAncestry resolves the tree-shaped prompt blocks for one run.
// `workflow` is the definition the run is executing — the caller already holds
// it on the run request, and it is where THIS run's non-goals come from, so
// re-decoding the run's own snapshot to read them would be a second answer to a
// question already in hand.
func (a *App) workflowPromptAncestry(itemID string, workflow def.Workflow) workflowrunner.PromptContext {
	// Summary, not the full row: nothing this file reads about the run ITSELF
	// lives in the snapshot, and this resolves once per element — every unit of
	// every wave, on the path that starts its turn.
	item, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		// The definition is in hand, so its non-goals still stand: they are
		// def-owned, and dropping the run's stated boundaries because a row read
		// failed would lose the one part of the block that never needed the store.
		log.Printf("workflow prompt context: load run %s: %v", itemID, err)
		return workflowrunner.PromptContext{Goals: a.workflowGoalChain(nil, workflow)}
	}
	ancestry, err := a.workflowAncestry(item)
	if err != nil {
		// The run's own facts still stand: it is its own root as far as anything
		// resolvable goes, and the non-goals it must respect are on the
		// definition in hand. Falling back to them beats dropping the block.
		log.Printf("workflow prompt context: resolve ancestry of run %s: %v", itemID, err)
		ancestry = []store.WorkItem{item}
	}
	context := workflowrunner.PromptContext{Goals: a.workflowGoalChain(ancestry, workflow)}
	// The tree comes from the ancestry just walked. Both blocks are facts about
	// the same call linkage, and resolving the tree from the run instead would
	// walk it a second time — the parent rows are already in hand.
	tree, err := a.workflowMemoryTreeOf(ancestry)
	if err != nil {
		log.Printf("workflow memory: %v", err)
		return context
	}
	context.Memory = a.workflowMemoryDigest(tree)
	return context
}

// workflowGoalChain assembles the goals of the call chain root-first plus the
// non-goals in force.
//
// CONSECUTIVE runs sharing one goal collapse into a single link. The engine
// copies a caller's goal onto every run it calls (`invokeCall`), so a forty-wave
// campaign's chain is forty copies of one sentence; rendering them all would
// cost every element forty times the bytes to say exactly what one link says.
// The link keeps the ROOT-most run that stated the goal, because that is the run
// the goal was actually recorded on.
func (a *App) workflowGoalChain(ancestry []store.WorkItem, workflow def.Workflow) workflowrunner.GoalChain {
	// The non-goals come off the definition rather than the ancestry, so an
	// unresolvable chain still carries them.
	chain := workflowrunner.GoalChain{NonGoals: workflow.NonGoals, WorkflowID: workflow.ID}
	if len(ancestry) == 0 {
		return chain
	}
	root, current := ancestry[0], ancestry[len(ancestry)-1]
	for index, run := range ancestry {
		goal := strings.TrimSpace(run.Goal)
		if goal == "" {
			continue
		}
		if last := len(chain.Links) - 1; last >= 0 && chain.Links[last].Goal == goal {
			continue
		}
		chain.Links = append(chain.Links, workflowrunner.GoalLink{
			RunID: run.ID, WorkflowID: run.WorkflowID, Goal: goal,
			Root: index == 0, Current: run.ID == current.ID,
		})
	}
	if root.ID == current.ID {
		return chain
	}
	// The root's non-goals bind this run too — it is executing inside the
	// campaign that root started. They are carried only when they DIFFER,
	// because a recursive campaign whose every wave runs one definition would
	// otherwise print the same list twice under two headings.
	//
	// This is the ONE snapshot the block needs, and it is read here — for the
	// root alone, only when the root is not this run — rather than by making the
	// ancestry walk carry a frozen definition for every ancestor it touched.
	rootRow, err := a.store.GetWorkItem(root.ID)
	if err != nil {
		log.Printf("workflow prompt context: load root %s for its non-goals: %v", root.ID, err)
		return chain
	}
	rootWorkflow, ok := workflowSnapshotWorkflow(rootRow)
	if !ok || len(rootWorkflow.NonGoals) == 0 || slices.Equal(rootWorkflow.NonGoals, workflow.NonGoals) {
		return chain
	}
	chain.RootNonGoals, chain.RootWorkflowID = rootWorkflow.NonGoals, rootWorkflow.ID
	return chain
}

// workflowSnapshotWorkflow decodes the definition a run froze at start. A run
// with no snapshot (one that never got past admission) and one whose snapshot
// will not decode both report false: the caller is reading context, and a
// missing block is strictly better than a failed attempt.
func workflowSnapshotWorkflow(item store.WorkItem) (def.Workflow, bool) {
	if len(item.Snapshot) == 0 {
		return def.Workflow{}, false
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		log.Printf("workflow prompt context: decode snapshot of run %s: %v", item.ID, err)
		return def.Workflow{}, false
	}
	return snapshot.Workflow, true
}
