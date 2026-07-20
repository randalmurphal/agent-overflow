package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// resolveBaseRef resolves the branch picker's short branch name to a
// revision git commands can use. The picker projects remote-only
// branches to their short name ("feature" for "origin/feature"), and
// git's revision resolution does not apply checkout's remote DWIM —
// `git log feature..HEAD` fails with "bad revision" when only
// refs/remotes/origin/feature exists. Local names (branches, tags,
// SHAs) resolve as-is; otherwise each remote's tracking ref is probed
// in `git remote` order.
func resolveBaseRef(ctx context.Context, workspace, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("gitdiff: base ref is required")
	}
	if strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("gitdiff: invalid base ref %q", base)
	}
	if revisionExists(ctx, workspace, base) {
		return base, nil
	}
	remotes, _, code, err := runGit(ctx, workspace, nil, true, "remote")
	if err != nil {
		return "", fmt.Errorf("gitdiff: list remotes: %w", err)
	}
	if code == 0 {
		for _, remote := range strings.Split(remotes, "\n") {
			remote = strings.TrimSpace(remote)
			if remote == "" {
				continue
			}
			candidate := remote + "/" + base
			if revisionExists(ctx, workspace, "refs/remotes/"+candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("gitdiff: branch %q not found locally or on any remote — fetch it first", base)
}

// revisionExists reports whether rev resolves to a commit in the
// workspace's repository. rev is caller-controlled (flag-guarded base
// names or fully-qualified refs), never raw user input.
func revisionExists(ctx context.Context, workspace, rev string) bool {
	_, _, code, err := runGit(ctx, workspace, nil, true,
		"rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil && code == 0
}
