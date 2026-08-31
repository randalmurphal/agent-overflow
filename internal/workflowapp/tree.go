package workflowapp

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

const (
	maxDiscardPreviewFiles   = 20
	maxDiscardPreviewCommits = 20
)

func (s *Service) git() (Git, error) {
	if s == nil || s.deps.Git == nil {
		return nil, errors.New("workflow application: git unavailable")
	}
	client := s.deps.Git()
	if client == nil {
		return nil, errors.New("workflow application: git unavailable")
	}
	return client, nil
}

// DiscardPreview reports the whole run-tree loss without mutating it.
func (s *Service) DiscardPreview(itemID string) (DiscardPreview, error) {
	item, err := s.discardRoot(itemID)
	if err != nil {
		return DiscardPreview{}, err
	}
	database, err := s.store()
	if err != nil {
		return DiscardPreview{}, err
	}
	project, err := database.GetProject(item.ProjectID)
	if err != nil {
		return DiscardPreview{}, err
	}
	loss, err := s.TreeLoss(item.ID, project.Path)
	if err != nil {
		return DiscardPreview{}, err
	}
	preview := DiscardPreview{
		ItemID: item.ID, Members: make([]string, 0, len(loss.Members)), LiveMembers: loss.Live,
	}
	for _, member := range loss.Members {
		preview.Members = append(preview.Members, member.ID)
	}
	preview.Worktrees, err = s.describeDiscardTargets(
		project.Path, loss.Targets, "workflow discard preview "+item.ID,
	)
	if err != nil {
		return DiscardPreview{}, err
	}
	return preview, nil
}

func (s *Service) discardRoot(itemID string) (store.WorkItem, error) {
	database, err := s.store()
	if err != nil {
		return store.WorkItem{}, err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return store.WorkItem{}, fmt.Errorf("workflow discard: item id is required")
	}
	item, err := database.GetWorkItem(itemID)
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

// TreeLoss is the single definition of the worktree set owned by a run tree.
func (s *Service) TreeLoss(rootID, projectPath string) (TreeLoss, error) {
	members, err := s.RunTree(rootID)
	if err != nil {
		return TreeLoss{}, err
	}
	base, err := s.dispositionBase(members[0])
	if err != nil {
		base = ""
	}
	loss := TreeLoss{Members: members, Live: make([]string, 0)}
	for _, member := range members {
		if engine.State(member.State) == engine.StateRunning {
			loss.Live = append(loss.Live, member.ID)
		}
	}
	loss.Targets, err = s.treeWorktrees(members, base, projectPath)
	if err != nil {
		return TreeLoss{}, err
	}
	return loss, nil
}

// RunTree returns the root and its transitively called runs, root first.
func (s *Service) RunTree(rootID string) ([]store.WorkItem, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	root, err := database.GetWorkItem(rootID)
	if err != nil {
		return nil, fmt.Errorf("workflow run tree %s: %w", rootID, err)
	}
	return walkRunTree(root, runCoord, database.ListWorkItemChildren)
}

// RunTreeNodes reads the same tree through the snapshot-free node projection.
func (s *Service) RunTreeNodes(rootID string) ([]store.WorkItemNode, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	root, err := database.GetWorkItemNode(rootID)
	if err != nil {
		return nil, fmt.Errorf("workflow run tree %s: %w", rootID, err)
	}
	return walkRunTree(root, nodeCoord, database.ListWorkItemChildNodes)
}

type treeCoord struct {
	id        string
	callDepth int
}

func runCoord(item store.WorkItem) treeCoord {
	return treeCoord{id: item.ID, callDepth: item.CallDepth}
}
func nodeCoord(node store.WorkItemNode) treeCoord {
	return treeCoord{id: node.ID, callDepth: node.CallDepth}
}

func walkRunTree[T any](root T, coord func(T) treeCoord, children func(string) ([]T, error)) ([]T, error) {
	rootCoord := coord(root)
	members := []T{root}
	for index := 0; index < len(members); index++ {
		member := coord(members[index])
		if member.callDepth-rootCoord.callDepth >= engine.MaxCallDepth {
			return nil, fmt.Errorf("workflow run tree %s: deeper than %d calls", rootCoord.id, engine.MaxCallDepth)
		}
		called, err := children(member.id)
		if err != nil {
			return nil, fmt.Errorf("workflow run tree %s: list children of %s: %w", rootCoord.id, member.id, err)
		}
		members = append(members, called...)
	}
	return members, nil
}

func (s *Service) treeWorktrees(members []store.WorkItem, base, projectPath string) ([]WorktreeTarget, error) {
	database, err := s.store()
	if err != nil {
		return nil, err
	}
	targets := make([]WorktreeTarget, 0, len(members))
	seen := make(map[string]bool, len(members))
	add := func(target WorktreeTarget) {
		if strings.TrimSpace(target.Path) == "" || gitops.SameFilesystemPath(projectPath, target.Path) {
			return
		}
		key := gitops.CanonicalPath(target.Path)
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, target)
	}
	for _, member := range members {
		add(WorktreeTarget{ItemID: member.ID, Path: member.WorktreePath, Branch: member.Branch, Base: base})
		units, err := database.ListWorkItemUnits(member.ID)
		if err != nil {
			return nil, fmt.Errorf("workflow discard %s: list fan-out units: %w", member.ID, err)
		}
		for _, unit := range units {
			add(WorktreeTarget{
				ItemID: member.ID, UnitID: unit.UnitID, Path: unit.WorktreePath,
				Branch: unit.Branch, Base: member.Branch,
			})
		}
	}
	return targets, nil
}

// ReadWorktrees reads one project's worktree registry, treating a missing
// project checkout as an absent registry rather than a permanent cleanup error.
func (s *Service) ReadWorktrees(projectPath, label string) (WorktreeRegistry, error) {
	info, err := os.Stat(projectPath)
	if errors.Is(err, fs.ErrNotExist) {
		return WorktreeRegistry{}, nil
	}
	if err != nil {
		return WorktreeRegistry{}, fmt.Errorf("%s: inspect project checkout %q: %w", label, projectPath, err)
	}
	if !info.IsDir() {
		return WorktreeRegistry{}, fmt.Errorf("%s: project checkout %q is not a directory", label, projectPath)
	}
	client, err := s.git()
	if err != nil {
		return WorktreeRegistry{}, err
	}
	worktrees, err := client.ListWorktrees(projectPath)
	if err != nil {
		return WorktreeRegistry{}, fmt.Errorf("%s: list worktrees: %w", label, err)
	}
	return WorktreeRegistry{Worktrees: worktrees, Present: true}, nil
}

func RegisteredWorktreeBranch(registry WorktreeRegistry, target WorktreeTarget) (string, bool) {
	for _, worktree := range registry.Worktrees {
		if !gitops.SameFilesystemPath(worktree.Path, target.Path) {
			continue
		}
		if strings.TrimSpace(target.Branch) == "" {
			return worktree.Branch, true
		}
		return target.Branch, true
	}
	return target.Branch, false
}

func WorktreeDirectoryPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (s *Service) describeDiscardTargets(projectPath string, targets []WorktreeTarget, label string) ([]DiscardWorktree, error) {
	described := make([]DiscardWorktree, 0, len(targets))
	if len(targets) == 0 {
		return described, nil
	}
	registry, err := s.ReadWorktrees(projectPath, label)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		described = append(described, s.describeDiscardWorktree(projectPath, registry, target))
	}
	return described, nil
}

func (s *Service) describeDiscardWorktree(projectPath string, registry WorktreeRegistry, target WorktreeTarget) DiscardWorktree {
	described := DiscardWorktree{
		ItemID: target.ItemID, UnitID: target.UnitID, Path: target.Path, Base: target.Base,
		DirtyFiles: make([]string, 0), UnmergedCommits: make([]gitdiff.Commit, 0),
	}
	described.Branch, described.Registered = RegisteredWorktreeBranch(registry, target)
	described.Present = WorktreeDirectoryPresent(target.Path)
	var errs []error
	client, gitErr := s.git()
	if gitErr != nil {
		errs = append(errs, gitErr)
	} else if described.Present {
		files, total, err := client.WorkingTreeChanges(target.Path, maxDiscardPreviewFiles)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect working tree: %w", err))
		} else {
			described.DirtyFiles = slicesx.OrEmpty(files)
			described.DirtyFileCount = total
		}
	}
	if registry.Present && described.Branch != "" && target.Base != "" && described.Branch != target.Base {
		if s.deps.ListBranchCommits == nil {
			errs = append(errs, errors.New("list unmerged commits: reader unavailable"))
		} else {
			commits, err := s.deps.ListBranchCommits(s.deps.Context(), projectPath, target.Base, described.Branch)
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
	}
	if joined := errors.Join(errs...); joined != nil {
		described.Error = joined.Error()
	}
	return described
}
