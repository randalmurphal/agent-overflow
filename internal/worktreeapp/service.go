package worktreeapp

import (
	"fmt"
	"slices"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// WorktreeStatus describes the loss-of-work and attachment signals shown by
// the cleanup UI.
type WorktreeStatus struct {
	Path             string `json:"path"`
	Branch           string `json:"branch"`
	Dirty            bool   `json:"dirty"`
	UncommittedCount int    `json:"uncommittedCount"`
	UnpushedCommits  int    `json:"unpushedCommits"`
	HasUpstream      bool   `json:"hasUpstream"`
	AttachedThreads  int    `json:"attachedThreads"`
}

// WorktreeListItem is the picker-facing worktree shape.
type WorktreeListItem struct {
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	DeleteBlocked bool   `json:"deleteBlocked"`
}

// WorkspaceActivity aggregates live work across every thread referencing one
// canonical checkout directory.
type WorkspaceActivity struct {
	ActiveTurnThreads      int          `json:"activeTurnThreads"`
	RunningBackgroundTasks int          `json:"runningBackgroundTasks"`
	BusyThreads            []BusyThread `json:"busyThreads"`
}

// BusyThread is one non-idle thread's contribution to WorkspaceActivity.
type BusyThread struct {
	ThreadID               string `json:"threadId"`
	ActiveTurn             bool   `json:"activeTurn"`
	RunningBackgroundTasks int    `json:"runningBackgroundTasks"`
}

// Deps are the store, git executor, and two live-runtime facts unavailable in
// SQLite. The callbacks keep provider/router state out of this package.
type Deps struct {
	Store                  *store.Store
	Core                   *gitops.Core
	TransientBusyThreadIDs func() []string
	CountBackgroundTasks   func(string) (int, error)
}

// Service answers worktree and workspace safety queries without mutating the
// filesystem or provider-session state.
type Service struct {
	store                  *store.Store
	core                   *gitops.Core
	transientBusyThreadIDs func() []string
	countBackgroundTasks   func(string) (int, error)
}

func New(deps Deps) *Service {
	core := deps.Core
	if core == nil {
		core = gitops.NewCore()
	}
	return &Service{
		store:                  deps.Store,
		core:                   core,
		transientBusyThreadIDs: deps.TransientBusyThreadIDs,
		countBackgroundTasks:   deps.CountBackgroundTasks,
	}
}

// Find resolves a path to one of the project's registered worktrees.
func (s *Service) Find(project, candidate string) (gitops.Worktree, bool, error) {
	worktrees, err := s.core.ListWorktrees(project)
	if err != nil {
		return gitops.Worktree{}, false, err
	}
	for _, worktree := range worktrees {
		if gitops.SameFilesystemPath(worktree.Path, candidate) {
			return worktree, true, nil
		}
	}
	return gitops.Worktree{}, false, nil
}

// ThreadsReferencingWorkspace returns every thread whose workspace_path or
// worktree_path names the supplied directory after symlink canonicalization.
func (s *Service) ThreadsReferencingWorkspace(path string) ([]string, error) {
	refs, err := s.store.ListThreadWorkspaceRefs()
	if err != nil {
		return nil, err
	}
	canonical := gitops.CanonicalPath(path)
	var ids []string
	for _, ref := range refs {
		if WorkspaceRefMatches(ref, canonical) {
			ids = append(ids, ref.ID)
		}
	}
	return ids, nil
}

// WorkspaceRefMatches compares both stored path columns to an already
// canonicalized directory.
func WorkspaceRefMatches(ref store.ThreadWorkspaceRef, canonicalPath string) bool {
	return gitops.CanonicalPath(ref.WorktreePath) == canonicalPath ||
		gitops.CanonicalPath(ref.WorkspacePath) == canonicalPath
}

// BusyThreadWorkspaceRefs returns workspace refs for every persisted or
// transiently busy thread across all projects.
func (s *Service) BusyThreadWorkspaceRefs() ([]store.ThreadWorkspaceRef, error) {
	refs, err := s.store.ListBlockedThreadWorkspaceRefs()
	if err != nil {
		return nil, err
	}
	if s.transientBusyThreadIDs == nil {
		return refs, nil
	}
	transient := make(map[string]struct{})
	for _, threadID := range s.transientBusyThreadIDs() {
		transient[threadID] = struct{}{}
	}
	for _, ref := range refs {
		delete(transient, ref.ID)
	}
	if len(transient) == 0 {
		return refs, nil
	}
	all, err := s.store.ListThreadWorkspaceRefs()
	if err != nil {
		return nil, err
	}
	for _, ref := range all {
		if _, ok := transient[ref.ID]; ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// Activity answers the directory-wide and per-thread live-work question used
// by both destructive affordances and backend refusal checks.
func (s *Service) Activity(workspacePath string) (WorkspaceActivity, error) {
	path := strings.TrimSpace(workspacePath)
	if path == "" {
		return WorkspaceActivity{}, fmt.Errorf("workspace activity: workspace path is required")
	}
	refs, err := s.BusyThreadWorkspaceRefs()
	if err != nil {
		return WorkspaceActivity{}, fmt.Errorf("workspace activity: list busy threads: %w", err)
	}
	canonical := gitops.CanonicalPath(path)
	var activity WorkspaceActivity
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, duplicate := seen[ref.ID]; duplicate || !WorkspaceRefMatches(ref, canonical) {
			continue
		}
		seen[ref.ID] = struct{}{}
		thread := BusyThread{ThreadID: ref.ID}
		if _, open, err := s.store.GetActiveTurn(ref.ID); err != nil {
			return WorkspaceActivity{}, fmt.Errorf("workspace activity: check active turn for %s: %w", ref.ID, err)
		} else if open {
			activity.ActiveTurnThreads++
			thread.ActiveTurn = true
		}
		count := 0
		if s.countBackgroundTasks != nil {
			count, err = s.countBackgroundTasks(ref.ID)
			if err != nil {
				return WorkspaceActivity{}, fmt.Errorf("workspace activity: count background tasks for %s: %w", ref.ID, err)
			}
		}
		activity.RunningBackgroundTasks += count
		thread.RunningBackgroundTasks = count
		if thread.ActiveTurn || count > 0 {
			activity.BusyThreads = append(activity.BusyThreads, thread)
		}
	}
	slices.SortFunc(activity.BusyThreads, func(left, right BusyThread) int {
		return strings.Compare(left.ThreadID, right.ThreadID)
	})
	return activity, nil
}

// Status computes one registered worktree's loss-of-work signals.
func (s *Service) Status(project, worktreePath string) (WorktreeStatus, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return WorktreeStatus{}, fmt.Errorf("worktree path is required")
	}
	worktree, ok, err := s.Find(project, worktreePath)
	if err != nil {
		return WorktreeStatus{}, err
	}
	if !ok {
		return WorktreeStatus{}, fmt.Errorf("%s is not a worktree for %s", worktreePath, project)
	}
	status := WorktreeStatus{Path: worktree.Path, Branch: worktree.Branch}
	count, err := s.core.CountWorkingTreeChanges(worktree.Path)
	if err != nil {
		return WorktreeStatus{}, fmt.Errorf("status: %w", err)
	}
	status.UncommittedCount = count
	status.Dirty = count > 0
	if status.Branch != "" {
		unpushed, hasUpstream, err := s.core.CountUnpushedCommits(worktree.Path, status.Branch)
		if err != nil {
			return WorktreeStatus{}, fmt.Errorf("unpushed commits: %w", err)
		}
		status.UnpushedCommits = unpushed
		status.HasUpstream = hasUpstream
	}
	attached, err := s.ThreadsReferencingWorkspace(worktree.Path)
	if err != nil {
		return WorktreeStatus{}, err
	}
	status.AttachedThreads = len(attached)
	return status, nil
}

// List returns picker worktrees with deletion blocked by activity from any
// thread, regardless of which project row names the checkout.
func (s *Service) List(project string) ([]WorktreeListItem, error) {
	worktrees, err := s.core.ListWorktrees(project)
	if err != nil {
		return nil, err
	}
	refs, err := s.BusyThreadWorkspaceRefs()
	if err != nil {
		return nil, err
	}
	items := make([]WorktreeListItem, len(worktrees))
	itemByPath := make(map[string]int, len(items))
	for index, worktree := range worktrees {
		items[index] = WorktreeListItem{Path: worktree.Path, Branch: worktree.Branch, Head: worktree.HEAD}
		itemByPath[gitops.CanonicalPath(worktree.Path)] = index
	}
	for _, ref := range refs {
		for _, path := range []string{ref.WorktreePath, ref.WorkspacePath} {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if index, ok := itemByPath[gitops.CanonicalPath(path)]; ok {
				items[index].DeleteBlocked = true
			}
		}
	}
	return items, nil
}
