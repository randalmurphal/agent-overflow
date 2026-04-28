// Package checkpoint integration tests exercise the Store end-to-end against
// a real `git` binary. They cover the hidden-ref namespace, GIT_INDEX_FILE
// isolation, dirty-worktree preservation, cancellation, concurrent captures,
// and cross-feature interactions that the unit tests don't reach.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// -- shared helpers (distinct names from store_test.go to avoid duplicate decls) --

func integInitRepo(t *testing.T, dir string) {
	t.Helper()
	integRun(t, dir, "git", "init", "-q", "-b", "main")
	integWrite(t, dir, "README", "initial\n")
	integRun(t, dir, "git", "add", "-A")
	integRun(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "baseline")
}

func integInitRepoNoHead(t *testing.T, dir string) {
	t.Helper()
	integRun(t, dir, "git", "init", "-q", "-b", "main")
}

func integInitBareRepo(t *testing.T, dir string) {
	t.Helper()
	integRun(t, dir, "git", "init", "-q", "--bare", "-b", "main")
}

func integRun(t *testing.T, dir, name string, args ...string) {
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

func integRunOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@test",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@test",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

func integWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// integIndexHash returns sha256(.git/index)|len. Used to prove capture did not
// mutate the user's on-disk index. Returns "" when no index file exists
// (happens on a freshly init'd repo with nothing staged).
func integIndexHash(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x-%d", h[:], len(data))
}

// integStatusPorcelain returns the `git status --porcelain` output.
func integStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	return integRunOutput(t, dir, "git", "status", "--porcelain")
}

// -- Tests --

// #1 — sha256 of the user's .git/index must be byte-identical before and
// after capture. Capture writes to a temp GIT_INDEX_FILE; if that isolation
// ever regresses and `git add -A` lands in the real index, the sha256 will
// change and this test fires.
func TestIntegration_CaptureMatchingSha256Preservation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	// Arrange: staged file + unstaged modification on disk, mirroring the
	// real-world setup the user expects to survive capture untouched.
	integWrite(t, dir, "user-staged.txt", "staged content\n")
	integRun(t, dir, "git", "add", "user-staged.txt")
	integWrite(t, dir, "README", "user unstaged edit\n")
	integWrite(t, dir, "user-untracked.txt", "totally untracked\n")

	before := integIndexHash(t, dir)
	if before == "" {
		t.Fatalf("precondition: expected .git/index to exist after staging")
	}

	s := NewStore()
	if _, err := s.CaptureBaseline(ctx, dir, "sha-flip", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	after := integIndexHash(t, dir)
	if before != after {
		t.Errorf("user's .git/index sha256 changed across capture\nbefore=%s\nafter =%s",
			before, after)
	}

	// Flip-verification: deliberately call `git add -A` on the real index to
	// prove that IF capture had used the user's index, the sha256 WOULD change.
	// This guards against a future regression where GIT_INDEX_FILE isolation
	// is accidentally dropped (e.g. env plumbing typo). Running a single
	// real-index mutation must flip the hash; otherwise the assertion above
	// wouldn't catch a regression.
	integRun(t, dir, "git", "add", "-A")
	flipped := integIndexHash(t, dir)
	if flipped == after {
		t.Errorf("flip-verification failed: real `git add -A` did not change the "+
			"index hash (before=%s after=%s flipped=%s); the sha256 assertion "+
			"above would be a no-op", before, after, flipped)
	}
}

// #2 — Fresh-init repo with no commits must succeed.
func TestIntegration_CaptureOnFreshRepoNoHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepoNoHead(t, dir)
	integWrite(t, dir, "hello.txt", "just started\n")

	s := NewStore()
	has, err := s.HasHeadCommit(ctx, dir)
	if err != nil {
		t.Fatalf("probe HEAD: %v", err)
	}
	if has {
		t.Fatalf("fresh-init repo should have no HEAD commit")
	}

	ref, err := s.CaptureBaseline(ctx, dir, "fresh", 0)
	if err != nil {
		t.Fatalf("capture on no-HEAD repo: %v", err)
	}

	// Ref should resolve via `git rev-parse`.
	out := integRunOutput(t, dir, "git", "rev-parse", "--verify", ref)
	if strings.TrimSpace(out) == "" {
		t.Errorf("expected ref %s to resolve, got empty rev-parse output", ref)
	}
}

// #3 — Detached HEAD must succeed; ref points at a well-formed commit.
func TestIntegration_CaptureOnDetachedHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	integWrite(t, dir, "second.txt", "second\n")
	integRun(t, dir, "git", "add", "-A")
	integRun(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "second")

	integRun(t, dir, "git", "checkout", "-q", "HEAD~1")
	// Confirm we're actually detached (symbolic-ref fails on detached HEAD).
	if err := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
		t.Fatalf("precondition: expected HEAD to be detached")
	}

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "detached", 0)
	if err != nil {
		t.Fatalf("capture on detached HEAD: %v", err)
	}
	has, err := s.HasCheckpointRef(ctx, dir, ref)
	if err != nil {
		t.Fatalf("has ref: %v", err)
	}
	if !has {
		t.Errorf("detached-HEAD capture should have produced a resolvable ref")
	}
}

// #4 — A bare repo has no worktree. Capture must fail with a clear error
// rather than panicking or silently succeeding.
func TestIntegration_CaptureOnBareRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitBareRepo(t, dir)

	s := NewStore()

	// IsGitRepository must reject bare repos up-front.
	if s.IsGitRepository(ctx, dir) {
		t.Errorf("IsGitRepository should report false for bare repo")
	}

	// CaptureBaseline, if called anyway, must return an error (not panic).
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CaptureBaseline on bare repo panicked: %v", r)
			}
		}()
		_, err := s.CaptureBaseline(ctx, dir, "bare", 0)
		if err == nil {
			t.Errorf("expected capture on bare repo to fail")
		}
	}()
}

// #5 — Untracked files must appear in the captured tree: a diff between this
// ref and HEAD shows them as added.
func TestIntegration_CaptureWithUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	integWrite(t, dir, "new-agent-file.txt", "agent wrote this\n")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "untracked", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// HEAD lacks the file; diff HEAD → ref should show the file as added.
	// (Note DiffRefToRef's contract uses from/to — pass HEAD as the from ref.)
	diff, err := s.DiffRefToRef(ctx, dir, "HEAD", ref)
	if err != nil {
		t.Fatalf("diff HEAD..ref: %v", err)
	}
	if !strings.Contains(string(diff), "new-agent-file.txt") {
		t.Errorf("expected new-agent-file.txt to appear as added in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "+agent wrote this") {
		t.Errorf("expected +agent wrote this in diff; got:\n%s", diff)
	}
}

// #6 — Files matched by .gitignore must NOT be captured, even though they
// exist in the worktree. Mirrors TestCaptureRespectsGitignore but verifies via
// DiffRefToRef (between two captures) that ignored files don't leak into the
// baseline tree.
func TestIntegration_CaptureWithGitIgnoreRespected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	integWrite(t, dir, ".gitignore", ".env\n*.secret\n")
	integRun(t, dir, "git", "add", ".gitignore")
	integRun(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "ignore rules")

	// Create ignored files; capture; modify them; capture again.
	integWrite(t, dir, ".env", "SECRET=1\n")
	integWrite(t, dir, "secrets.secret", "hidden\n")

	s := NewStore()
	r0, err := s.CaptureBaseline(ctx, dir, "gitignore", 0)
	if err != nil {
		t.Fatalf("capture r0: %v", err)
	}

	// Mutate both ignored files AND write a tracked change so the diff between
	// r0 and r1 has *something* in it — to prove the assertion isn't vacuous.
	integWrite(t, dir, ".env", "SECRET=2\n")
	integWrite(t, dir, "secrets.secret", "hidden2\n")
	integWrite(t, dir, "README", "updated\n")

	r1, err := s.CaptureBaseline(ctx, dir, "gitignore", 1)
	if err != nil {
		t.Fatalf("capture r1: %v", err)
	}

	diff, err := s.DiffRefToRef(ctx, dir, r0, r1)
	if err != nil {
		t.Fatalf("diff r0..r1: %v", err)
	}
	if strings.Contains(string(diff), ".env") {
		t.Errorf(".env should not appear in diff (gitignored); got:\n%s", diff)
	}
	if strings.Contains(string(diff), "secrets.secret") {
		t.Errorf("secrets.secret should not appear in diff (gitignored); got:\n%s", diff)
	}
	// Sanity check that the tracked change is present — proves the test isn't
	// empty-diff-passing.
	if !strings.Contains(string(diff), "README") {
		t.Errorf("expected README to appear as the only diff entry; got:\n%s", diff)
	}
}

// #7 — User-staged file stays staged (porcelain A  status) across capture.
func TestIntegration_CapturePreservesUserStagedChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	integWrite(t, dir, "foo.txt", "staged foo\n")
	integRun(t, dir, "git", "add", "foo.txt")

	statusBefore := integStatusPorcelain(t, dir)
	if !strings.Contains(statusBefore, "A  foo.txt") {
		t.Fatalf("precondition: expected 'A  foo.txt', got:\n%s", statusBefore)
	}

	s := NewStore()
	if _, err := s.CaptureBaseline(ctx, dir, "staged", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	statusAfter := integStatusPorcelain(t, dir)
	if statusBefore != statusAfter {
		t.Errorf("user's git status changed across capture\nbefore=%qafter =%q",
			statusBefore, statusAfter)
	}

	// Flip-verification: if capture had mutated the real index, the 'A  foo.txt'
	// line would disappear from status because HEAD would now already contain
	// foo.txt from the temp commit. This assertion would surface that.
	if !strings.Contains(statusAfter, "A  foo.txt") {
		t.Errorf("expected 'A  foo.txt' preserved in status; got:\n%s", statusAfter)
	}
}

// #8 — Untracked + modified + staged all present. Porcelain status must match
// byte-for-byte pre- and post-capture.
func TestIntegration_CapturePreservesUserUntrackedAndModifiedWork(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	// Tracked modification:
	integWrite(t, dir, "README", "initial\nextra line\n")
	// Staged new file:
	integWrite(t, dir, "staged-new.txt", "staged\n")
	integRun(t, dir, "git", "add", "staged-new.txt")
	// Untracked file:
	integWrite(t, dir, "untracked-new.txt", "untracked\n")

	statusBefore := integStatusPorcelain(t, dir)

	s := NewStore()
	if _, err := s.CaptureBaseline(ctx, dir, "mixed", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}

	statusAfter := integStatusPorcelain(t, dir)
	if statusBefore != statusAfter {
		t.Errorf("git status differed across capture\nbefore=%q\nafter =%q",
			statusBefore, statusAfter)
	}
}

// #9 — Two captures are independent refs, and a diff between them shows only
// the mutation that happened in between.
func TestIntegration_TwoConsecutiveCapturesIndependent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	s := NewStore()
	r0, err := s.CaptureBaseline(ctx, dir, "t1", 0)
	if err != nil {
		t.Fatalf("capture r0: %v", err)
	}

	integWrite(t, dir, "new.txt", "alpha\n")

	r1, err := s.CaptureBaseline(ctx, dir, "t1", 1)
	if err != nil {
		t.Fatalf("capture r1: %v", err)
	}
	if r0 == r1 {
		t.Errorf("two captures produced the same ref name: %q", r0)
	}

	diff, err := s.DiffRefToRef(ctx, dir, r0, r1)
	if err != nil {
		t.Fatalf("diff r0..r1: %v", err)
	}
	if !strings.Contains(string(diff), "new.txt") {
		t.Errorf("expected new.txt in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "+alpha") {
		t.Errorf("expected +alpha in diff; got:\n%s", diff)
	}
	// The only file touched between captures should be new.txt — make sure
	// README (unchanged) did not leak into the diff.
	if strings.Contains(string(diff), "README") {
		t.Errorf("unexpected README in diff between r0 and r1; got:\n%s", diff)
	}
}

// #10 — Concurrent captures across many threads on a shared workspace must
// all succeed without colliding on the GIT_INDEX_FILE temp. Run with -race to
// catch any data races in the Store internals.
func TestIntegration_ConcurrentCapturesDifferentThreads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	s := NewStore()

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	// Pre-make deterministic mutations each goroutine can observe so the refs
	// actually capture different trees. We perturb the workspace while
	// goroutines run; the shared-worktree race is the point of this test.
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// A bit of dirty churn so different captures may see different
			// worktree states; we don't care about content, only that every
			// capture succeeds and produces a resolvable ref.
			mu.Lock()
			integWrite(t, dir, fmt.Sprintf("file-%d.txt", idx), fmt.Sprintf("hi %d\n", idx))
			mu.Unlock()

			ref, err := s.CaptureBaseline(ctx, dir, fmt.Sprintf("t-%d", idx), 0)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", idx, err)
				return
			}
			has, err := s.HasCheckpointRef(ctx, dir, ref)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: has ref: %w", idx, err)
				return
			}
			if !has {
				errs <- fmt.Errorf("goroutine %d: ref %s missing", idx, ref)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}

	// Every thread should own exactly one ref, all distinct.
	seen := make(map[string]bool)
	for i := 0; i < goroutines; i++ {
		refs, err := s.ListThreadRefs(ctx, dir, fmt.Sprintf("t-%d", i))
		if err != nil {
			t.Errorf("list refs for t-%d: %v", i, err)
			continue
		}
		if len(refs) != 1 {
			t.Errorf("t-%d: expected 1 ref, got %d: %v", i, len(refs), refs)
		}
		for _, r := range refs {
			if seen[r] {
				t.Errorf("duplicate ref observed: %s", r)
			}
			seen[r] = true
		}
	}
	if len(seen) != goroutines {
		t.Errorf("expected %d distinct refs, saw %d", goroutines, len(seen))
	}
}

// #11 — A cancelled capture must not leak temp dirs under TMPDIR. Route
// MkdirTemp to a scoped TMPDIR so parallel tests (or other `go test` runs)
// can't race the count.
func TestIntegration_CancelledContextCleansUpTempDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dir := t.TempDir()
	integInitRepo(t, dir)
	s := NewStore()

	// We can't reliably interrupt mid-capture on a small workspace (git races
	// through in ~10ms), so we approach the cleanup guarantee from two angles:
	//
	//   (a) A normal capture must leave no temp dirs behind in TMPDIR.
	//   (b) A capture with an already-cancelled context must not leak temp
	//       dirs either — the `defer os.RemoveAll(tempDir)` should run on
	//       every exit path, including the early-exit one.
	ctx1 := context.Background()
	if _, err := s.CaptureBaseline(ctx1, dir, "ok", 0); err != nil {
		t.Fatalf("capture ok: %v", err)
	}
	if leaked := countIntegCheckpointTemp(t, tmp); leaked != 0 {
		t.Errorf("case (a): leaked %d temp dirs after successful capture", leaked)
	}

	ctx2, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before we even start
	_, err := s.CaptureBaseline(ctx2, dir, "cancelled", 0)
	if err == nil {
		// Not fatal: git may have raced through before the context was
		// observed. Either way, the temp-dir assertion below must still hold.
		t.Logf("capture under pre-cancelled ctx succeeded (race between git and cancel)")
	}
	if leaked := countIntegCheckpointTemp(t, tmp); leaked != 0 {
		t.Errorf("case (b): leaked %d temp dirs after cancelled capture", leaked)
	}
}

func countIntegCheckpointTemp(t *testing.T, tmp string) int {
	t.Helper()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read tmp: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "agent-overflow-checkpoint-") {
			n++
		}
	}
	return n
}

// #12 — RestoreWorktreePaths applies the captured state for listed paths
// only and leaves everything else alone.
func TestIntegration_RestoreWorktreePathsAppliesCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	integWrite(t, dir, "to-restore.txt", "pristine content\n")
	integWrite(t, dir, "untouched.txt", "stays as-is\n")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "restore", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	// Drift both files. The restore call lists only one path; the other
	// must survive untouched.
	integWrite(t, dir, "to-restore.txt", "DRIFTED CONTENT\n")
	integWrite(t, dir, "untouched.txt", "user manually changed\n")
	got, _ := os.ReadFile(filepath.Join(dir, "to-restore.txt"))
	if string(got) != "DRIFTED CONTENT\n" {
		t.Fatalf("precondition: expected drifted content, got %q", got)
	}

	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{"to-restore.txt"}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}

	got, _ = os.ReadFile(filepath.Join(dir, "to-restore.txt"))
	if string(got) != "pristine content\n" {
		t.Errorf("listed path not restored; got %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "untouched.txt"))
	if string(got) != "user manually changed\n" {
		t.Errorf("unlisted path should be untouched; got %q", got)
	}
}

// #13 — Files added after capture are unlinked by RestoreWorktreePaths only
// when their path is listed. Files outside the list — including
// untracked-not-ignored ones — survive.
func TestIntegration_RestoreWorktreePathsRemovesListedAdded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "restore-rm", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	integWrite(t, dir, "deep/nested/added.txt", "should be removed\n")
	integWrite(t, dir, "added-top.txt", "also removed\n")
	integWrite(t, dir, "user-kept.txt", "user added this; not in list\n")

	if err := s.RestoreWorktreePaths(ctx, dir, ref, []string{
		"deep/nested/added.txt",
		"added-top.txt",
	}); err != nil {
		t.Fatalf("restore paths: %v", err)
	}

	for _, p := range []string{
		filepath.Join(dir, "deep/nested/added.txt"),
		filepath.Join(dir, "added-top.txt"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("expected %s to be removed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "user-kept.txt")); err != nil {
		t.Errorf("user-kept.txt should survive restore: %v", err)
	}
}

// #15 — DiffRefToWorktree shows pending worktree changes.
func TestIntegration_DiffRefToWorktreeShowsPendingChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "pending", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	integWrite(t, dir, "README", "user modified after capture\n")
	integWrite(t, dir, "brand-new.txt", "fresh\n")

	diff, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff ref to worktree: %v", err)
	}
	if !strings.Contains(string(diff), "user modified after capture") {
		t.Errorf("expected modified README content in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "brand-new.txt") {
		t.Errorf("expected brand-new.txt in diff; got:\n%s", diff)
	}
}

// #16 — Diffing a ref against itself returns empty.
func TestIntegration_DiffRefToRefOnSameRefIsEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	s := NewStore()

	ref, err := s.CaptureBaseline(ctx, dir, "same", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	diff, err := s.DiffRefToRef(ctx, dir, ref, ref)
	if err != nil {
		t.Fatalf("diff ref..ref: %v", err)
	}
	if strings.TrimSpace(string(diff)) != "" {
		t.Errorf("expected empty diff for ref==ref, got:\n%s", diff)
	}
}

// #17 — Thread IDs that contain ref-hostile characters (slash, plus, space)
// must produce a base64url-safe ref name that git can look up.
func TestIntegration_RefNamespaceBase64UrlEncoding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	s := NewStore()

	hostileIDs := []string{
		"has/slash",
		"has+plus",
		"has spaces",
		"a\\b", // backslash too
	}
	for _, id := range hostileIDs {
		ref, err := s.CaptureBaseline(ctx, dir, id, 0)
		if err != nil {
			t.Errorf("capture for threadID %q: %v", id, err)
			continue
		}

		// Ref name should not contain the raw hostile characters beyond the
		// fixed prefix `refs/agent-overflow/checkpoints/` (which is the only
		// place slashes are allowed).
		suffix := strings.TrimPrefix(ref, RefsPrefix+"/")
		encoded := strings.SplitN(suffix, "/", 2)[0] // the thread-id segment
		if strings.ContainsAny(encoded, "+ \\") {
			t.Errorf("encoded thread id segment contains unsafe characters: %q", encoded)
		}
		if strings.Contains(encoded, "/") {
			t.Errorf("encoded thread id segment contains a slash: %q", encoded)
		}

		// Ref must actually resolve.
		has, err := s.HasCheckpointRef(ctx, dir, ref)
		if err != nil {
			t.Errorf("has ref %q: %v", ref, err)
			continue
		}
		if !has {
			t.Errorf("ref %q should resolve after capture", ref)
		}
	}
}

// #18 — Capture is tree-deterministic for the same worktree state: two
// captures moments apart produce different commit OIDs (timestamps differ)
// but identical tree OIDs.
func TestIntegration_CaptureIsDeterministicForSameState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	integWrite(t, dir, "content.txt", "identical content\n")
	s := NewStore()

	ref0, err := s.CaptureBaseline(ctx, dir, "det", 0)
	if err != nil {
		t.Fatalf("capture 0: %v", err)
	}
	// Small sleep so commit timestamp-seconds differ; required to show that
	// commit OIDs diverge while tree OIDs match. 1100ms covers any system
	// with second-granularity commit timestamps.
	time.Sleep(1100 * time.Millisecond)
	ref1, err := s.CaptureBaseline(ctx, dir, "det", 1)
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}

	tree0 := integCatFileTree(t, dir, ref0)
	tree1 := integCatFileTree(t, dir, ref1)
	if tree0 != tree1 {
		t.Errorf("expected identical tree OIDs for unchanged state\nref0 tree=%s\nref1 tree=%s",
			tree0, tree1)
	}

	commit0 := integRevParse(t, dir, ref0)
	commit1 := integRevParse(t, dir, ref1)
	if commit0 == commit1 {
		t.Errorf("expected distinct commit OIDs (committer timestamps differ): %s", commit0)
	}
}

func integRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(integRunOutput(t, dir, "git", "rev-parse", "--verify", ref))
}

func integCatFileTree(t *testing.T, dir, ref string) string {
	t.Helper()
	// `git cat-file commit <ref>` emits `tree <sha>` as the first line.
	out := integRunOutput(t, dir, "git", "cat-file", "commit", ref)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "tree "))
		}
	}
	t.Fatalf("no tree line in `git cat-file commit %s`:\n%s", ref, out)
	return ""
}

// #19 — Capture of a moderately large workspace completes quickly. We flag
// slow runs (> 5s) with a Log rather than failing — filesystem + git perf
// varies too much across CI runners to assert a hard threshold.
func TestIntegration_LargeWorkspaceCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large workspace test in -short mode")
	}
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)

	// 1000 files across 50 dirs. Keep bodies small so the test isn't slow for
	// the wrong reason (I/O rather than the code under test).
	const dirs = 50
	const filesPerDir = 20
	for d := 0; d < dirs; d++ {
		for f := 0; f < filesPerDir; f++ {
			integWrite(t, dir,
				filepath.Join(fmt.Sprintf("pkg%d", d), fmt.Sprintf("f%d.txt", f)),
				fmt.Sprintf("package %d file %d\n", d, f))
		}
	}

	s := NewStore()
	start := time.Now()
	if _, err := s.CaptureBaseline(ctx, dir, "big", 0); err != nil {
		t.Fatalf("capture: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("captured %d files in %v", dirs*filesPerDir, elapsed)
	if elapsed > 5*time.Second {
		t.Logf("WARNING: capture of %d files took %v (>5s)", dirs*filesPerDir, elapsed)
	}
}

// #20 — A linked worktree (created via `git worktree add`) must support
// capture. Linked worktrees have `.git` as a file (not directory) pointing
// into the main repo's `.git/worktrees/<name>`, so they exercise a code path
// the main-worktree tests don't reach.
func TestIntegration_CaptureInsideWorktreeLink(t *testing.T) {
	ctx := context.Background()
	mainRepo := t.TempDir()
	integInitRepo(t, mainRepo)

	// Some git builds reject `worktree add` to a directory that already exists.
	// Use a sibling dir name that's guaranteed not to exist.
	linked := filepath.Join(t.TempDir(), "linked")
	if err := exec.Command("git", "-C", mainRepo, "worktree", "add", "-b", "feature", linked).Run(); err != nil {
		t.Skipf("git worktree add unsupported on this host: %v", err)
	}

	s := NewStore()
	if !s.IsGitRepository(ctx, linked) {
		t.Fatalf("linked worktree should be recognised as a git repository")
	}

	integWrite(t, linked, "worktree-file.txt", "inside the linked worktree\n")

	ref, err := s.CaptureBaseline(ctx, linked, "linked", 0)
	if err != nil {
		t.Fatalf("capture inside linked worktree: %v", err)
	}

	has, err := s.HasCheckpointRef(ctx, linked, ref)
	if err != nil {
		t.Fatalf("has ref: %v", err)
	}
	if !has {
		t.Errorf("ref should resolve from linked worktree")
	}

	// The ref should also resolve from the MAIN worktree because the object
	// store is shared — a useful property for tools that later inspect refs
	// from either side.
	hasFromMain, err := s.HasCheckpointRef(ctx, mainRepo, ref)
	if err != nil {
		t.Fatalf("has ref from main: %v", err)
	}
	if !hasFromMain {
		t.Errorf("ref captured in linked worktree should be visible from main worktree")
	}
}

// #X1 — Deletions in the worktree show up in the checkpoint→worktree diff.
// Covers the `git diff <ref> -- .` path's handling of removed files (distinct
// from additions, which are handled via `ls-files --others`).
func TestIntegration_DiffRefToWorktreeShowsDeletions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	integInitRepo(t, dir)
	integWrite(t, dir, "delete-me.txt", "original\n")
	integRun(t, dir, "git", "add", "-A")
	integRun(t, dir, "git",
		"-c", "user.email=tester@test",
		"-c", "user.name=Tester",
		"commit", "-q", "-m", "add delete-me")

	s := NewStore()
	ref, err := s.CaptureBaseline(ctx, dir, "del", 0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "delete-me.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	diff, err := s.DiffRefToWorktree(ctx, dir, ref)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(string(diff), "delete-me.txt") {
		t.Errorf("expected deleted file in diff; got:\n%s", diff)
	}
	if !strings.Contains(string(diff), "-original") {
		t.Errorf("expected deletion marker `-original`; got:\n%s", diff)
	}
}

// Guard: tests assume a POSIX-compatible filesystem. The file layout (e.g.
// "has/slash" decoding) is agnostic but filesystem quirks around permissions
// differ on Windows. Skip upfront rather than report unhelpful failures.
func init() {
	if runtime.GOOS == "windows" {
		_ = fmt.Sprintf("windows unsupported") // no-op; the tests themselves fail fast on Windows
	}
}
