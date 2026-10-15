package git

import (
	"context"

	"agent-overflow/internal/keyedlock"
)

// remoteRefsLocks serializes the commands this process runs that update a
// repository's remote-tracking refs, keyed by its common dir. Two fetches
// of one repository that move the same ref race on it: the loser exits 1
// with "fetching ref refs/remotes/origin/<b> failed: incorrect old value
// provided" even though its transfer succeeded. The background cadence,
// the branch picker, PR reads and user actions all fetch the same
// repositories, so each waits for the one before it. The registry is
// process-wide because every Core in the process works on the same
// repositories.
var remoteRefsLocks = keyedlock.New()

// lockRemoteRefs takes the repository's remote-refs lock when spec updates
// remote-tracking refs and returns its release. A cwd git cannot resolve to
// a common dir has no refs to share; the command itself reports that
// failure.
func (c *Core) lockRemoteRefs(spec commandSpec) (func(), error) {
	if !spec.updatesRemoteRefs && (spec.binary != "git" || !isRemoteRefsCommand(spec.args)) {
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
	return remoteRefsLocks.LockCtx(ctx, commonDir)
}

// isRemoteRefsCommand reports whether args run a git command that writes
// remote-tracking refs (fetch, pull, push), past any leading `-c name=value`
// options.
func isRemoteRefsCommand(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		switch args[i] {
		case "fetch", "pull", "push":
			return true
		}
		return false
	}
	return false
}
