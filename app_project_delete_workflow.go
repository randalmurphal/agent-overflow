package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// The workflow half of project deletion (decision D25) — the read half: what
// the deletion will do, said before it does it. `app_project_delete_cleanup.go`
// is the half that does it.
//
// Deleting a project used to leave its workflow work behind: `work_items` and
// its record tables carry no foreign key to `projects`, so the runs survived
// with a project id that resolved to nothing, unreachable from every
// project-scoped query, and the worktrees the app had created for them stayed
// registered in the user's repository. Deletion now takes both with it.
//
// Deletion is CLEANUP, not discard. It removes what the app owns — its own
// rows, and the checkouts it created in an app-managed directory — and it never
// deletes a branch: every commit those runs produced is still reachable in the
// repository afterwards. Removing a project from Agent Overflow is a statement
// about Agent Overflow. Git is a system the user owns independently and has
// their own tools for; a sidebar housekeeping action does not get to rewrite
// it. D23's per-run discard stays the only flow in the app that deletes a
// branch (`app_workflow_discard_apply.go`), because there the human has said
// the work itself is not wanted.
//
// Nothing here is unrecoverable, so nothing here takes a consent. The preview
// exists so the deletion can be described honestly — including the checkouts
// git will refuse to remove, which are left in place and reported.

// The reasons cleanup gives for leaving a checkout on disk, in the words the
// user reads. The preview predicts them and the cleanup reports them, so they
// are written once: two surfaces wording the same outcome differently is how a
// user ends up believing neither.
const (
	retainedRepositoryGone = "the project's repository is no longer on disk, so git cannot remove this checkout"
	retainedNotRegistered  = "git no longer tracks this path as a worktree of the project"
)

// ProjectCleanupWorktree is one checkout the deletion will clean up, and what it
// expects to manage to do with it.
type ProjectCleanupWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	// DirtyFileCount is the uncommitted and untracked files in the checkout. It
	// is the same `git status --porcelain` question `git worktree remove` asks
	// itself — gitignored files excluded on both sides — so it decides Retained
	// rather than merely sitting next to it.
	DirtyFileCount int `json:"dirtyFileCount"`
	// Retained reports that the deletion will leave this checkout on disk.
	Retained bool `json:"retained"`
	// Reason is why, in the words the user reads. Empty unless Retained.
	Reason string `json:"reason,omitempty"`
}

// ProjectDeletionPreview is what deleting one project would do on the workflow
// side. There is no loss to consent to — no branch is deleted and no commit
// becomes unreachable — so this describes the cleanup rather than a cost.
type ProjectDeletionPreview struct {
	ProjectID string `json:"projectId"`
	// RunCount is every run the project owns — roots and the runs they called.
	RunCount int `json:"runCount"`
	// LiveRunIDs is the subset still in flight. Deletion cancels them first;
	// they are called out because that is work the human is stopping.
	LiveRunIDs []string `json:"liveRunIds"`
	// AutomationCount is how many triggers go with the project.
	AutomationCount int `json:"automationCount"`
	// Worktrees is every checkout the deletion will act on, deduplicated across
	// the whole forest, each carrying whether it will survive the cleanup.
	Worktrees []ProjectCleanupWorktree `json:"worktrees"`
	// HasWork is the one signal a caller branches on. It is true when the
	// project owns any run or any automation — deriving it again from the counts
	// is how two surfaces end up disagreeing about what a deletion involves.
	HasWork bool `json:"hasWork"`
}

// ProjectDeletionPreview reports what deleting a project would do on the
// workflow side. It runs read-only SQLite and git queries and mutates nothing.
//
// LocalOnly: it reads local checkouts and their uncommitted paths.
func (a *App) ProjectDeletionPreview(projectID string) (ProjectDeletionPreview, error) {
	if a.store == nil {
		return ProjectDeletionPreview{}, fmt.Errorf("project deletion preview: store unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ProjectDeletionPreview{}, fmt.Errorf("project deletion preview: project id is required")
	}
	footprint, err := a.projectWorkflowFootprint(projectID)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	preview := ProjectDeletionPreview{
		ProjectID:       projectID,
		RunCount:        len(footprint.runIDs),
		LiveRunIDs:      make([]string, 0),
		AutomationCount: len(footprint.automationIDs),
		Worktrees:       make([]ProjectCleanupWorktree, 0),
		HasWork:         footprint.hasWork(),
	}
	if len(footprint.roots) == 0 {
		return preview, nil
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	checkouts, err := a.projectWorkflowCheckouts(footprint, project.Path)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	preview.LiveRunIDs = checkouts.live
	preview.Worktrees, err = a.describeCleanupTargets(
		project.Path, checkouts.targets, "project deletion preview "+projectID,
	)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	return preview, nil
}

// projectWorkflowFootprint is the git-free answer to "what workflow work does
// this project own": every run id, the roots of the forest those runs form, and
// every automation id. It is one pair of SQLite reads, so DeleteProject can
// afford to take it twice and prove nothing appeared underneath it.
type projectWorkflowFootprint struct {
	// roots are the runs nothing else in the project called — where a tree walk
	// has to start.
	roots []store.WorkItem
	// runIDs and automationIDs are sorted so two footprints of the same project
	// compare directly.
	runIDs        []string
	automationIDs []string
}

func (f projectWorkflowFootprint) hasWork() bool {
	return len(f.runIDs) > 0 || len(f.automationIDs) > 0
}

// sameAs reports whether two readings of a project describe the same work. It
// is an id-set comparison rather than a count: a cron automation that fires
// mid-deletion replaces nothing, it adds a run, and a count that happened to
// match would hide it.
func (f projectWorkflowFootprint) sameAs(other projectWorkflowFootprint) bool {
	return slices.Equal(f.runIDs, other.runIDs) &&
		slices.Equal(f.automationIDs, other.automationIDs)
}

func (a *App) projectWorkflowFootprint(projectID string) (projectWorkflowFootprint, error) {
	// Summaries, not full rows: this needs ids, linkage, and workspace pointers,
	// never every run's frozen workflow snapshot.
	items, err := a.store.ListWorkItemSummaries(store.WorkItemListFilter{ProjectID: projectID})
	if err != nil {
		return projectWorkflowFootprint{}, fmt.Errorf("project %s workflow footprint: list runs: %w", projectID, err)
	}
	automations, err := a.store.ListAutomations(projectID)
	if err != nil {
		return projectWorkflowFootprint{}, fmt.Errorf("project %s workflow footprint: list automations: %w", projectID, err)
	}
	footprint := projectWorkflowFootprint{
		roots:         make([]store.WorkItem, 0, len(items)),
		runIDs:        make([]string, 0, len(items)),
		automationIDs: make([]string, 0, len(automations)),
	}
	owned := make(map[string]bool, len(items))
	for _, item := range items {
		owned[item.ID] = true
	}
	for _, item := range items {
		footprint.runIDs = append(footprint.runIDs, item.ID)
		// A caller that is not in this project's set cannot walk to this run, so
		// treating it as a root is what keeps "every run is reached exactly once"
		// true even for a record whose parent row is gone.
		if item.ParentItemID == "" || !owned[item.ParentItemID] {
			footprint.roots = append(footprint.roots, item)
		}
	}
	for _, automation := range automations {
		footprint.automationIDs = append(footprint.automationIDs, automation.ID)
	}
	slices.Sort(footprint.runIDs)
	slices.Sort(footprint.automationIDs)
	return footprint, nil
}

// projectWorkflowCheckouts is every checkout the project's run forest owns, plus
// the runs still in flight, as one reading of the store.
type projectWorkflowCheckouts struct {
	live    []string
	targets []workflowWorktreeTarget
}

// projectWorkflowCheckouts walks every root the project owns and collects the
// checkouts its trees registered. It goes through the same workflowTreeLoss walk
// the per-run discard uses, so the preview, the cleanup, and a single run's
// discard cannot disagree about which checkouts belong to a run.
//
// Each tree already deduplicates the checkouts its own members share; this drops
// a checkout two trees both recorded, so one path is never handled twice.
func (a *App) projectWorkflowCheckouts(
	footprint projectWorkflowFootprint, projectPath string,
) (projectWorkflowCheckouts, error) {
	collected := projectWorkflowCheckouts{
		live:    make([]string, 0),
		targets: make([]workflowWorktreeTarget, 0, len(footprint.roots)),
	}
	seen := make(map[string]bool, len(footprint.roots))
	for _, root := range footprint.roots {
		loss, err := a.workflowTreeLoss(root.ID, projectPath)
		if err != nil {
			return projectWorkflowCheckouts{}, err
		}
		collected.live = append(collected.live, loss.live...)
		for _, target := range loss.targets {
			key := gitops.CanonicalPath(target.path)
			if seen[key] {
				continue
			}
			seen[key] = true
			collected.targets = append(collected.targets, target)
		}
	}
	return collected, nil
}

// describeCleanupTargets inspects a whole target set against one reading of the
// project's worktree registry. An empty set asks git nothing: that is a
// read-only run, one that worked in the project checkout, or one whose
// checkouts were already released.
//
// label prefixes the one failure that aborts the report rather than being
// recorded on a row, so the caller's flow is named in the message.
func (a *App) describeCleanupTargets(
	projectPath string, targets []workflowWorktreeTarget, label string,
) ([]ProjectCleanupWorktree, error) {
	described := make([]ProjectCleanupWorktree, 0, len(targets))
	if len(targets) == 0 {
		return described, nil
	}
	registry, err := a.readProjectWorktrees(projectPath, label)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		described = append(described, a.describeCleanupWorktree(registry, target))
	}
	return described, nil
}

// describeCleanupWorktree answers, for one checkout, what the cleanup expects to
// manage. The prediction is exact rather than approximate: it decides on the
// same question `git worktree remove` decides on, so a checkout this reports as
// removable is one git will remove.
//
// A checkout that is no longer on disk is reported plainly, not as a retention:
// there is nothing left there to leave behind, and git prunes a registration
// whose directory is gone without complaint.
func (a *App) describeCleanupWorktree(
	registry projectWorktreeRegistry, target workflowWorktreeTarget,
) ProjectCleanupWorktree {
	branch, registered := registeredWorktreeBranch(registry, target)
	described := ProjectCleanupWorktree{Path: target.path, Branch: branch}
	if !worktreeDirectoryPresent(target.path) {
		return described
	}
	if !registry.present {
		described.Retained, described.Reason = true, retainedRepositoryGone
		return described
	}
	if !registered {
		described.Retained, described.Reason = true, retainedNotRegistered
		return described
	}
	// Paths are not collected: the count is what decides the outcome, and the
	// checkout stays exactly where the user can look at it themselves.
	_, total, err := a.gitCore().WorkingTreeChanges(target.path, 0)
	if err != nil {
		// An uninspectable checkout is one the cleanup cannot promise to remove,
		// so it is reported as staying rather than silently assumed clean.
		described.Retained = true
		described.Reason = fmt.Sprintf("it could not be inspected: %v", err)
		return described
	}
	described.DirtyFileCount = total
	if total > 0 {
		described.Retained, described.Reason = true, gitops.RetainedDirtyReason(total)
	}
	return described
}

// registeredWorktreeBranch reports whether git still knows a target's path as a
// worktree of the project, and the branch to name it by: the record's own when
// it has one, the registry's otherwise.
func registeredWorktreeBranch(
	registry projectWorktreeRegistry, target workflowWorktreeTarget,
) (string, bool) {
	for _, worktree := range registry.worktrees {
		if !gitops.SameFilesystemPath(worktree.Path, target.path) {
			continue
		}
		if strings.TrimSpace(target.branch) == "" {
			return worktree.Branch, true
		}
		return target.branch, true
	}
	return target.branch, false
}

func worktreeDirectoryPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
