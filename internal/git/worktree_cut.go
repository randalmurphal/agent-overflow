package git

import (
	"errors"
	"fmt"
	"strings"
)

// CutKept is an undone cut that left part of itself in place because it
// was no longer the cut's alone: a checkout git would not remove without
// force, or a branch that moved or is checked out elsewhere. Path or Branch
// is empty for the part that was taken back.
type CutKept struct {
	Path, PathReason     string
	Branch, BranchReason string
}

func (k *CutKept) Error() string {
	var said []string
	if k.Path != "" {
		said = append(said, fmt.Sprintf("The worktree at %s was kept: %s.", k.Path, k.PathReason))
	}
	if k.Branch != "" {
		said = append(said, fmt.Sprintf("Branch %s was kept: %s.", k.Branch, k.BranchReason))
	}
	return strings.Join(said, " ")
}

// UndoCut takes back what one worktree cut made, as far as it is still the
// cut's alone. It holds the repository's checkout lock throughout, so no
// attach or cut of this process lands between a check and its removal.
//
// The checkout at path is removed without git's force flag: git keeps one
// holding a change or an untracked file, and one with initialized
// submodules or a lock someone else wrote stays too. path is "" for a
// branch made with no checkout cut on it yet. branch goes only when the
// call made it, at tip ("" when it did not), its checkout went, it still
// points at tip and no worktree has it checked out. What stays is returned
// as *CutKept, naming why; any other error is git failing.
func (c *Core) UndoCut(cwd, path, branch, tip string) error {
	unlock, err := c.lockWorktreeCheckout(cwd)
	if err != nil {
		return err
	}
	defer unlock()
	if path != "" {
		if err := c.removeWorktree(cwd, path, false, unattendedLift); err != nil {
			reason, kept := c.keptCheckoutReason(path, err)
			if !kept {
				return err
			}
			stays := &CutKept{Path: path, PathReason: reason}
			if tip != "" {
				stays.Branch, stays.BranchReason = branch, "it is checked out in that worktree"
			}
			return stays
		}
	}
	if tip == "" {
		return nil
	}
	reason, err := c.deleteCutBranch(cwd, branch, tip)
	if err != nil || reason == "" {
		return err
	}
	return &CutKept{Branch: branch, BranchReason: reason}
}

// keptCheckoutReason says why a removal without force refused the checkout
// at path, when it refused rather than failed: the lock or the submodules
// it named, or the work git's own dirty check found.
func (c *Core) keptCheckoutReason(path string, removeErr error) (string, bool) {
	var locked *WorktreeLockedError
	switch {
	case errors.As(removeErr, &locked):
		return "it is locked: " + locked.Reason, true
	case errors.Is(removeErr, ErrWorktreeHasSubmodules):
		return "it has initialized submodules", true
	}
	if _, total, err := c.WorkingTreeChanges(path, 0); err == nil && total > 0 {
		return "it holds " + RetainedDirtyReason(total), true
	}
	return "", false
}

// deleteCutBranch deletes branch, which a cut made at tip, unless it is no
// longer only the cut's, and returns why it stayed ("" when it went or was
// already gone).
func (c *Core) deleteCutBranch(cwd, branch, tip string) (string, error) {
	entries, err := c.listWorktreeEntries(cwd)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Branch == branch {
			return "it is checked out at " + entry.Path, nil
		}
	}
	current, found, err := c.branchCommit(cwd, branch)
	if err != nil || !found {
		return "", err
	}
	if current != tip {
		return fmt.Sprintf("it has moved to %s since the cut made it at %s", shortCommit(current), shortCommit(tip)), nil
	}
	return "", c.DeleteBranch(cwd, branch, true)
}

// branchCommit is the commit local branch points at; found is false when
// there is no such branch.
func (c *Core) branchCommit(cwd, branch string) (commit string, found bool, err error) {
	result, err := c.run(cwd, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", false, err
	}
	switch result.exitCode {
	case 0:
		return strings.TrimSpace(result.stdout), true, nil
	case 1:
		return "", false, nil
	}
	return "", false, fmt.Errorf("resolve branch %s: %s", branch, strings.TrimSpace(result.stderr))
}

// madeBranchTip is the commit a branch this call just made points at,
// which undoing the cut compares against. A branch it cannot read is
// deleted again and the failure returned, since nothing could undo it later.
func (c *Core) madeBranchTip(cwd, branch string) (string, error) {
	tip, found, err := c.branchCommit(cwd, branch)
	if err == nil && !found {
		err = fmt.Errorf("branch %s is missing right after it was made", branch)
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("git worktree add: %w", err), c.DeleteBranch(cwd, branch, true))
	}
	return tip, nil
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
