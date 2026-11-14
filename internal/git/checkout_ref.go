package git

import (
	"fmt"
	"strings"
)

// CheckoutRef switches cwd's checkout to ref. A tag or commit leaves HEAD
// detached there. A branch is checked out as itself even when a tag shares
// its name; one only a remote has becomes a local branch tracking it, named
// explicitly so a same-named tag cannot answer for it.
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
	if local, ok := c.localBranchFromRemote(cwd, branch); ok {
		if err := validateBranchName(local); err != nil {
			return err
		}
		remoteRef := "refs/remotes/" + branch
		if c.branchExists(cwd, local) {
			return c.executeCheckout(cwd, local)
		}
		return c.executeCheckout(cwd, "-b", local, "--track", remoteRef)
	}
	local, err := c.branchExistsChecked(cwd, branch)
	if err != nil {
		return fmt.Errorf("check branch %q: %w", branch, err)
	}
	if local {
		// git prefers refs/heads/<name> over a same-named tag here.
		return c.executeCheckout(cwd, branch)
	}
	remote, err := c.remoteTrackingCandidate(cwd, branch)
	if err != nil {
		return err
	}
	return c.executeCheckout(cwd, "-b", branch, "--track", "refs/remotes/"+remote+"/"+branch)
}

// remoteTrackingCandidate names the remote whose tracking branch a local
// branch of that name should be made from: origin when it has the branch,
// else the one remote that does.
func (c *Core) remoteTrackingCandidate(cwd, branch string) (string, error) {
	var found []string
	for _, remote := range c.listRemoteNames(cwd) {
		exists, err := c.refExists(cwd, "refs/remotes/"+remote+"/"+branch)
		if err != nil {
			return "", fmt.Errorf("check branch %s/%s: %w", remote, branch, err)
		}
		if !exists {
			continue
		}
		if remote == "origin" {
			return remote, nil
		}
		found = append(found, remote)
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("branch %q not found", branch)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("branch %q exists on several remotes (%s); check out one of them by its remote name", branch, strings.Join(found, ", "))
	}
}
