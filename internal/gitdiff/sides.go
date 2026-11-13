package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Sides are the two trees a review diff compares: old and new are commit or
// tree object IDs, resolved once, so the patch and every blob read from the
// same Sides describe one state. A worktree side is a snapshot whose
// temporary objects live until Close.
type Sides struct {
	workspace string
	env       []string
	old       string
	new       string
	// worktree marks a new side that is the working tree: new names a
	// snapshot tree in a temporary object directory (env), or nothing for
	// Sides reopened from a Pin.
	worktree bool
	// paths limits the diff to these literal paths; nil is every path.
	paths   []string
	release func() error
}

// Close removes a worktree snapshot's temporary objects. Closing Sides that
// handed its snapshot to a Diff (OpenDiff) is a no-op.
func (s *Sides) Close() error {
	release := s.release
	s.release = nil
	if release == nil {
		return nil
	}
	return release()
}

// OpenDiff returns the patch between the sides. The diff takes over the
// snapshot: it is removed when the diff is closed, and Close on s then does
// nothing.
func (s *Sides) OpenDiff(opts Options) *Diff {
	args := opts.gitArgs("diff", s.old, s.new, "--")
	if s.paths != nil {
		args = append(append([]string{"--literal-pathspecs"}, args...), s.paths...)
	}
	diff := newDiff(s.workspace, s.env, s.release, args)
	s.release = nil
	return diff
}

// WorktreeSides compares HEAD (the empty tree before the first commit) with
// a snapshot of everything uncommitted: the sides of OpenWorktreeDiff.
func WorktreeSides(ctx context.Context, workspace, tempRoot string) (*Sides, error) {
	snapshot, err := CaptureWorktree(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	return &Sides{workspace: workspace, env: snapshot.tree.env, old: snapshot.oldSide, new: snapshot.tree.oid, worktree: true, release: snapshot.Close}, nil
}

// WorktreeFilesSides is WorktreeSides limited to the given literal paths:
// the sides of OpenWorktreeFilesDiff. Pass both paths of a rename.
func WorktreeFilesSides(ctx context.Context, workspace, tempRoot string, paths []string) (*Sides, error) {
	if err := validatePaths(paths); err != nil {
		return nil, err
	}
	sides, err := WorktreeSides(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	sides.paths = paths
	return sides, nil
}

// BranchBaseSides compares the merge base of baseBranch and HEAD with a
// snapshot of the worktree: the sides of OpenBranchBaseDiff.
func BranchBaseSides(ctx context.Context, workspace, tempRoot, baseBranch string) (*Sides, error) {
	base, err := resolveBaseRef(ctx, workspace, baseBranch)
	if err != nil {
		return nil, err
	}
	snapshot, err := captureSyntheticWorktreeTree(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	if snapshot.head == "" {
		return nil, errors.Join(
			fmt.Errorf("gitdiff: HEAD has no commit to measure against %s", base),
			snapshot.release())
	}
	mergeBase, err := mergeBaseOID(ctx, workspace, base, snapshot.head)
	if err != nil {
		return nil, errors.Join(err, snapshot.release())
	}
	return &Sides{workspace: workspace, env: snapshot.env, old: mergeBase, new: snapshot.oid, worktree: true, release: snapshot.release}, nil
}

// MergeBaseSides compares the merge base of base and head with head, the
// sides a pull request from head onto base shows. base is a revision (a
// remote-tracking branch or a commit); head must be a commit OID. Both
// must already exist locally.
func MergeBaseSides(ctx context.Context, workspace, base, head string) (*Sides, error) {
	base = strings.TrimSpace(base)
	if err := validateRefArg(base, "base ref"); err != nil {
		return nil, err
	}
	headOID, err := resolveCommit(ctx, workspace, head, "head")
	if err != nil {
		return nil, err
	}
	mergeBase, err := mergeBaseOID(ctx, workspace, base, headOID)
	if err != nil {
		return nil, err
	}
	return &Sides{workspace: workspace, old: mergeBase, new: headOID}, nil
}

// CommitSides compares a commit with its first parent, or with the empty
// tree for a root commit: what the commit introduced, as GitHub and GitLab
// render a merge commit's changes. The SHA must be hex; it comes from a
// commit list this package produced or a forge API response.
func CommitSides(ctx context.Context, workspace, sha string) (*Sides, error) {
	return commitRunSides(ctx, workspace, sha, sha)
}

// CommitRangeSides compares the first parent of first with last, where
// first and last bound a contiguous run of the commits a pull request
// carries: `base..head` in the order ListCommitsRange lists them (newest
// first), within the commits that list shows. first is the older end; a
// run of one commit is CommitSides. A run of more than one commit that
// covers the whole list is refused: it is the pull request's whole diff,
// which MergeBaseSides reads.
func CommitRangeSides(ctx context.Context, workspace, base, head, first, last string) (*Sides, error) {
	for _, arg := range []struct{ value, label string }{{base, "base"}, {head, "head"}, {first, "first commit"}, {last, "last commit"}} {
		if !commitSHAPattern.MatchString(arg.value) {
			return nil, fmt.Errorf("gitdiff: invalid %s %q", arg.label, arg.value)
		}
	}
	// One past the cap tells a list the selector shows whole from one it
	// cut short.
	commits, err := logRev(ctx, workspace, maxListedCommits+1, base+".."+head)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: log %s..%s: %w", base, head, err)
	}
	truncated := len(commits) > maxListedCommits
	if truncated {
		commits = commits[:maxListedCommits]
	}
	index := func(sha string) int {
		return slices.IndexFunc(commits, func(c Commit) bool { return c.SHA == sha })
	}
	firstAt, lastAt := index(first), index(last)
	switch {
	case firstAt < 0:
		return nil, fmt.Errorf("gitdiff: commit %s is not one of the pull request's commits", first)
	case lastAt < 0:
		return nil, fmt.Errorf("gitdiff: commit %s is not one of the pull request's commits", last)
	case lastAt > firstAt:
		return nil, fmt.Errorf("gitdiff: commit range %s..%s is out of order: %s is newer than %s", first, last, first, last)
	case firstAt != lastAt && lastAt == 0 && firstAt == len(commits)-1 && !truncated:
		return nil, errors.New("gitdiff: the commit range covers every commit of the pull request; read the whole pull request instead")
	}
	return commitRunSides(ctx, workspace, first, last)
}

// commitRunSides compares first's first parent (the empty tree for a root
// commit) with last.
func commitRunSides(ctx context.Context, workspace, first, last string) (*Sides, error) {
	first = strings.TrimSpace(first)
	if !commitSHAPattern.MatchString(first) {
		return nil, fmt.Errorf("gitdiff: invalid commit sha %q", first)
	}
	lastOID, err := resolveCommit(ctx, workspace, last, "commit sha")
	if err != nil {
		return nil, err
	}
	parents, _, _, err := runGit(ctx, workspace, nil, false, "rev-list", "--parents", "-n", "1", first, "--")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: resolve parents of %s: %w", first, err)
	}
	hashes := strings.Fields(parents)
	if len(hashes) == 0 {
		return nil, fmt.Errorf("gitdiff: rev-list returned nothing for %s", first)
	}
	old := ""
	if len(hashes) > 1 {
		old = hashes[1]
	} else if old, err = emptyTree(ctx, workspace); err != nil {
		return nil, err
	}
	return &Sides{workspace: workspace, old: old, new: lastOID}, nil
}

// resolveCommit resolves a hex commit SHA to its full object ID.
func resolveCommit(ctx context.Context, workspace, sha, label string) (string, error) {
	sha = strings.TrimSpace(sha)
	if !commitSHAPattern.MatchString(sha) {
		return "", fmt.Errorf("gitdiff: invalid %s %q", label, sha)
	}
	oid, _, _, err := runGit(ctx, workspace, nil, false, "rev-parse", "--verify", "--end-of-options", sha+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("gitdiff: resolve %s %s: %w", label, sha, err)
	}
	return strings.TrimSpace(oid), nil
}

// emptyTree returns the empty tree's object ID in the repository's hash
// without writing it: git knows the empty tree whether or not the object
// store holds it.
func emptyTree(ctx context.Context, workspace string) (string, error) {
	stdout, _, _, err := runGitWithStdin(ctx, workspace, nil, nil, false, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", fmt.Errorf("gitdiff: empty tree: %w", err)
	}
	return strings.TrimSpace(stdout), nil
}
