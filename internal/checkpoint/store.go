package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Store shells out to `git` to capture, diff, and restore workspace snapshots.
// It is safe for concurrent use across goroutines: each capture uses a unique
// temp index file so GIT_INDEX_FILE paths never collide even when multiple
// threads snapshot the same workspace at the same time.
type Store struct{}

// NewStore returns a Store ready for use. The zero-value Store is also valid;
// this constructor exists so callers can mirror the rest of the package layout.
func NewStore() *Store { return &Store{} }

// Author metadata stamped on every checkpoint commit.
const (
	authorName  = "Agent Overflow"
	authorEmail = "agent-overflow@users.noreply.github.com"
)

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

	// Seed the temp index from HEAD so the snapshot includes tracked files that
	// exist only on HEAD. Skip on a fresh-init repo where HEAD doesn't resolve.
	hasHead, err := s.HasHeadCommit(ctx, workspace)
	if err != nil {
		return fmt.Errorf("checkpoint: probe HEAD: %w", err)
	}
	if hasHead {
		if _, _, _, err := runGit(ctx, workspace, env, false, "read-tree", "HEAD"); err != nil {
			return fmt.Errorf("checkpoint: read-tree HEAD: %w", err)
		}
	}

	// Stage every tracked + untracked-not-ignored file in the workspace.
	if _, _, _, err := runGit(ctx, workspace, env, false, "add", "-A", "--", "."); err != nil {
		return fmt.Errorf("checkpoint: git add -A: %w", err)
	}

	tree, _, _, err := runGit(ctx, workspace, env, false, "write-tree")
	if err != nil {
		return fmt.Errorf("checkpoint: write-tree: %w", err)
	}
	treeOID := strings.TrimSpace(tree)
	if treeOID == "" {
		return errors.New("checkpoint: write-tree returned empty oid")
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
	stdout, _, _, err := runGit(ctx, workspace, nil, false,
		"diff", "--patch", "--minimal", "--no-color", from, to)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff %s..%s: %w", fromRef, toRef, err)
	}
	return []byte(stdout), nil
}

// DiffRefToWorktree returns the unified patch from the checkpoint ref to the
// current worktree, including untracked-new files via `git diff --no-index`.
func (s *Store) DiffRefToWorktree(ctx context.Context, workspace, ref string) ([]byte, error) {
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return nil, err
	}
	if oid == "" {
		return nil, fmt.Errorf("checkpoint: ref %q is unavailable", ref)
	}

	tracked, _, _, err := runGit(ctx, workspace, nil, false,
		"diff", "--patch", "--minimal", "--no-color", oid, "--")
	if err != nil {
		return nil, fmt.Errorf("checkpoint: diff tracked: %w", err)
	}

	untracked, _, code, err := runGit(ctx, workspace, nil, true,
		"ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("checkpoint: ls-files others: %w", err)
	}

	var parts []string
	if t := strings.TrimSpace(tracked); t != "" {
		parts = append(parts, t)
	}
	if code == 0 {
		for _, p := range strings.Split(untracked, "\x00") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// `git diff --no-index` exits 1 when files differ (expected). Any
			// other non-zero exit is a hard error.
			patch, _, exit, err := runGit(ctx, workspace, nil, true,
				"diff", "--no-index", "--patch", "--minimal", "--no-color", "--",
				"/dev/null", p)
			if err != nil {
				return nil, fmt.Errorf("checkpoint: diff new file %s: %w", p, err)
			}
			if exit != 0 && exit != 1 {
				return nil, fmt.Errorf("checkpoint: diff new file %s exited %d", p, exit)
			}
			if t := strings.TrimSpace(patch); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return []byte(strings.Join(parts, "\n\n")), nil
}

// RestoreWorktree overwrites the working tree + index from the checkpoint
// ref, then cleans untracked files not in the checkpoint. This is destructive:
// callers must have warned the user before invoking it.
//
// On a fresh-init repo (no HEAD) the final `git reset` is skipped because it
// has nothing to reset against; `restore` and `clean` still apply.
func (s *Store) RestoreWorktree(ctx context.Context, workspace, ref string) error {
	oid, err := s.resolveRefCommit(ctx, workspace, ref)
	if err != nil {
		return err
	}
	if oid == "" {
		return fmt.Errorf("checkpoint: ref %q is unavailable", ref)
	}

	if _, _, _, err := runGit(ctx, workspace, nil, false,
		"restore", "--source", oid, "--worktree", "--staged", "--", "."); err != nil {
		return fmt.Errorf("checkpoint: restore from %s: %w", ref, err)
	}
	if _, _, _, err := runGit(ctx, workspace, nil, false, "clean", "-fd", "--", "."); err != nil {
		return fmt.Errorf("checkpoint: clean: %w", err)
	}

	hasHead, err := s.HasHeadCommit(ctx, workspace)
	if err != nil {
		return fmt.Errorf("checkpoint: probe HEAD after restore: %w", err)
	}
	if hasHead {
		if _, _, _, err := runGit(ctx, workspace, nil, false, "reset", "--quiet", "--", "."); err != nil {
			return fmt.Errorf("checkpoint: reset after restore: %w", err)
		}
	}
	return nil
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
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = out.String()
	stderr = errBuf.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}
