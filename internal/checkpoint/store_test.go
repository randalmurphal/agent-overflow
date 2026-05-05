package checkpoint

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
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

func countPatchChanges(patch string) (additions int, deletions int) {
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			additions++
		}
		if strings.HasPrefix(line, "-") {
			deletions++
		}
	}
	return additions, deletions
}

func countLooseGitObjects(t *testing.T, dir string) int {
	t.Helper()
	objectDir := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "--git-path", "objects"))
	if objectDir == "" {
		t.Fatal("empty git object dir")
	}
	if !filepath.IsAbs(objectDir) {
		objectDir = filepath.Join(dir, objectDir)
	}
	count := 0
	if err := filepath.WalkDir(objectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
			return nil
		}
		switch filepath.Base(path) {
		case "info", "pack":
			return filepath.SkipDir
		default:
			return nil
		}
	}); err != nil {
		t.Fatalf("walk git objects: %v", err)
	}
	return count
}

func writeFakeGit(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// commitAll stages everything in dir and commits it under the test
// author. Used by RestoreWorktreePaths tests that need files tracked at
// HEAD before they can drift.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	runCommand(t, dir, "git", "add", "-A")
	runCommand(t, dir,
		"git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", message)
}

// -- tests --

func TestRunGitWithStdoutLimitHandlesInheritedStderrWriters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	writeFakeGit(t, dir, `#!/bin/sh
if [ "$1" = "stderr-open-after-exit" ]; then
  (sleep 0.05) 1>&2 &
  printf 'done\n'
  exit 0
fi
echo "unsupported fake git invocation: $*" 1>&2
exit 99
`)

	stdout, stderr, code, err := runGitWithStdoutLimit(ctx, dir, nil, false, 1024, "stderr-open-after-exit")
	if err != nil {
		t.Fatalf("runGitWithStdoutLimit: %v", err)
	}
	if stdout != "done\n" {
		t.Fatalf("stdout = %q, want done newline", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

func TestRunGitWithStdoutLimitDoesNotWaitForLongLivedStderrDescendant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	writeFakeGit(t, dir, `#!/bin/sh
if [ "$1" = "stderr-held-open" ]; then
  (sleep 1) 1>&2 &
  printf 'done\n'
  exit 0
fi
echo "unsupported fake git invocation: $*" 1>&2
exit 99
`)

	start := time.Now()
	stdout, _, _, err := runGitWithStdoutLimit(ctx, dir, nil, false, 1024, "stderr-held-open")
	if err != nil {
		t.Fatalf("runGitWithStdoutLimit: %v", err)
	}
	if stdout != "done\n" {
		t.Fatalf("stdout = %q, want done newline", stdout)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("runGitWithStdoutLimit waited %s for descendant stderr writer", elapsed)
	}
}

func TestRunGitWithStdoutLimitOversizeDoesNotDrainInheritedStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	writeFakeGit(t, dir, `#!/bin/sh
if [ "$1" = "oversize-held-stdout" ]; then
  (sleep 1) &
  i=0
  while [ "$i" -lt 2048 ]; do
    printf x
    i=$((i + 1))
  done
  exit 0
fi
echo "unsupported fake git invocation: $*" 1>&2
exit 99
`)

	start := time.Now()
	_, _, _, err := runGitWithStdoutLimit(ctx, dir, nil, false, 16, "oversize-held-stdout")
	if !errors.Is(err, errGitOutputTooLarge) {
		t.Fatalf("runGitWithStdoutLimit error = %v, want errGitOutputTooLarge", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("runGitWithStdoutLimit waited %s for descendant stdout writer after oversize", elapsed)
	}
}

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

func TestCaptureRefDoesNotRunCleanFilters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	runCommand(t, dir, "git", "config", "filter.aofilter.clean", "sh -c 'echo filter-ran > .filter-ran; cat'")
	writeTestFile(t, dir, ".gitattributes", "*.secret filter=aofilter\n")
	writeTestFile(t, dir, "payload.secret", "keep raw\n")

	ref := ThreadRefPrefix("t-filter") + "message/no-filter"
	if err := s.CaptureRef(ctx, dir, ref); err != nil {
		t.Fatalf("capture ref: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".filter-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean filter ran during checkpoint capture: stat err=%v", err)
	}
	out := gitOutput(t, dir, "show", ref+":payload.secret")
	if out != "keep raw\n" {
		t.Fatalf("captured payload = %q, want raw file content", out)
	}
}

func TestDiffRefToRefRejectsOversizePatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	originalLimit := maxDiffOutputBytes
	maxDiffOutputBytes = 128
	t.Cleanup(func() { maxDiffOutputBytes = originalLimit })

	r0, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture r0: %v", err)
	}
	writeTestFile(t, dir, "large.txt", strings.Repeat("x\n", 200))
	r1, err := s.CaptureBaseline(ctx, dir, "t1", 1)
	if err != nil {
		t.Fatalf("capture r1: %v", err)
	}

	if _, err := s.DiffRefToRef(ctx, dir, r0, r1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("DiffRefToRef error = %v, want exceeds limit", err)
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

func TestRestoreWorktreePathsRestoresOnlyListedPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	writeTestFile(t, dir, "agent.txt", "pristine\n")
	writeTestFile(t, dir, "user.txt", "pristine user content\n")
	commitAll(t, dir, "seed both files")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Agent rewrites agent.txt; user manually edits user.txt. Only the
	// agent's path is in the toolPaths list.
	writeTestFile(t, dir, "agent.txt", "agent corrupted\n")
	writeTestFile(t, dir, "user.txt", "user manually edited\n")

	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"agent.txt"}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}

	if got := readTestFile(t, dir, "agent.txt"); got != "pristine\n" {
		t.Errorf("agent.txt not restored: %q", got)
	}
	if got := readTestFile(t, dir, "user.txt"); got != "user manually edited\n" {
		t.Errorf("user.txt should be untouched: %q", got)
	}
}

func TestRestoreWorktreePathsRemovesAgentCreatedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Agent created a brand-new file after the checkpoint; user added an
	// unrelated brand-new file. Only the agent's path is listed.
	writeTestFile(t, dir, "agent-junk.txt", "junk\n")
	writeTestFile(t, dir, "user-note.txt", "user kept this\n")

	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"agent-junk.txt"}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}
	if readTestFile(t, dir, "agent-junk.txt") != "" {
		t.Errorf("agent-junk.txt should have been unlinked")
	}
	if got := readTestFile(t, dir, "user-note.txt"); got != "user kept this\n" {
		t.Errorf("user-note.txt should be untouched: %q", got)
	}
}

func TestRestoreWorktreePathsPreservesUntrackedFileStatusFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "before\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "agent-note.txt", "after\n")
	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"agent-note.txt"}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}

	if got := readTestFile(t, dir, "agent-note.txt"); got != "before\n" {
		t.Fatalf("agent-note.txt = %q, want checkpoint content", got)
	}
	status := gitOutput(t, dir, "status", "--short", "--", "agent-note.txt")
	if status != "?? agent-note.txt\n" {
		t.Fatalf("git status = %q, want untracked restored file", status)
	}
}

func TestRestoreWorktreePathsClearsStagedCheckpointOnlyFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "before\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "agent-note.txt", "staged after checkpoint\n")
	runCommand(t, dir, "git", "add", "--", "agent-note.txt")
	writeTestFile(t, dir, "agent-note.txt", "worktree after checkpoint\n")
	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"agent-note.txt"}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}

	if got := readTestFile(t, dir, "agent-note.txt"); got != "before\n" {
		t.Fatalf("agent-note.txt = %q, want checkpoint content", got)
	}
	status := gitOutput(t, dir, "status", "--short", "--", "agent-note.txt")
	if status != "?? agent-note.txt\n" {
		t.Fatalf("git status = %q, want untracked restored file", status)
	}
}

func TestRestoreWorktreePathsEmptyListIsNoop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "drifted.txt", "user edit\n")
	if err := s.RestoreWorktreePaths(ctx, dir, ref, nil); err != nil {
		t.Fatalf("restore empty: %v", err)
	}
	if got := readTestFile(t, dir, "drifted.txt"); got != "user edit\n" {
		t.Errorf("empty restore should leave files alone: %q", got)
	}
}

func TestRestoreWorktreePathsRejectsEscapingPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"../outside.txt"}); err == nil {
		t.Fatal("restore escaping path succeeded, want error")
	}
}

func TestDiffRefToWorktreeScopedRejectsSymlinkPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{"link/secret.txt"}); err == nil {
		t.Fatal("diff through symlink path succeeded, want error")
	}
}

func TestDiffRefToWorktreeScopedIncludesTrackedUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "agent-new.txt", "created by agent\n")
	writeTestFile(t, dir, "user-new.txt", "created by user\n")

	patch, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{"agent-new.txt"})
	if err != nil {
		t.Fatalf("diff scoped: %v", err)
	}
	got := string(patch)
	if !strings.Contains(got, "+++ b/agent-new.txt") {
		t.Fatalf("patch should include tracked untracked file, got:\n%s", got)
	}
	if strings.Contains(got, "user-new.txt") {
		t.Fatalf("patch should not include unlisted untracked file, got:\n%s", got)
	}
}

func TestDiffRefToWorktreeScopedComparesUntrackedFileCapturedAtRef(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two\nline three\nline four\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two (updated)\nline three\nline four\n")
	patch, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{"agent-note.txt"})
	if err != nil {
		t.Fatalf("diff scoped: %v", err)
	}
	additions, deletions := countPatchChanges(string(patch))
	if additions != 1 || deletions != 1 {
		t.Fatalf("patch changes = +%d -%d, want +1 -1:\n%s", additions, deletions, patch)
	}
	if strings.Contains(string(patch), "deleted file mode") || strings.Contains(string(patch), "new file mode") {
		t.Fatalf("patch should be a line edit, not delete+add:\n%s", patch)
	}
}

func TestDiffRefToWorktreeScopedIgnoresUnrelatedFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO test is POSIX-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "unrelated.pipe"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two updated\n")
	if _, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{"agent-note.txt"}); err != nil {
		t.Fatalf("diff scoped: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("scoped diff touched unrelated FIFO: %v", err)
	}
}

func TestDiffRefToWorktreeScopedPreservesTrailingWhitespacePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	const name = "agent-note.txt "
	writeTestFile(t, dir, name, "line one\nline two\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, name, "line one\nline two updated\n")
	patch, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{name})
	if err != nil {
		t.Fatalf("diff scoped: %v", err)
	}
	additions, deletions := countPatchChanges(string(patch))
	if additions != 1 || deletions != 1 {
		t.Fatalf("patch changes = +%d -%d, want +1 -1:\n%s", additions, deletions, patch)
	}
	if !strings.Contains(string(patch), "agent-note.txt ") {
		t.Fatalf("patch should preserve trailing-space filename:\n%s", patch)
	}
}

func TestDiffRefToWorktreeScopedDoesNotWritePreviewBlobsToRepoObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	beforeObjects := countLooseGitObjects(t, dir)

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two updated\n")
	if _, err := s.DiffRefToWorktreeScoped(ctx, dir, ref, []string{"agent-note.txt"}); err != nil {
		t.Fatalf("diff scoped: %v", err)
	}
	afterObjects := countLooseGitObjects(t, dir)
	if afterObjects != beforeObjects {
		t.Fatalf("loose object count changed after preview diff: before=%d after=%d", beforeObjects, afterObjects)
	}
}

func TestDiffRefToWorktreeComparesUntrackedFileCapturedAtRef(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two\nline three\nline four\n")
	ref, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	writeTestFile(t, dir, "agent-note.txt", "line one\nline two (updated)\nline three\nline four\n")
	patch, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	additions, deletions := countPatchChanges(string(patch))
	if additions != 1 || deletions != 1 {
		t.Fatalf("patch changes = +%d -%d, want +1 -1:\n%s", additions, deletions, patch)
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
	// A successful capture must clean up its own temp dir. Route MkdirTemp
	// through a test-scoped TMPDIR so a parallel test (or another `go test`
	// on the same host) can't race the global count we observe.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	s := NewStore()

	if _, err := s.CaptureBaseline(ctx, dir, "t1", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read scoped temp: %v", err)
	}
	if leaked := countCheckpointTemp(entries); leaked != 0 {
		t.Errorf("leaked %d agent-overflow-checkpoint temp dirs in %s", leaked, tmp)
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

	// Empty path list short-circuits before ref lookup; pass a non-empty
	// list so the missing-ref branch fires.
	err := s.RestoreWorktreePaths(ctx, dir, RefForThreadTurn("ghost", 0), []string{"any.txt"})
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
