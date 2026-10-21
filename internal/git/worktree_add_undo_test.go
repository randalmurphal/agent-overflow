package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
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
	registered, err := core.worktreeRegisteredAt(repo, path)
	if err != nil || !registered {
		t.Fatalf("the existing worktree was removed (%v)", err)
	}
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

// holdBeforeAdd makes each core hold every checkout between its
// registration snapshot and its add until two checkouts reached that
// point or wait elapsed, and reports whether the wait ever elapsed.
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
// attach is held after its registration snapshot until the other
// reaches it too, which only an attach not waiting on the other's whole
// operation can.
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
	registered, err := core.worktreeRegisteredAt(repo, path)
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
