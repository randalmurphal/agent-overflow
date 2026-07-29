package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// preparedWorkflowWorkspace is one resolved workspace: where work runs and, when
// that is an isolated worktree, the branch it is checked out on. Branch travels
// with the path because every consumer that needs one needs the other — the
// thread row, a unit's sub-worktree base, the run record.
type preparedWorkflowWorkspace struct {
	path   string
	branch string
	// baseBranch is what the worktree was cut from. Empty on the project root,
	// where there is no cut and the run works on whatever branch is checked out.
	baseBranch string
	project    store.Project
}

// prepareWorkspace resolves (and, on first use, provisions) the item's primary
// workspace.
//
// Provisioning is serialized per item because a fan-out starts several units at
// once and every one of them resolves this same workspace: two concurrent
// resolutions of an item that has no worktree yet would each cut one, and the
// loser would be orphaned on disk with nothing in the run record pointing at
// it. Serializing here rather than at the call sites means a future caller
// cannot forget.
func (r *workflowAppRunner) prepareWorkspace(ctx context.Context, request engine.RunRequest) (preparedWorkflowWorkspace, error) {
	unlock := r.workspaceLocks.Lock(request.Key.ItemID)
	defer unlock()
	return r.resolveWorkspace(ctx, request)
}

func (r *workflowAppRunner) resolveWorkspace(ctx context.Context, request engine.RunRequest) (preparedWorkflowWorkspace, error) {
	project, err := r.app.store.GetProject(request.Item.ProjectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("load project: %w", err)
	}
	// The need is frozen with the run and already accounts for the whole call
	// graph (§9): a workflow that calls a writing workflow needs a worktree even
	// though its own phases never write. Re-deriving it here from the definition
	// alone would silently under-provision such a root.
	switch request.WorkspaceNeed {
	case def.WorkspaceProjectRoot, def.WorkspaceWorktree:
	default:
		return preparedWorkflowWorkspace{}, fmt.Errorf(
			"run request for item %q carries no frozen workspace need", request.Key.ItemID,
		)
	}
	item, err := r.app.store.GetWorkItem(request.Key.ItemID)
	if err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("load work item: %w", err)
	}
	if item.ParentItemID != "" {
		return r.resolveCalledWorkspace(ctx, item, project)
	}
	if request.WorkspaceNeed == def.WorkspaceProjectRoot {
		return preparedWorkflowWorkspace{path: project.Path, project: project}, nil
	}
	return r.provisionWorkspace(ctx, item, request.Workflow.ID, project)
}

// resolveCalledWorkspace answers for a run a call created. A called run
// provisions nothing of its own (§9): it executes in its caller's workspace
// context, so a recursive workflow iterates in one worktree instead of cutting
// one per level. The stamped columns are the fast path — a serial call copies
// the caller's workspace onto the child row when it creates it — and the linkage
// is the authority whenever they are empty, which is what makes a workflow whose
// *first* phase is a call still run its child in an isolated worktree.
//
// Which workspace context that is depends on where the call was declared.
// Isolation is introduced by fan-out and only by fan-out, so a run a *unit*
// called executes in that unit's sub-worktree, and one a phase called executes
// in the tree's own worktree.
func (r *workflowAppRunner) resolveCalledWorkspace(
	ctx context.Context, item store.WorkItem, project store.Project,
) (preparedWorkflowWorkspace, error) {
	recorded, ok, err := r.recordedWorkspace(item, project)
	if err != nil || ok {
		return recorded, err
	}
	root, err := r.rootWorkItem(item)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	need, err := frozenWorkspaceNeed(root)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if need == def.WorkspaceProjectRoot {
		// Nothing in the tree writes, so there is no branch to isolate from and
		// nothing to isolate. This covers the unit-call case too: a fan-out of
		// read-only sub-workflows shares the project root exactly as its phases do.
		return preparedWorkflowWorkspace{path: project.Path, project: project}, nil
	}
	prepared, err := r.prepareRootWorkspace(ctx, root, project)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if item.ParentUnitID != "" {
		prepared, err = r.resolveUnitCallWorkspace(ctx, item, prepared)
		if err != nil {
			return preparedWorkflowWorkspace{}, err
		}
	}
	// Record the answer on the child so its run record shows where it ran, and so
	// every later phase of this child resolves through the fast path above.
	// The base branch comes from what provisioning resolved, not from the root row
	// read before it: on the first cut of a tree, that read predates the answer.
	if err := r.app.store.UpdateWorkItemWorkspace(
		item.ID, prepared.path, prepared.branch, prepared.baseBranch,
	); err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("stamp called run %q workspace: %w", item.ID, err)
	}
	return prepared, nil
}

// resolveUnitCallWorkspace resolves the sub-worktree a call-bound fan-out unit's
// child run executes in (§9). It is the same isolation an agent unit gets and
// the same code that cuts it: one branch off the item's branch per unit try,
// registered on the unit row before anything runs in it, so the checkout is
// discoverable — by the join that merges it, by the discard preview, by a
// crash-recovery sweep — from the moment it exists.
//
// The try number comes from the unit row rather than from the child, so a
// retried unit's second child cuts a fresh checkout instead of inheriting what
// the failed try left behind, exactly as a retried agent unit does.
func (r *workflowAppRunner) resolveUnitCallWorkspace(
	ctx context.Context, item store.WorkItem, primary preparedWorkflowWorkspace,
) (preparedWorkflowWorkspace, error) {
	unit, found, err := r.app.store.GetWorkItemUnit(
		item.ParentItemID, item.ParentPhaseID, item.ParentAttempt, item.ParentUnitID,
	)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if !found {
		return preparedWorkflowWorkspace{}, fmt.Errorf(
			"called run %q names fan-out unit %s/%s/%d/%s, which has no persisted row",
			item.ID, item.ParentItemID, item.ParentPhaseID, item.ParentAttempt, item.ParentUnitID,
		)
	}
	return r.provisionUnitWorktree(ctx, workflowUnitWorkspaceRef{
		projectID: item.ProjectID, itemID: item.ParentItemID, phaseID: item.ParentPhaseID,
		attempt: item.ParentAttempt, unitID: item.ParentUnitID, unitAttempt: unit.UnitAttempt,
	}, primary)
}

// prepareRootWorkspace provisions (or adopts) the root's workspace under the
// root's own lock, so a called run cannot race the root — or a sibling call —
// into cutting a second worktree for one tree.
func (r *workflowAppRunner) prepareRootWorkspace(
	ctx context.Context, root store.WorkItem, project store.Project,
) (preparedWorkflowWorkspace, error) {
	unlock := r.workspaceLocks.Lock(root.ID)
	defer unlock()
	current, err := r.app.store.GetWorkItem(root.ID)
	if err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("load root work item %q: %w", root.ID, err)
	}
	return r.provisionWorkspace(ctx, current, current.WorkflowID, project)
}

// rootWorkItem walks the call linkage to the run tree's root. Linkage is
// immutable, so this is a stable fact about the item.
func (r *workflowAppRunner) rootWorkItem(item store.WorkItem) (store.WorkItem, error) {
	current := item
	for depth := 0; current.ParentItemID != ""; depth++ {
		if depth > engine.MaxCallDepth {
			return store.WorkItem{}, fmt.Errorf("resolve root of work item %q: tree is deeper than %d", item.ID, engine.MaxCallDepth)
		}
		parent, err := r.app.store.GetWorkItem(current.ParentItemID)
		if err != nil {
			return store.WorkItem{}, fmt.Errorf("resolve root of work item %q: %w", item.ID, err)
		}
		current = parent
	}
	return current, nil
}

// frozenWorkspaceNeed reads the need the run froze at start. Older snapshots
// predate the field; deriving from the frozen definition is the same answer for
// them, because a call graph is what the field exists to account for and they
// have none.
func frozenWorkspaceNeed(item store.WorkItem) (def.WorkspaceNeed, error) {
	if len(item.Snapshot) == 0 {
		return "", fmt.Errorf("work item %q has no frozen workflow snapshot", item.ID)
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return "", fmt.Errorf("decode work item %q snapshot: %w", item.ID, err)
	}
	if snapshot.WorkspaceNeed != "" {
		return snapshot.WorkspaceNeed, nil
	}
	return def.DeriveWorkspaceNeed(snapshot.Workflow), nil
}

// recordedWorkspace verifies the workspace an item already recorded. A recorded
// worktree that is gone or no longer registered on its branch is an error, never
// a reason to cut a fresh one: the run's work lives there.
func (r *workflowAppRunner) recordedWorkspace(
	item store.WorkItem, project store.Project,
) (preparedWorkflowWorkspace, bool, error) {
	if item.WorktreePath == "" && item.Branch == "" {
		return preparedWorkflowWorkspace{}, false, nil
	}
	if item.WorktreePath == "" || item.Branch == "" || item.BaseBranch == "" {
		return preparedWorkflowWorkspace{}, false, fmt.Errorf("work item has incomplete workspace fields")
	}
	info, statErr := os.Stat(item.WorktreePath)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return preparedWorkflowWorkspace{}, false, fmt.Errorf("recorded worktree %q is missing", item.WorktreePath)
		}
		return preparedWorkflowWorkspace{}, false, fmt.Errorf("inspect recorded worktree %q: %w", item.WorktreePath, statErr)
	}
	if !info.IsDir() {
		return preparedWorkflowWorkspace{}, false, fmt.Errorf("recorded worktree %q is not a directory", item.WorktreePath)
	}
	registered, ok, findErr := r.app.findWorktree(project.Path, item.WorktreePath)
	if findErr != nil {
		return preparedWorkflowWorkspace{}, false, fmt.Errorf("verify recorded worktree %q: %w", item.WorktreePath, findErr)
	}
	if !ok || registered.Branch != item.Branch {
		return preparedWorkflowWorkspace{}, false, fmt.Errorf("recorded worktree %q is not registered on branch %q", item.WorktreePath, item.Branch)
	}
	return preparedWorkflowWorkspace{
		path: item.WorktreePath, branch: item.Branch, baseBranch: item.BaseBranch, project: project,
	}, true, nil
}

// provisionWorkspace adopts the item's recorded worktree, or cuts one. It is the
// only place a run's primary worktree is created, and it always runs under that
// item's workspace lock.
func (r *workflowAppRunner) provisionWorkspace(
	ctx context.Context, item store.WorkItem, workflowID string, project store.Project,
) (preparedWorkflowWorkspace, error) {
	recorded, ok, err := r.recordedWorkspace(item, project)
	if err != nil || ok {
		return recorded, err
	}
	projectProfile, err := r.projectProfile(ctx, item.ProjectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	baseBranch := strings.TrimSpace(item.BaseBranch)
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(projectProfile.BaseBranch)
	}
	if baseBranch == "" {
		baseBranch = strings.TrimSpace(r.app.gitCore().CurrentBranch(project.Path))
	}
	if baseBranch == "" {
		return preparedWorkflowWorkspace{}, fmt.Errorf("resolve base branch: repository has no current branch")
	}
	core := r.app.gitCore()
	branchPrefix := workflowWorktreeBranchPrefix(r.app.worktreeBranchPrefix(), workflowID, item.ID)
	branch, worktreePath, err := findInterruptedWorkflowProvisioning(core, project.Path, branchPrefix)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if worktreePath == "" {
		branch = gitops.BuildTemporaryWorktreeBranchNameWithPrefix(branchPrefix)
		worktreePath, err = r.app.defaultWorktreePath(project.Path, branch)
		if err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("choose worktree path: %w", err)
		}
		if err := core.CreateWorktreeFromBranch(project.Path, worktreePath, baseBranch, branch); err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("create worktree from %q: %w", baseBranch, err)
		}
	}
	rollback := func(cause error) error {
		if removeErr := core.RemoveWorktreeForce(project.Path, worktreePath, true); removeErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback worktree %q: %w", worktreePath, removeErr))
		}
		// Clear provisioning state but keep the intake-time base branch: it is
		// user intent from enqueue, not a product of the rolled-back provisioning.
		if clearErr := r.app.store.UpdateWorkItemWorkspace(item.ID, "", "", item.BaseBranch); clearErr != nil {
			return errors.Join(cause, fmt.Errorf("clear rolled-back worktree %q: %w", worktreePath, clearErr))
		}
		return cause
	}
	if err := runWorkflowWorktreeSetup(ctx, project.Path, worktreePath, projectProfile.WorktreeSetup); err != nil {
		return preparedWorkflowWorkspace{}, rollback(err)
	}
	if err := r.app.store.UpdateWorkItemWorkspace(item.ID, worktreePath, branch, baseBranch); err != nil {
		return preparedWorkflowWorkspace{}, rollback(fmt.Errorf("persist workspace: %w", err))
	}
	return preparedWorkflowWorkspace{
		path: worktreePath, branch: branch, baseBranch: baseBranch, project: project,
	}, nil
}

// workflowUnitWorkspaceRef names the fan-out unit a sub-worktree belongs to.
// It exists because the two callers of provisionUnitWorktree identify that unit
// from different places: the unit's own runner start knows it from the
// `engine.RunRequest`, while a unit-call child run knows it only from its
// parent linkage plus the persisted unit row. Passing the coordinates instead
// of a request keeps one provisioning path for both.
type workflowUnitWorkspaceRef struct {
	projectID   string
	itemID      string
	phaseID     string
	attempt     int
	unitID      string
	unitAttempt int
}

// provisionUnitWorktree cuts one writing fan-out unit's isolated sub-worktree
// from the item's branch (spec §9). The branch and path are registered on the
// unit row before the session starts, so a crash between `git worktree add` and
// the first turn can never strand either.
//
// The unit's branch name is a deterministic function of the item's branch, the
// unit id, and the unit's try number: re-entering the same try finds its own
// worktree and adopts it instead of cutting a second, and a retry (a new try
// number) always cuts fresh from the item branch rather than inheriting what
// the failed try left behind. That adoption is also what makes a call-bound
// unit work: its child run resolves the same coordinates and lands in the
// checkout the unit already owns.
func (r *workflowAppRunner) provisionUnitWorktree(
	ctx context.Context, ref workflowUnitWorkspaceRef, primary preparedWorkflowWorkspace,
) (preparedWorkflowWorkspace, error) {
	if primary.branch == "" {
		// DeriveWorkspaceNeed counts a writing unit, so an item whose units write
		// always has a worktree by the time one starts. Reaching here means the
		// frozen definition and the provisioned workspace disagree.
		return preparedWorkflowWorkspace{}, fmt.Errorf(
			"unit %q declares write access but item %q has no branch to cut from",
			ref.unitID, ref.itemID,
		)
	}
	if ref.unitAttempt < 1 {
		return preparedWorkflowWorkspace{}, fmt.Errorf(
			"unit %q of item %q has no try number to name its worktree from", ref.unitID, ref.itemID,
		)
	}
	branch := workflowUnitBranch(primary.branch, ref.unitID, ref.unitAttempt)
	if err := gitops.ValidateBranchName(branch); err != nil {
		return preparedWorkflowWorkspace{}, fmt.Errorf("unit branch %q: %w", branch, err)
	}
	core := r.app.gitCore()
	worktreePath, adopted, err := r.existingUnitWorktree(primary.project.Path, branch)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	if !adopted {
		worktreePath, err = r.app.defaultWorktreePath(primary.project.Path, branch)
		if err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("choose unit worktree path: %w", err)
		}
		if err := core.CreateWorktreeFromBranch(primary.project.Path, worktreePath, primary.branch, branch); err != nil {
			return preparedWorkflowWorkspace{}, fmt.Errorf("create unit worktree from %q: %w", primary.branch, err)
		}
	}
	rollback := func(cause error) error {
		if removeErr := core.RemoveWorktreeForce(primary.project.Path, worktreePath, true); removeErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback unit worktree %q: %w", worktreePath, removeErr))
		}
		return cause
	}
	if err := r.app.store.AttachWorkItemUnitWorkspace(
		ref.itemID, ref.phaseID, ref.attempt, ref.unitID, branch, worktreePath,
	); err != nil {
		return preparedWorkflowWorkspace{}, rollback(fmt.Errorf("register unit workspace: %w", err))
	}
	projectProfile, err := r.projectProfile(ctx, ref.projectID)
	if err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	// A sub-worktree is a fresh checkout: without the project's setup hooks it
	// lacks exactly the installed state the item's own worktree was given.
	if err := runWorkflowWorktreeSetup(ctx, primary.project.Path, worktreePath, projectProfile.WorktreeSetup); err != nil {
		return preparedWorkflowWorkspace{}, err
	}
	return preparedWorkflowWorkspace{
		path: worktreePath, branch: branch, baseBranch: primary.branch, project: primary.project,
	}, nil
}

// existingUnitWorktree finds the worktree already checked out on a unit's
// branch. A found one is adopted rather than replaced: it is this unit's own
// try, re-entered.
func (r *workflowAppRunner) existingUnitWorktree(projectPath, branch string) (string, bool, error) {
	worktrees, err := r.app.gitCore().ListWorktrees(projectPath)
	if err != nil {
		return "", false, fmt.Errorf("list worktrees before unit provisioning: %w", err)
	}
	for _, worktree := range worktrees {
		if worktree.Branch == branch {
			return worktree.Path, true, nil
		}
	}
	return "", false, nil
}

// workflowUnitBranch names a fan-out unit's sub-worktree branch. It extends the
// item's own branch so every branch a run created shares one prefix — which is
// what makes the run's branches findable from the item alone.
func workflowUnitBranch(itemBranch, unitID string, unitAttempt int) string {
	if unitAttempt < 1 {
		unitAttempt = 1
	}
	return fmt.Sprintf("%s-%s-%d", itemBranch, gitops.SanitizeBranchFragment(unitID), unitAttempt)
}

func workflowWorktreeBranch(prefix, workflowID, itemID string) string {
	return gitops.BuildTemporaryWorktreeBranchNameWithPrefix(workflowWorktreeBranchPrefix(prefix, workflowID, itemID))
}

func workflowWorktreeBranchPrefix(prefix, workflowID, itemID string) string {
	configuredPrefix := strings.TrimSpace(prefix)
	if configuredPrefix != "" && !strings.HasSuffix(configuredPrefix, "-") && !strings.HasSuffix(configuredPrefix, "_") {
		configuredPrefix += "-"
	}
	return configuredPrefix + "workflow-" + gitops.SanitizeBranchFragment(workflowID) + "-" + gitops.SanitizeBranchFragment(itemID) + "-"
}

func findInterruptedWorkflowProvisioning(core *gitops.Core, projectPath, branchPrefix string) (branch, worktreePath string, err error) {
	worktrees, err := core.ListWorktrees(projectPath)
	if err != nil {
		return "", "", fmt.Errorf("list worktrees before provisioning: %w", err)
	}
	for _, worktree := range worktrees {
		if !strings.HasPrefix(worktree.Branch, branchPrefix) {
			continue
		}
		if worktreePath != "" {
			return "", "", fmt.Errorf("multiple interrupted workflow worktrees match branch prefix %q", branchPrefix)
		}
		branch, worktreePath = worktree.Branch, worktree.Path
	}
	return branch, worktreePath, nil
}
