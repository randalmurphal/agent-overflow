package git

import (
	"errors"
	"fmt"
	"strings"
)

// MergeResult records the successful strategy and resulting base-branch HEAD.
type MergeResult struct {
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
}

// MergeBranch checks out base in the project root and merges head without
// force or stash operations. A fast-forward is used whenever possible;
// otherwise git creates a merge commit. Conflict preflight leaves the index
// and working tree untouched.
func (c *Core) MergeBranch(cwd, base, head string) (MergeResult, error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if base == "" || head == "" {
		return MergeResult{}, fmt.Errorf("git merge base and head branches are required")
	}
	if err := validateBranchName(base); err != nil {
		return MergeResult{}, err
	}
	if err := validateBranchName(head); err != nil {
		return MergeResult{}, err
	}
	if base == head {
		return MergeResult{}, fmt.Errorf("git merge base and head branches must differ")
	}
	for _, branch := range []string{base, head} {
		exists, err := c.branchExistsChecked(cwd, branch)
		if err != nil {
			return MergeResult{}, fmt.Errorf("verify local branch %q: %w", branch, err)
		}
		if !exists {
			return MergeResult{}, fmt.Errorf("local branch %q does not exist", branch)
		}
	}
	if err := c.requireCleanWorkingTree(cwd); err != nil {
		return MergeResult{}, err
	}
	current, err := c.currentBranch(cwd)
	if err != nil {
		return MergeResult{}, err
	}
	if current != base {
		if err := c.Checkout(cwd, base); err != nil {
			return MergeResult{}, fmt.Errorf("checkout merge base %q: %w", base, err)
		}
		current, err = c.currentBranch(cwd)
		if err != nil {
			return MergeResult{}, err
		}
		if current != base {
			return MergeResult{}, fmt.Errorf("checkout merge base selected %q, want exact local branch %q", current, base)
		}
	}
	if err := c.requireCleanWorkingTree(cwd); err != nil {
		return MergeResult{}, err
	}
	preflight, err := c.MergeTreeConflicts(cwd, base, head)
	if err != nil {
		return MergeResult{}, fmt.Errorf("preflight merge %q into %q: %w", head, base, err)
	}
	if preflight.Conflicted {
		return MergeResult{}, fmt.Errorf("merge %q into %q has conflicts: %s", head, base, strings.Join(preflight.Paths, ", "))
	}

	mode, err := c.mergeMode(cwd, base, head)
	if err != nil {
		return MergeResult{}, err
	}
	args := []string{"merge", "--ff-only", head}
	if mode == "merge" {
		args = []string{"merge", "--no-ff", "--no-edit", head}
	}
	if _, _, err := c.Execute(cwd, args...); err != nil {
		mergeErr := fmt.Errorf("merge %q into %q: %w", head, base, err)
		return MergeResult{}, errors.Join(mergeErr, c.abortMergeIfActive(cwd))
	}
	sha, err := c.HeadSHA(cwd)
	if err != nil {
		return MergeResult{}, fmt.Errorf("read merged HEAD: %w", err)
	}
	return MergeResult{Mode: mode, SHA: sha}, nil
}

// HeadSHA returns the full object id of the current HEAD.
func (c *Core) HeadSHA(cwd string) (string, error) {
	stdout, _, err := c.Execute(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(stdout)
	if !treeOIDPattern.MatchString(sha) {
		return "", fmt.Errorf("git rev-parse HEAD returned invalid object id %q", sha)
	}
	return sha, nil
}

func (c *Core) requireCleanWorkingTree(cwd string) error {
	count, err := c.CountWorkingTreeChanges(cwd)
	if err != nil {
		return fmt.Errorf("inspect merge base working tree: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("merge base working tree is dirty (%d changed files)", count)
	}
	return nil
}

func (c *Core) mergeMode(cwd, base, head string) (string, error) {
	result, err := c.run(cwd, "merge-base", "--is-ancestor", base, head)
	if err != nil {
		return "", err
	}
	switch result.exitCode {
	case 0:
		return "ff", nil
	case 1:
		return "merge", nil
	default:
		return "", fmt.Errorf("git merge-base failed: %s", commandOutputMessage(result.stdout, result.stderr))
	}
}

func (c *Core) abortMergeIfActive(cwd string) error {
	result, err := c.run(cwd, "rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	if err != nil || result.exitCode != 0 {
		return err
	}
	if _, _, err := c.Execute(cwd, "merge", "--abort"); err != nil {
		return fmt.Errorf("abort failed merge: %w", err)
	}
	return nil
}
