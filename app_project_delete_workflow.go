package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/scheduler"
)

// The workflow half of project deletion (decision D25).
//
// Deleting a project used to leave its workflow work behind: `work_items` and
// its record tables carry no foreign key to `projects`, so the runs survived
// with a project id that resolved to nothing, unreachable from every
// project-scoped query, and their worktrees and branches stayed on disk. D25
// settled that the other way — deletion destroys them — but never silently.
//
// ProjectDeletionPreview is the consent, the same shape D23 gives a single
// discard: it walks every run tree the project owns, reports every checkout the
// deletion would remove along with what is in it, and mutates nothing.
// DeleteProject refuses outright until that consent is handed back.
//
// The consent is a separate METHOD, not a parameter on DeleteProject, because
// the transport authorizes by method name: `LocalOnlyMethods` can refuse
// DeleteProjectDiscardingWorkflowWork from a LAN peer, and cannot express "the
// same method, but only when this argument is false". Ordinary project deletion
// stays reachable from a remote client, as it has always been; the one flow in
// the app that deletes a git branch does not become reachable with it.

// ErrProjectOwnsWorkflowWork is the refusal a project deletion takes when the
// caller has not consented to destroying the workflow work the project owns.
// Nothing is mutated when it is returned.
var ErrProjectOwnsWorkflowWork = errors.New("project owns workflow work")

// ProjectDeletionPreview is what deleting one project would destroy on the
// workflow side.
type ProjectDeletionPreview struct {
	ProjectID string `json:"projectId"`
	// RootRunIDs names each run tree the deletion would discard. A run whose
	// caller's record is missing counts as a root of its own, so every run the
	// project owns is covered by exactly one tree.
	RootRunIDs []string `json:"rootRunIds"`
	// RunCount is every run the project owns — roots and the runs they called.
	RunCount int `json:"runCount"`
	// LiveRunIDs is the subset still in flight. Deletion cancels them first; they
	// are called out because that is work the human is stopping, not just work
	// they are throwing away.
	LiveRunIDs []string `json:"liveRunIds"`
	// AutomationCount is how many triggers go with the project.
	AutomationCount int `json:"automationCount"`
	// Worktrees is every checkout the deletion would remove, deduplicated across
	// the whole forest, with the branch, dirty files, and unmerged commits that
	// live in it and nowhere else.
	Worktrees []WorkflowDiscardWorktree `json:"worktrees"`
	// HasWork is the one signal a caller branches on. It is true when the project
	// owns any run or any automation — deriving it again from the counts is how
	// two surfaces end up disagreeing about whether a deletion needs consent.
	HasWork bool `json:"hasWork"`
}

// ProjectDeletionPreview reports what deleting a project would destroy on the
// workflow side. It runs read-only SQLite and git queries and mutates nothing.
//
// LocalOnly: it reads local checkouts and repository history.
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
		RootRunIDs:      make([]string, 0, len(footprint.roots)),
		RunCount:        len(footprint.runIDs),
		LiveRunIDs:      make([]string, 0),
		AutomationCount: len(footprint.automationIDs),
		Worktrees:       make([]WorkflowDiscardWorktree, 0),
		HasWork:         footprint.hasWork(),
	}
	if len(footprint.roots) == 0 {
		return preview, nil
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	targets := make([]discardWorktreeTarget, 0, len(footprint.roots))
	seen := make(map[string]bool, len(footprint.roots))
	for _, root := range footprint.roots {
		preview.RootRunIDs = append(preview.RootRunIDs, root.ID)
		loss, err := a.workflowTreeLoss(root.ID, project.Path)
		if err != nil {
			return ProjectDeletionPreview{}, err
		}
		preview.LiveRunIDs = append(preview.LiveRunIDs, loss.live...)
		// Each tree already deduplicates the checkouts its own members share;
		// this drops a checkout two trees both recorded, so one loss is never
		// listed twice.
		for _, target := range loss.targets {
			key := gitops.CanonicalPath(target.path)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, target)
		}
	}
	preview.Worktrees, err = a.describeDiscardTargets(
		project.Path, targets, "project deletion preview "+projectID,
	)
	if err != nil {
		return ProjectDeletionPreview{}, err
	}
	return preview, nil
}

// DeleteProjectDiscardingWorkflowWork deletes a project AND the workflow work it
// owns: every run and its phase/unit/effect records, its automations, and the
// run worktrees and branches on disk. It is the consenting form of
// DeleteProject, which refuses a project that owns any of that.
//
// LocalOnly: it removes local checkouts and deletes branches — the same class as
// WorkflowDiscardItem, whose per-run destruction this performs project-wide.
func (a *App) DeleteProjectDiscardingWorkflowWork(id string) ([]string, error) {
	return a.deleteProject(id, true)
}

// projectWorkflowFootprint is the git-free answer to "what workflow work does
// this project own": every run id, the roots of the forest those runs form, and
// every automation id. It is one pair of SQLite reads, so both the preview and
// DeleteProject's consent check can afford to take it — and DeleteProject can
// take it twice to prove nothing appeared underneath it.
type projectWorkflowFootprint struct {
	// roots are the runs nothing else in the project called — where a tree walk,
	// and therefore a discard, has to start.
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

// describe renders the footprint as the phrase a refusal puts in front of a
// human: counts, never ids.
func (f projectWorkflowFootprint) describe() string {
	parts := make([]string, 0, 2)
	if count := len(f.runIDs); count > 0 {
		parts = append(parts, fmt.Sprintf("%d workflow run%s", count, pluralSuffix(count)))
	}
	if count := len(f.automationIDs); count > 0 {
		parts = append(parts, fmt.Sprintf("%d automation%s", count, pluralSuffix(count)))
	}
	return strings.Join(parts, " and ")
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func (a *App) projectWorkflowFootprint(projectID string) (projectWorkflowFootprint, error) {
	// Summaries, not full rows: the consent check needs ids, linkage, and
	// workspace pointers, never every run's frozen workflow snapshot.
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

// discardProjectWorkflowWork runs the D23 discard over every run tree the
// project owns, then ends the provider sessions those trees were running on.
//
// It MUST run before DeleteProject takes any thread lock. Discard cancels the
// live members first, and cancelling reaches App.InterruptTurn through the
// engine's teardown → Runner.Stop path; InterruptTurn takes
// a.threadLocks().Lock(threadID) on the workflow phase thread, which is one of
// the very locks DeleteProject would already be holding. Cancelling under those
// locks deadlocks.
//
// A failure aborts the deletion with the project intact. Discard tolerates a
// checkout that is already gone and a branch that is already deleted, so the
// retry after a partial failure picks up where this left off.
func (a *App) discardProjectWorkflowWork(footprint projectWorkflowFootprint) error {
	var errs []error
	for _, root := range footprint.roots {
		if _, err := a.discardWorkflowTree(root); err != nil {
			errs = append(errs, fmt.Errorf("discard run %s: %w", root.ID, err))
			continue
		}
		members, err := a.workflowRunTree(root.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.stopWorkflowTreeSessions(members); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopWorkflowTreeSessions ends the provider processes the tree's phases and
// fan-out units were running on.
//
// The discard has already cancelled those runs, so the sessions have nothing
// left to produce. Stopping them settles their open turn rows synchronously —
// session teardown runs triage's cleanup, which synthesizes the truncated
// turn-complete — and that is what lets DeleteProject's thread-activity check
// see an idle project instead of racing a provider that was told to stop and
// has not yet said so. A CLI that acks an interrupt and then never emits a
// result would otherwise block the deletion the human just consented to,
// forever.
//
// The run's origin thread is deliberately untouched: that is the human's own
// conversation, which the ordinary thread teardown handles like any other.
func (a *App) stopWorkflowTreeSessions(members []store.WorkItem) error {
	var errs []error
	for _, member := range members {
		threadIDs, err := a.workflowRunThreadIDs(member)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, threadID := range threadIDs {
			if err := a.stopSession(threadID); err != nil {
				errs = append(errs, fmt.Errorf("stop workflow session on thread %s: %w", threadID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// workflowRunThreadIDs lists the threads one run's phase attempts and fan-out
// units ran on, deduplicated — a re-entered phase reuses its thread.
func (a *App) workflowRunThreadIDs(item store.WorkItem) ([]string, error) {
	phases, err := a.store.ListWorkItemPhases(item.ID)
	if err != nil {
		return nil, fmt.Errorf("list phases of run %s: %w", item.ID, err)
	}
	units, err := a.store.ListWorkItemUnits(item.ID)
	if err != nil {
		return nil, fmt.Errorf("list fan-out units of run %s: %w", item.ID, err)
	}
	threadIDs := make([]string, 0, len(phases)+len(units))
	seen := make(map[string]bool, len(phases)+len(units))
	add := func(threadID string) {
		if threadID == "" || seen[threadID] {
			return
		}
		seen[threadID] = true
		threadIDs = append(threadIDs, threadID)
	}
	for _, phase := range phases {
		add(phase.ThreadID)
	}
	for _, unit := range units {
		add(unit.ThreadID)
	}
	return threadIDs, nil
}

// refreshWorkflowSchedule recomputes the automation schedule after rows have
// been deleted, so the scheduler drops them from its in-memory arming instead
// of firing a trigger whose definition is gone.
//
// A nil scheduler — a bare test App, or a boot that never reached workflow
// init — has no schedule to correct, and neither does a stopped one: it arms
// nothing, so the deleted automations are already unreachable.
func (a *App) refreshWorkflowSchedule() error {
	if a.workflowScheduler == nil {
		return nil
	}
	if err := a.workflowScheduler.Refresh(); err != nil && !errors.Is(err, scheduler.ErrStopped) {
		return err
	}
	return nil
}
