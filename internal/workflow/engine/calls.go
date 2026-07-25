package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// WorkItemSourceCall marks a run that exists because a call phase invoked it.
// It is the item's provenance, exactly like `manual` or `automation`: nobody
// enqueued this run, a parent phase did.
const WorkItemSourceCall = "call"

// MaxCallDepth is the absolute ceiling on how deep a call tree may nest,
// independent of any declared `max_depth`. Validation requires a bound on every
// cycle it can see, but the child of a call is resolved *live* at call time —
// an edit that introduces a cycle after the parent's dry-run would otherwise
// recurse until the process died. This is the structural stop; a declared
// max_depth is the author's much tighter one.
const MaxCallDepth = 32

// startCall enters a call phase: it resolves the child workflow at call time,
// evaluates the argument map, enforces the depth bounds, and starts the child
// as a real run linked into this item's tree (§3a).
//
// The parent then rests. It holds no runner, no resources, and no provider
// capacity while the child runs — the child's terminal state is what re-enters
// it (settleCallChild).
func (e *Engine) startCall(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	target := phase.CallTarget()
	if target == "" {
		return e.parkCallSetup(item, ReasonWiringError,
			fmt.Errorf("call phase %q of item %q names no workflow", phase.ID, item.item.ID))
	}
	chain, err := e.callChain(item)
	if err != nil {
		return e.parkCallSetup(item, ReasonSetupFailed, err)
	}
	if err := checkCallDepth(chain, item.workflow.ID, phase, target); err != nil {
		// A cycle past its declared bound is the definition and the run disagreeing
		// about how far this may go: a wiring error, carrying the chain that got
		// here so the human can see the recursion.
		return e.parkCallSetup(item, ReasonWiringError, err)
	}
	args, err := callArgs(phase, vars)
	if err != nil {
		return e.parkCallSetup(item, ReasonWiringError,
			fmt.Errorf("call %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	seeds, err := json.Marshal(args)
	if err != nil {
		return e.parkCallSetup(item, ReasonWiringError,
			fmt.Errorf("call %s/%s/%d: encode args: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	resolved, err := e.definitions.ResolveCall(e.ctx, item.item.ProjectID, target)
	if err != nil {
		return e.parkCallSetup(item, ReasonWiringError,
			fmt.Errorf("call %s/%s/%d: resolve workflow %q: %w", item.item.ID, phase.ID, item.attempt, target, err))
	}
	// The workspace is provisioned by the runner and persisted on the row, so the
	// engine's in-memory copy of it is stale by construction — re-read before
	// stamping a child with it, and keep the refreshed values so later calls in
	// this run stamp from the same fact.
	if err := e.refreshWorkspace(item); err != nil {
		return e.parkCallSetup(item, ReasonSetupFailed,
			fmt.Errorf("call %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	child := store.WorkItem{
		ID: uuid.NewString(), ProjectID: item.item.ProjectID, Goal: item.item.Goal,
		WorkflowID: resolved.Workflow.ID, WorkflowScope: string(resolved.Scope),
		State: string(StateRunning), Seeds: seeds, StepMode: item.item.StepMode,
		// Workspace flows down the call stack (§9): the child executes in the
		// caller's workspace and provisions nothing of its own, so it is stamped
		// with the caller's worktree. It can legitimately be empty here — a tree
		// running read-only on the project root has none, and a call that is the
		// root's first phase runs before one is cut — in which case the child
		// resolves its tree's root workspace when it starts its own first phase.
		WorktreePath: item.item.WorktreePath, Branch: item.item.Branch,
		BaseBranch: item.item.BaseBranch,
		Source:     WorkItemSourceCall, SourceRef: callSourceRef(item, phase),
		ParentItemID: item.item.ID, ParentPhaseID: phase.ID, ParentAttempt: item.attempt,
		CallDepth: item.item.CallDepth + 1,
		CreatedAt: e.timestamp(),
	}
	// The parent attempt row is persisted before the child exists (enterPhase
	// wrote it with these same args), so a crash between the two leaves an
	// attempt with no child — which rebuild re-invokes. The reverse order would
	// leave a child whose parent has no record of calling it.
	return e.startNewItem(child, &resolved)
}

// refreshWorkspace re-reads the workspace columns the runner owns. The engine
// never writes them, so its in-memory item carries whatever they were when the
// run started.
func (e *Engine) refreshWorkspace(item *runtimeItem) error {
	stored, err := e.store.GetWorkItem(item.item.ID)
	if err != nil {
		return fmt.Errorf("reload workspace of item %q: %w", item.item.ID, err)
	}
	item.item.WorktreePath = stored.WorktreePath
	item.item.Branch = stored.Branch
	item.item.BaseBranch = stored.BaseBranch
	return nil
}

// callSourceRef records which attempt of which parent phase invoked a child, so
// a child row explains its own existence without a join.
func callSourceRef(item *runtimeItem, phase def.Phase) string {
	return fmt.Sprintf("%s/%s/%d", item.item.ID, phase.ID, item.attempt)
}

func (e *Engine) parkCallSetup(item *runtimeItem, reason Reason, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			output:      parkCauseEnvelope(cause),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}

// callChainStep is one edge of the ancestry that produced the current item: the
// workflow that called, and the phase it called from.
type callChainStep struct {
	workflowID string
	phaseID    string
	itemID     string
}

// callChain walks the parent linkage from this item to the run tree's root and
// returns the call edges that produced it, root-most first. Linkage is
// immutable, so this is a stable fact about the item rather than live state.
func (e *Engine) callChain(item *runtimeItem) ([]callChainStep, error) {
	if item.item.ParentItemID == "" {
		return nil, nil
	}
	var reversed []callChainStep
	current := item.item
	for depth := 0; current.ParentItemID != ""; depth++ {
		if depth > MaxCallDepth {
			return nil, fmt.Errorf("call chain for item %q exceeds %d ancestors", item.item.ID, MaxCallDepth)
		}
		parent, err := e.store.GetWorkItem(current.ParentItemID)
		if err != nil {
			return nil, fmt.Errorf("load parent %q of item %q: %w", current.ParentItemID, current.ID, err)
		}
		reversed = append(reversed, callChainStep{
			workflowID: parent.WorkflowID, phaseID: current.ParentPhaseID, itemID: parent.ID,
		})
		current = parent
	}
	chain := make([]callChainStep, 0, len(reversed))
	for index := len(reversed) - 1; index >= 0; index-- {
		chain = append(chain, reversed[index])
	}
	return chain, nil
}

// checkCallDepth enforces both bounds on one call edge: the author's declared
// `max_depth` for the edge, and the engine's absolute MaxCallDepth for the tree.
//
// A declared max_depth bounds how many times *this edge* may appear in one
// ancestry chain, which is what makes a self-call loop terminate: the Nth
// invocation is refused rather than the (N+1)th silently starting.
func checkCallDepth(chain []callChainStep, workflowID string, phase def.Phase, target string) error {
	if len(chain) >= MaxCallDepth {
		return fmt.Errorf(
			"call to %q from phase %q exceeds the maximum call depth of %d; chain: %s",
			target, phase.ID, MaxCallDepth, renderCallChain(chain, workflowID, phase.ID),
		)
	}
	if phase.MaxDepth < 1 {
		return nil
	}
	traversals := 0
	for _, step := range chain {
		if step.workflowID == workflowID && step.phaseID == phase.ID {
			traversals++
		}
	}
	if traversals >= phase.MaxDepth {
		return fmt.Errorf(
			"call to %q from phase %q reached its max_depth of %d; chain: %s",
			target, phase.ID, phase.MaxDepth, renderCallChain(chain, workflowID, phase.ID),
		)
	}
	return nil
}

// renderCallChain writes the ancestry as `workflow.phase -> workflow.phase`,
// ending at the edge being refused, so a parked wiring error shows the
// recursion instead of just naming its bound.
func renderCallChain(chain []callChainStep, workflowID, phaseID string) string {
	parts := make([]string, 0, len(chain)+1)
	for _, step := range chain {
		parts = append(parts, step.workflowID+"."+step.phaseID)
	}
	parts = append(parts, workflowID+"."+phaseID)
	return strings.Join(parts, " -> ")
}

// callArgs evaluates a call phase's argument map against the caller's variable
// context. Every argument is a reference into the caller's variables; a
// reference that does not resolve is a wiring error rather than a silently
// absent child input, because the child would then fail its own input
// validation with no trace of where the value was supposed to come from.
func callArgs(phase def.Phase, vars map[string]any) (map[string]any, error) {
	if len(phase.Args) == 0 {
		return map[string]any{}, nil
	}
	names := make([]string, 0, len(phase.Args))
	for name := range phase.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	args := make(map[string]any, len(phase.Args))
	var missing []string
	for _, name := range names {
		value, ok := def.LookupVariable(vars, phase.Args[name])
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (%s)", name, phase.Args[name]))
			continue
		}
		args[name] = value
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("arguments do not resolve: %s", strings.Join(missing, ", "))
	}
	return args, nil
}

// callChildOf returns the child run one call attempt created, if it exists. A
// call attempt creates at most one child: a rerun of the phase is a new attempt
// with a new child, and a repaired attempt keeps the child it already has.
func (e *Engine) callChildOf(itemID, phaseID string, attempt int) (store.WorkItem, bool, error) {
	children, err := e.store.ListWorkItemCallChildren(itemID, phaseID, attempt)
	if err != nil {
		return store.WorkItem{}, false, fmt.Errorf("load call children of %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	if len(children) == 0 {
		return store.WorkItem{}, false, nil
	}
	// Ordered oldest-first by the store; the newest is the invocation this
	// attempt is actually waiting on if a repair ever produced more than one.
	return children[len(children)-1], true, nil
}

// settleCallChild maps a finished child run onto the parent phase that called
// it. It runs on the command loop like every other transition, queued by the
// child's own terminal transition so the child's state is fully persisted
// before the parent reads it.
func (e *Engine) settleCallChild(child store.WorkItem) error {
	if child.ParentItemID == "" {
		return nil
	}
	parent, resident := e.items[child.ParentItemID]
	if !resident {
		// The parent is parked or terminal: it is not waiting on this child any
		// more (a takeover, a cancel, or a crash park got there first), and a
		// human action is what resumes it.
		return nil
	}
	if parent.phaseID != child.ParentPhaseID || parent.attempt != child.ParentAttempt ||
		State(parent.item.State) != StateRunning {
		return nil // The parent moved on; this completion is stale, not an error.
	}
	phase, ok := findPhase(parent.workflow, parent.phaseID)
	if !ok || !phase.IsCall() {
		return errors.Join(
			e.teardown(parent, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			fmt.Errorf("call child %q settled onto phase %q of item %q, which is not a call phase",
				child.ID, parent.phaseID, parent.item.ID),
		)
	}
	switch State(child.State) {
	case StateDone:
		envelope, err := e.childOutputEnvelope(child)
		if err != nil {
			return errors.Join(
				e.teardown(parent, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
				err,
			)
		}
		return e.completeDone(parent, envelope)
	case StateFailed:
		return e.teardown(parent, teardownRequest{
			output:      childOutcomeEnvelope(child, "failed"),
			phaseStatus: "failed", nextState: StateFailed, reason: ReasonChildFailed,
		})
	case StateCancelled:
		// Cancelling the parent too is the human's call, not the engine's: the
		// child was stopped on purpose and the parent's own work is intact.
		return e.teardown(parent, teardownRequest{
			output:      childOutcomeEnvelope(child, "cancelled"),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonAgentError,
		})
	default:
		return nil // Still running or parked: the parent keeps waiting.
	}
}

// childOutputEnvelope synthesizes the call phase's envelope from a completed
// child run: `status: done` with the child workflow's declared `outputs:` (§3a).
// This is the whole downstream surface of a call phase, and it is what the
// parent's gate routes on.
func (e *Engine) childOutputEnvelope(child store.WorkItem) (json.RawMessage, error) {
	snapshot, err := decodeSnapshot(child.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("read child %q snapshot: %w", child.ID, err)
	}
	vars, _, err := e.variableContext(&runtimeItem{item: child}, nil)
	if err != nil {
		return nil, fmt.Errorf("read child %q variables: %w", child.ID, err)
	}
	outputs := make(map[string]any, len(snapshot.Workflow.Outputs))
	var missing []string
	for name, declaration := range snapshot.Workflow.Outputs {
		value, ok := def.LookupVariable(vars, declaration.From)
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (%s)", name, declaration.From))
			continue
		}
		outputs[name] = value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("child %q completed without its declared outputs: %s", child.ID, strings.Join(missing, ", "))
	}
	envelope, err := json.Marshal(controlEnvelope{Status: "done", Outputs: outputs})
	if err != nil {
		return nil, fmt.Errorf("encode child %q outputs: %w", child.ID, err)
	}
	return envelope, nil
}

// childOutcomeEnvelope is the partial envelope a non-done child leaves on the
// parent's call phase. The parent ran no turn, so this is the only record of
// why its phase ended, and it names the child so the tree is navigable from it.
func childOutcomeEnvelope(child store.WorkItem, outcome string) json.RawMessage {
	reason := fmt.Sprintf("child run %s (workflow %q) %s", child.ID, child.WorkflowID, outcome)
	if child.Reason != "" {
		reason += ": " + child.Reason
	}
	envelope, err := json.Marshal(controlEnvelope{Status: "stuck", Outputs: map[string]any{}, Reason: reason})
	if err != nil {
		return nil
	}
	return envelope
}

// cancelCallChildren brings a parent's whole live child subtree down before the
// parent itself moves, which is the tree-aware half of the teardown contract
// (§12, D23). A descendant whose parent has left its call phase can never be
// consumed by anything, so leaving one running would strand a real provider
// session with no reader.
func (e *Engine) cancelCallChildren(item *runtimeItem) error {
	if item.phaseID == "" {
		return nil
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok || !phase.IsCall() {
		// Only a call phase can have live children: it is the one phase that does
		// not finish until its child is terminal.
		return nil
	}
	return e.cancelDescendants(item.item.ID, 0)
}

// cancelDescendants cancels every non-terminal descendant of an item, deepest
// first. Resident runs go through the same teardown every cancel uses; a parked
// descendant holds nothing and is transitioned in place, because the engine
// evicts parked items from memory and it must still come down with its tree.
func (e *Engine) cancelDescendants(itemID string, depth int) error {
	if depth > MaxCallDepth {
		return fmt.Errorf("cancel descendants of %q: tree is deeper than %d", itemID, MaxCallDepth)
	}
	children, err := e.store.ListWorkItemChildren(itemID)
	if err != nil {
		return fmt.Errorf("list children of %q: %w", itemID, err)
	}
	var errs []error
	for _, child := range children {
		state := State(child.State)
		if state != StateRunning && state != StateNeedsHuman {
			continue
		}
		if err := e.cancelDescendants(child.ID, depth+1); err != nil {
			errs = append(errs, err)
		}
		resident, tracked := e.items[child.ID]
		if tracked {
			errs = append(errs, e.teardown(resident, teardownRequest{
				phaseStatus: "cancelled", nextState: StateCancelled, reason: ReasonInterrupted,
			}))
			continue
		}
		errs = append(errs, e.cancelEvictedChild(child))
	}
	return errors.Join(errs...)
}

// cancelEvictedChild cancels a parked descendant the scheduler no longer holds
// in memory. It owns no resources and no runner — parking released both — so
// the transition is the whole teardown for it.
func (e *Engine) cancelEvictedChild(child store.WorkItem) error {
	endedAt := e.timestamp()
	if err := e.store.UpdateWorkItemState(child.ID, string(StateCancelled), string(ReasonInterrupted), endedAt); err != nil {
		return fmt.Errorf("cancel parked child %q: %w", child.ID, err)
	}
	e.emitItemState(child.ID, child.ProjectID, State(child.State), StateCancelled, ReasonInterrupted)
	return nil
}

// callPhase reports whether a frozen phase id names a call phase, which is what
// makes a `running` attempt with no envelope and no runner a legitimate resting
// state rather than an interrupted one.
func callPhase(workflow def.Workflow, phaseID string) bool {
	phase, ok := findPhase(workflow, phaseID)
	return ok && phase.IsCall()
}

// recoverCall re-links a rebuilt parent to the child its call attempt created.
// Rebuild order does not matter: a child that is still active rebuilds as an
// ordinary item and reports later, and a child that already reached a terminal
// state settles the parent here exactly as a live completion would.
//
// The invocation order is what makes the missing-child case safe: enterPhase
// persists the attempt (with its args) *before* startCall creates the child, so
// the only gap a crash can open is an attempt with no child — which re-invokes
// cleanly. The reverse order could leave a child no attempt row claims, running
// with nothing to consume it.
func (e *Engine) recoverCall(item *runtimeItem, attempt store.WorkItemPhase) error {
	phase, ok := findPhase(item.workflow, attempt.PhaseID)
	if !ok {
		return fmt.Errorf("phase %q is absent from item %q snapshot", attempt.PhaseID, item.item.ID)
	}
	child, found, err := e.callChildOf(item.item.ID, attempt.PhaseID, attempt.Attempt)
	if err != nil {
		return err
	}
	if !found {
		var input PhaseInput
		if len(attempt.InputEnvelope) > 0 {
			if err := decodeJSON(attempt.InputEnvelope, &input); err != nil {
				return fmt.Errorf("decode call attempt %s/%s/%d input: %w",
					item.item.ID, attempt.PhaseID, attempt.Attempt, err)
			}
		}
		return e.startCall(item, phase, input.Vars)
	}
	if State(child.State) == StateRunning || State(child.State) == StateNeedsHuman {
		return nil // The parent rests; the child's own completion re-enters it.
	}
	return e.settleCallChild(child)
}
