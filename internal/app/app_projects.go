package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/projectapp"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/worktreesetup"
)

func (a *App) projectApplication() *projectapp.Service {
	a.projectAppOnce.Do(func() {
		a.projectApp = projectapp.New(projectapp.Deps{
			Store:     a.store,
			Workspace: appProjectWorkspace{app: a},
			Identity:  a.repoIdentity,
		})
	})
	return a.projectApp
}

type appProjectWorkspace struct{ app *App }

func (w appProjectWorkspace) FindWorktree(projectPath, candidate string) (string, bool, error) {
	worktree, ok, err := w.app.findWorktree(projectPath, candidate)
	return worktree.Path, ok, err
}

// repoIdentity reads a checkout's repository identity for projectapp.
// Resolved through gitCore() at call time rather than captured when the
// service is built, so the shared Core is the one the rest of the app uses.
func (a *App) repoIdentity(ctx context.Context, path string) (gitops.RepoIdentity, error) {
	identity, err := a.gitCore().ReadRepoIdentity(ctx, path)
	if err != nil {
		return identity, err
	}
	return a.gitCore().ResolveRepository(ctx, path, identity), ctx.Err()
}

// projectIdentityRetryDelays spaces the retries of rows whose forge was
// unavailable. The last delay repeats until every row resolves or the app
// stops: one forge call per such row per interval.
var projectIdentityRetryDelays = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// startProjectIdentityRefresh re-derives the repository identity of every
// project row once per boot, after the first client's catalog reads, and
// announces each row it moved so a client that already loaded its sidebar
// converges without a refresh. Rows whose forge was unavailable are retried
// on a backoff, so a lookup made before the network or the CLI login was
// ready does not stay failed until the next boot. Shutdown joins it before
// closing the store.
func (a *App) startProjectIdentityRefresh() {
	a.projectIdentityWG.Add(1)
	go func() {
		defer a.projectIdentityWG.Done()
		ctx := a.lifeCtx()
		if err := a.awaitFirstReadsSettled(ctx); err != nil {
			return
		}
		persist := func(row store.Project) {
			a.broadcastProjectRow(triage.ProjectActionFull, row)
		}
		retry, err := a.projectApplication().RefreshIdentity(ctx, persist)
		for attempt := 0; err == nil && len(retry) > 0; attempt++ {
			timer := time.NewTimer(a.projectIdentityRetryDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			retry, err = a.projectApplication().RetryIdentity(ctx, retry, persist)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("project identity refresh: %v", err)
		}
	}()
}

func (a *App) projectIdentityRetryDelay(attempt int) time.Duration {
	if a.maintenance.identityRetry > 0 {
		return a.maintenance.identityRetry
	}
	return projectIdentityRetryDelays[min(attempt, len(projectIdentityRetryDelays)-1)]
}

// ListProjects returns projects with a lightweight thread count per
// project for the sidebar. An answer is a client's catalog read, which
// releases heavy post-boot work.
//
//ao:scope threads:read
//ao:route all
func (a *App) ListProjects() (rows []store.ProjectWithCounts, err error) {
	defer a.firstReads.read(firstReadProjects)(&err)
	return a.projectApplication().List()
}

// CreateProject validates that path exists, is a directory, and is not
// already backing another project. Returns ErrProjectPathInUse when the
// path already has a project row — the frontend interprets that as
// "redirect to the existing project" rather than a failure.
//
//ao:scope git:operate
//ao:route selected
func (a *App) CreateProject(path string) (store.Project, error) {
	// Isolated boots admit only fixture workspaces (app_isolated_workspace.go).
	if err := a.requireIsolatedWorkspace(path); err != nil {
		return store.Project{}, err
	}
	row, err := a.projectApplication().Create(path)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectRow(triage.ProjectActionListed, row)
	return row, nil
}

// InspectProjectFolder reads the repository identity of a directory on the
// selected computer without creating a project there. The machine picker
// checks with it that a folder chosen for an existing project is a checkout
// of that project's repository before CreateProjectCheckout adopts it.
//
//ao:scope git:operate
//ao:route selected
func (a *App) InspectProjectFolder(ctx context.Context, path string) (projectapp.FolderIdentity, error) {
	if err := a.requireIsolatedWorkspace(path); err != nil {
		return projectapp.FolderIdentity{}, err
	}
	return a.projectApplication().InspectFolder(ctx, path)
}

// RenameProject updates the display name. Path is immutable.
//
//ao:scope threads:operate
//ao:route project
func (a *App) RenameProject(id, name string) (store.Project, error) {
	write, err := a.projectApplication().Rename(id, name)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectWrite(triage.ProjectActionFull, write)
	return write.Project, nil
}

// ArchiveProject hides the project without deleting it.
//
//ao:scope threads:operate
//ao:route project
func (a *App) ArchiveProject(id string) error {
	write, err := a.projectApplication().Archive(id)
	if err != nil {
		return err
	}
	a.broadcastProjectWrite(triage.ProjectActionUnlisted, write)
	return nil
}

// UnarchiveProject reverses ArchiveProject and returns the refreshed row.
//
//ao:scope threads:operate
//ao:route project
func (a *App) UnarchiveProject(id string) (store.Project, error) {
	write, err := a.projectApplication().Unarchive(id)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectWrite(triage.ProjectActionListed, write)
	return write.Project, nil
}

// UpdateProjectSortPositions re-orders the projects list. The frontend
// emits the full ordered list when the user drops a drag-reorder so the
// store assigns dense positions 0..N-1 in one transaction.
//
// One frame per row written. Every listed project is written, not only the
// ones whose index moved, because the reorder also bumps updated_at — that
// bump is deliberate (it makes a reorder count as project activity), so those
// rows really did change and other clients need them.
//
//ao:scope threads:operate
//ao:route home
func (a *App) UpdateProjectSortPositions(orderedIDs []string) error {
	moved, err := a.projectApplication().UpdateSortPositions(orderedIDs)
	if err != nil {
		return err
	}
	for _, row := range moved {
		a.broadcastProjectRow(triage.ProjectActionFull, row)
	}
	return nil
}

// WorktreeSetupConfig is the wire shape of a project's worktree setup recipe.
// It mirrors worktreesetup.Config with the slices always materialised, so the
// editor binds against `[]` rather than having to treat null as empty.
type WorktreeSetupConfig struct {
	Copy    []string   `json:"copy"`
	Run     [][]string `json:"run"`
	Timeout string     `json:"timeout"`
}

// GetProjectWorktreeSetup returns the project's recipe. An unconfigured project
// returns the empty recipe — the editor's starting state — rather than an
// error, because "not configured yet" is the normal case, not a fault.
//
//ao:scope terminal:operate
func (a *App) GetProjectWorktreeSetup(projectID string) (WorktreeSetupConfig, error) {
	config, err := a.projectApplication().GetWorktreeSetup(projectID)
	if err != nil {
		return WorktreeSetupConfig{}, err
	}
	return toWireWorktreeSetup(config), nil
}

// SetProjectWorktreeSetup validates and persists the project's recipe, and
// returns the stored result so the editor re-seeds from what was actually
// saved. An invalid recipe is a save error and is NEVER persisted: these argv
// commands run unattended on every worktree this project cuts.
//
// A recipe that asks for nothing clears the row, so "remove everything" and
// "never configured" are the same state.
//
//ao:scope terminal:operate
//ao:stepup
func (a *App) SetProjectWorktreeSetup(projectID string, config WorktreeSetupConfig) (WorktreeSetupConfig, error) {
	stored := worktreesetup.Config{Copy: config.Copy, Run: config.Run, Timeout: config.Timeout}
	saved, write, err := a.projectApplication().SetWorktreeSetup(projectID, stored)
	if err != nil {
		return WorktreeSetupConfig{}, err
	}
	// The frame carries the project row, which does not include the recipe —
	// the recipe has its own read (GetProjectWorktreeSetup), and putting it on
	// every project frame would be wire weight for a surface almost nobody has
	// open. What the frame says is that this project moved.
	a.broadcastProjectWrite(triage.ProjectActionFull, write)
	return toWireWorktreeSetup(saved), nil
}

func toWireWorktreeSetup(config worktreesetup.Config) WorktreeSetupConfig {
	return WorktreeSetupConfig{
		Copy:    slicesx.OrEmpty(config.Copy),
		Run:     slicesx.OrEmpty(config.Run),
		Timeout: config.Timeout,
	}
}

// ProjectDeletionResult is what deleting one project did: the threads that went
// with it, so the frontend can purge pane state, and the checkouts that are
// still on disk afterwards.
//
// The two travel together because a deletion that removed most of its litter is
// still a partial outcome, and a caller that only saw the thread ids would
// report it as a clean success.
type ProjectDeletionResult struct {
	ThreadIDs []string `json:"threadIds"`
	// RetainedWorktrees is empty on the ordinary path. It carries the checkouts
	// git declined to remove — see RetainedWorktree — so the user can finish the
	// job with their own tools.
	RetainedWorktrees []RetainedWorktree `json:"retainedWorktrees"`
}

// DeleteProject removes a project from Agent Overflow: every contained thread,
// every workflow run and automation the project owns, the worktrees the app
// created for those runs, and the project row. A project with active turns or
// running background tasks is refused: deleting the workspace container must
// never implicitly interrupt work in one of its threads.
//
// It is cleanup, not discard (decision D25). No branch is deleted, so every
// commit the runs produced stays reachable in the repository; a checkout
// carrying uncommitted work is left alone by git's own refusal and reported in
// the result. Nothing here is unrecoverable, which is why nothing here takes a
// consent — see app_project_delete_workflow.go.
//
// The order below is load-bearing (invariant 35). The workflow cleanup runs
// FIRST, before a single thread lock is taken: cancelling a live run reaches
// App.InterruptTurn, which locks the run's phase thread — one of the locks this
// method would otherwise already hold on every thread in the project.
//
//ao:scope threads:operate
//ao:route project
func (a *App) DeleteProject(id string) (ProjectDeletionResult, error) {
	if a.store == nil {
		return ProjectDeletionResult{}, fmt.Errorf("delete project: store unavailable")
	}
	if err := a.store.CheckProjectTransferAccess(id); err != nil {
		return ProjectDeletionResult{}, err
	}
	result := ProjectDeletionResult{ThreadIDs: []string{}, RetainedWorktrees: []RetainedWorktree{}}
	footprint, err := a.projectApplication().WorkflowFootprint(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	if footprint.HasWork() {
		retained, err := a.cleanUpProjectWorkflowWork(id, footprint)
		if err != nil {
			return ProjectDeletionResult{}, fmt.Errorf("delete project %s: clean up workflow work: %w", id, err)
		}
		result.RetainedWorktrees = retained
	}

	ids, err := a.store.ListThreadIDsForProject(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	threads := make(map[string]store.Thread, len(ids))
	for _, threadID := range ids {
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			return ProjectDeletionResult{}, fmt.Errorf("delete project: load thread %s: %w", threadID, err)
		}
		threads[threadID] = thread
	}

	unlocks := make([]func(), 0, len(ids))
	defer func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}()
	for _, threadID := range projectapp.ThreadLockOrder(threads) {
		unlocks = append(unlocks, a.threadLocks().Lock(threadID))
	}

	lockedIDs, err := a.store.ListThreadIDsForProject(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	slices.Sort(lockedIDs)
	lockedIDs = slices.Compact(lockedIDs)
	if !slices.Equal(ids, lockedIDs) {
		return ProjectDeletionResult{}, fmt.Errorf("delete project: contained threads changed during deletion; retry")
	}
	// The same guard for the workflow side: the cleanup above ran unlocked, and a
	// cron automation firing in that window would start a run this deletion never
	// described and never cleaned up after.
	lockedFootprint, err := a.projectApplication().WorkflowFootprint(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	if !footprint.SameAs(lockedFootprint) {
		return ProjectDeletionResult{}, fmt.Errorf("delete project: contained workflow work changed during deletion; retry")
	}

	var deletedLocalIDs []string
	for _, threadID := range ids {
		retired, err := a.threadApplication().CheckCleanup(threadID)
		if err != nil {
			return ProjectDeletionResult{}, err
		}
		if retired {
			continue
		}
		deletedLocalIDs = append(deletedLocalIDs, threadID)
		reason, err := a.threadActivityBlockReason(threadID)
		if err != nil {
			return ProjectDeletionResult{}, fmt.Errorf("delete project: check thread %s activity: %w", threadID, err)
		}
		if reason != "" {
			return ProjectDeletionResult{}, fmt.Errorf("cannot delete project while thread %s is running: %s", threadID, reason)
		}
	}

	for _, threadID := range ids {
		parentID := threads[threadID].ParentThreadID
		if _, parentIsInProject := threads[parentID]; parentIsInProject {
			continue
		}
		if err := a.deleteThreadTreeWithSubtreeLocksHeld(threadID); err != nil {
			return ProjectDeletionResult{}, fmt.Errorf("delete project: delete thread %s: %w", threadID, err)
		}
	}
	// The cleanup above frees disk and stops processes; it never touches a row,
	// because a run record is not litter — it is the history the app is being
	// asked to forget. It is dropped here, with the project. `work_items` has no
	// foreign key to `projects` to cascade through, and a row left behind would
	// carry a project id that resolves to nothing.
	// Campaign memory goes with the records for the same reason: a memory tree is
	// keyed by its root run, so one whose root row is gone is unreachable by
	// every read verb and every prompt injection — litter that nothing can ever
	// name again. This is the ONLY flow that removes one. Discard leaves run
	// records in place and so leaves the memory alone, exactly as it leaves the
	// narratives and envelopes of the campaign it discarded.
	for _, root := range footprint.Roots() {
		if err := a.removeWorkflowMemoryTree(root.ID); err != nil {
			return ProjectDeletionResult{}, fmt.Errorf("delete project %s: %w", id, err)
		}
	}
	if err := a.store.DeleteProjectWorkflowRecords(id); err != nil {
		return ProjectDeletionResult{}, err
	}
	// Automations went with those rows; the scheduler still has them armed until
	// it re-reads.
	if err := a.refreshWorkflowSchedule(); err != nil {
		return ProjectDeletionResult{}, fmt.Errorf("delete project %s: refresh automation schedule: %w", id, err)
	}
	if err := a.store.DeleteProject(id); err != nil {
		if errors.Is(err, store.ErrProjectHasThreads) {
			return ProjectDeletionResult{}, fmt.Errorf("delete project: contained threads changed during deletion; retry")
		}
		return ProjectDeletionResult{}, err
	}
	a.broadcastProjectDeleted(id)
	// The same ID now belongs to another computer after a move. Returning its
	// retired cache ID would make the frontend close that computer's live pane.
	result.ThreadIDs = slicesx.OrEmpty(deletedLocalIDs)
	return result, nil
}

// ensureProjectForWorkspace resolves workspacePath to its project through
// projectapp, which records a created row's repository identity. A workspace no project covers yet mints one, which is a new sidebar entry
// and is announced like any other creation. Resolving to an existing project
// is the common case and says nothing.
func (a *App) ensureProjectForWorkspace(workspacePath string) (store.Project, error) {
	write, err := a.projectApplication().EnsureForWorkspace(workspacePath)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectWrite(triage.ProjectActionListed, write)
	return write.Project, nil
}

// RefreshProjectIdentity rechecks a project's checkout on its owning computer.
//
//ao:scope threads:read
//ao:route project
func (a *App) RefreshProjectIdentity(ctx context.Context, projectID string) (store.Project, error) {
	write, err := a.projectApplication().RefreshProject(ctx, projectID)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectWrite(triage.ProjectActionFull, write)
	return write.Project, nil
}

// CreateProjectCheckout checks identity before registering the selected folder.
//
//ao:scope git:operate
//ao:route selected
func (a *App) CreateProjectCheckout(ctx context.Context, path string, expected store.ProjectIdentity) (store.Project, error) {
	if err := a.requireIsolatedWorkspace(path); err != nil {
		return store.Project{}, err
	}
	write, err := a.projectApplication().CreateCheckout(ctx, path, expected)
	if err != nil {
		return store.Project{}, err
	}
	a.broadcastProjectWrite(triage.ProjectActionListed, write)
	return write.Project, nil
}
