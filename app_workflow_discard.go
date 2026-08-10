package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/engine"
)

// Discard with loss preview (decision D23).
//
// Discard is the only flow in the app that deletes branches, so it is the only
// one that can destroy work no other surface would ever show again. The preview
// is the consent: it walks the whole run tree, reports every worktree the
// discard would remove along with what is in it, and mutates nothing. The
// discard itself then removes exactly what the preview described.
//
// Project deletion (D25) walks the same trees and removes the same checkouts,
// but it is cleanup: it deletes no branch and takes no consent, because nothing
// it does is unrecoverable. Keep it that way — "discard is the only flow that
// deletes a branch" is what makes this preview worth reading.

const (
	// maxDiscardPreviewFiles bounds the named dirty paths per worktree. The
	// count is always exact; the names are the sample a human reads.
	maxDiscardPreviewFiles = 20
	// maxDiscardPreviewCommits bounds the unmerged commits listed per
	// worktree. gitdiff caps its own log at 300, so this is the tighter bound.
	maxDiscardPreviewCommits = 20
)

// WorkflowDiscardWorktree is one checkout the discard would remove, plus the
// work that lives in it and nowhere else.
type WorkflowDiscardWorktree struct {
	ItemID string `json:"itemId"`
	// UnitID is set when this is a fan-out unit's sub-worktree.
	UnitID string `json:"unitId,omitempty"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// Base is the ref the unmerged commits are measured against: the run's base
	// branch for a run worktree, the owning run's branch for a unit worktree
	// (so a unit's landed commits are not counted twice).
	Base string `json:"base"`
	// Present reports whether the checkout is still on disk. A registered
	// worktree whose directory is gone carries no dirty files but its branch
	// still exists and is still deleted.
	Present bool `json:"present"`
	// Registered reports whether git still knows this path as a worktree of the
	// project. An unregistered path is reported, not removed.
	Registered          bool             `json:"registered"`
	DirtyFiles          []string         `json:"dirtyFiles"`
	DirtyFileCount      int              `json:"dirtyFileCount"`
	UnmergedCommits     []gitdiff.Commit `json:"unmergedCommits"`
	UnmergedCommitCount int              `json:"unmergedCommitCount"`
	// Error carries a per-worktree inspection failure. The preview reports it
	// rather than failing outright: a human deciding whether to discard is
	// better served by "this one could not be inspected" than by no preview.
	Error string `json:"error,omitempty"`
}

// WorkflowDiscardPreview is what a discard of one run tree would destroy.
type WorkflowDiscardPreview struct {
	ItemID string `json:"itemId"`
	// Members is the run tree, root first.
	Members []string `json:"members"`
	// LiveMembers is the subset still in flight. Discarding cancels them first;
	// they are called out because that is work the human is stopping, not just
	// work they are throwing away. It is narrower than what the discard settles
	// (workflowDiscardStops): a parked member is also cancelled, but naming it
	// here as "still working" would be a lie about what it is doing.
	LiveMembers []string                  `json:"liveMembers"`
	Worktrees   []WorkflowDiscardWorktree `json:"worktrees"`
}

// WorkflowDiscardResult is what a discard actually destroyed. It rides the
// disposition receipt into the durable run record, because a discard is the one
// disposition whose effects cannot be recovered by looking at git afterwards:
// the branches it deleted are exactly the ones nothing else references.
type WorkflowDiscardResult struct {
	// Members is the run tree the discard covered, root first.
	Members []string `json:"members"`
	// Cancelled is the subset that was still in flight and was stopped.
	Cancelled []string `json:"cancelled"`
	// RemovedWorktrees and DeletedBranches are what git was actually asked to
	// destroy — a subset of the preview, since a checkout can be released
	// between the preview and the discard.
	RemovedWorktrees []string `json:"removedWorktrees"`
	DeletedBranches  []string `json:"deletedBranches"`
}

// WorkflowDiscardPreview reports what discarding a run tree would destroy. It
// runs read-only git queries and mutates nothing.
//
// LocalOnly: it reads local checkouts and repository history.
func (a *App) WorkflowDiscardPreview(itemID string) (WorkflowDiscardPreview, error) {
	item, err := a.workflowDiscardRoot(itemID)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	project, err := a.store.GetProject(item.ProjectID)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	loss, err := a.workflowTreeLoss(item.ID, project.Path)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	preview := WorkflowDiscardPreview{
		ItemID:      item.ID,
		Members:     make([]string, 0, len(loss.members)),
		LiveMembers: loss.live,
	}
	for _, member := range loss.members {
		preview.Members = append(preview.Members, member.ID)
	}
	preview.Worktrees, err = a.describeDiscardTargets(
		project.Path, loss.targets, "workflow discard preview "+item.ID,
	)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	return preview, nil
}

// workflowTreeLoss is what one run tree holds: its members, the subset still in
// flight, and the checkouts it owns. It is the single definition of "what a run
// tree owns" — the per-run discard preview, the discard itself, and project
// deletion's preview and cleanup (D25) all build on it, so none of them can
// describe a different set than the others. What each does with the set is
// their own business: the discard deletes the branches too, the cleanup never
// does.
type workflowTreeLoss struct {
	members []store.WorkItem
	live    []string
	targets []workflowWorktreeTarget
}

func (a *App) workflowTreeLoss(rootID, projectPath string) (workflowTreeLoss, error) {
	members, err := a.workflowRunTree(rootID)
	if err != nil {
		return workflowTreeLoss{}, err
	}
	// members[0] is the tree's root as the store has it right now, which is what
	// the base branch has to be resolved from.
	base, err := a.workflowDispositionBase(members[0])
	if err != nil {
		// A run with no resolvable base branch can still be discarded; the loss
		// report just cannot say what "unmerged" means for it.
		base = ""
	}
	loss := workflowTreeLoss{members: members, live: make([]string, 0)}
	for _, member := range members {
		if engine.State(member.State) == engine.StateRunning {
			loss.live = append(loss.live, member.ID)
		}
	}
	loss.targets, err = a.workflowTreeWorktrees(members, base, projectPath)
	if err != nil {
		return workflowTreeLoss{}, err
	}
	return loss, nil
}

// describeDiscardTargets inspects a whole target set against one reading of the
// project's worktree registry. An empty set asks git nothing: that is a
// read-only run, one that worked in the project checkout, or one whose
// checkouts were already released — there is nothing to lose.
//
// label prefixes the one failure that aborts the report rather than being
// recorded on a row, so the caller's flow is named in the message.
func (a *App) describeDiscardTargets(
	projectPath string, targets []workflowWorktreeTarget, label string,
) ([]WorkflowDiscardWorktree, error) {
	described := make([]WorkflowDiscardWorktree, 0, len(targets))
	if len(targets) == 0 {
		return described, nil
	}
	registry, err := a.readProjectWorktrees(projectPath, label)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		described = append(described, a.describeDiscardWorktree(projectPath, registry, target))
	}
	return described, nil
}

// projectWorktreeRegistry is one reading of which paths git still knows as
// worktrees of a project, plus whether the project checkout was there to be
// read at all. The two facts travel together because an empty list means
// opposite things without the second: nothing is registered, or there is no
// repository left to register anything.
type projectWorktreeRegistry struct {
	worktrees []gitops.Worktree
	present   bool
}

// readProjectWorktrees reads the project's worktree registry.
//
// A project whose checkout has been deleted from disk has no registry to read
// and nothing git can be asked to remove or delete — the repository that held
// those branches went with it. That is reported as an absent registry, not as
// an error, because the alternative is a run that can never be discarded and a
// project that can never be deleted, both permanently. Any other failure is a
// real one and propagates.
//
// label prefixes those failures so the caller's flow is named in the message.
func (a *App) readProjectWorktrees(projectPath, label string) (projectWorktreeRegistry, error) {
	info, err := os.Stat(projectPath)
	if errors.Is(err, fs.ErrNotExist) {
		return projectWorktreeRegistry{}, nil
	}
	if err != nil {
		return projectWorktreeRegistry{}, fmt.Errorf(
			"%s: inspect project checkout %q: %w", label, projectPath, err,
		)
	}
	if !info.IsDir() {
		return projectWorktreeRegistry{}, fmt.Errorf(
			"%s: project checkout %q is not a directory", label, projectPath,
		)
	}
	worktrees, err := a.gitCore().ListWorktrees(projectPath)
	if err != nil {
		return projectWorktreeRegistry{}, fmt.Errorf("%s: list worktrees: %w", label, err)
	}
	return projectWorktreeRegistry{worktrees: worktrees, present: true}, nil
}

// workflowDiscardRoot loads a run and refuses the actions that only make sense
// at the root of a tree. A called run has no workspace of its own (§9), so
// discarding one would delete its caller's branch.
func (a *App) workflowDiscardRoot(itemID string) (store.WorkItem, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("workflow discard: item id is required")
	}
	item, err := a.store.GetWorkItem(itemID)
	if err != nil {
		return store.WorkItem{}, err
	}
	if item.ParentItemID != "" {
		return store.WorkItem{}, fmt.Errorf(
			"workflow discard %s: this run was called by %s; discard the run that called it",
			item.ID, item.ParentItemID,
		)
	}
	return item, nil
}

// workflowRunTree returns the root and every run it called, transitively, root
// first. Depth is bounded by the same constant that bounds a call chain.
//
// It takes an id and reads the root itself rather than accepting a caller's
// copy: the discard re-walks the tree after cancelling it, and a root supplied
// from before that cancel would still read `running` and make the discard
// refuse the very work it just stopped.
func (a *App) workflowRunTree(rootID string) ([]store.WorkItem, error) {
	root, err := a.store.GetWorkItem(rootID)
	if err != nil {
		return nil, fmt.Errorf("workflow run tree %s: %w", rootID, err)
	}
	return a.walkWorkflowRunTree(root, a.store.ListWorkItemChildren)
}

// workflowRunTreeSummaries is the same tree read through the SUMMARY
// projection: ids, linkage, and state, with no snapshot, seeds, or budget blob.
//
// It exists for the readers that need the SHAPE of a tree rather than its
// contents, and `agent-overflow run watch --tree` is the one that makes the
// distinction matter. A watch re-resolves its set on every broadcast, and the
// broadcast is global — one transition anywhere in the app wakes every watcher
// — so a supervisor watching a forty-child campaign would otherwise re-read and
// JSON-decode forty-one frozen workflows, every prompt inlined, per transition.
// Discard needs the full rows (it inspects each member's workspace); nothing on
// the watch path reads a field the summary omits.
func (a *App) workflowRunTreeSummaries(rootID string) ([]store.WorkItem, error) {
	root, err := a.store.GetWorkItemSummary(rootID)
	if err != nil {
		return nil, fmt.Errorf("workflow run tree %s: %w", rootID, err)
	}
	return a.walkWorkflowRunTree(root, func(parentID string) ([]store.WorkItem, error) {
		return a.store.ListWorkItemSummaries(store.WorkItemListFilter{ParentItemID: parentID})
	})
}

// walkWorkflowRunTree is the breadth-first walk both readings share, so the
// membership, the ordering, and the depth bound have one definition and the two
// projections cannot describe different trees.
func (a *App) walkWorkflowRunTree(
	root store.WorkItem, children func(parentID string) ([]store.WorkItem, error),
) ([]store.WorkItem, error) {
	members := []store.WorkItem{root}
	for index := 0; index < len(members); index++ {
		member := members[index]
		if member.CallDepth-root.CallDepth >= engine.MaxCallDepth {
			return nil, fmt.Errorf("workflow run tree %s: deeper than %d calls", root.ID, engine.MaxCallDepth)
		}
		called, err := children(member.ID)
		if err != nil {
			return nil, fmt.Errorf("workflow run tree %s: list children of %s: %w", root.ID, member.ID, err)
		}
		members = append(members, called...)
	}
	return members, nil
}

// isProjectCheckout reports whether a discard target is the user's own checkout
// rather than one the run created. Discard never removes it and never deletes
// the branch it holds, so the preview never lists it either — the collector and
// the remover share this one definition so they cannot describe different sets.
func isProjectCheckout(projectPath, targetPath string) bool {
	return gitops.SameFilesystemPath(projectPath, targetPath)
}

// workflowWorktreeTarget is one checkout a run tree owns, before inspection.
// The name is not discard-specific because the collection is not: the per-run
// discard and project deletion's cleanup (D25) both act on this same set, which
// is what stops them describing different checkouts as a run's own.
type workflowWorktreeTarget struct {
	itemID string
	unitID string
	path   string
	branch string
	base   string
}

// workflowTreeWorktrees collects every checkout a run tree registered: each
// member's own worktree plus each member's fan-out unit sub-worktrees.
//
// Members share their caller's worktree (§9), so the same path arrives once per
// member and is deduplicated by canonical path — otherwise a tree of five runs
// would report five copies of one loss and try to remove it five times.
//
// The project checkout is never a target. A run that worked directly in it (a
// read-only workflow, or one whose worktree was already released) owns nothing
// there: discard will not remove the user's checkout or delete the branch they
// are sitting on, so the preview must not offer it as a loss either. Filtering
// here is what keeps the preview and the discard describing the same set.
func (a *App) workflowTreeWorktrees(
	members []store.WorkItem, base, projectPath string,
) ([]workflowWorktreeTarget, error) {
	targets := make([]workflowWorktreeTarget, 0, len(members))
	seen := make(map[string]bool, len(members))
	add := func(target workflowWorktreeTarget) {
		if strings.TrimSpace(target.path) == "" {
			return
		}
		if isProjectCheckout(projectPath, target.path) {
			return
		}
		key := gitops.CanonicalPath(target.path)
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, target)
	}
	for _, member := range members {
		add(workflowWorktreeTarget{itemID: member.ID, path: member.WorktreePath, branch: member.Branch, base: base})
		units, err := a.store.ListWorkItemUnits(member.ID)
		if err != nil {
			return nil, fmt.Errorf("workflow discard %s: list fan-out units: %w", member.ID, err)
		}
		for _, unit := range units {
			// A unit branch is cut from its run's branch, so that is what its
			// commits are unmerged against.
			add(workflowWorktreeTarget{
				itemID: member.ID, unitID: unit.UnitID,
				path: unit.WorktreePath, branch: unit.Branch, base: member.Branch,
			})
		}
	}
	return targets, nil
}

// describeDiscardWorktree inspects one checkout. Inspection failures are
// reported on the row instead of aborting the preview — a half-broken worktree
// is exactly the state a human most needs the rest of the report for.
func (a *App) describeDiscardWorktree(
	projectPath string, registry projectWorktreeRegistry, target workflowWorktreeTarget,
) WorkflowDiscardWorktree {
	described := WorkflowDiscardWorktree{
		ItemID: target.itemID, UnitID: target.unitID,
		Path: target.path, Base: target.base,
		DirtyFiles: make([]string, 0), UnmergedCommits: make([]gitdiff.Commit, 0),
	}
	described.Branch, described.Registered = registeredWorktreeBranch(registry, target)
	described.Present = worktreeDirectoryPresent(target.path)
	var errs []error
	if described.Present {
		files, total, err := a.gitCore().WorkingTreeChanges(target.path, maxDiscardPreviewFiles)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect working tree: %w", err))
		} else {
			described.DirtyFiles = slicesx.OrEmpty(files)
			described.DirtyFileCount = total
		}
	}
	// Without the project repository there is no branch graph to walk, so the
	// comparison is skipped rather than reported as a failure per row: the
	// commits are already gone with the repository that held them.
	if registry.present && described.Branch != "" && target.base != "" && described.Branch != target.base {
		commits, err := gitdiff.ListBranchCommits(a.lifeCtx(), projectPath, target.base, described.Branch)
		if err != nil {
			errs = append(errs, fmt.Errorf("list unmerged commits: %w", err))
		} else {
			described.UnmergedCommitCount = len(commits)
			if len(commits) > maxDiscardPreviewCommits {
				commits = commits[:maxDiscardPreviewCommits]
			}
			described.UnmergedCommits = slicesx.OrEmpty(commits)
		}
	}
	if joined := errors.Join(errs...); joined != nil {
		described.Error = joined.Error()
	}
	return described
}
