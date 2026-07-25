package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/project"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// ListProjects returns projects with a lightweight thread count per
// project for the sidebar.
func (a *App) ListProjects() ([]store.ProjectWithCounts, error) {
	if a.store == nil {
		return nil, fmt.Errorf("list projects: store unavailable")
	}
	return a.store.ListProjectsWithThreadCounts()
}

// CreateProject validates that path exists, is a directory, and is not
// already backing another project. Returns ErrProjectPathInUse when the
// path already has a project row — the frontend interprets that as
// "redirect to the existing project" rather than a failure.
func (a *App) CreateProject(path string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("create project: store unavailable")
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("create project: path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return store.Project{}, fmt.Errorf("create project: resolve absolute path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return store.Project{}, fmt.Errorf("create project: stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return store.Project{}, fmt.Errorf("create project: %s is not a directory", abs)
	}

	now := time.Now().UnixMilli()
	p := store.Project{
		ID:        uuid.NewString(),
		Path:      abs,
		Name:      filepath.Base(abs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.store.CreateProject(p); err != nil {
		return store.Project{}, err
	}
	return p, nil
}

// RenameProject updates the display name. Path is immutable.
func (a *App) RenameProject(id, name string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("rename project: store unavailable")
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return store.Project{}, fmt.Errorf("rename project: name is required")
	}
	if err := a.store.UpdateProjectName(id, trimmed); err != nil {
		return store.Project{}, err
	}
	return a.store.GetProject(id)
}

// ArchiveProject hides the project without deleting it.
func (a *App) ArchiveProject(id string) error {
	if a.store == nil {
		return fmt.Errorf("archive project: store unavailable")
	}
	return a.store.ArchiveProject(id)
}

// UnarchiveProject reverses ArchiveProject and returns the refreshed row.
func (a *App) UnarchiveProject(id string) (store.Project, error) {
	if a.store == nil {
		return store.Project{}, fmt.Errorf("unarchive project: store unavailable")
	}
	if err := a.store.UnarchiveProject(id); err != nil {
		return store.Project{}, err
	}
	return a.store.GetProject(id)
}

// UpdateProjectSortPositions re-orders the projects list. The frontend
// emits the full ordered list when the user drops a drag-reorder so the
// store assigns dense positions 0..N-1 in one transaction.
func (a *App) UpdateProjectSortPositions(orderedIDs []string) error {
	if a.store == nil {
		return fmt.Errorf("update project sort positions: store unavailable")
	}
	return a.store.UpdateProjectSortPositions(orderedIDs)
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
func (a *App) DeleteProject(id string) (ProjectDeletionResult, error) {
	if a.store == nil {
		return ProjectDeletionResult{}, fmt.Errorf("delete project: store unavailable")
	}
	result := ProjectDeletionResult{ThreadIDs: []string{}, RetainedWorktrees: []RetainedWorktree{}}
	footprint, err := a.projectWorkflowFootprint(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	if footprint.hasWork() {
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
	for _, threadID := range projectThreadLockOrder(threads) {
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
	lockedFootprint, err := a.projectWorkflowFootprint(id)
	if err != nil {
		return ProjectDeletionResult{}, err
	}
	if !footprint.sameAs(lockedFootprint) {
		return ProjectDeletionResult{}, fmt.Errorf("delete project: contained workflow work changed during deletion; retry")
	}

	for _, threadID := range ids {
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
	result.ThreadIDs = slicesx.OrEmpty(ids)
	return result, nil
}

// projectThreadLockOrder returns a stable parent-before-descendant ordering.
// DeleteThread acquires locks recursively in that hierarchy, so project
// deletion must use the same order to avoid a parent/child lock inversion.
func projectThreadLockOrder(threads map[string]store.Thread) []string {
	children := make(map[string][]string, len(threads))
	var roots []string
	for id, thread := range threads {
		if _, parentInProject := threads[thread.ParentThreadID]; parentInProject {
			children[thread.ParentThreadID] = append(children[thread.ParentThreadID], id)
		} else {
			roots = append(roots, id)
		}
	}
	slices.Sort(roots)
	for parentID := range children {
		slices.Sort(children[parentID])
	}

	order := make([]string, 0, len(threads))
	visited := make(map[string]bool, len(threads))
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		order = append(order, id)
		for _, childID := range children[id] {
			visit(childID)
		}
	}
	for _, rootID := range roots {
		visit(rootID)
	}
	// Parent cycles are invalid but should not make deletion silently omit
	// rows. Append any corrupted leftovers deterministically; the subsequent
	// store operations will still fail loudly if their relationships break.
	var leftovers []string
	for id := range threads {
		if !visited[id] {
			leftovers = append(leftovers, id)
		}
	}
	slices.Sort(leftovers)
	for _, id := range leftovers {
		visit(id)
	}
	return order
}

// ensureProjectForWorkspace delegates to project.EnsureForWorkspace.
// Kept as an *App method so existing callers (and tests in this package)
// don't need to thread the store + git core through their call sites.
func (a *App) ensureProjectForWorkspace(workspacePath string) (store.Project, error) {
	return project.EnsureForWorkspace(a.store, a.gitCore(), workspacePath)
}
