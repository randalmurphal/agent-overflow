package main

import (
	"errors"
	"fmt"
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
	// work they are throwing away.
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
	members, err := a.workflowRunTree(item)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	project, err := a.store.GetProject(item.ProjectID)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	base, err := a.workflowDispositionBase(item)
	if err != nil {
		// A run with no resolvable base branch can still be discarded; the loss
		// report just cannot say what "unmerged" means for it.
		base = ""
	}
	preview := WorkflowDiscardPreview{
		ItemID:      item.ID,
		Members:     make([]string, 0, len(members)),
		LiveMembers: make([]string, 0),
		Worktrees:   make([]WorkflowDiscardWorktree, 0),
	}
	for _, member := range members {
		preview.Members = append(preview.Members, member.ID)
		if engine.State(member.State) == engine.StateRunning {
			preview.LiveMembers = append(preview.LiveMembers, member.ID)
		}
	}
	targets, err := a.workflowTreeWorktrees(members, base, project.Path)
	if err != nil {
		return WorkflowDiscardPreview{}, err
	}
	if len(targets) == 0 {
		// A read-only run, one that worked in the project checkout, or one whose
		// checkouts were already released. There is nothing to lose, and nothing
		// to ask git about.
		return preview, nil
	}
	worktrees, err := a.gitCore().ListWorktrees(project.Path)
	if err != nil {
		return WorkflowDiscardPreview{}, fmt.Errorf("workflow discard preview %s: list worktrees: %w", item.ID, err)
	}
	for _, target := range targets {
		preview.Worktrees = append(preview.Worktrees, a.describeDiscardWorktree(project.Path, worktrees, target))
	}
	return preview, nil
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
func (a *App) workflowRunTree(root store.WorkItem) ([]store.WorkItem, error) {
	members := []store.WorkItem{root}
	for index := 0; index < len(members); index++ {
		member := members[index]
		if member.CallDepth-root.CallDepth >= engine.MaxCallDepth {
			return nil, fmt.Errorf("workflow run tree %s: deeper than %d calls", root.ID, engine.MaxCallDepth)
		}
		children, err := a.store.ListWorkItemChildren(member.ID)
		if err != nil {
			return nil, fmt.Errorf("workflow run tree %s: list children of %s: %w", root.ID, member.ID, err)
		}
		members = append(members, children...)
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

// discardWorktreeTarget is one checkout the tree owns, before inspection.
type discardWorktreeTarget struct {
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
) ([]discardWorktreeTarget, error) {
	targets := make([]discardWorktreeTarget, 0, len(members))
	seen := make(map[string]bool, len(members))
	add := func(target discardWorktreeTarget) {
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
		add(discardWorktreeTarget{itemID: member.ID, path: member.WorktreePath, branch: member.Branch, base: base})
		units, err := a.store.ListWorkItemUnits(member.ID)
		if err != nil {
			return nil, fmt.Errorf("workflow discard %s: list fan-out units: %w", member.ID, err)
		}
		for _, unit := range units {
			// A unit branch is cut from its run's branch, so that is what its
			// commits are unmerged against.
			add(discardWorktreeTarget{
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
	projectPath string, worktrees []gitops.Worktree, target discardWorktreeTarget,
) WorkflowDiscardWorktree {
	described := WorkflowDiscardWorktree{
		ItemID: target.itemID, UnitID: target.unitID,
		Path: target.path, Branch: target.branch, Base: target.base,
		DirtyFiles: make([]string, 0), UnmergedCommits: make([]gitdiff.Commit, 0),
	}
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, target.path) {
			described.Registered = true
			if described.Branch == "" {
				described.Branch = worktree.Branch
			}
			break
		}
	}
	if info, err := os.Stat(target.path); err == nil && info.IsDir() {
		described.Present = true
	}
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
	if described.Branch != "" && target.base != "" && described.Branch != target.base {
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
