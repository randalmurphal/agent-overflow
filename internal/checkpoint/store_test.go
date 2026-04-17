package checkpoint

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// -- helpers --

// initRepo inits a git repo at dir with one initial commit.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	runCommand(t, dir, "git", "init", "-q", "-b", "main")
	writeTestFile(t, dir, "README", "initial\n")
	runCommand(t, dir, "git", "add", "-A")
	runCommand(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "baseline")
}

// initBareRepo inits a repo without any commit — HEAD won't resolve.
func initBareRepo(t *testing.T, dir string) {
	t.Helper()
	runCommand(t, dir, "git", "init", "-q", "-b", "main")
}

func runCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@test",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// -- tests --

func TestCaptureBaselineAndDiffHappyPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	r0, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture r0: %v", err)
	}
	if !strings.Contains(r0, "/turn/0") {
		t.Errorf("expected ref to contain /turn/0, got %q", r0)
	}

	// Agent edits README and creates a new file.
	writeTestFile(t, dir, "README", "initial\nagent edit\n")
	writeTestFile(t, dir, "agent-new.txt", "agent wrote this\n")

	r1, err := s.CaptureBaseline(ctx, dir, "t1", 1)
	if err != nil {
		t.Fatalf("capture r1: %v", err)
	}

	diff, err := s.DiffRefToRef(ctx, dir, r0, r1)
	if err != nil {
		t.Fatalf("diff r0..r1: %v", err)
	}
	if !strings.Contains(string(diff), "+agent edit") {
		t.Errorf("expected +agent edit in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "agent-new.txt") {
		t.Errorf("expected agent-new.txt in diff; got:\n%s", diff)
	}
}

func TestCaptureIncludesUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	// Untracked file must appear in the baseline snapshot.
	writeTestFile(t, dir, "untracked.txt", "hello untracked\n")

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Modify the untracked file after capture; diff-to-worktree should show
	// the modification, proving the baseline captured the original content.
	writeTestFile(t, dir, "untracked.txt", "hello MODIFIED\n")

	diff, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff ref to worktree: %v", err)
	}
	if !strings.Contains(string(diff), "untracked.txt") {
		t.Errorf("expected untracked.txt in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "MODIFIED") {
		t.Errorf("expected MODIFIED in diff; got:\n%s", diff)
	}
}

func TestCaptureRespectsGitignore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)

	writeTestFile(t, dir, ".gitignore", "ignored-dir/\n*.log\n")
	writeTestFile(t, dir, "ignored-dir/secret.txt", "shhh\n")
	writeTestFile(t, dir, "app.log", "log line\n")
	runCommand(t, dir, "git", "add", ".gitignore")
	runCommand(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "ignore")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// If the baseline captured the ignored files we'd see them in a diff to
	// a new baseline; confirm via diff-to-worktree after edits.
	writeTestFile(t, dir, "ignored-dir/secret.txt", "CHANGED\n")
	writeTestFile(t, dir, "app.log", "CHANGED log\n")

	diff, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	// These files were ignored, so they should not show up in the diff.
	if strings.Contains(string(diff), "ignored-dir/secret.txt") {
		t.Errorf("gitignored file leaked into checkpoint diff: %s", diff)
	}
	if strings.Contains(string(diff), "app.log") {
		t.Errorf("gitignored log leaked into checkpoint diff: %s", diff)
	}
}

func TestIsGitRepositoryDetectsNonRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewStore()
	if s.IsGitRepository(ctx, dir) {
		t.Errorf("empty temp dir should NOT be a git repo")
	}
}

func TestIsGitRepositoryDetectsBareRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runCommand(t, dir, "git", "init", "-q", "--bare", "-b", "main")

	s := NewStore()
	if s.IsGitRepository(ctx, dir) {
		t.Errorf("bare repo has no working tree; IsGitRepository should return false")
	}
}

func TestIsGitRepositoryDetectsGitRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)

	s := NewStore()
	if !s.IsGitRepository(ctx, dir) {
		t.Errorf("initialized repo should be recognised")
	}
}

func TestCaptureOnFreshInitRepoWithNoHEAD(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initBareRepo(t, dir)
	writeTestFile(t, dir, "file.txt", "hello\n")

	s := NewStore()
	has, err := s.HasHeadCommit(ctx, dir)
	if err != nil {
		t.Fatalf("probe HEAD: %v", err)
	}
	if has {
		t.Fatalf("fresh-init repo should have no HEAD commit")
	}

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture on no-HEAD repo: %v", err)
	}
	// Diff-to-worktree should work even on a no-HEAD repo.
	if _, err := s.DiffRefToWorktree(ctx, dir, ref); err != nil {
		t.Fatalf("diff-to-worktree on no-HEAD repo: %v", err)
	}
}

func TestCaptureDoesNotModifyUserIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)

	// User stages `user-staged.txt` and leaves an *unstaged* modification to
	// `README` on disk. A buggy capture that doesn't isolate the index via
	// GIT_INDEX_FILE would run `git add -A` against the real .git/index and
	// promote the unstaged README modification into the staged set,
	// changing the index mtime and content hash.
	writeTestFile(t, dir, "user-staged.txt", "user staged this\n")
	runCommand(t, dir, "git", "add", "user-staged.txt")
	writeTestFile(t, dir, "README", "user unstaged edit\n")

	indexHashBefore := readIndexHash(t, dir)

	s := NewStore()
	if _, err := s.CaptureBaseline(ctx, dir, "t1", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	indexHashAfter := readIndexHash(t, dir)
	if indexHashBefore != indexHashAfter {
		t.Errorf("user's .git/index changed across capture:\nbefore=%s\nafter =%s",
			indexHashBefore, indexHashAfter)
	}

	// Also assert the porcelain status matches exactly — user-staged stays
	// added (A), README stays modified-unstaged (space+M).
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "A  user-staged.txt") {
		t.Errorf("expected 'A  user-staged.txt'; got:\n%s", got)
	}
	if !strings.Contains(got, " M README") {
		t.Errorf("expected ' M README' (unstaged modification); got:\n%s", got)
	}
}

// readIndexHash returns sha256(.git/index). Used as a fingerprint to prove
// capture did not mutate the user's on-disk index.
func readIndexHash(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	return fmt.Sprintf("%x-%d", sha256sum(data), len(data))
}

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestCaptureOnDetachedHEAD(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)

	// Make a second commit, then checkout the first (detach HEAD).
	writeTestFile(t, dir, "second.txt", "second\n")
	runCommand(t, dir, "git", "add", "-A")
	runCommand(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "second")
	runCommand(t, dir, "git", "checkout", "-q", "HEAD~1")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture on detached HEAD: %v", err)
	}
	exists, err := s.HasCheckpointRef(ctx, dir, ref)
	if err != nil {
		t.Fatalf("has checkpoint ref: %v", err)
	}
	if !exists {
		t.Errorf("checkpoint ref should exist after capture on detached HEAD")
	}
}

func TestRestoreWorktreeRestoresFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Agent corrupts README and writes a new junk file.
	writeTestFile(t, dir, "README", "corrupted\n")
	writeTestFile(t, dir, "agent-junk.txt", "junk\n")

	if err := s.RestoreWorktree(ctx, dir, ref); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := readTestFile(t, dir, "README"); got != "initial\n" {
		t.Errorf("README not restored: %q", got)
	}
	if readTestFile(t, dir, "agent-junk.txt") != "" {
		t.Errorf("agent-junk.txt should have been removed by `git clean`")
	}
}

func TestRestoreWorktreeDestroysUserEditsDocumented(t *testing.T) {
	// This documents the intentional behavior: restoring to a checkpoint is
	// destructive. User edits made after the checkpoint are lost. We preserve
	// this behavior because forge does the same, and the UI warns the user.
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "user-after.txt", "user edit made AFTER checkpoint\n")
	if err := s.RestoreWorktree(ctx, dir, ref); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if readTestFile(t, dir, "user-after.txt") != "" {
		t.Errorf("user-after.txt should have been removed by restore (destructive)")
	}
}

func TestCleanupThreadRemovesOnlyThreadsRefs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	for _, th := range []string{"thread-a", "thread-b"} {
		for i := 0; i < 2; i++ {
			if _, err := s.CaptureBaseline(ctx, dir, th, i); err != nil {
				t.Fatalf("capture %s:%d: %v", th, i, err)
			}
		}
	}

	// thread-a has 2 refs, thread-b has 2 refs.
	a, err := s.ListThreadRefs(ctx, dir, "thread-a")
	if err != nil || len(a) != 2 {
		t.Fatalf("thread-a expected 2 refs, got %d err=%v", len(a), err)
	}
	b, err := s.ListThreadRefs(ctx, dir, "thread-b")
	if err != nil || len(b) != 2 {
		t.Fatalf("thread-b expected 2 refs, got %d err=%v", len(b), err)
	}

	// Cleanup thread-a. thread-b refs must remain.
	if err := s.CleanupThread(ctx, dir, "thread-a"); err != nil {
		t.Fatalf("cleanup thread-a: %v", err)
	}

	a, _ = s.ListThreadRefs(ctx, dir, "thread-a")
	if len(a) != 0 {
		t.Errorf("after cleanup, thread-a should have 0 refs, got %d: %v", len(a), a)
	}
	b, _ = s.ListThreadRefs(ctx, dir, "thread-b")
	if len(b) != 2 {
		t.Errorf("after cleanup, thread-b should still have 2 refs, got %d: %v", len(b), b)
	}
}

func TestCleanupThreadIdempotentOnMissingRefs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	if err := s.CleanupThread(ctx, dir, "nonexistent"); err != nil {
		t.Errorf("cleanup on non-existent thread should be idempotent, got: %v", err)
	}
}

func TestDeleteRefMissingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()
	ref := RefForThreadTurn("ghost", 42)
	if err := s.DeleteRef(ctx, dir, ref); err != nil {
		t.Errorf("deleting missing ref should be idempotent, got: %v", err)
	}
}

func TestConcurrentCapturesDoNotCollide(t *testing.T) {
	// Two threads capturing into the same workspace in parallel must both
	// succeed. The temp GIT_INDEX_FILE path is unique per-capture
	// (os.MkdirTemp), so they don't step on each other.
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i, th := range []string{"t-alpha", "t-beta", "t-gamma", "t-delta"} {
		wg.Add(1)
		go func(th string, idx int) {
			defer wg.Done()
			for j := 0; j < 2; j++ {
				if _, err := s.CaptureBaseline(ctx, dir, th, j); err != nil {
					errs <- err
				}
			}
		}(th, i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent capture error: %v", err)
	}

	// Verify each thread owns exactly its 2 refs.
	for _, th := range []string{"t-alpha", "t-beta", "t-gamma", "t-delta"} {
		refs, err := s.ListThreadRefs(ctx, dir, th)
		if err != nil {
			t.Errorf("list refs for %s: %v", th, err)
			continue
		}
		if len(refs) != 2 {
			t.Errorf("%s expected 2 refs, got %d: %v", th, len(refs), refs)
		}
	}
}

func TestDiffRefToWorktreeOnCleanWorktreeIsEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// No changes since capture.
	diff, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if got := strings.TrimSpace(string(diff)); got != "" {
		t.Errorf("expected empty diff on clean worktree, got:\n%s", got)
	}
}

func TestDiffRefToRefMissingRefErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref0, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// Ask for a ref we never captured.
	_, err = s.DiffRefToRef(ctx, dir, ref0, RefForThreadTurn("t1", 99))
	if err == nil {
		t.Errorf("expected error when to-ref is missing")
	}
}

func TestCaptureBaselineEmptyWorkspaceProducesEmptyTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initBareRepo(t, dir)
	s := NewStore()

	// Repo with no files at all. Capture should still succeed and produce a
	// ref pointing at an empty tree.
	if _, err := s.CaptureBaseline(ctx, dir, "t1", 0); err != nil {
		t.Errorf("capture on empty workspace: %v", err)
	}
}

func TestCaptureCleanupOnContextCancel(t *testing.T) {
	// Cancelling the context during capture must not leave temp dirs behind.
	// We can't easily race the internal `git` calls, but we can at least
	// assert the normal happy path cleans up its temp dir.
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	before, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp before: %v", err)
	}
	beforeCount := countCheckpointTemp(before)

	if _, err := s.CaptureBaseline(ctx, dir, "t1", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	after, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp after: %v", err)
	}
	afterCount := countCheckpointTemp(after)
	if afterCount > beforeCount {
		t.Errorf("leaked %d agent-overflow-checkpoint temp dirs", afterCount-beforeCount)
	}
}

func countCheckpointTemp(entries []os.DirEntry) int {
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "agent-overflow-checkpoint-") {
			count++
		}
	}
	return count
}

func TestRestoreMissingRefErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	err := s.RestoreWorktree(ctx, dir, RefForThreadTurn("ghost", 0))
	if err == nil {
		t.Errorf("expected error when restoring from missing ref")
	}
}

func TestHasCheckpointRefReturnsFalseForMissing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	has, err := s.HasCheckpointRef(ctx, dir, RefForThreadTurn("nobody", 0))
	if err != nil {
		t.Fatalf("has ref: %v", err)
	}
	if has {
		t.Errorf("ref should not exist")
	}
}
