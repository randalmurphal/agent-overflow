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
//
// It is sized for the deliberate long chain, not for the accident. A campaign —
// a root run whose final gate calls itself for the next wave — is one call per
// wave, and a campaign of a hundred-plus waves is the shape this exists to
// permit rather than refuse. The cost of depth is the O(depth) parent walks
// (`callChain`, budget-root and workspace-root resolution, the pause and cancel
// subtree walks), each a point read on an indexed primary key, so a ceiling in
// the hundreds is bounded by arithmetic rather than by risk. Runaway recursion
// still dies here, hundreds of runs before anything else notices.
const MaxCallDepth = 256

// startCall enters a call phase: it resolves the child workflow at call time,
// evaluates the argument map, enforces the depth bounds, and starts the child
// as a real run linked into this item's tree (§3a).
//
// The parent then rests. It holds no runner, no resources, and no provider
// capacity while the child runs — the child's terminal state is what re-enters
// it (settleCallChild).
func (e *Engine) startCall(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	// The soft stop's one checkpoint (D36). It is read here — before the target
	// is resolved, before a child row exists, before anything is spent — because
	// this is the moment the next wave would begin, and it is the only moment at
	// which stopping costs nothing.
	root, stop, err := e.softStopArmed(item)
	if err != nil {
		return e.parkCallSetup(item, ReasonSetupFailed,
			fmt.Errorf("call %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	if stop {
		return e.parkSoftStop(item, root)
	}
	if phase.CallTarget() == "" {
		return e.parkCallSetup(item, ReasonWiringError,
			fmt.Errorf("call phase %q of item %q names no workflow", phase.ID, item.item.ID))
	}
	invocation := callInvocation{
		edge:   callEdge{phaseID: phase.ID, maxDepth: phase.MaxDepth},
		target: phase.CallTarget(), declared: phase.Args, vars: vars,
		// Workspace flows down the call stack (§9): a serial call's child executes
		// in the caller's own workspace and provisions nothing of its own.
		inheritWorkspace: true,
	}
	plan, reason, err := e.planCall(item, invocation)
	if err != nil {
		return e.parkCallSetup(item, reason, err)
	}
	reason, err = e.invokeCall(item, invocation, plan)
	if reason != "" {
		return e.parkCallSetup(item, reason, err)
	}
	return err
}

// callInvocation is one call edge's whole request: where it is declared, what it
// invokes, the argument map and caller context the child is seeded from, and
// whether the child inherits the caller's workspace.
//
// inheritWorkspace is false for a fan-out unit's call. Isolation is introduced
// by fan-out (§9), so that child runs in the *unit's* sub-worktree rather than
// the caller's — and the sub-worktree belongs to the app, which resolves it
// through the child's parent linkage when the child's first phase starts.
// Stamping the caller's worktree here would put every unit's child back in the
// one checkout the fan-out exists to keep them out of.
type callInvocation struct {
	edge             callEdge
	target           string
	declared         map[string]string
	vars             map[string]any
	inheritWorkspace bool
}

// callPlan is one call edge decided but not yet made: the child definition this
// invocation froze and the seeds it will carry. Nothing is written to produce
// it, so an edge that cannot be made leaves no unit row, no child run, and no
// provider session behind.
type callPlan struct {
	resolved ResolvedDefinition
	args     map[string]any
}

// planCall decides one call edge without making it: the ancestry, both depth
// bounds, the live definition resolution, and the argument evaluation that only
// the resolved child can judge. It is shared by `shape: call` phases and
// call-bound fan-out units, so the two edges cannot disagree about what a
// makeable call is.
//
// A non-nil error always carries the Reason the caller must park under; the
// caller has written nothing at that point, which is what lets a unit edge park
// its attempt without recording a unit outcome.
func (e *Engine) planCall(item *runtimeItem, invocation callInvocation) (callPlan, Reason, error) {
	site := fmt.Sprintf("call %s/%s/%d", item.item.ID, invocation.edge, item.attempt)
	chain, err := e.callChain(item)
	if err != nil {
		return callPlan{}, ReasonSetupFailed, err
	}
	if err := checkCallDepth(chain, item.workflow.ID, invocation.edge, invocation.target); err != nil {
		// A cycle past its declared bound is the definition and the run disagreeing
		// about how far this may go: a wiring error, carrying the chain that got
		// here so the human can see the recursion.
		return callPlan{}, ReasonWiringError, err
	}
	resolved, err := e.definitions.ResolveCall(e.ctx, item.item.ProjectID, invocation.target)
	if err != nil {
		return callPlan{}, ReasonWiringError, fmt.Errorf("%s: resolve workflow %q: %w", site, invocation.target, err)
	}
	// The child is resolved before the arguments are judged because the child is
	// what says which of them may be absent.
	args, unresolved := resolveCallArgs(invocation.declared, invocation.vars)
	if err := requireResolvedArgs(resolved.Workflow, unresolved); err != nil {
		return callPlan{}, ReasonWiringError, fmt.Errorf("%s: %w", site, err)
	}
	return callPlan{resolved: resolved, args: args}, "", nil
}

// invokeCall creates and starts one planned call edge's child run, stamping the
// workspace the edge inherits and the linkage that makes the child recoverable.
//
// A non-empty Reason means the caller should park under it with the returned
// cause. An empty Reason with a non-nil error is a child that could not be
// admitted at all; the parent is left as it is, exactly as it was before, and
// rebuild re-invokes the call from the persisted attempt.
func (e *Engine) invokeCall(item *runtimeItem, invocation callInvocation, plan callPlan) (Reason, error) {
	site := fmt.Sprintf("call %s/%s/%d", item.item.ID, invocation.edge, item.attempt)
	seeds, err := json.Marshal(plan.args)
	if err != nil {
		return ReasonWiringError, fmt.Errorf("%s: encode args: %w", site, err)
	}
	child := store.WorkItem{
		ID: uuid.NewString(), ProjectID: item.item.ProjectID, Goal: item.item.Goal,
		WorkflowID: plan.resolved.Workflow.ID, WorkflowScope: string(plan.resolved.Scope),
		State: string(StateRunning), Seeds: seeds, StepMode: item.item.StepMode,
		Source: WorkItemSourceCall, SourceRef: callSourceRef(item, invocation.edge),
		ParentItemID: item.item.ID, ParentPhaseID: invocation.edge.phaseID,
		ParentUnitID: invocation.edge.unitID, ParentAttempt: item.attempt,
		CallDepth: item.item.CallDepth + 1,
		CreatedAt: e.timestamp(),
	}
	if invocation.inheritWorkspace {
		// The workspace is provisioned by the runner and persisted on the row, so
		// the engine's in-memory copy of it is stale by construction — re-read
		// before stamping a child with it, and keep the refreshed values so later
		// calls in this run stamp from the same fact.
		if err := e.refreshWorkspace(item); err != nil {
			return ReasonSetupFailed, fmt.Errorf("%s: %w", site, err)
		}
		// It can legitimately be empty here — a tree running read-only on the
		// project root has none, and a call that is the root's first phase runs
		// before one is cut — in which case the child resolves its tree's root
		// workspace when it starts its own first phase.
		child.WorktreePath, child.Branch = item.item.WorktreePath, item.item.Branch
		child.BaseBranch = item.item.BaseBranch
	}
	// The parent attempt row is persisted before the child exists (enterPhase
	// wrote it with these same args), so a crash between the two leaves an
	// attempt with no child — which rebuild re-invokes. The reverse order would
	// leave a child whose parent has no record of calling it.
	return "", e.startNewItem(child, &plan.resolved)
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

// callSourceRef records which attempt of which call site invoked a child, so a
// child row explains its own existence without a join.
func callSourceRef(item *runtimeItem, edge callEdge) string {
	return fmt.Sprintf("%s/%s/%d", item.item.ID, edge, item.attempt)
}

// parkCallSetup parks a call phase whose edge could not be made. The cause rides
// the attempt's `park_cause` — the call chain of a depth refusal, the argument
// that named no input of the child — because the phase ran no turn and there is
// no envelope for it to be in.
func (e *Engine) parkCallSetup(item *runtimeItem, reason Reason, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked", nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}

// callEdge identifies one call site inside a workflow: the phase it lives on
// and, for a fan-out unit's call, that unit. Depth is counted per edge, so a
// phase call and a unit call on the same phase are different edges with
// independent bounds — which is what lets a campaign's fan-out unit recurse
// under its own ceiling while the root's self-call keeps its own.
type callEdge struct {
	phaseID  string
	unitID   string
	maxDepth int
}

func (e callEdge) String() string {
	if e.unitID == "" {
		return e.phaseID
	}
	return e.phaseID + "[" + e.unitID + "]"
}

// callChainStep is one edge of the ancestry that produced the current item: the
// workflow that called, and the call site it called from.
type callChainStep struct {
	workflowID string
	edge       callEdge
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
			workflowID: parent.WorkflowID,
			edge:       callEdge{phaseID: current.ParentPhaseID, unitID: current.ParentUnitID},
			itemID:     parent.ID,
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
// invocation is refused rather than the (N+1)th silently starting. A fan-out
// unit's call edge is counted by (workflow, phase, unit), so sibling units of
// one attempt do not spend each other's budget and a unit edge does not spend
// the budget of a phase edge on the same phase.
func checkCallDepth(chain []callChainStep, workflowID string, edge callEdge, target string) error {
	if len(chain) >= MaxCallDepth {
		return fmt.Errorf(
			"call to %q from %q exceeds the maximum call depth of %d; chain: %s",
			target, edge, MaxCallDepth, renderCallChain(chain, workflowID, edge),
		)
	}
	if edge.maxDepth < 1 {
		return nil
	}
	traversals := 0
	for _, step := range chain {
		if step.workflowID == workflowID && step.edge.phaseID == edge.phaseID && step.edge.unitID == edge.unitID {
			traversals++
		}
	}
	if traversals >= edge.maxDepth {
		return fmt.Errorf(
			"call to %q from %q reached its max_depth of %d; chain: %s",
			target, edge, edge.maxDepth, renderCallChain(chain, workflowID, edge),
		)
	}
	return nil
}

// renderCallChain writes the ancestry as `workflow.call-site -> workflow.call-site`,
// ending at the edge being refused, so a parked wiring error shows the
// recursion instead of just naming its bound.
func renderCallChain(chain []callChainStep, workflowID string, edge callEdge) string {
	parts := make([]string, 0, len(chain)+1)
	for _, step := range chain {
		parts = append(parts, step.workflowID+"."+step.edge.String())
	}
	parts = append(parts, workflowID+"."+edge.String())
	return strings.Join(parts, " -> ")
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
	if child.ParentUnitID != "" {
		return e.settleUnitCallChild(parent, child)
	}
	phase, ok := findPhase(parent.workflow, parent.phaseID)
	if !ok || !phase.IsCall() {
		cause := fmt.Errorf("call child %q settled onto phase %q of item %q, which is not a call phase",
			child.ID, parent.phaseID, parent.item.ID)
		return errors.Join(
			e.teardown(parent, teardownRequest{
				cause: cause, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: ReasonWiringError,
			}),
			cause,
		)
	}
	switch State(child.State) {
	case StateDone:
		envelope, err := e.childOutputEnvelope(child)
		if err != nil {
			return errors.Join(
				e.teardown(parent, teardownRequest{
					cause: err, phaseStatus: "parked",
					nextState: StateNeedsHuman, reason: ReasonWiringError,
				}),
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
			output: childOutcomeEnvelope(child, "cancelled"),
			cause: fmt.Errorf(
				"called run %s (workflow %q) was cancelled, so this phase has no result to route on; cancelling this run too is a human's decision",
				child.ID, child.WorkflowID),
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
	vars, _, err := e.variableContext(&runtimeItem{item: child}, attemptRef{})
	if err != nil {
		return nil, fmt.Errorf("read child %q variables: %w", child.ID, err)
	}
	outputs := make(map[string]any, len(snapshot.Workflow.Outputs))
	var missing []string
	for name, declaration := range snapshot.Workflow.Outputs {
		value, ok := def.LookupVariable(vars, declaration.From)
		if !ok {
			// An optional deliverable the completion path did not produce is
			// OMITTED, not failed and not nulled — the same shape an absent
			// optional call argument takes (D45), so the parent's optional input
			// sees the "not supplied" a direct start would have produced. A
			// required one still fails: the caller's gate routes on these names.
			if !declaration.Optional {
				missing = append(missing, fmt.Sprintf("%s (%s)", name, declaration.From))
			}
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
