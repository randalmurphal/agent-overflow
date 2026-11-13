package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/procutil"
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
	release   func() error
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
	diff := newDiff(s.workspace, s.env, s.release, opts.gitArgs("diff", s.old, s.new, "--"))
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
	return &Sides{workspace: workspace, env: snapshot.tree.env, old: snapshot.oldSide, new: snapshot.tree.oid, release: snapshot.Close}, nil
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
	return &Sides{workspace: workspace, env: snapshot.env, old: mergeBase, new: snapshot.oid, release: snapshot.release}, nil
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

// Blob is one side of a changed path. Exists is false where the path is
// absent on that side (an added file's old side, a deleted file's new side).
type Blob struct {
	Exists bool
	OID    string
	// Mode is git's octal mode: 100644 or 100755 for a file, 120000 for a
	// symlink, 160000 for a submodule.
	Mode string
	Size int64
}

// IsFile reports whether the side is a regular file's content.
func (b Blob) IsFile() bool {
	return b.Exists && (b.Mode == "100644" || b.Mode == "100755")
}

// ChangedPath is one path the diff between the sides changes, each side on
// its own: a rename is its old path deleted and its new path added.
type ChangedPath struct {
	Old Blob
	New Blob
}

// MaxChangedPaths and MaxChangedPathBytes bound one ChangedPaths call: the
// paths ride git's argument list.
const (
	MaxChangedPaths     = 200
	MaxChangedPathBytes = 64 << 10
)

// ChangedPaths answers, for each of paths the diff between the sides
// changes, its blob on each side with the blob's size. A path the diff does
// not change is absent from the answer. Paths match literally, never as
// patterns.
func (s *Sides) ChangedPaths(ctx context.Context, paths []string) (map[string]ChangedPath, error) {
	if len(paths) > MaxChangedPaths {
		return nil, fmt.Errorf("gitdiff: %d paths exceeds %d", len(paths), MaxChangedPaths)
	}
	total := 0
	for _, p := range paths {
		if p == "" || strings.ContainsRune(p, 0) {
			return nil, fmt.Errorf("gitdiff: invalid path %q", p)
		}
		total += len(p)
	}
	if total > MaxChangedPathBytes {
		return nil, fmt.Errorf("gitdiff: paths exceed %d bytes", MaxChangedPathBytes)
	}
	changed := map[string]ChangedPath{}
	if len(paths) == 0 {
		return changed, nil
	}
	args := append([]string{"--literal-pathspecs", "diff", "-z", "--raw", "--no-abbrev", "--no-renames",
		"--no-color", "--no-ext-diff", "--no-textconv", "--no-relative", s.old, s.new, "--"}, paths...)
	stdout, _, _, err := runGit(ctx, s.workspace, s.env, false, args...)
	if err != nil {
		return nil, fmt.Errorf("gitdiff: list changed paths: %w", err)
	}
	tokens := strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00")
	var oids []string
	for i := 0; i+1 < len(tokens); i += 2 {
		fields := strings.Fields(tokens[i])
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
			return nil, fmt.Errorf("gitdiff: malformed raw record %q", tokens[i])
		}
		side := func(mode, oid string) Blob {
			if strings.Trim(oid, "0") == "" {
				return Blob{}
			}
			oids = append(oids, oid)
			return Blob{Exists: true, OID: oid, Mode: mode}
		}
		changed[tokens[i+1]] = ChangedPath{Old: side(fields[0][1:], fields[2]), New: side(fields[1], fields[3])}
	}
	if len(oids) == 0 {
		return changed, nil
	}
	sizes, err := s.blobSizes(ctx, oids)
	if err != nil {
		return nil, err
	}
	for path, entry := range changed {
		entry.Old.Size = sizes[entry.Old.OID]
		entry.New.Size = sizes[entry.New.OID]
		changed[path] = entry
	}
	return changed, nil
}

// blobSizes reads the size of each object in one `git cat-file
// --batch-check`.
func (s *Sides) blobSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	input := strings.Join(oids, "\n") + "\n"
	stdout, _, _, err := runGitWithStdin(ctx, s.workspace, s.env, []byte(input), false,
		"cat-file", "--batch-check=%(objectname) %(objectsize)")
	if err != nil {
		return nil, fmt.Errorf("gitdiff: read blob sizes: %w", err)
	}
	sizes := make(map[string]int64, len(oids))
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		oid, size, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("gitdiff: malformed batch-check line %q", line)
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("gitdiff: object %s: %q", oid, line)
		}
		sizes[oid] = n
	}
	return sizes, nil
}

// ReadBlob returns the content of a blob ChangedPaths reported, refusing
// one larger than maxBytes before reading it.
func (s *Sides) ReadBlob(ctx context.Context, blob Blob, maxBytes int64) ([]byte, error) {
	if !blob.Exists || !commitSHAPattern.MatchString(blob.OID) {
		return nil, fmt.Errorf("gitdiff: no blob to read")
	}
	if blob.Size > maxBytes {
		return nil, fmt.Errorf("gitdiff: blob %s is %d bytes, over %d", blob.OID, blob.Size, maxBytes)
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", blob.OID)
	cmd.Dir = s.workspace
	cmd.Env = gitEnv(s.env)
	out := &boundedBuffer{limit: int(maxBytes) + 1}
	errOut := &boundedBuffer{limit: maxStreamStderrBytes}
	runErr := procutil.RunDrained(ctx, cmd, out, errOut, gitPipeWaitDelay)
	if _, err := gitResult(ctx, runErr, false, cmd.Args[1:], errOut.String()); err != nil {
		return nil, fmt.Errorf("gitdiff: read blob %s: %w", blob.OID, err)
	}
	if int64(len(out.buf)) != blob.Size {
		return nil, fmt.Errorf("gitdiff: blob %s read %d bytes, expected %d", blob.OID, len(out.buf), blob.Size)
	}
	return bytes.Clone(out.buf), nil
}
