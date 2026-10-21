package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// failCheckoutHook makes every checkout in repo fail after git has
// written the worktree: a post-checkout hook that exits non-zero, which
// git reports as the add's failure while leaving the worktree registered.
func failCheckoutHook(t *testing.T, repo string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hook is unix-only")
	}
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\necho injected checkout failure >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "config", "core.hooksPath", hooks)
}

// occupiedPath is a non-empty directory git refuses to add a worktree at.
func occupiedPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertAddUndone fails unless repo's main checkout is its only worktree
// and branch exists exactly when wantBranch.
func assertAddUndone(t *testing.T, repo, branch string, wantBranch bool) {
	t.Helper()
	core := NewCore()
	worktrees, err := core.ListWorktrees(repo)
	if err != nil || len(worktrees) != 1 {
		t.Fatalf("worktrees = %+v (%v), want the main checkout alone", worktrees, err)
	}
	exists, err := core.branchExistsChecked(repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	if exists != wantBranch {
		t.Fatalf("branch %s exists = %v, want %v", branch, exists, wantBranch)
	}
}

// worktreeRegisteredAt reports whether git lists a worktree of the
// repository at cwd at path.
func worktreeRegisteredAt(cwd, path string) (bool, error) {
	worktrees, err := NewCore().ListWorktrees(cwd)
	if err != nil {
		return false, err
	}
	for _, worktree := range worktrees {
		if SameFilesystemPath(worktree.Path, path) {
			return true, nil
		}
	}
	return false, nil
}

func assertOccupiedPathKept(t *testing.T, path string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(path, "keep.txt")); err != nil || string(data) != "mine\n" {
		t.Fatalf("the occupied destination changed: %q, %v", data, err)
	}
}

// A branch only origin has is made locally for the worktree; a failed
// checkout deletes it and leaves no worktree, and an occupied destination
// is left as it was.
func TestAttachWorktreeOfAnOriginBranchUndoesAFailedCheckout(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo, bare := testutil.InitGitRepoWithOrigin(t)
		originOnlyBranch(t, bare, "feature/mr")
		testutil.RunGit(t, repo, "fetch", "-q", "origin")
		path := filepath.Join(t.TempDir(), "mr")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		created, err := NewCore().AttachWorktree(repo, path, "feature/mr")
		if err == nil || created {
			t.Fatalf("%s: AttachWorktree = created %v, %v; want a failure that made nothing", failure, created, err)
		}
		if strings.Contains(err.Error(), "undo") {
			t.Fatalf("%s: the undo failed: %v", failure, err)
		}
		assertAddUndone(t, repo, "feature/mr", false)
		if failure == "occupied" {
			assertOccupiedPathKept(t, path)
		}
	}
}

// A local branch the attach did not make survives its failed checkout.
func TestAttachWorktreeOfALocalBranchKeepsItWhenTheCheckoutFails(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "branch", "feature/local")
		path := filepath.Join(t.TempDir(), "local")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		created, err := NewCore().AttachWorktree(repo, path, "feature/local")
		if err == nil || created {
			t.Fatalf("%s: AttachWorktree = created %v, %v; want a failure", failure, created, err)
		}
		assertAddUndone(t, repo, "feature/local", true)
	}
}

// An attach git refuses because the branch is already checked out at the
// path leaves that worktree registered: it predates the call.
func TestAttachWorktreeKeepsAWorktreeThatWasAlreadyThere(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	core := NewCore()
	testutil.RunGit(t, repo, "branch", "feature/there")
	path := filepath.Join(t.TempDir(), "there")
	if _, err := core.AttachWorktree(repo, path, "feature/there"); err != nil {
		t.Fatal(err)
	}

	if _, err := core.AttachWorktree(repo, path, "feature/there"); err == nil {
		t.Fatal("a second attach at the same path succeeded")
	}
	registered, err := worktreeRegisteredAt(repo, path)
	if err != nil || !registered {
		t.Fatalf("the existing worktree was removed (%v)", err)
	}
}

// Another process (a terminal, another app process) that checks the
// branch out at the path between this attach's branch checks and its add
// keeps its checkout, untracked work included: the attach git refuses
// removes only a registration locked with its own reason.
func TestAttachWorktreeKeepsAnotherProcessesCheckoutAtThePath(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/theirs")
	path := filepath.Join(t.TempDir(), "theirs")
	core := NewCore()
	core.beforeWorktreeAdd = func(string) {
		testutil.RunGit(t, repo, "worktree", "add", "--", path, "feature/theirs")
		if err := os.WriteFile(filepath.Join(path, "work.txt"), []byte("theirs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := core.AttachWorktree(repo, path, "feature/theirs"); err == nil {
		t.Fatal("the attach succeeded over another process's checkout")
	} else if strings.Contains(err.Error(), "undo") {
		t.Fatalf("the refused attach undid something: %v", err)
	}
	registered, err := worktreeRegisteredAt(repo, path)
	if err != nil || !registered {
		t.Fatalf("the other process's worktree was removed (%v)", err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "work.txt")); err != nil || string(data) != "theirs\n" {
		t.Fatalf("the other process's untracked work changed: %q, %v", data, err)
	}
}

// A checkout that succeeded drops the lock its add took.
func TestCheckoutsLeaveTheWorktreeUnlocked(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	core := NewCore()
	testutil.RunGit(t, repo, "branch", "feature/open")
	attached := filepath.Join(t.TempDir(), "attached")
	if _, err := core.AttachWorktree(repo, attached, "feature/open"); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(t.TempDir(), "created")
	if err := core.CreateWorktreeFromBranch(repo, created, "", "ao-open"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{attached, created} {
		if locked, err := core.worktreeLocked(repo, path); err != nil || locked {
			t.Fatalf("%s locked = %v (%v), want unlocked", path, locked, err)
		}
	}
}

// A checkout whose unlock fails is a failed checkout: the worktree and
// the branch the attach made from origin's are taken back, and the error
// names the unlock. The fake git fails the first unlock alone, so the
// undo's own unlock runs.
func TestAttachWorktreeUndoesACheckoutItCouldNotUnlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper is unix-only")
	}
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	originOnlyBranch(t, bare, "feature/held")
	testutil.RunGit(t, repo, "fetch", "-q", "origin")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	mockexec.Write(t, filepath.Join(binDir, "git"), `#!/bin/sh
if [ "$1" = worktree ] && [ "$2" = unlock ] && [ ! -f "$AO_UNLOCK_FAILED" ]; then
  : > "$AO_UNLOCK_FAILED"
  echo "error: injected unlock failure" >&2
  exit 1
fi
exec "$AO_REAL_GIT" "$@"
`)
	t.Setenv("AO_REAL_GIT", realGit)
	t.Setenv("AO_UNLOCK_FAILED", filepath.Join(t.TempDir(), "unlock-failed"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "held")

	created, err := NewCore().AttachWorktree(repo, path, "feature/held")
	if err == nil || created || !strings.Contains(err.Error(), "injected unlock failure") {
		t.Fatalf("AttachWorktree = created %v, %v; want the unlock's failure", created, err)
	}
	if strings.Contains(err.Error(), "undo") {
		t.Fatalf("the undo failed: %v", err)
	}
	assertAddUndone(t, repo, "feature/held", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the checkout directory remains (%v)", err)
	}
}

// A checkout a crash left locked with this package's reason, between the
// add and the unlock, is listed and removable.
func TestRemoveWorktreeLiftsTheLockOfAnInterruptedCheckout(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/crashed")
	path := filepath.Join(t.TempDir(), "crashed")
	testutil.RunGit(t, repo, "worktree", "add", "--lock", "--reason", checkoutLockReason+"00000000-0000-0000-0000-000000000000", "--", path, "feature/crashed")
	core := NewCore()
	if locked, err := core.worktreeLocked(repo, path); err != nil || !locked {
		t.Fatalf("worktree locked = %v (%v)", locked, err)
	}
	if registered, err := worktreeRegisteredAt(repo, path); err != nil || !registered {
		t.Fatalf("ListWorktrees does not show the locked checkout (%v)", err)
	}
	if err := core.RemoveWorktree(repo, path); err != nil {
		t.Fatalf("RemoveWorktree of the locked checkout: %v", err)
	}
	assertAddUndone(t, repo, "feature/crashed", true)
}

// A new branch cut for a worktree is deleted when its checkout fails, and
// the failure is the checkout's, not a duplicate branch.
func TestCreateWorktreeUndoesTheBranchOfAFailedCheckout(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"occupied", "hook"} {
		repo, _ := testutil.InitGitRepoWithOrigin(t)
		path := filepath.Join(t.TempDir(), "cut")
		if failure == "occupied" {
			path = occupiedPath(t)
		} else {
			failCheckoutHook(t, repo)
		}

		err := NewCore().CreateWorktreeFromBranch(repo, path, "main", "ao-cut")
		if err == nil {
			t.Fatalf("%s: CreateWorktreeFromBranch succeeded", failure)
		}
		if strings.Contains(err.Error(), `branch "ao-cut" already exists`) || strings.Contains(err.Error(), "undo") {
			t.Fatalf("%s: CreateWorktreeFromBranch = %v, want the checkout's failure and a clean undo", failure, err)
		}
		assertAddUndone(t, repo, "ao-cut", false)
		if failure == "occupied" {
			assertOccupiedPathKept(t, path)
		}

		// The fresh-base cut from origin takes the same path.
		_, err = NewCore().CreateWorktreeFromFreshBase(t.Context(), repo, path, "main", "ao-fresh")
		if err == nil {
			t.Fatalf("%s: CreateWorktreeFromFreshBase succeeded", failure)
		}
		assertAddUndone(t, repo, "ao-fresh", false)
	}
}

// A cut whose branch already exists fails before it makes anything and
// leaves that branch alone.
func TestCreateWorktreeKeepsAnExistingBranch(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "ao-taken")
	err := NewCore().CreateWorktreeFromBranch(repo, filepath.Join(t.TempDir(), "wt"), "", "ao-taken")
	if err == nil || !strings.Contains(err.Error(), `branch "ao-taken" already exists`) {
		t.Fatalf("CreateWorktreeFromBranch = %v, want the duplicate branch refusal", err)
	}
	assertAddUndone(t, repo, "ao-taken", true)
}

// holdBeforeAdd makes each core hold every checkout just before its add
// until two checkouts reached that point or wait elapsed, and reports
// whether the wait ever elapsed.
func holdBeforeAdd(wait time.Duration, cores ...*Core) (elapsed *atomic.Bool) {
	var arrived atomic.Int32
	both := make(chan struct{})
	elapsed = &atomic.Bool{}
	for _, core := range cores {
		core.beforeWorktreeAdd = func(string) {
			if arrived.Add(1) == 2 {
				close(both)
			}
			select {
			case <-both:
			case <-time.After(wait):
				elapsed.Store(true)
			}
		}
	}
	return elapsed
}

// Two attaches of one branch to one path, from two Cores: one wins, and
// the one git refuses leaves the winner's checkout as it was. Each
// attach is held before its add until the other reaches it too, which
// only an attach not waiting on the other's whole operation can.
func TestCompetingAttachesOfOneBranchKeepTheWinnersWorktree(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/race")
	path := filepath.Join(t.TempDir(), "race")
	cores := []*Core{NewCore(), NewCore()}
	holdBeforeAdd(500*time.Millisecond, cores...)

	errs := make([]error, len(cores))
	var wg sync.WaitGroup
	for i, core := range cores {
		wg.Go(func() { _, errs[i] = core.AttachWorktree(repo, path, "feature/race") })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("attaches = %v, %v; want exactly one to succeed", errs[0], errs[1])
	}
	for _, err := range errs {
		if err != nil && strings.Contains(err.Error(), "undo") {
			t.Fatalf("the refused attach undid something: %v", err)
		}
	}
	core := NewCore()
	registered, err := worktreeRegisteredAt(repo, path)
	if err != nil || !registered {
		t.Fatalf("the winner's worktree is not registered (%v)", err)
	}
	head, _, err := core.Execute(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(head) != "feature/race" {
		t.Fatalf("the winner's worktree is on %q (%v)", head, err)
	}
	status, _, err := core.Execute(path, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("the winner's worktree changed: %q (%v)", status, err)
	}
}

// The checkout lock is per repository: creates in two repositories run
// at once.
func TestCreatesInTwoRepositoriesDoNotWaitOnEachOther(t *testing.T) {
	t.Parallel()
	repos := []string{initGitRepo(t), initGitRepo(t)}
	cores := []*Core{NewCore(), NewCore()}
	elapsed := holdBeforeAdd(5*time.Second, cores...)

	errs := make([]error, len(cores))
	paths := []string{filepath.Join(t.TempDir(), "wt"), filepath.Join(t.TempDir(), "wt")}
	var wg sync.WaitGroup
	for i, core := range cores {
		wg.Go(func() {
			errs[i] = core.CreateWorktreeFromBranch(repos[i], paths[i], "", fmt.Sprintf("ao-cut-%d", i))
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if elapsed.Load() {
		t.Fatal("a create in one repository waited for a create in another")
	}
}
