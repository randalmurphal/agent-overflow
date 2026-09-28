package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// syntheticWorktreeTree is a temp-index tree object of the current
// worktree (committed + staged + unstaged + untracked-not-ignored),
// written into a temp object dir with the repo's objects as alternates
// so nothing lands in the user's .git.
type syntheticWorktreeTree struct {
	oid string
	env []string
	// head is the commit HEAD named when the snapshot was taken, or ""
	// when HEAD has no commit yet. The old side of a worktree diff is this
	// OID rather than HEAD, so a commit made while the diff is read cannot
	// change what a re-read returns.
	head    string
	release func() error
}

// IsGitRepository reports whether workspace is inside a (non-bare) git work
// tree. Returns false — never an error — for any scenario in which a diff
// would be invalid: no git binary, not a repo, a bare repo, detached file
// system. Callers use the returned bool to decide whether to skip diffing.
func IsGitRepository(ctx context.Context, workspace string) bool {
	stdout, _, code, err := runGit(ctx, workspace, nil, true, "rev-parse", "--is-inside-work-tree")
	if err != nil || code != 0 {
		return false
	}
	return strings.TrimSpace(stdout) == "true"
}

// headCommit returns the commit HEAD names, or "" on a fresh-init repo
// with no commits yet.
func headCommit(ctx context.Context, workspace string) (string, error) {
	stdout, _, code, err := runGit(ctx, workspace, nil, true, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", nil
	}
	return strings.TrimSpace(stdout), nil
}

// OpenWorktreeDiff opens the patch of everything currently uncommitted in
// the workspace: tracked changes against HEAD plus untracked-not-ignored
// files, in one diff stream. Mirrors `git status` semantics — the caller
// wants to see manual edits alongside agent work. On a fresh-init repo with
// no HEAD the diff runs against the empty tree, so untracked files still
// show. The snapshot lives in a directory under tempRoot until Close.
func OpenWorktreeDiff(ctx context.Context, workspace, tempRoot string, opts Options) (*Diff, error) {
	snapshot, err := captureSyntheticWorktreeTree(ctx, workspace, tempRoot)
	if err != nil {
		return nil, err
	}
	oldSide := snapshot.head
	if oldSide == "" {
		oldSide, err = emptyTreeOID(ctx, workspace, snapshot.env)
		if err != nil {
			return nil, errors.Join(err, snapshot.release())
		}
	}
	return newDiff(workspace, snapshot.env, snapshot.release,
		opts.gitArgs("diff", oldSide, snapshot.oid, "--")), nil
}

// emptyTreeOID writes (into the temp object dir the env points at) and
// returns the empty tree, the old side of a fresh-init workspace diff.
func emptyTreeOID(ctx context.Context, workspace string, env []string) (string, error) {
	stdout, _, _, err := runGitWithStdin(ctx, workspace, env, nil, false, "mktree")
	if err != nil {
		return "", fmt.Errorf("gitdiff: write empty tree: %w", err)
	}
	oid := strings.TrimSpace(stdout)
	if oid == "" {
		return "", errors.New("gitdiff: mktree returned empty oid")
	}
	return oid, nil
}

// OpenBranchBaseDiff opens the patch a PR from HEAD plus the current
// worktree would carry onto baseBranch. It diffs the merge-base of
// baseBranch and HEAD against a synthetic tree of the current worktree so
// committed changes, unstaged/staged changes, and untracked-not-ignored
// files all share one patch stream.
func OpenBranchBaseDiff(ctx context.Context, workspace, tempRoot, baseBranch string, opts Options) (*Diff, error) {
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
	return newDiff(workspace, snapshot.env, snapshot.release,
		opts.gitArgs("diff", mergeBase, snapshot.oid, "--")), nil
}

func captureSyntheticWorktreeTree(ctx context.Context, workspace, tempRoot string) (syntheticWorktreeTree, error) {
	if tempRoot == "" {
		return syntheticWorktreeTree{}, errors.New("gitdiff: snapshot directory is required")
	}
	tempDir, err := os.MkdirTemp(tempRoot, "snapshot-")
	if err != nil {
		return syntheticWorktreeTree{}, fmt.Errorf("gitdiff: create snapshot dir: %w", err)
	}
	release := func() error {
		if err := os.RemoveAll(tempDir); err != nil {
			return fmt.Errorf("gitdiff: remove snapshot dir: %w", err)
		}
		return nil
	}
	fail := func(err error) (syntheticWorktreeTree, error) {
		return syntheticWorktreeTree{}, errors.Join(err, release())
	}

	indexPath := filepath.Join(tempDir, "index")
	objectPath := filepath.Join(tempDir, "objects")
	if err := os.MkdirAll(objectPath, 0o755); err != nil {
		return fail(fmt.Errorf("gitdiff: create temp object dir: %w", err))
	}
	repoObjectPath, err := gitObjectPath(ctx, workspace)
	if err != nil {
		return fail(err)
	}
	env := []string{
		"GIT_INDEX_FILE=" + indexPath,
		"GIT_OBJECT_DIRECTORY=" + objectPath,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + repoObjectPath,
	}
	head, err := headCommit(ctx, workspace)
	if err != nil {
		return fail(fmt.Errorf("gitdiff: probe HEAD: %w", err))
	}
	treeOID, err := writeWorktreeTree(ctx, workspace, env, head)
	if err != nil {
		return fail(err)
	}
	return syntheticWorktreeTree{
		oid:     treeOID,
		env:     env,
		head:    head,
		release: release,
	}, nil
}

func writeWorktreeTree(ctx context.Context, workspace string, env []string, head string) (string, error) {
	if !hasGitIndexFileEnv(env) {
		return "", errors.New("gitdiff: refusing to snapshot without temporary GIT_INDEX_FILE")
	}
	// Seed the temp index from HEAD so the snapshot includes tracked files that
	// exist only on HEAD. Skip on a fresh-init repo where HEAD doesn't resolve.
	if head != "" {
		if _, _, _, err := runGit(ctx, workspace, env, false, "read-tree", head); err != nil {
			return "", fmt.Errorf("gitdiff: read-tree HEAD: %w", err)
		}
	}

	// Stage tracked changes and untracked-not-ignored files without invoking
	// clean/smudge filters from repo config or .gitattributes. These diffs run
	// automatically on review-pane loads, so honoring arbitrary filter
	// commands from an opened repo would be an unwanted execution surface.
	if err := stageWorktreeNoFilters(ctx, workspace, env); err != nil {
		return "", err
	}

	tree, _, _, err := runGit(ctx, workspace, env, false, "write-tree")
	if err != nil {
		return "", fmt.Errorf("gitdiff: write-tree: %w", err)
	}
	treeOID := strings.TrimSpace(tree)
	if treeOID == "" {
		return "", errors.New("gitdiff: write-tree returned empty oid")
	}
	return treeOID, nil
}

func hasGitIndexFileEnv(env []string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_INDEX_FILE=") && strings.TrimPrefix(entry, "GIT_INDEX_FILE=") != "" {
			return true
		}
	}
	return false
}

func gitObjectPath(ctx context.Context, workspace string) (string, error) {
	stdout, _, _, err := runGit(ctx, workspace, nil, false, "rev-parse", "--git-path", "objects")
	if err != nil {
		return "", fmt.Errorf("gitdiff: locate git object dir: %w", err)
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		return "", errors.New("gitdiff: git object dir was empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("gitdiff: resolve git object dir %q: %w", path, err)
	}
	return abs, nil
}

func stageWorktreeNoFilters(ctx context.Context, workspace string, env []string) error {
	changed, _, _, err := runGit(ctx, workspace, env, false,
		"ls-files", "-z", "--modified", "--deleted", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("gitdiff: list worktree changes: %w", err)
	}
	// Partition once: deletions and symlinks need per-path handling; the
	// (overwhelmingly common) regular files batch into ONE hash-object
	// and ONE update-index so a large worktree doesn't fan out O(files)
	// subprocesses.
	seen := map[string]struct{}{}
	var deleted []string // batched `update-index --force-remove --stdin`
	var regular []string // batched `hash-object --stdin-paths`
	var regularModes []string
	var indexInfo strings.Builder // NUL-terminated `<mode> <oid>\t<path>` records
	writeIndexInfo := func(mode, oid, path string) {
		indexInfo.WriteString(mode)
		indexInfo.WriteByte(' ')
		indexInfo.WriteString(oid)
		indexInfo.WriteByte('\t')
		indexInfo.WriteString(path)
		indexInfo.WriteByte(0)
	}
	for _, p := range strings.Split(changed, "\x00") {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		info, statErr := os.Lstat(filepath.Join(workspace, p))
		if errors.Is(statErr, os.ErrNotExist) {
			deleted = append(deleted, p)
			continue
		}
		if statErr != nil {
			return fmt.Errorf("gitdiff: inspect %s: %w", p, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || strings.ContainsRune(p, '\n') {
			// Symlinks hash their target text via stdin, and a path holding
			// a newline can't ride the LF-separated --stdin-paths batch.
			oid, err := hashWorktreePathNoFilters(ctx, workspace, p, info.Mode(), env)
			if err != nil {
				return fmt.Errorf("gitdiff: hash %s: %w", p, err)
			}
			if oid == "" {
				return fmt.Errorf("gitdiff: hash %s returned empty oid", p)
			}
			writeIndexInfo(gitIndexMode(info.Mode()), oid, p)
			continue
		}
		regular = append(regular, p)
		regularModes = append(regularModes, gitIndexMode(info.Mode()))
	}
	if len(regular) > 0 {
		stdin := strings.Join(regular, "\n") + "\n"
		stdout, _, _, err := runGitWithStdin(ctx, workspace, env, []byte(stdin), false,
			"hash-object", "-w", "--no-filters", "--stdin-paths")
		if err != nil {
			return fmt.Errorf("gitdiff: hash worktree files: %w", err)
		}
		oids := strings.Fields(stdout)
		if len(oids) != len(regular) {
			return fmt.Errorf("gitdiff: hash-object returned %d oids for %d paths", len(oids), len(regular))
		}
		for i, p := range regular {
			writeIndexInfo(regularModes[i], oids[i], p)
		}
	}
	if indexInfo.Len() > 0 {
		if _, _, _, err := runGitWithStdin(ctx, workspace, env, []byte(indexInfo.String()), false,
			"update-index", "-z", "--index-info"); err != nil {
			return fmt.Errorf("gitdiff: stage worktree changes: %w", err)
		}
	}
	if len(deleted) > 0 {
		stdin := strings.Join(deleted, "\x00") + "\x00"
		if _, _, _, err := runGitWithStdin(ctx, workspace, env, []byte(stdin), false,
			"update-index", "-z", "--force-remove", "--stdin"); err != nil {
			return fmt.Errorf("gitdiff: stage deletions: %w", err)
		}
	}
	return nil
}

func hashWorktreePathNoFilters(ctx context.Context, workspace, p string, mode os.FileMode, env []string) (string, error) {
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(filepath.Join(workspace, p))
		if err != nil {
			return "", err
		}
		stdout, _, _, err := runGitWithStdin(ctx, workspace, env, []byte(target), false,
			"hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(stdout), nil
	}
	oid, _, _, err := runGit(ctx, workspace, env, false,
		"hash-object", "--no-filters", "-w", "--", p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(oid), nil
}

func gitIndexMode(mode os.FileMode) string {
	if mode&os.ModeSymlink != 0 {
		return "120000"
	}
	if mode&0o111 != 0 {
		return "100755"
	}
	return "100644"
}
