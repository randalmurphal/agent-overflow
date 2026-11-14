package git

import (
	"errors"
	"fmt"
	"strings"
)

// CheckoutRef switches cwd's checkout to ref. A tag or commit leaves HEAD
// detached there. A branch is checked out as itself even when a tag shares
// its name; one only a remote has (checkoutSource) becomes a local branch
// tracking it, named explicitly so a same-named tag cannot answer for it.
// A name no branch has that reads "<remote>/<name>" is that remote's branch.
func (c *Core) CheckoutRef(cwd string, ref Ref) error {
	if err := ref.validate(); err != nil {
		return err
	}
	switch ref.Kind {
	case RefHead:
		return fmt.Errorf("git checkout target is required")
	case RefTag, RefCommit:
		return c.executeCheckout(cwd, "--detach", ref.revision())
	}
	branch := ref.Name
	local, remote, err := c.checkoutSource(cwd, branch)
	var notFound branchNotFoundError
	switch {
	case err == nil && local:
		// git prefers refs/heads/<name> over a same-named tag here.
		return c.executeCheckout(cwd, branch)
	case err == nil:
		return c.executeCheckout(cwd, "-b", branch, "--track", "refs/remotes/"+remote+"/"+branch)
	case !errors.As(err, &notFound):
		return err
	}
	// No branch has this exact name. "<remote>/<name>" then names that
	// remote's branch: its local branch, made tracking it when missing.
	short, ok := c.localBranchFromRemote(cwd, branch)
	if !ok {
		return err
	}
	if err := validateBranchName(short); err != nil {
		return err
	}
	exists, err := c.branchExistsChecked(cwd, short)
	if err != nil {
		return fmt.Errorf("check branch %q: %w", short, err)
	}
	if exists {
		return c.executeCheckout(cwd, short)
	}
	return c.executeCheckout(cwd, "-b", short, "--track", "refs/remotes/"+branch)
}

// checkoutSource is the one rule for where a checkout of branch comes from,
// as the refs stand: local when the branch exists locally, else remote, the
// remote whose tracking branch a new local branch of that name tracks:
// origin when origin has it, else the one remote that does. A branch no one
// has, or only several remotes other than origin have, is an error.
func (c *Core) checkoutSource(cwd, branch string) (local bool, remote string, err error) {
	local, err = c.branchExistsChecked(cwd, branch)
	if err != nil {
		return false, "", fmt.Errorf("check branch %q: %w", branch, err)
	}
	if local {
		return true, "", nil
	}
	var found []string
	for _, remote := range c.listRemoteNames(cwd) {
		exists, err := c.refExists(cwd, "refs/remotes/"+remote+"/"+branch)
		if err != nil {
			return false, "", fmt.Errorf("check branch %s/%s: %w", remote, branch, err)
		}
		if !exists {
			continue
		}
		if remote == "origin" {
			return false, remote, nil
		}
		found = append(found, remote)
	}
	switch len(found) {
	case 0:
		return false, "", branchNotFoundError{branch: branch}
	case 1:
		return false, found[0], nil
	default:
		return false, "", fmt.Errorf("branch %q exists on several remotes (%s) and not on origin; make a local branch of it from the one you mean first", branch, strings.Join(found, ", "))
	}
}

// branchNotFoundError is checkoutSource's answer for a branch no local
// branch or remote has.
type branchNotFoundError struct{ branch string }

func (e branchNotFoundError) Error() string { return fmt.Sprintf("branch %q not found", e.branch) }
