package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/eventchan"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// Execution of one fan-out unit. A unit is a piece of its phase attempt, not a
// phase of its own: it shares the reliability timers, envelope-retry, narrative,
// and masked-output machinery, and differs only in what its envelope must
// satisfy and where it runs.
//
// The join is an ordinary unit here with two exceptions, both consequences of
// one rule — its envelope IS the phase's: it answers the phase's contract, and
// it runs in the item's primary workspace rather than a sub-worktree, because
// the result it produces is the phase's result.

// workflowUnitPlan is one unit's resolved execution context: what its envelope
// must satisfy, what its prompt or command may reference, where it runs, and
// where its files live. Resolving it is separate from running it so the agent
// and tool paths cannot answer those questions differently.
type workflowUnitPlan struct {
	kind        engine.UnitKind
	label       string
	unitAttempt int
	// contract is the phase's for a join and the unit's own for a work unit.
	contract     def.EnvelopeContract
	declarations map[string]def.Variable
	// accountsForUnits and accountedUnits carry the merge contract a join opted
	// into (`accounts_for_units:`). They are resolved once, here, so the set the
	// join's prompt names and the set its envelope is verified against are the
	// same list — the whole point of the contract is that those cannot differ.
	accountsForUnits bool
	accountedUnits   []string
	workspace        PreparedWorkspace
	narrativePath    string
	envelopePath     string
}

func (r *Runner) startUnit(ctx context.Context, request engine.RunRequest, complete func(engine.Outcome)) error {
	if request.Unit == nil {
		return fmt.Errorf(
			"workflow runner: unit %q of phase %q arrived without its stamped definition",
			request.Key.UnitID, request.Key.PhaseID,
		)
	}
	if request.Phase.EffectiveShape() != def.ShapeFanOut {
		return fmt.Errorf(
			"workflow runner: phase %q has shape %q and runs no units",
			request.Phase.ID, request.Phase.EffectiveShape(),
		)
	}
	unit := *request.Unit
	driver, runsWork := unit.EffectiveDriver()
	if !runsWork {
		// A call unit's work is a child run the engine starts and settles; it never
		// reaches a runner. One arriving here is a scheduling bug, not a definition
		// problem, and starting a turn for it would run an unbound prompt.
		return fmt.Errorf(
			"workflow runner: unit %q of phase %q invokes workflow %q and runs no turn of its own",
			request.Key.UnitID, request.Key.PhaseID, unit.CallTarget(),
		)
	}
	plan, err := r.planUnit(ctx, request, unit)
	if err != nil {
		return err
	}
	if driver == def.DriverTool {
		if request.Launch.FinalizesTakeover() {
			return fmt.Errorf("workflow runner: tool %s cannot finalize a takeover", plan.label)
		}
		return r.startToolUnit(ctx, request, unit, plan, complete)
	}
	return r.startAgentUnit(ctx, request, unit, plan, complete)
}

// planUnit resolves everything a unit needs before anything is started, and
// provisions the sub-worktree a writing unit runs in.
func (r *Runner) planUnit(ctx context.Context, request engine.RunRequest, unit def.Unit) (workflowUnitPlan, error) {
	plan := workflowUnitPlan{
		kind:        request.UnitKind,
		label:       fmt.Sprintf("unit %q of phase %q", request.Key.UnitID, request.Key.PhaseID),
		unitAttempt: request.UnitAttempt,
	}
	if plan.unitAttempt < 1 {
		return workflowUnitPlan{}, fmt.Errorf("workflow runner: %s arrived without a try number", plan.label)
	}
	// Kind decides which contract the unit's envelope answers and whether it is
	// isolated, so an unrecognized one must not fall through to the work-unit
	// branch: that would run a real turn against the wrong contract.
	switch plan.kind {
	case engine.UnitWork, engine.UnitJoin:
	default:
		return workflowUnitPlan{}, fmt.Errorf(
			"workflow runner: %s arrived with unknown unit kind %q", plan.label, plan.kind,
		)
	}
	if plan.kind == engine.UnitJoin {
		// The ids come out of the reserved `units` binding the engine bound the
		// attempt's results under, which is the same list the join's prompt
		// renders — so a join can never be refused for omitting a unit it was
		// never shown. `JoinEnvelope` is a no-op for a join that did not opt in.
		plan.accountsForUnits = request.Phase.Join != nil && request.Phase.Join.AccountsForUnits
		plan.accountedUnits = def.UnitIDsFromResults(request.Vars)
		plan.contract = def.JoinEnvelope(request.Phase, plan.accountedUnits)
		plan.declarations = def.JoinDeclarations(request.Phase)
	} else {
		plan.contract = def.UnitEnvelope(unit)
		plan.declarations = def.ResolveUnitDeclarations(request.Workflow, request.Phase)
	}
	r.markStartStep(request.Key, workflowStartStepWorkspace)
	primary, err := r.prepareWorkspace(ctx, request)
	if err != nil {
		return workflowUnitPlan{}, errors.Join(engine.ErrSetupFailed, err)
	}
	plan.workspace = primary
	// A writing work unit gets its own sub-worktree on its own branch (spec §9):
	// siblings run at the same time and would otherwise fight over one checkout.
	// The join never gets one — it consolidates the units' branches, and the
	// result it produces is the phase's, which belongs on the item's branch.
	if plan.kind != engine.UnitJoin && unit.EffectiveAccess() == def.AccessWrite {
		sub, err := r.provisionUnitWorktree(ctx, UnitWorkspaceRef{
			ProjectID: request.Item.ProjectID, ItemID: request.Key.ItemID, PhaseID: request.Key.PhaseID,
			Attempt: request.Key.Attempt, UnitID: request.Key.UnitID, UnitAttempt: plan.unitAttempt,
		}, primary)
		if err != nil {
			return workflowUnitPlan{}, errors.Join(engine.ErrSetupFailed, err)
		}
		plan.workspace = sub
	}
	if plan.kind == engine.UnitJoin {
		// Before anything interpolates `units` into the join's prompt or argv:
		// both shapes read the same map, so enriching it once here is what makes
		// the agent join and the tool join see the same facts.
		r.enrichJoinUnits(request, primary)
	}
	// Everything above is arbitrary-length work on the repository — provisioning a
	// sub-worktree, and one `git` enumeration per unit branch on a join, which on a
	// large history is not fast. Only bounded internal work is left from here, so
	// this is the same boundary the phase path arms at, for the same reason.
	r.armStartDeadline(request.Key)
	r.markStartStep(request.Key, workflowStartStepNarrative)
	plan.narrativePath, err = workflowrunner.UnitNarrativePath(
		r.dataRoot, request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt,
		request.Key.UnitID, plan.unitAttempt,
	)
	if err != nil {
		return workflowUnitPlan{}, err
	}
	plan.envelopePath, err = workflowrunner.UnitEnvelopePath(
		r.dataRoot, request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt,
		request.Key.UnitID, plan.unitAttempt,
	)
	if err != nil {
		return workflowUnitPlan{}, err
	}
	return plan, nil
}

// enrichJoinUnits adds the per-unit git state to the reserved `units` results
// the join consolidates from. The engine builds those results from store rows
// and is git-free by boundary, so the two fields that require asking git are
// added here, in the app runner, on the one seam both join shapes pass through.
//
// `commitsAhead` is what the unit's branch carries that the item's branch does
// not — the only durable measure of what a lane produced, since the join merges
// branches and a done join then retires the checkouts. `dirty` is what that
// branch does NOT carry: work still sitting in the unit's worktree, which the
// merge cannot see. Together they let a merge command refuse a lane, commit it,
// or report it, instead of silently merging nothing.
//
// It is informational, so it never fails a join. A read fails, the field is
// absent for that entry, and the failure is reported once on the error channel
// rather than swallowed — a join that cannot start because a git count failed
// would be strictly worse than a join that runs without the count.
func (r *Runner) enrichJoinUnits(request engine.RunRequest, primary PreparedWorkspace) {
	entries, ok := request.Vars[def.UnitsVariable].([]any)
	if !ok || len(entries) == 0 {
		return
	}
	// The project root, not a worktree: a unit's checkout may already be gone
	// (a re-run join after retirement), and refs are repository-wide anyway.
	projectPath := primary.Project.Path
	base := strings.TrimSpace(primary.Branch)
	var errs []error
	for _, entry := range entries {
		result, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		branch, _ := result["branch"].(string)
		if strings.TrimSpace(branch) == "" {
			// A unit that never started, or was dropped before it cut anything,
			// has no branch and no checkout: there is nothing to ask git about.
			continue
		}
		unitID, _ := result["id"].(string)
		if base == "" {
			// A unit branch exists but the item records none to count it against.
			// Unit branches are cut FROM the item's, so this cannot happen without
			// something else being wrong — it is reported rather than skipped.
			errs = append(errs, fmt.Errorf(
				"unit %q is on branch %q but the item workspace records no branch to count against",
				unitID, branch,
			))
		} else if ahead, err := r.host.GitCore().CountCommitsAhead(projectPath, branch, base); err != nil {
			errs = append(errs, fmt.Errorf("count commits on unit %q branch %q: %w", unitID, branch, err))
		} else {
			result["commitsAhead"] = ahead
		}
		worktree, _ := result["worktree"].(string)
		if strings.TrimSpace(worktree) == "" {
			// The checkout is already retired, so there is no working tree to
			// have anything in. The key is omitted rather than reported false:
			// "no answer" and "clean" are different facts to a merge script.
			continue
		}
		changes, err := r.host.GitCore().CountWorkingTreeChanges(worktree)
		if err != nil {
			errs = append(errs, fmt.Errorf("read unit %q worktree %q state: %w", unitID, worktree, err))
			continue
		}
		result["dirty"] = changes > 0
	}
	if len(errs) > 0 {
		r.reportUnitGitStateFailure(request.Key.ItemID, errors.Join(errs...))
	}
}

// startAgentUnit runs one unit as a provider turn on its own AO thread.
func (r *Runner) startAgentUnit(
	ctx context.Context, request engine.RunRequest, unit def.Unit,
	plan workflowUnitPlan, complete func(engine.Outcome),
) error {
	if strings.TrimSpace(unit.Provider) == "" {
		return errors.Join(engine.ErrWiringFailed, fmt.Errorf(
			"workflow runner: %s runs an agent turn but declares no provider", plan.label,
		))
	}
	schema, err := plan.contract.Schema()
	if err != nil {
		return fmt.Errorf("workflow runner: build %s envelope schema: %w", plan.label, err)
	}
	r.markStartStep(request.Key, workflowStartStepReliability)
	watchdog, backoff, err := r.reliability(ctx, request)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plan.narrativePath), appdirs.PrivateDirPerm); err != nil {
		return fmt.Errorf("workflow runner: create %s narrative directory: %w", plan.label, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}

	spec := ThreadSpec{
		ItemID: request.Key.ItemID, Label: plan.label,
		Title:        UnitThreadTitle(request.Phase, request.Key.UnitID),
		ProviderName: unit.Provider, Model: unit.Model,
		Effort: unit.Effort,
		Access: unit.EffectiveAccess(), Workspace: plan.workspace,
	}
	promptContext := r.host.WorkflowPromptAncestry(request.Key.ItemID, request.Workflow)
	promptContext.NarrativePath = plan.narrativePath
	promptContext.Access = unit.EffectiveAccess()
	// A join that opted into the merge contract is told the exact set it will be
	// post-validated against, and is told it whichever way its turn is entered:
	// a takeover finalize answers the same contract its first try did.
	promptContext.AccountsForUnits, promptContext.AccountedUnits = plan.accountsForUnits, plan.accountedUnits
	r.markStartStep(request.Key, workflowStartStepSessionProof)
	prepared, err := r.prepareAgentTurn(ctx, workflowAgentTurnPlan{
		request: request, thread: spec, schema: schema,
		promptContext: promptContext,
		buildFull: func(context workflowrunner.PromptContext) (string, error) {
			return workflowrunner.BuildUnitPrompt(unit, plan.declarations, request.Vars, context)
		},
		attach: func(threadID string) error { return r.attachUnitRun(request.Key, plan, threadID) },
	})
	if err != nil {
		return err
	}
	if request.Launch.FinalizesTakeover() && unit.Provider == string(provider.Claude) && !prepared.startedSession {
		restarted, err := r.restartClaudeTakeoverWithSchema(ctx, prepared.threadID, schema)
		if err != nil {
			return prepared.discard(r, err)
		}
		prepared.startedSession = restarted
	}
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{
			key: request.Key, unitKind: plan.kind, workflow: request.Workflow,
			narrativePath: plan.narrativePath,
			workspace:     plan.workspace.Path, projectPath: plan.workspace.Project.Path,
		},
		threadID: prepared.threadID,
		schema:   append(json.RawMessage(nil), schema...), contract: plan.contract,
		provider: unit.Provider, phase: request.Phase, complete: complete,
		currentPrompt: prepared.prompt, watchdog: watchdog, backoff: backoff,
	}
	return r.installAttempt(ctx, attempt, prepared)
}

// startToolUnit runs one unit as a deterministic command through the same
// process supervision a tool phase gets.
func (r *Runner) startToolUnit(
	ctx context.Context, request engine.RunRequest, unit def.Unit,
	plan workflowUnitPlan, complete func(engine.Outcome),
) error {
	r.markStartStep(request.Key, workflowStartStepReliability)
	projectProfile, err := r.projectProfile(ctx, request.Item.ProjectID)
	if err != nil {
		return err
	}
	// A unit has no watchdog field of its own: the phase's inactivity window
	// bounds every piece of work the attempt runs.
	watchdog, _, err := workflowReliability(projectProfile, request.Phase)
	if err != nil {
		return err
	}
	r.markStartStep(request.Key, workflowStartStepToolResolve)
	binding, argv, err := workflowUnitToolCommand(projectProfile, unit, plan.declarations, request.Vars)
	if err != nil {
		return errors.Join(engine.ErrWiringFailed, fmt.Errorf(
			"workflow runner: resolve tool %s command: %w", plan.label, err,
		))
	}
	secrets, err := projectProfile.ResolveSecrets()
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, fmt.Errorf(
			"workflow runner: resolve tool %s secrets: %w", plan.label, err,
		))
	}
	if err := prepareWorkflowToolFiles(plan.narrativePath, plan.envelopePath); err != nil {
		return errors.Join(engine.ErrSetupFailed, err)
	}
	if err := r.attachUnitRun(request.Key, plan, ""); err != nil {
		return err
	}
	return r.startToolRun(ctx, workflowToolRun{
		workflowCompletion: workflowCompletion{
			key: request.Key, unitKind: plan.kind, workflow: request.Workflow,
			narrativePath: plan.narrativePath,
			workspace:     plan.workspace.Path, projectPath: plan.workspace.Project.Path,
		},
		label: plan.label, contract: plan.contract, unitAttempt: plan.unitAttempt,
		binding: binding, argv: argv, envelopePath: plan.envelopePath,
		secrets: secrets, watchdog: watchdog,
	}, complete)
}

// attachUnitRun records where a unit's work can be read from the moment it
// exists. A join additionally stamps its thread and narrative onto the phase
// attempt row: the join's envelope IS the phase's, so every phase-level
// continuation (Answer, CompleteTakeover) looks for a thread there. Without it a
// fan-out attempt could park on a question with nothing to answer it on.
func (r *Runner) attachUnitRun(key engine.RunKey, plan workflowUnitPlan, threadID string) error {
	var err error
	if plan.kind == engine.UnitJoin {
		err = r.store.AttachWorkItemJoinRun(
			key.ItemID, key.PhaseID, key.Attempt, key.UnitID, threadID, plan.narrativePath,
		)
	} else {
		err = r.store.AttachWorkItemUnitRun(
			key.ItemID, key.PhaseID, key.Attempt, key.UnitID, threadID, plan.narrativePath,
		)
	}
	if err != nil {
		return fmt.Errorf("workflow runner: attach %s run: %w", plan.label, err)
	}
	return nil
}

// retireUnitWorktrees removes the sub-worktrees of a fan-out attempt whose join
// finished successfully. A unit's worktree is an input the join has now
// consumed; leaving N of them per fan-out on disk is how a run's checkouts
// become unmanageable.
//
// The branches are deliberately kept. They are the durable record of what each
// unit produced — a human comparing units, or recovering work a join summarized
// badly, has nothing else to look at — and removing a worktree does not touch
// the branch it was checked out on. Every non-done ending (park, failure,
// takeover) keeps both: the work is still live, or still evidence.
//
// A dropped unit is retired with the rest. The attempt reached a done join, so
// no checkout is still being worked in, and the drop decision is already
// recorded — its branch keeps whatever it committed.
//
// Removal is deliberately NOT forced. `git worktree remove` refuses a checkout
// carrying uncommitted or untracked work, and that refusal is the safety valve:
// every writing element is told to leave its work committed on its branch (the
// system prompt suffix states it), so a dirty unit checkout here means that
// contract slipped — and forcing would destroy the only copy of work no branch
// carries. Such a checkout is KEPT, path and all, and named loudly instead.
// `workflowapp.Service.removeUnitWorktrees` stays forced for explicit
// disposition cleanup, where the run has already finished and the checkout is
// being torn down as part of the chosen policy.
//
// A removal failure is reported and never changes the phase's outcome: the join
// is done, and an undeleted directory is not a reason to fail a run.
func (r *Runner) retireUnitWorktrees(done workflowCompletion) {
	units, err := r.store.ListWorkItemPhaseUnits(done.key.ItemID, done.key.PhaseID, done.key.Attempt)
	if err != nil {
		r.reportUnitCleanupFailure(done.key.ItemID, fmt.Errorf("list units: %w", err))
		return
	}
	for _, unit := range units {
		if unit.Kind == store.WorkItemUnitKindJoin || strings.TrimSpace(unit.WorktreePath) == "" {
			continue
		}
		if err := r.host.GitCore().RemoveWorktree(done.projectPath, unit.WorktreePath); err != nil {
			// Ask the checkout the same question `git worktree remove` decides
			// on, rather than matching its refusal text: that sentence is
			// localized and free to change between git versions.
			if _, changes, stateErr := r.host.GitCore().WorkingTreeChanges(unit.WorktreePath, 0); stateErr == nil && changes > 0 {
				r.reportUnitWorktreeRetained(done.key.ItemID, unit, changes)
				continue
			}
			// Clean, or the question itself failed: git broke rather than
			// refused, so its own words are what gets reported.
			r.reportUnitCleanupFailure(done.key.ItemID, fmt.Errorf(
				"remove unit %q worktree %q: %w", unit.UnitID, unit.WorktreePath, err,
			))
			continue
		}
		// The row keeps its branch and loses only the path, which is exactly what
		// is no longer true about the unit.
		if err := r.store.AttachWorkItemUnitWorkspace(
			unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.Branch, "",
		); err != nil {
			r.reportUnitCleanupFailure(done.key.ItemID, fmt.Errorf(
				"clear unit %q worktree path: %w", unit.UnitID, err,
			))
		}
	}
}

func (r *Runner) reportUnitCleanupFailure(itemID string, cause error) {
	log.Printf("workflow unit worktree cleanup %s: %v", itemID, cause)
	r.host.Emit(eventchan.WorkflowError, map[string]any{
		"itemId": itemID,
		"error":  "workflow fan-out unit worktrees could not be cleaned up; inspect local diagnostics",
	})
}

// reportUnitWorktreeRetained names a unit checkout the retirement kept because
// it still held uncommitted work. Unlike a cleanup failure this is not a broken
// git operation, so the message is specific rather than a pointer at the logs:
// the join consolidated branches, and whatever is in this directory reached no
// branch — which is a slip of the commit contract every writing element is
// given, and the only place a human can still recover it.
func (r *Runner) reportUnitWorktreeRetained(itemID string, unit store.WorkItemUnit, changes int) {
	message := fmt.Sprintf(
		"fan-out unit %q left %s in %s; the checkout was kept instead of removed, and none of that work is on branch %q",
		unit.UnitID, gitops.RetainedDirtyReason(changes), unit.WorktreePath, unit.Branch,
	)
	log.Printf("workflow unit worktree retained %s: %s", itemID, message)
	r.host.Emit(eventchan.WorkflowError, map[string]any{
		"itemId": itemID,
		"error":  message,
	})
}

// reportUnitGitStateFailure reports informational git reads that failed while
// enriching a join's unit results. The join still runs — the fields are simply
// absent — so this exists to keep "the count is missing" from being silent.
func (r *Runner) reportUnitGitStateFailure(itemID string, cause error) {
	log.Printf("workflow join unit git state %s: %v", itemID, cause)
	r.host.Emit(eventchan.WorkflowError, map[string]any{
		"itemId": itemID,
		"error":  "some fan-out unit branch or worktree state could not be read for the join; inspect local diagnostics",
	})
}
