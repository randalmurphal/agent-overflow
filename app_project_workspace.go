package main

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// Project-scoped workspace provisioning: cut a worktree, attach a worktree, or
// create a branch WITHOUT a thread row.
//
// A draft thread is not a row. The thread-scoped equivalents
// (PrepareThreadWorktree, AttachThreadWorktree, GitCreateBranchFrom) all begin
// by reading one, which forced the composer to materialize an empty thread just
// to name a workspace — and that materialized-but-empty row races the frontend's
// own empty-draft cleanup, which is where the "sql: no rows in result set"
// failures came from.
//
// So the draft's workspace choice happens against the PROJECT, and the thread
// adopts the result at creation time through CreateThread's inherit-worktree
// option. Nothing here reads, writes, or locks a thread row. What it does share
// with the thread-scoped paths is every git sequence: the carry bracket
// (cutWorktreeWithCarry) and the branch checkout (createBranchInWorkspace) have
// exactly one implementation each.
//
// The one guard that has to be RESTATED rather than shared is the busy check.
// ensureWorkspaceChangeAllowed is keyed by thread id, and a draft has none;
// what matters here is the DIRECTORY, so these paths use the same rule
// removeProjectWorktree applies to a worktree it is about to delete — refuse
// while any thread working there has a turn or a background task open.
//
// "Project-scoped" names what OWNS the operation, not where its git runs. A
// draft pane can already be parked in a worktree (the user cut one, then
// staged a second choice), and every source-side git command — the carry
// stash, the base-match comparison, the branch checkout — belongs in THAT
// checkout. So each of these methods takes a sourceWorkspace, validated
// against the project's registered worktrees because it is a cwd for
// destructive git. Empty means the project root, which is what a pane sitting
// in the root sends.

// ProjectWorkspaceResult is what a project-scoped workspace operation hands
// back: enough for the composer to show the choice, and exactly what
// CreateThread's WorktreePath / Branch options take.
//
// WorktreePath is empty for CreateProjectBranch — branching happens in the
// project root, which is not a worktree.
type ProjectWorkspaceResult struct {
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
}

// PrepareProjectWorktree cuts a new worktree for the project and returns it,
// unattached. The thread-scoped PrepareThreadWorktree minus the thread: same
// branch resolution, same base resolution, same carry semantics, same
// fetch-the-base rule, and the same setup-recipe kickoff — except the run is
// registered against the WORKSPACE, because there is nothing to bind it to yet
// (app_worktree_setup_workspace.go).
//
// requestedBranch is optional: blank means "create a temporary auto branch
// using the configured prefix". carryLocalChanges moves sourceWorkspace's
// dirty tree into the new worktree, and only applies when the base matches
// THAT checkout's current branch — moving changes onto an unrelated base is a
// rebase, which is a different request.
//
// sourceWorkspace is the checkout the carry stashes FROM: empty for the
// project root, or one of the project's registered worktrees when the draft
// pane is already parked in one. A carry also yanks uncommitted work out of a
// checkout other threads may be running in, so it takes the same
// directory-keyed occupancy refusal the destructive branch path does.
//
// LocalOnly: cuts a git worktree on disk and runs the project's argv recipe
// over it.
func (a *App) PrepareProjectWorktree(projectID, baseBranch, requestedBranch string, carryLocalChanges bool, sourceWorkspace string) (ProjectWorkspaceResult, error) {
	project, err := a.projectForWorkspaceOp(projectID)
	if err != nil {
		return ProjectWorkspaceResult{}, fmt.Errorf("create worktree: %w", err)
	}
	source, err := a.resolveProjectSourceWorkspace(project, sourceWorkspace)
	if err != nil {
		return ProjectWorkspaceResult{}, fmt.Errorf("create worktree: %w", err)
	}

	resolvedBranch := a.resolveWorktreeBranch(requestedBranch)
	if resolvedBranch == "" {
		return ProjectWorkspaceResult{}, fmt.Errorf("create worktree: branch is required")
	}

	core := a.gitCore()
	currentBranch := core.CurrentBranch(source)
	resolvedBase := strings.TrimSpace(baseBranch)
	if resolvedBase == "" {
		resolvedBase = currentBranch
	}
	if resolvedBase == "" {
		return ProjectWorkspaceResult{}, fmt.Errorf("create worktree: base branch is required")
	}
	// Carry-over only makes sense from the source's current branch — the dirty
	// state being moved lives there, and the thread variant asks the same
	// question of the thread's own workspace.
	if carryLocalChanges && resolvedBase != currentBranch {
		return ProjectWorkspaceResult{}, fmt.Errorf("create worktree: %w", errCarryRequiresCurrentBase)
	}
	if carryLocalChanges {
		// The stash below empties the source checkout. That is a mutation of a
		// directory other threads may be mid-turn in, so it earns the same
		// refusal moving its HEAD would.
		if err := a.ensureWorkspaceDirectoryChangeAllowed(source, "create worktree"); err != nil {
			return ProjectWorkspaceResult{}, err
		}
	}

	worktreePath, err := a.cutWorktreeWithCarry(worktreeCutRequest{
		projectPath:       project.Path,
		sourceWorkspace:   source,
		baseBranch:        resolvedBase,
		newBranch:         resolvedBranch,
		carryLocalChanges: carryLocalChanges,
	})
	if err != nil {
		return ProjectWorkspaceResult{}, err
	}

	// This call cut the worktree, so the project's recipe runs over it. The run
	// outlives this call and is adopted by whichever thread is created into the
	// path — or lost on restart, which is what an unbound run is worth.
	a.startWorkspaceWorktreeSetup(project, worktreePath)
	return ProjectWorkspaceResult{WorktreePath: worktreePath, Branch: resolvedBranch}, nil
}

// AttachProjectWorktree creates a worktree pointing at an EXISTING branch and
// returns it, unattached. AttachThreadWorktree minus the thread.
//
// Distinct from PrepareProjectWorktree (which always creates a new branch via
// `git worktree add -b`): this path is `git worktree add <path> <existing>` and
// refuses if the branch is already checked out elsewhere — git's own
// one-branch-one-worktree invariant, surfaced verbatim. The frontend dedups by
// offering the existing worktree instead of calling here.
//
// LocalOnly: cuts a git worktree on disk and runs the project's argv recipe
// over it.
func (a *App) AttachProjectWorktree(projectID, branch string) (ProjectWorkspaceResult, error) {
	project, err := a.projectForWorkspaceOp(projectID)
	if err != nil {
		return ProjectWorkspaceResult{}, fmt.Errorf("attach worktree: %w", err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ProjectWorkspaceResult{}, fmt.Errorf("attach worktree: branch is required")
	}

	worktreePath, err := a.defaultWorktreePath(project.Path, branch)
	if err != nil {
		return ProjectWorkspaceResult{}, err
	}
	if err := a.gitCore().AttachWorktree(project.Path, worktreePath, branch); err != nil {
		return ProjectWorkspaceResult{}, err
	}

	// The branch already existed, but the checkout is freshly created (attach
	// refuses a branch checked out anywhere else), so the project's recipe runs
	// over it like any other fresh cut — the recipe is a convention about the
	// directory, not the branch.
	a.startWorkspaceWorktreeSetup(project, worktreePath)
	return ProjectWorkspaceResult{WorktreePath: worktreePath, Branch: branch}, nil
}

// CreateProjectBranch creates a branch in sourceWorkspace and checks it out.
// GitCreateBranchFrom minus the thread, with a caller-named checkout standing
// in for the thread's workspace: the project root by default, or one of the
// project's registered worktrees when the draft pane is parked in one.
//
// carryLocalChanges has the same three meaningful combinations with baseBranch
// as the thread variant:
//   - base = current branch, carry = true: the "Local with changes" path —
//     `git checkout -b <name>` from HEAD; uncommitted changes stay attached.
//   - base = current branch, carry = false: same git command; carry is a no-op
//     since there's no checkout that would clobber the working tree.
//   - base != current branch, carry = false: the destructive path — stash the
//     working tree, checkout the base, branch off it, drop the stash. The
//     frontend gates this behind an explicit "discards uncommitted changes"
//     confirmation; we trust the caller has confirmed.
//   - base != current branch, carry = true: rejected.
//
// The destructive path moves a SHARED checkout's HEAD, which every thread
// working there is running in. That is why it repeats removeProjectWorktree's
// occupancy rule rather than the thread-keyed workspace guard: the question is
// whether anyone is working in this directory, not whether the caller is.
//
// LocalOnly: mutates the user's checkout.
func (a *App) CreateProjectBranch(projectID, name, baseBranch string, carryLocalChanges bool, sourceWorkspace string) (ProjectWorkspaceResult, error) {
	project, err := a.projectForWorkspaceOp(projectID)
	if err != nil {
		return ProjectWorkspaceResult{}, fmt.Errorf("create branch: %w", err)
	}
	source, err := a.resolveProjectSourceWorkspace(project, sourceWorkspace)
	if err != nil {
		return ProjectWorkspaceResult{}, fmt.Errorf("create branch: %w", err)
	}

	core := a.gitCore()
	sanitized, resolvedBase, baseIsCurrent, err := resolveBranchCreatePlan(
		core.CurrentBranch(source), name, baseBranch, carryLocalChanges)
	if err != nil {
		return ProjectWorkspaceResult{}, err
	}

	if !baseIsCurrent {
		if err := a.ensureWorkspaceDirectoryChangeAllowed(source, "create branch"); err != nil {
			return ProjectWorkspaceResult{}, err
		}
		if err := core.EnsureLocalBranchDoesNotExist(source, sanitized); err != nil {
			return ProjectWorkspaceResult{}, fmt.Errorf("create branch: %w", err)
		}
	}

	if err := a.createBranchInWorkspace(source, sanitized, resolvedBase, baseIsCurrent); err != nil {
		return ProjectWorkspaceResult{}, err
	}

	if a.workspaceFiles != nil {
		a.workspaceFiles.Invalidate(source)
	}
	return ProjectWorkspaceResult{Branch: core.CurrentBranch(source)}, nil
}

// resolveProjectSourceWorkspace validates the checkout a project-scoped
// operation runs its SOURCE-side git in and answers with git's own spelling of
// it. Empty means the project root.
//
// The membership check is the boundary, not a convenience: this path is where
// a stash empties a working tree and where a checkout moves a HEAD, so an
// arbitrary caller-supplied directory must never reach it. findWorktree lists
// the project's registered worktrees, which includes the main one — the
// SameFilesystemPath short-circuit above it just avoids the git call in the
// overwhelmingly common case.
func (a *App) resolveProjectSourceWorkspace(project store.Project, sourceWorkspace string) (string, error) {
	sourceWorkspace = strings.TrimSpace(sourceWorkspace)
	if sourceWorkspace == "" || gitops.SameFilesystemPath(project.Path, sourceWorkspace) {
		return project.Path, nil
	}
	worktree, ok, err := a.findWorktree(project.Path, sourceWorkspace)
	if err != nil {
		return "", fmt.Errorf("validate source workspace: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("%s is not a workspace of project %s", sourceWorkspace, project.Path)
	}
	return worktree.Path, nil
}

// projectForWorkspaceOp resolves the project row every method in this file
// starts from. Distinct from gitProjectPath because the setup-run kickoff needs
// the id and the path together.
func (a *App) projectForWorkspaceOp(projectID string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return store.Project{}, fmt.Errorf("projectId is required")
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return store.Project{}, fmt.Errorf("resolve project %s: %w", projectID, err)
	}
	if strings.TrimSpace(project.Path) == "" {
		return store.Project{}, fmt.Errorf("project %s has no path", projectID)
	}
	return project, nil
}

// ensureWorkspaceDirectoryChangeAllowed refuses to mutate a checkout while any
// thread working IN it has a turn or a background tool call in flight. It is
// the directory-keyed sibling of ensureWorkspaceChangeAllowed (which asks the
// same question of one thread), and it uses the pair removeProjectWorktree
// uses for the same reason: a provider session is bound to its cwd, and moving
// that checkout's HEAD underneath a running turn corrupts what the model sees.
//
// action names the operation in the refusal. The final `: `-segment must stand
// alone — the frontend's userFacingError keeps only that segment for toasts.
func (a *App) ensureWorkspaceDirectoryChangeAllowed(workspacePath, action string) error {
	ids, err := a.threadsReferencingWorkspace(workspacePath)
	if err != nil {
		return err
	}
	for _, id := range ids {
		reason, err := a.threadActivityBlockReason(id)
		if err != nil {
			return fmt.Errorf("thread %s: %w", id, err)
		}
		if reason != "" {
			return fmt.Errorf("workspace %s in use by thread %s: cannot %s while %s", workspacePath, id, action, reason)
		}
	}
	return nil
}
