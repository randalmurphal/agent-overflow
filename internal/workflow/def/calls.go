package def

import (
	"fmt"
	"sort"
	"strings"
)

// CallResolver resolves a call phase's static target by workflow id under the
// §8 scoping rules (project scope wins over shared). def stays pure: loading
// definitions from disk is the caller's job, and every call-aware derivation
// here takes the resolver as an argument rather than reaching for a filesystem.
type CallResolver interface {
	ResolveCall(id string) (ResolvedWorkflow, error)
}

// CallTarget is the workflow id a call phase invokes. It is always a static id
// (never a variable), which is what lets the dry-run validate the whole call
// graph before anything runs.
func (p Phase) CallTarget() string { return strings.TrimSpace(p.Call) }

// IsCall reports whether the phase invokes another workflow instead of running
// work of its own.
func (p Phase) IsCall() bool { return p.EffectiveShape() == ShapeCall }

// CallPhaseOutputs types a call phase's downstream variable surface. A call
// phase's envelope carries the child workflow's declared `outputs:` (§3a), so
// what the parent's gates and later phases may consume is exactly the child's
// deliverables, typed by the child phases that produce them.
//
// An output whose `from` does not resolve inside the child is omitted rather
// than guessed: the child's own validation reports it, and inventing a type
// here would let a parent consumer type-check against a fiction.
func CallPhaseOutputs(child Workflow) map[string]Variable {
	if len(child.Outputs) == 0 {
		return nil
	}
	phaseIndex := make(map[string]int, len(child.Phases))
	for index, phase := range child.Phases {
		if _, exists := phaseIndex[phase.ID]; !exists {
			phaseIndex[phase.ID] = index
		}
	}
	outputs := make(map[string]Variable, len(child.Outputs))
	for name, declaration := range child.Outputs {
		variable, _, ok := resolveReference(child, phaseIndex, declaration.From)
		if !ok {
			continue
		}
		outputs[name] = variable
	}
	if len(outputs) == 0 {
		return nil
	}
	return outputs
}

// PropagatedWorkspaceNeed derives the workspace a run needs from its own graph
// *and* the graphs of every workflow reachable through its call edges (§9). A
// workflow that calls a writing workflow is itself write-needing: the child
// executes in the caller's workspace and never provisions one of its own, so
// the root is the only place a worktree can be cut.
//
// DeriveWorkspaceNeed stays the pure single-definition answer; this is the one
// place resolution is folded in, and the start path must use this answer when
// provisioning a root item.
func PropagatedWorkspaceNeed(workflow Workflow, calls CallResolver) (WorkspaceNeed, error) {
	if DeriveWorkspaceNeed(workflow) == WorkspaceWorktree {
		return WorkspaceWorktree, nil
	}
	if calls == nil {
		if len(CallTargets(workflow)) > 0 {
			return "", fmt.Errorf("workflow %q has call phases but no call resolver was supplied", workflow.ID)
		}
		return WorkspaceProjectRoot, nil
	}
	visited := map[string]bool{workflow.ID: true}
	pending := CallTargets(workflow)
	for len(pending) > 0 {
		target := pending[0]
		pending = pending[1:]
		if visited[target] {
			continue
		}
		visited[target] = true
		resolved, err := calls.ResolveCall(target)
		if err != nil {
			return "", fmt.Errorf("workflow %q call target %q: %w", workflow.ID, target, err)
		}
		if DeriveWorkspaceNeed(resolved.Workflow) == WorkspaceWorktree {
			return WorkspaceWorktree, nil
		}
		pending = append(pending, CallTargets(resolved.Workflow)...)
	}
	return WorkspaceProjectRoot, nil
}

// CallTargets returns every workflow id this definition calls, deduplicated and
// sorted so traversals over the call graph are deterministic.
func CallTargets(workflow Workflow) []string {
	seen := make(map[string]struct{})
	targets := make([]string, 0)
	for _, phase := range workflow.Phases {
		if !phase.IsCall() {
			continue
		}
		target := phase.CallTarget()
		if target == "" {
			continue
		}
		if _, exists := seen[target]; exists {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}
