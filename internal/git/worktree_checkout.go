package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CreateWorktree creates a new worktree backed by a new branch.
func (c *Core) CreateWorktree(cwd, path, branch string) error {
	return c.CreateWorktreeFromBranch(cwd, path, "", branch)
}

// AttachWorktree is AttachWorktreeAt for an existing branch.
func (c *Core) AttachWorktree(cwd, path, branch string) (createdBranch bool, err error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, errors.New("git worktree branch is required")
	}
	return c.AttachWorktreeAt(cwd, path, BranchRef(branch))
}

// AttachWorktreeAt creates a new worktree at path checking out ref as it
// is. A branch, a local one or one only origin has, is checked out on
// itself and fails if another worktree has it (git's one branch, one
// worktree invariant). A tag or commit leaves the worktree detached there,
// and any number of worktrees may sit on one tag. createdBranch reports
// that the branch was origin's alone and this call made the local branch,
// which a caller undoing the attach deletes with the worktree. A failed
// attach leaves neither a worktree nor a branch it made (checkoutWorktree).
func (c *Core) AttachWorktreeAt(cwd, path string, ref Ref) (createdBranch bool, err error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("git worktree path is required")
	}
	if ref.IsZero() {
		return false, errors.New("git worktree ref is required")
	}
	if err := ref.validate(); err != nil {
		return false, err
	}
	unlock, err := c.lockWorktreeCheckout(cwd)
	if err != nil {
		return false, err
	}
	defer unlock()
	if ref.IsBranch() {
		// A branch only origin has is checked out as a new local branch
		// tracking origin's, named explicitly: git's own guess needs exactly
		// one remote to have the name, and a fork's second remote often does
		// too.
		local, err := c.branchExistsChecked(cwd, ref.Name)
		if err != nil {
			return false, err
		}
		if remoteRef, ok := c.originTrackingRef(cwd, ref.Name); !local && ok {
			_, stderr, err := c.Execute(cwd, "branch", "--track", "--", ref.Name, "refs/remotes/"+remoteRef)
			if err != nil {
				return false, fmt.Errorf("git worktree add failed: %s", firstNonEmpty(strings.TrimSpace(stderr), err.Error()))
			}
			createdBranch = true
		}
	}
	if err := c.checkoutWorktree(cwd, path, ref, createdBranch); err != nil {
		return false, err
	}
	return createdBranch, nil
}

// CreateWorktreeFromBranch is CreateWorktreeFromRef from a base branch;
// an empty baseBranch cuts from the current HEAD.
func (c *Core) CreateWorktreeFromBranch(cwd, path, baseBranch, newBranch string) error {
	return c.CreateWorktreeFromRef(cwd, path, branchArg(baseBranch), newBranch)
}

// CreateWorktreeFromRef creates a new worktree backed by newBranch cut from
// base exactly as it exists locally; the zero base is the current HEAD. A
// tag or commit base gives the branch no upstream.
//
// Use this when the base is a ref only this machine has (a branch another
// worktree just cut). When the base is a branch that also lives on origin,
// CreateWorktreeFromFreshRef is the one to call: it refreshes origin first
// so the new worktree doesn't inherit a stale local base.
func (c *Core) CreateWorktreeFromRef(cwd, path string, base Ref, newBranch string) error {
	if err := base.validate(); err != nil {
		return err
	}
	return c.createWorktreeAt(cwd, path, base.revision(), newBranch, !base.IsZero() && !base.IsBranch())
}

// branchArg is the Ref of a branch argument where empty means HEAD.
func branchArg(branch string) Ref {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return Ref{}
	}
	return BranchRef(branch)
}

// createWorktreeAt makes newBranch and checks it out in a new worktree at
// path. startPoint is a fully qualified revision (refs/heads/<b>,
// refs/tags/<t>, refs/remotes/<r>/<b> or a commit id) so a branch and a tag
// of one name never make git guess; empty means the repository's current
// HEAD. It runs `git branch` and then `git worktree add`, which is what
// `git worktree add -b` does, so that a failed checkout knows the branch is
// this call's to delete (checkoutWorktree).
//
// noTrack suppresses git's branch.autoSetupMerge DWIM, which would otherwise
// set the new branch's upstream whenever the start point is a remote-tracking
// ref. It is the caller's decision because it is only correct for a start
// point the caller resolved: a base the user named keeps git's default
// behaviour.
func (c *Core) createWorktreeAt(cwd, path, startPoint, newBranch string, noTrack bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}
	newBranch = strings.TrimSpace(newBranch)
	startPoint = strings.TrimSpace(startPoint)
	if newBranch == "" {
		return errors.New("git worktree branch is required")
	}
	if err := validateBranchName(newBranch); err != nil {
		return err
	}
	if startPoint != "" {
		if err := validateBranchName(startPoint); err != nil {
			return err
		}
	}
	unlock, err := c.lockWorktreeCheckout(cwd)
	if err != nil {
		return err
	}
	defer unlock()
	if err := c.ensureBranchDoesNotExist(cwd, newBranch); err != nil {
		return err
	}

	args := []string{"branch"}
	if noTrack {
		args = append(args, "--no-track")
	}
	args = append(args, "--", newBranch)
	if startPoint != "" {
		args = append(args, startPoint)
	}
	if _, stderr, err := c.Execute(cwd, args...); err != nil {
		if c.branchAlreadyExistsAfterCreationError(cwd, stderr, newBranch) {
			return branchAlreadyExistsError(newBranch)
		}
		return fmt.Errorf("git worktree add failed: %s", firstNonEmpty(strings.TrimSpace(stderr), err.Error()))
	}
	return c.checkoutWorktree(cwd, path, BranchRef(newBranch), true)
}

// checkoutWorktree checks ref out in a new worktree at path: a branch on
// itself, by its short name, which `git worktree add` reads as
// refs/heads/<name> first (a qualified refs/heads/ argument would detach);
// a tag or commit detached. The add locks the worktree with a reason unique
// to this attempt (checkoutLockReason), and a failed checkout takes back
// only what carries it: a failing post-checkout hook leaves the worktree
// registered and locked, so the registration locked with this attempt's
// reason is removed, and then, when ownsBranch (the caller made the branch
// for this worktree), the branch is deleted. A registration another process
// made at path, or one git refused the add for, carries no such lock and
// stays as it was; a branch the caller did not make is never deleted. What
// the undo could not take back is joined to git's diagnosis. A checkout that
// succeeded and could not be unlocked is a failure too, undone the same
// way, so an error means the call left nothing behind.
func (c *Core) checkoutWorktree(cwd, path string, ref Ref, ownsBranch bool) error {
	reason := checkoutLockReason + uuid.NewString()
	if c.beforeWorktreeAdd != nil {
		c.beforeWorktreeAdd(path)
	}
	args := []string{"worktree", "add", "--lock", "--reason", reason}
	target := ref.Name
	if !ref.IsBranch() {
		args = append(args, "--detach")
		target = ref.revision()
	}
	args = append(args, "--", path, target)
	ownedBranch := ""
	if ownsBranch && ref.IsBranch() {
		ownedBranch = ref.Name
	}
	_, stderr, err := c.Execute(cwd, args...)
	if err != nil {
		failure := fmt.Errorf("git worktree add failed: %s", firstNonEmpty(strings.TrimSpace(stderr), err.Error()))
		return c.undoCheckout(cwd, ownedBranch, reason, failure)
	}
	if _, stderr, err := c.Execute(cwd, "worktree", "unlock", "--", path); err != nil {
		failure := fmt.Errorf("git worktree add: git worktree unlock failed: %s", firstNonEmpty(strings.TrimSpace(stderr), err.Error()))
		return c.undoCheckout(cwd, ownedBranch, reason, failure)
	}
	return nil
}

// undoCheckout takes back what a failed checkoutWorktree made: the
// registration locked with reason and, when ownedBranch is set, that
// branch. It returns failure joined with what it could not take back.
func (c *Core) undoCheckout(cwd, ownedBranch, reason string, failure error) error {
	var undo []error
	entries, listErr := c.listWorktreeEntries(cwd)
	if listErr != nil {
		undo = append(undo, listErr)
	}
	for _, entry := range entries {
		if entry.locked && entry.lockReason == reason {
			undo = append(undo, c.RemoveWorktreeForce(cwd, entry.Path, true))
			break
		}
	}
	if ownedBranch != "" {
		undo = append(undo, c.DeleteBranch(cwd, ownedBranch, true))
	}
	if undoErr := errors.Join(undo...); undoErr != nil {
		return errors.Join(failure, fmt.Errorf("undo the failed worktree add: %w", undoErr))
	}
	return failure
}

// checkoutLockReason prefixes the lock reason checkoutWorktree adds a
// worktree with; a random id follows it, so the lock names one attempt.
const checkoutLockReason = "Agent Overflow checkout "

// CreateWorktreeFromFreshBase is CreateWorktreeFromFreshRef from a base
// branch; an empty baseBranch cuts from the current HEAD.
func (c *Core) CreateWorktreeFromFreshBase(
	ctx context.Context, cwd, path, baseBranch, newBranch string,
) (WorktreeSeed, error) {
	return c.CreateWorktreeFromFreshRef(ctx, cwd, path, branchArg(baseBranch), newBranch)
}
