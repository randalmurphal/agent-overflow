package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/diffsummary"
)

// Store shells out to `git` to capture, diff, and restore workspace snapshots.
// It is safe for concurrent use across goroutines: each capture uses a unique
// temp index file so GIT_INDEX_FILE paths never collide even when multiple
// threads snapshot the same workspace at the same time.
type Store struct{}

// NewStore returns a Store ready for use. The zero-value Store is also valid;
// this constructor exists so callers can mirror the rest of the package layout.
func NewStore() *Store { return &Store{} }

type syntheticWorktreeTree struct {
	oid     string
	env     []string
	cleanup func()
}

// Author metadata stamped on every checkpoint commit.
const (
	authorName  = "Agent Overflow"
	authorEmail = "agent-overflow@users.noreply.github.com"
)

var maxDiffOutputBytes int64 = 10 * 1024 * 1024

var errGitOutputTooLarge = errors.New("git output exceeded limit")

var gitPipeWaitDelay = time.Second

// CaptureBaseline snapshots the current worktree at (threadID, turnIndex). It
// returns the ref name that was written so callers can persist it alongside
// the checkpoint row.
//
// The user's index is NOT touched: we create a temp GIT_INDEX_FILE and operate
// there. The final `git update-ref` is the only operation that mutates repo
// state, and it only writes to our hidden ref namespace.
//
// Captures both tracked-with-changes and untracked-but-not-ignored files.
func (s *Store) CaptureBaseline(
	ctx context.Context,
	workspace string,
	threadID string,
	turnIndex int,
) (string, error) {
	ref := RefForThreadTurn(threadID, turnIndex)
	if err := s.captureToRef(ctx, workspace, ref); err != nil {
		return "", err
	}
	return ref, nil
}

// CaptureRef is like CaptureBaseline but takes a pre-built ref name. Used when
// the caller wants explicit control over ref naming (tests and internal glue).
func (s *Store) CaptureRef(ctx context.Context, workspace, ref string) error {
	return s.captureToRef(ctx, workspace, ref)
}

// CopyRef points destRef at the same commit as sourceRef. The copied ref is a
// new ownership handle for the same immutable snapshot.
func (s *Store) CopyRef(ctx context.Context, workspace, sourceRef, destRef string) error {
	oid, err := s.resolveRefCommit(ctx, workspace, sourceRef)
	if err != nil {
		return err
	}
	if oid == "" {
		return fmt.Errorf("checkpoint: source ref %q is unavailable", sourceRef)
	}
	if _, _, _, err := runGit(ctx, workspace, nil, false, "update-ref", destRef, oid); err != nil {
		return fmt.Errorf("checkpoint: copy ref %s -> %s: %w", sourceRef, destRef, err)
	}
	return nil
}

func (s *Store) captureToRef(ctx context.Context, workspace, ref string) error {
	tempDir, err := os.MkdirTemp("", "agent-overflow-checkpoint-")
	if err != nil {
		return fmt.Errorf("checkpoint: create temp index dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	indexPath := filepath.Join(tempDir, "index")
	env := []string{
		"GIT_INDEX_FILE=" + indexPath,
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName,
		"GIT_COMMITTER_EMAIL=" + authorEmail,
	}

	treeOID, err := s.writeWorktreeTree(ctx, workspace, env, nil)
	if err != nil {
		return err
	}

	msg := "agent-overflow checkpoint ref=" + ref
	commit, _, _, err := runGit(ctx, workspace, env, false, "commit-tree", treeOID, "-m", msg)
	if err != nil {
		return fmt.Errorf("checkpoint: commit-tree: %w", err)
	}
	commitOID := strings.TrimSpace(commit)
	if commitOID == "" {
		return errors.New("checkpoint: commit-tree returned empty oid")
	}

	if _, _, _, err := runGit(ctx, workspace, nil, false, "update-ref", ref, commitOID); err != nil {
		return fmt.Errorf("checkpoint: update-ref %s: %w", ref, err)
	}
	return nil
}

func (s *Store) captureSyntheticWorktreeTree(ctx context.Context, workspace string, paths []string) (syntheticWorktreeTree, error) {
	tempDir, err := os.MkdirTemp("", "agent-overflow-checkpoint-")
	if err != nil {
		return syntheticWorktreeTree{}, fmt.Errorf("checkpoint: create temp index dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	indexPath := filepath.Join(tempDir, "index")
	objectPath := filepath.Join(tempDir, "objects")
	if err := os.MkdirAll(objectPath, 0o755); err != nil {
		cleanup()
		return syntheticWorktreeTree{}, fmt.Errorf("checkpoint: create temp object dir: %w", err)
	}
	repoObjectPath, err := gitObjectPath(ctx, workspace)
	if err != nil {
		cleanup()
		return syntheticWorktreeTree{}, err
	}
	env := []string{
		"GIT_INDEX_FILE=" + indexPath,
		"GIT_OBJECT_DIRECTORY=" + objectPath,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + repoObjectPath,
	}
	treeOID, err := s.writeWorktreeTree(ctx, workspace, env, paths)
	if err != nil {
		cleanup()
		return syntheticWorktreeTree{}, err
	}
	return syntheticWorktreeTree{
		oid:     treeOID,
		env:     env,
		cleanup: cleanup,
	}, nil
}

func (s *Store) writeWorktreeTree(ctx context.Context, workspace string, env []string, paths []string) (string, error) {
	if !hasGitIndexFileEnv(env) {
		return "", errors.New("checkpoint: refusing to snapshot without temporary GIT_INDEX_FILE")
	}
	// Seed the temp index from HEAD so the snapshot includes tracked files that
	// exist only on HEAD. Skip on a fresh-init repo where HEAD doesn't resolve.
	hasHead, err := s.HasHeadCommit(ctx, workspace)
	if err != nil {
		return "", fmt.Errorf("checkpoint: probe HEAD: %w", err)
	}
	if hasHead {
		if _, _, _, err := runGit(ctx, workspace, env, false, "read-tree", "HEAD"); err != nil {
			return "", fmt.Errorf("checkpoint: read-tree HEAD: %w", err)
		}
	}

	// Stage tracked changes and untracked-not-ignored files without invoking
	// clean/smudge filters from repo config or .gitattributes. Checkpoint
	// capture runs automatically on user send, so honoring arbitrary filter
	// commands from an opened repo would be an unwanted execution surface.
	if err := stageWorktreeNoFilters(ctx, workspace, env, paths); err != nil {
		return "", err
	}

	tree, _, _, err := runGit(ctx, workspace, env, false, "write-tree")
	if err != nil {
		return "", fmt.Errorf("checkpoint: write-tree: %w", err)
	}
	treeOID := strings.TrimSpace(tree)
	if treeOID == "" {
		return "", errors.New("checkpoint: write-tree returned empty oid")
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
		return "", fmt.Errorf("checkpoint: locate git object dir: %w", err)
	}
	path := strings.TrimSpace(stdout)
	if path == "" {
		return "", errors.New("checkpoint: git object dir was empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("checkpoint: resolve git object dir %q: %w", path, err)
	}
	return abs, nil
}

func stageWorktreeNoFilters(ctx context.Context, workspace string, env []string, paths []string) error {
	args := []string{"ls-files", "-z", "--modified", "--deleted", "--others", "--exclude-standard"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	changed, _, _, err := runGit(ctx, workspace, env, false, args...)
	if err != nil {
		return fmt.Errorf("checkpoint: list worktree changes: %w", err)
	}
	seen := map[string]struct{}{}
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
			if _, _, _, err := runGit(ctx, workspace, env, true,
				"update-index", "--force-remove", "--", p); err != nil {
				return fmt.Errorf("checkpoint: stage deletion %s: %w", p, err)
			}
			continue
		}
		if statErr != nil {
			return fmt.Errorf("checkpoint: inspect %s: %w", p, statErr)
		}
		mode := gitIndexMode(info.Mode())
		oid, err := hashWorktreePathNoFilters(ctx, workspace, p, info.Mode(), env)
		if err != nil {
			return fmt.Errorf("checkpoint: hash %s: %w", p, err)
		}
		if oid == "" {
			return fmt.Errorf("checkpoint: hash %s returned empty oid", p)
		}
		if _, _, _, err := runGit(ctx, workspace, env, false,
			"update-index", "--add", "--cacheinfo", mode, oid, p); err != nil {
			return fmt.Errorf("checkpoint: stage %s: %w", p, err)
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

// DiffRefToRef returns the unified patch between two checkpoint refs.
func (s *Store) DiffRefToRef(ctx context.Context, workspace, fromRef, toRef string) ([]byte, error) {
	from, err := s.resolveRefCommit(ctx, workspace, fromRef)
	if err != nil {
		return nil, err
	}
	to, err := s.resolveRefCommit(ctx, workspace, toRef)
	if err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, fmt.Errorf("checkpoint: diff refs unavailable: from=%q to=%q", fromRef, toRef)
	}
	stdout, _, _, err := runGitWithStdoutLimit(ctx, workspace, nil, false, maxDiffOutputBytes,
		"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv", from, to)
	if errors.Is(err, errGitOutputTooLarge) {
		return nil, fmt.Errorf("checkpoint: diff %s..%s exceeds %d byte limit", fromRef, toRef, maxDiffOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff %s..%s: %w", fromRef, toRef, err)
	}
	return []byte(stdout), nil
}

// DiffRefToRefSummary returns compact per-file diff metadata between two refs
// without materializing the full patch text.
func (s *Store) DiffRefToRefSummary(ctx context.Context, workspace, fromRef, toRef string) ([]diffsummary.File, error) {
	from, err := s.resolveRefCommit(ctx, workspace, fromRef)
	if err != nil {
		return nil, err
	}
	to, err := s.resolveRefCommit(ctx, workspace, toRef)
	if err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, fmt.Errorf("checkpoint: diff refs unavailable: from=%q to=%q", fromRef, toRef)
	}

	nameStatus, _, _, err := runGit(ctx, workspace, nil, false,
		"diff", "--name-status", "--no-renames", "-z", "--no-color", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff name-status %s..%s: %w", fromRef, toRef, err)
	}
	numstat, _, _, err := runGit(ctx, workspace, nil, false,
		"diff", "--numstat", "--no-renames", "-z", "--no-color", "--no-ext-diff", "--no-textconv", from, to)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff numstat %s..%s: %w", fromRef, toRef, err)
	}
	return diffsummary.ParseGitNameStatusNumstat(nameStatus, numstat), nil
}

// DiffRefToWorktree returns the unified patch from the checkpoint ref to a
// synthetic tree of the current worktree. The synthetic tree includes
// untracked-not-ignored files, so files that exist only in checkpoint refs are
// compared blob-to-blob instead of appearing as "delete whole file, add whole
// file" replacements.
func (s *Store) DiffRefToWorktree(ctx context.Context, workspace, ref string) ([]byte, error) {
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, fmt.Errorf("checkpoint: ref %q is unavailable", ref)
	}

	worktreeTree, err := s.captureSyntheticWorktreeTree(ctx, workspace, nil)
	if err != nil {
		return nil, err
	}
	defer worktreeTree.cleanup()
	stdout, _, _, err := runGitWithStdoutLimit(ctx, workspace, worktreeTree.env, false, maxDiffOutputBytes,
		"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
		oid, worktreeTree.oid, "--")
	if errors.Is(err, errGitOutputTooLarge) {
		return nil, fmt.Errorf("checkpoint: diff worktree exceeds %d byte limit", maxDiffOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff worktree: %w", err)
	}
	return []byte(stdout), nil
}

// RestoreWorktreePaths restores only the listed paths from the checkpoint
// ref, leaving every other file in the worktree alone. Paths that exist at
// both HEAD and the ref are restored in the worktree and index. Paths that
// exist at the ref but not HEAD are restored as worktree-only files, preserving
// the "untracked file captured in a checkpoint" shape. Paths that don't exist
// at the ref (i.e. files the agent created and we want gone after a revert)
// are unlinked from disk and their index entry cleared. Workspace-relative
// paths only; absolute paths must already be normalized via
// `triage.normalizeWorkspaceRelativePath` before reaching this function.
//
// Unlike the wholesale-restore approach this replaced, no `git clean` and
// no `git reset` runs — both would touch files outside the listed paths
// and clobber unrelated user edits between turns.
func (s *Store) RestoreWorktreePaths(ctx context.Context, workspace, ref string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	paths, err := validateScopedWorktreePaths(workspace, paths)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return err
	}
	if oid == "" {
		return fmt.Errorf("checkpoint: ref %q is unavailable", ref)
	}
	existing, missing, err := s.partitionPathsAtRef(ctx, workspace, oid, paths)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		trackedAtHead, untrackedAtHead, err := s.partitionExistingPathsByHead(ctx, workspace, existing)
		if err != nil {
			return err
		}
		if len(trackedAtHead) > 0 {
			args := append([]string{"restore", "--source", oid, "--worktree", "--staged", "--"}, trackedAtHead...)
			if _, _, _, err := runGit(ctx, workspace, nil, false, args...); err != nil {
				return fmt.Errorf("checkpoint: restore tracked paths from %s: %w", ref, err)
			}
		}
		if len(untrackedAtHead) > 0 {
			args := append([]string{"restore", "--source", oid, "--worktree", "--"}, untrackedAtHead...)
			if _, _, _, err := runGit(ctx, workspace, nil, false, args...); err != nil {
				return fmt.Errorf("checkpoint: restore untracked paths from %s: %w", ref, err)
			}
			if err := clearCachedPaths(ctx, workspace, untrackedAtHead); err != nil {
				return err
			}
		}
	}
	for _, p := range missing {
		// File didn't exist at the checkpoint; agent created it after.
		// Remove the working-tree copy if present and drop its index
		// entry so `git status` matches the checkpoint state. ENOENT on
		// the worktree side is fine — the file may already be gone.
		abs := filepath.Join(workspace, p)
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checkpoint: remove %s: %w", p, err)
		}
		if err := clearCachedPaths(ctx, workspace, []string{p}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) partitionExistingPathsByHead(ctx context.Context, workspace string, paths []string) (trackedAtHead, untrackedAtHead []string, err error) {
	head, err := s.resolveRefCommit(ctx, workspace, "HEAD")
	if err != nil {
		return nil, nil, err
	}
	if head == "" {
		return nil, paths, nil
	}
	return s.partitionPathsAtRef(ctx, workspace, head, paths)
}

func clearCachedPaths(ctx context.Context, workspace string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	// `git rm --cached --ignore-unmatch` clears index entries if staged,
	// no-ops if absent. This keeps files that were untracked at the checkpoint
	// untracked after restore, and removes index entries for files the agent
	// created after the checkpoint.
	args := append([]string{"rm", "--cached", "-f", "--quiet", "--ignore-unmatch", "--"}, paths...)
	if _, _, _, err := runGit(ctx, workspace, nil, false, args...); err != nil {
		return fmt.Errorf("checkpoint: rm cached paths: %w", err)
	}
	return nil
}

// partitionPathsAtRef splits paths into the set that exists at the
// checkpoint ref vs. the set that does not. Used by RestoreWorktreePaths
// to decide between `git restore --source <ref>` (existing) and `os.Remove`
// (missing). One `git ls-tree` per ref is cheaper than N `git cat-file -e`
// invocations.
func (s *Store) partitionPathsAtRef(ctx context.Context, workspace, oid string, paths []string) (existing, missing []string, err error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}
	args := append([]string{"ls-tree", "--name-only", "-z", oid, "--"}, paths...)
	stdout, _, _, err := runGit(ctx, workspace, nil, false, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("checkpoint: ls-tree %s: %w", oid, err)
	}
	present := make(map[string]struct{})
	for _, p := range strings.Split(stdout, "\x00") {
		if p != "" {
			present[p] = struct{}{}
		}
	}
	for _, p := range paths {
		if _, ok := present[p]; ok {
			existing = append(existing, p)
		} else {
			missing = append(missing, p)
		}
	}
	return existing, missing, nil
}

func validateScopedWorktreePaths(workspace string, paths []string) ([]string, error) {
	workspaceAbs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: resolve workspace %q: %w", workspace, err)
	}
	workspaceAbs = filepath.Clean(workspaceAbs)
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		cleaned, err := cleanWorkspaceRelativePath(p)
		if err != nil {
			return nil, fmt.Errorf("checkpoint: unsafe path %q: %w", p, err)
		}
		if cleaned == "" {
			continue
		}
		if err := rejectSymlinkedParents(workspaceAbs, cleaned); err != nil {
			return nil, err
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out, nil
}

func cleanWorkspaceRelativePath(p string) (string, error) {
	p = filepath.ToSlash(p)
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, ":") {
		return "", fmt.Errorf("pathspec magic is not allowed")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path must stay inside the workspace")
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("path contains invalid component %q", part)
		}
		if part == ".git" {
			return "", fmt.Errorf("paths under .git are not allowed")
		}
	}
	return cleaned, nil
}

func rejectSymlinkedParents(workspaceAbs, relPath string) error {
	parts := strings.Split(relPath, "/")
	current := workspaceAbs
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checkpoint: inspect path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("checkpoint: path %q crosses symlink %q", relPath, part)
		}
	}
	return nil
}

// DiffRefToRefScoped is DiffRefToRef constrained to a pathspec. Empty
// `paths` returns an empty patch — the caller's path filter is the
// authority on what's worth diffing.
func (s *Store) DiffRefToRefScoped(ctx context.Context, workspace, fromRef, toRef string, paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	paths, err := validateScopedWorktreePaths(workspace, paths)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	from, err := s.resolveRefCommit(ctx, workspace, fromRef)
	if err != nil {
		return nil, err
	}
	to, err := s.resolveRefCommit(ctx, workspace, toRef)
	if err != nil {
		return nil, err
	}
	if from == "" || to == "" {
		return nil, fmt.Errorf("checkpoint: diff refs unavailable: from=%q to=%q", fromRef, toRef)
	}
	args := append([]string{
		"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
		from, to, "--",
	}, paths...)
	stdout, _, _, err := runGitWithStdoutLimit(ctx, workspace, nil, false, maxDiffOutputBytes, args...)
	if errors.Is(err, errGitOutputTooLarge) {
		return nil, fmt.Errorf("checkpoint: diff %s..%s exceeds %d byte limit", fromRef, toRef, maxDiffOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff %s..%s scoped: %w", fromRef, toRef, err)
	}
	return []byte(stdout), nil
}

// DiffRefToWorktreeScoped returns the unified patch from a checkpoint ref to a
// synthetic tree of the current worktree for a known path set. The path list is
// the authority: empty paths return an empty patch.
func (s *Store) DiffRefToWorktreeScoped(ctx context.Context, workspace, ref string, paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	paths, err := validateScopedWorktreePaths(workspace, paths)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, fmt.Errorf("checkpoint: ref %q is unavailable", ref)
	}
	worktreeTree, err := s.captureSyntheticWorktreeTree(ctx, workspace, paths)
	if err != nil {
		return nil, err
	}
	defer worktreeTree.cleanup()
	args := append([]string{
		"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv",
		oid, worktreeTree.oid, "--",
	}, paths...)
	stdout, _, _, err := runGitWithStdoutLimit(ctx, workspace, worktreeTree.env, false, maxDiffOutputBytes, args...)
	if errors.Is(err, errGitOutputTooLarge) {
		return nil, fmt.Errorf("checkpoint: diff %s..worktree exceeds %d byte limit", ref, maxDiffOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff %s..worktree scoped: %w", ref, err)
	}
	return []byte(stdout), nil
}

// DiffWorkspaceVsHead returns the unified patch for everything currently
// uncommitted in the workspace: tracked changes against HEAD plus
// untracked-not-ignored files. Mirrors `git status` semantics rather
// than checkpoint semantics — the caller wants to see manual edits
// alongside any post-checkpoint agent work. Returns an empty slice on
// fresh-init repos that have no HEAD to diff against (no commits yet, so
// "uncommitted" doesn't apply).
func (s *Store) DiffWorkspaceVsHead(ctx context.Context, workspace string) ([]byte, error) {
	hasHead, err := s.HasHeadCommit(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: probe HEAD: %w", err)
	}

	remainingBytes := maxDiffOutputBytes
	var parts []string
	if hasHead {
		tracked, _, _, err := runGitWithStdoutLimit(ctx, workspace, nil, false, remainingBytes,
			"diff", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", "--")
		if errors.Is(err, errGitOutputTooLarge) {
			return nil, fmt.Errorf("checkpoint: workspace diff exceeds %d byte limit", maxDiffOutputBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("checkpoint: diff HEAD: %w", err)
		}
		if t := strings.TrimSpace(tracked); t != "" {
			parts = append(parts, t)
		}
		remainingBytes -= int64(len(tracked))
	}

	untracked, _, code, err := runGit(ctx, workspace, nil, true,
		"ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("checkpoint: ls-files others: %w", err)
	}
	if code == 0 {
		for _, p := range strings.Split(untracked, "\x00") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if remainingBytes <= 0 {
				return nil, fmt.Errorf("checkpoint: workspace diff exceeds %d byte limit", maxDiffOutputBytes)
			}
			patch, _, exit, err := runGitWithStdoutLimit(ctx, workspace, nil, true, remainingBytes,
				"diff", "--no-index", "--patch", "--minimal", "--no-color", "--no-ext-diff", "--no-textconv", "--",
				"/dev/null", p)
			if errors.Is(err, errGitOutputTooLarge) {
				return nil, fmt.Errorf("checkpoint: workspace diff exceeds %d byte limit", maxDiffOutputBytes)
			}
			if err != nil {
				return nil, fmt.Errorf("checkpoint: diff new file %s: %w", p, err)
			}
			if exit != 0 && exit != 1 {
				return nil, fmt.Errorf("checkpoint: diff new file %s exited %d", p, exit)
			}
			if t := strings.TrimSpace(patch); t != "" {
				parts = append(parts, t)
			}
			remainingBytes -= int64(len(patch))
		}
	}
	return []byte(strings.Join(parts, "\n\n")), nil
}

// CleanupThread deletes every checkpoint ref owned by threadID. Idempotent.
func (s *Store) CleanupThread(ctx context.Context, workspace, threadID string) error {
	refs, err := s.ListThreadRefs(ctx, workspace, threadID)
	if err != nil {
		return err
	}
	var errs []error
	for _, ref := range refs {
		if _, _, _, err := runGit(ctx, workspace, nil, true, "update-ref", "-d", ref); err != nil {
			errs = append(errs, fmt.Errorf("checkpoint: delete ref %s: %w", ref, err))
		}
	}
	return errors.Join(errs...)
}

// CleanupLegacyTurnRefs deletes retired turn-index checkpoint refs for a
// thread. Message checkpoints use refs under `message/`; keeping old `turn/`
// refs after the v40 DB rebuild would leave hidden snapshots with no metadata
// row left to manage or clean up.
func (s *Store) CleanupLegacyTurnRefs(ctx context.Context, workspace, threadID string) error {
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"for-each-ref", "--format=%(refname)", LegacyTurnRefPattern(threadID))
	if err != nil {
		return fmt.Errorf("checkpoint: list legacy turn refs: %w", err)
	}
	var errs []error
	for _, ref := range strings.Split(stdout, "\n") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if _, _, _, err := runGit(ctx, workspace, nil, true, "update-ref", "-d", ref); err != nil {
			errs = append(errs, fmt.Errorf("checkpoint: delete legacy ref %s: %w", ref, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteRef removes a single checkpoint ref. Missing refs are not an error.
func (s *Store) DeleteRef(ctx context.Context, workspace, ref string) error {
	if _, _, _, err := runGit(ctx, workspace, nil, true, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("checkpoint: delete ref %s: %w", ref, err)
	}
	return nil
}

// ListThreadRefs returns every checkpoint ref owned by threadID.
func (s *Store) ListThreadRefs(ctx context.Context, workspace, threadID string) ([]string, error) {
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"for-each-ref", "--format=%(refname)", ThreadRefPattern(threadID))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: list thread refs: %w", err)
	}
	var refs []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// HasCheckpointRef reports whether the given ref resolves to a commit.
func (s *Store) HasCheckpointRef(ctx context.Context, workspace, ref string) (bool, error) {
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return false, err
	}
	return oid != "", nil
}

// IsGitRepository reports whether workspace is inside a (non-bare) git work
// tree. Returns false — never an error — for any scenario in which capture
// would be invalid: no git binary, not a repo, a bare repo, detached file
// system. Callers use the returned bool to decide whether to skip capture.
func (s *Store) IsGitRepository(ctx context.Context, workspace string) bool {
	stdout, _, code, err := runGit(ctx, workspace, nil, true, "rev-parse", "--is-inside-work-tree")
	if err != nil || code != 0 {
		return false
	}
	return strings.TrimSpace(stdout) == "true"
}

// HasHeadCommit reports whether HEAD resolves to a commit. False on
// fresh-init repos with no commits yet.
func (s *Store) HasHeadCommit(ctx context.Context, workspace string) (bool, error) {
	_, _, code, err := runGit(ctx, workspace, nil, true, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// resolveRefCommit returns the commit OID a ref points at, or "" if missing.
func (s *Store) resolveRefCommit(ctx context.Context, workspace, ref string) (string, error) {
	stdout, _, code, err := runGit(ctx, workspace, nil, true,
		"rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("checkpoint: resolve ref %s: %w", ref, err)
	}
	if code != 0 {
		return "", nil
	}
	return strings.TrimSpace(stdout), nil
}

// runGit runs `git <args>` with the given extra env vars. allowNonZero lets
// the caller handle exit codes without this helper treating them as errors —
// useful for probes (`rev-parse --verify`) and for `diff --no-index` which
// exits 1 when files differ.
func runGit(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	allowNonZero bool,
	args ...string,
) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = out.String()
	stderr = errBuf.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), runErr)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

func runGitWithStdin(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	stdin []byte,
	allowNonZero bool,
	args ...string,
) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = out.String()
	stderr = errBuf.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), runErr)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

func runGitWithStdoutLimit(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	allowNonZero bool,
	maxStdoutBytes int64,
	args ...string,
) (stdout, stderr string, code int, err error) {
	if maxStdoutBytes <= 0 {
		return runGit(ctx, workspace, extraEnv, allowNonZero, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, fmt.Errorf("git %s: stdout pipe: %w", strings.Join(args, " "), err)
	}
	stderrFile, err := os.CreateTemp("", "agent-overflow-git-stderr-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("git %s: stderr temp file: %w", strings.Join(args, " "), err)
	}
	stderrPath := stderrFile.Name()
	defer os.Remove(stderrPath)
	defer stderrFile.Close()
	readStderr := func() string {
		_ = stderrFile.Close()
		data, err := os.ReadFile(stderrPath)
		if err != nil {
			return ""
		}
		return string(data)
	}
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		return "", "", 0, fmt.Errorf("git %s: start: %w", strings.Join(args, " "), err)
	}

	data, readErr := io.ReadAll(io.LimitReader(stdoutPipe, maxStdoutBytes+1))
	if int64(len(data)) > maxStdoutBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", readStderr(), 0, errGitOutputTooLarge
	}
	waitErr := cmd.Wait()
	stdout = string(data)
	stderr = readStderr()
	if readErr != nil {
		return stdout, stderr, 0, fmt.Errorf("git %s: read stdout: %w", strings.Join(args, " "), readErr)
	}
	if waitErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), waitErr)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

func gitEnv(extraEnv []string) []string {
	env := make([]string, 0, len(os.Environ())+len(extraEnv)+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_EXTERNAL_DIFF=") || strings.HasPrefix(entry, "GIT_DIFF_OPTS=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_EXTERNAL_DIFF=", "GIT_DIFF_OPTS=")
	return append(env, extraEnv...)
}
