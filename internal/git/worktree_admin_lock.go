package git

import (
	"context"
	"fmt"

	"agent-overflow/internal/keyedlock"
)

// worktreeAdminLocks serializes the `git worktree` commands this process runs
// against one repository, keyed by its common dir. Adding, listing or
// removing a worktree reads every linked worktree's admin directory
// (.git/worktrees/<id>), and git 2.43 dies on one that a concurrent
// `git worktree add` has created but not yet filled ("fatal: failed to read
// .../commondir: Success"). Fan-out units provisioned at once in one
// repository hit that. The registry is process-wide because every Core in the
// process works on the same repositories.
var worktreeAdminLocks = keyedlock.New()

// worktreeCheckoutLocks serializes the worktree attaches and creates of one
// repository, keyed by its common dir, across the whole operation: the
// branch checks, the branch creation, the add and the undo of a failed
// add. The admin lock covers one git command, so without this one two
// attaches of one origin branch would both see no local branch and the
// slower would fail creating it instead of at git's one-checkout rule.
// Which registration an undo removes does not depend on it: that is the
// one locked with the attempt's own reason (checkoutWorktree), which also
// holds against another process. Process-wide like worktreeAdminLocks:
// the app builds more than one Core.
var worktreeCheckoutLocks = keyedlock.New()

// lockWorktreeCheckout takes the checkout lock of cwd's repository and
// returns its release. Every holder runs git commands bounded by the
// Core's timeout, so the wait is bounded too.
func (c *Core) lockWorktreeCheckout(cwd string) (func(), error) {
	commonDir, err := c.CommonDir(cwd)
	if err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	return worktreeCheckoutLocks.Lock(commonDir), nil
}

// lockWorktreeAdmin takes the repository's worktree admin lock when spec is a
// `git worktree` command and returns its release. A cwd git cannot resolve to
// a common dir has no admin directory to share; the command itself reports
// that failure.
func (c *Core) lockWorktreeAdmin(spec commandSpec) (func(), error) {
	if spec.binary != "git" || !isWorktreeCommand(spec.args) {
		return func() {}, nil
	}
	commonDir, err := c.CommonDir(spec.cwd)
	if err != nil {
		return func() {}, nil
	}
	ctx := spec.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return worktreeAdminLocks.LockCtx(ctx, commonDir)
}

// isWorktreeCommand reports whether args run `git worktree`, past any leading
// `-c name=value` options.
func isWorktreeCommand(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i] == "worktree"
	}
	return false
}
