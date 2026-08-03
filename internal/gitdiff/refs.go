package gitdiff

import (
	"context"
	"fmt"
	"strings"
)

// resolveBaseRef resolves the BASE side of a comparison — the branch a
// diff or commit list is measured against — to a revision git commands
// can use.
//
// The base is resolved to its remote-tracking ref whenever one exists,
// because that is the ref a PR would actually be opened against: a local
// `main` is only as fresh as the user's last fetch, and measuring a
// branch against a stale one under-reports (a commit already on the real
// base still shows as "yours") or over-reports (the base moved and the
// merge base with it). Preferring the remote-tracking ref makes the
// review pane agree with what GitHub/GitLab will show for the same
// branch.
//
// It changes nothing when there is no remote-tracking ref — a
// worktree-local branch, an offline clone, a repo with no remote — and it
// changes nothing about the merge base when the local branch is simply
// behind: the merge base of `origin/main` and a branch cut from a stale
// local `main` IS that stale local tip. The difference only appears where
// the two genuinely diverge, and there the remote is the honest answer.
//
// Resolution order:
//
//  1. The base branch's configured upstream (`<base>@{upstream}`), so a
//     fork whose `main` tracks `upstream/main` measures against upstream
//     rather than against the fork's own copy. Rejected when it does not
//     name a remote-tracking ref — `branch.<name>.remote = .` makes the
//     upstream another LOCAL branch, which is not evidence of anything.
//  2. `origin/<base>`, the un-configured common case.
//  3. Whatever resolveNamedRef finds: the local ref as-is, else the same
//     remote-DWIM the picker's short names need.
//
// Deliberately NOT probing every remote for a base that already exists
// locally: a second remote's copy of a branch the base does not track is
// a coincidence of naming, not an upstream.
func resolveBaseRef(ctx context.Context, workspace, base string) (string, error) {
	base = strings.TrimSpace(base)
	if err := validateRefArg(base, "base ref"); err != nil {
		return "", err
	}
	if tracking, ok := remoteTrackingRef(ctx, workspace, base); ok {
		return tracking, nil
	}
	return resolveNamedRef(ctx, workspace, base)
}

// resolveNamedRef resolves a ref the caller is DESCRIBING rather than
// measuring against — the branch whose own commits are being listed. It
// prefers the local ref and only falls back to a remote's copy when
// nothing local matches, which is the opposite bias from resolveBaseRef
// and the only correct one here: "which commits would deleting this
// branch lose" must count the commits sitting in the local branch,
// including the ones that were never pushed.
//
// The remote fallback exists because the branch picker projects
// remote-only branches to their short name ("feature" for
// "origin/feature") and git's revision resolution does not apply
// checkout's remote DWIM — `git log feature..HEAD` fails with "bad
// revision" when only refs/remotes/origin/feature exists. Remotes are
// probed in `git remote` order.
func resolveNamedRef(ctx context.Context, workspace, name string) (string, error) {
	name = strings.TrimSpace(name)
	if err := validateRefArg(name, "branch ref"); err != nil {
		return "", err
	}
	if revisionExists(ctx, workspace, name) {
		return name, nil
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
			candidate := remote + "/" + name
			if revisionExists(ctx, workspace, "refs/remotes/"+candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("gitdiff: branch %q not found locally or on any remote — fetch it first", name)
}

// remoteTrackingRef returns the remote-tracking ref for branch — its
// configured upstream, else origin's copy — in the `<remote>/<branch>`
// short form git commands accept. The bool is false when neither exists.
func remoteTrackingRef(ctx context.Context, workspace, branch string) (string, bool) {
	if upstream, ok := configuredUpstream(ctx, workspace, branch); ok {
		return upstream, true
	}
	candidate := "origin/" + branch
	if revisionExists(ctx, workspace, "refs/remotes/"+candidate) {
		return candidate, true
	}
	return "", false
}

// configuredUpstream returns branch's `@{upstream}` when it is set AND
// names a remote-tracking ref. The second half is the load-bearing one: a
// branch configured with `branch.<name>.remote = .` has a LOCAL branch as
// its upstream, and treating that as a remote-tracking ref would silently
// re-point a diff base at another local branch.
func configuredUpstream(ctx context.Context, workspace, branch string) (string, bool) {
	stdout, _, code, err := runGit(ctx, workspace, nil, true,
		"rev-parse", "--verify", "--quiet", "--abbrev-ref", branch+"@{upstream}")
	if err != nil || code != 0 {
		return "", false
	}
	upstream := strings.TrimSpace(stdout)
	if upstream == "" || upstream == branch {
		return "", false
	}
	if !revisionExists(ctx, workspace, "refs/remotes/"+upstream) {
		return "", false
	}
	return upstream, true
}

// validateRefArg rejects the ref names that must never reach argv: empty
// (nothing to resolve) and leading-`-` (parses as a flag). label names the
// role the ref plays in the caller's error message.
func validateRefArg(ref, label string) error {
	if ref == "" {
		return fmt.Errorf("gitdiff: %s is required", label)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("gitdiff: invalid %s %q", label, ref)
	}
	return nil
}

// revisionExists reports whether rev resolves to a commit in the
// workspace's repository. rev is caller-controlled (flag-guarded base
// names or fully-qualified refs), never raw user input.
func revisionExists(ctx context.Context, workspace, rev string) bool {
	_, _, code, err := runGit(ctx, workspace, nil, true,
		"rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil && code == 0
}

// RevisionsExist reports whether every rev is non-empty, flag-safe,
// and resolves to a commit in the workspace. Callers use it to skip a
// network fetch when the objects they need are already local.
func RevisionsExist(ctx context.Context, workspace string, revs ...string) bool {
	for _, rev := range revs {
		rev = strings.TrimSpace(rev)
		if rev == "" || strings.HasPrefix(rev, "-") || !revisionExists(ctx, workspace, rev) {
			return false
		}
	}
	return true
}
