//go:build unix

package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/threadtools"
)

// What a spawn whose request loses its naming takes back: only what is
// still its own. The checkout it cut goes unless something in it is not
// the cut's, the branch it made goes unless it moved, and a group it made
// goes unless something joined it.

// gateCheckoutHook installs a post-checkout hook in repo that holds every
// checkout until release runs. started receives the checkout's path once
// the hook is running inside its `git worktree add`.
func gateCheckoutHook(t *testing.T, repo string) (started <-chan string, release func()) {
	t.Helper()
	dir := t.TempDir()
	startedPipe, gatePipe := filepath.Join(dir, "started"), filepath.Join(dir, "gate")
	for _, pipe := range []string{startedPipe, gatePipe} {
		if err := syscall.Mkfifo(pipe, 0o600); err != nil {
			t.Fatalf("make the hook's pipe: %v", err)
		}
	}
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\npwd -P > '" + startedPipe + "'\nread gate < '" + gatePipe + "'\n"
	mockexec.WriteFile(t, filepath.Join(hooks, "post-checkout"), []byte(script), 0o755)
	testutil.RunGit(t, repo, "config", "core.hooksPath", hooks)
	paths := make(chan string, 1)
	go func() {
		// Opening a pipe waits for its other end: this read returns once
		// the hook has written the checkout's path.
		if out, err := os.ReadFile(startedPipe); err == nil {
			paths <- strings.TrimSpace(string(out))
		}
	}()
	release = sync.OnceFunc(func() {
		if err := os.WriteFile(gatePipe, []byte("go\n"), 0); err != nil {
			t.Errorf("release the checkout hook: %v", err)
		}
	})
	return paths, release
}

// cancelSpawnDuringItsCheckout spawns into a fresh worktree of branch,
// runs during on the checkout while its hook runs, cancels the spawn by
// its token, then lets the hook finish, and returns the checkout's path
// and what the spawn returned.
func cancelSpawnDuringItsCheckout(t *testing.T, f *requestFixture, repo, branch string, during func(checkout string)) (string, error) {
	t.Helper()
	started, release := gateCheckoutHook(t, repo)
	spawned := make(chan error, 1)
	go func() {
		_, err := f.adapter().Spawn(context.WithoutCancel(t.Context()), f.callerIdentity(),
			threadtools.SpawnCall{Prompt: "work in a fresh checkout", WorktreeBranch: branch})
		spawned <- err
	}()
	var checkout string
	select {
	case checkout = <-started:
	case err := <-spawned:
		t.Fatalf("the spawn ended before its checkout hook ran: %v", err)
	}
	defer release()
	during(checkout)
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 4, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the spawn's request: %d rows, %v", len(rows), err)
	}
	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: rows[0].Token}); err != nil {
		t.Fatalf("cancel the spawn while its checkout hook runs: %v", err)
	}
	release()
	return checkout, <-spawned
}

// A spawn cancelled while its checkout hook runs takes back the checkout
// and the branch its cut made when nothing else touched them, keeps a
// checkout holding a file written meanwhile with its branch, and keeps a
// branch that moved; the spawner is told what stayed and why.
func TestACancelledSpawnTakesBackOnlyWhatItsCutStillOwns(t *testing.T) {
	t.Parallel()
	t.Run("untouched", func(t *testing.T) {
		t.Parallel()
		repo, _ := initGitRepoWithOrigin(t)
		f := newRequestFixtureIn(t, repo)
		f.mockClaude(t, "never asked")
		const branch = "untouched-cut"
		checkout, err := cancelSpawnDuringItsCheckout(t, f, repo, branch, func(string) {})
		if err == nil {
			t.Fatal("a spawn settled while its thread was made was accepted")
		}
		if strings.Contains(err.Error(), "kept") {
			t.Errorf("an untouched cut reported something kept: %v", err)
		}
		if _, statErr := os.Stat(checkout); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the checkout %s is still there: %v", checkout, statErr)
		}
		if path := worktreeOfBranch(t, repo, branch); path != "" {
			t.Errorf("git still lists the worktree %s", path)
		}
		if branchExists(repo, branch) {
			t.Errorf("the branch %s the cut made is still there", branch)
		}
	})
	t.Run("a file written while the hook runs", func(t *testing.T) {
		t.Parallel()
		repo, _ := initGitRepoWithOrigin(t)
		f := newRequestFixtureIn(t, repo)
		f.mockClaude(t, "never asked")
		const branch = "written-during-cut"
		var notes string
		checkout, err := cancelSpawnDuringItsCheckout(t, f, repo, branch, func(checkout string) {
			notes = filepath.Join(checkout, "notes.txt")
			if err := os.WriteFile(notes, []byte("mine\n"), 0o644); err != nil {
				t.Fatalf("write into the checkout: %v", err)
			}
		})
		if err == nil {
			t.Fatal("a spawn settled while its thread was made was accepted")
		}
		if got, readErr := os.ReadFile(notes); readErr != nil || string(got) != "mine\n" {
			t.Fatalf("the file written into the checkout = %q, %v; want it kept", got, readErr)
		}
		if path := worktreeOfBranch(t, repo, branch); path == "" {
			t.Error("git no longer lists the kept checkout")
		}
		if !branchExists(repo, branch) {
			t.Errorf("the branch %s checked out in the kept checkout was deleted", branch)
		}
		for _, want := range []string{checkout, branch, "1 uncommitted or untracked file"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the spawn's refusal %q does not name %q", err, want)
			}
		}
	})
	t.Run("a commit made while the hook runs", func(t *testing.T) {
		t.Parallel()
		repo, _ := initGitRepoWithOrigin(t)
		f := newRequestFixtureIn(t, repo)
		f.mockClaude(t, "never asked")
		const branch = "committed-during-cut"
		checkout, err := cancelSpawnDuringItsCheckout(t, f, repo, branch, func(checkout string) {
			if err := os.WriteFile(filepath.Join(checkout, "work.txt"), []byte("work\n"), 0o644); err != nil {
				t.Fatalf("write into the checkout: %v", err)
			}
			testutil.RunGit(t, checkout, "add", "work.txt")
			testutil.RunGit(t, checkout, "-c", "core.hooksPath=/dev/null", "commit", "-m", "work")
		})
		if err == nil {
			t.Fatal("a spawn settled while its thread was made was accepted")
		}
		// The checkout is clean, so it goes; the commit is on the branch,
		// which stays.
		if _, statErr := os.Stat(checkout); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the clean checkout %s is still there: %v", checkout, statErr)
		}
		if !branchExists(repo, branch) {
			t.Fatalf("the branch %s holding a commit made meanwhile was deleted", branch)
		}
		if !strings.Contains(err.Error(), branch) || !strings.Contains(err.Error(), "moved") {
			t.Errorf("the spawn's refusal %q does not say branch %s was kept because it moved", err, branch)
		}
	})
}

// A spawn whose naming loses drops the group it made for its thread, when
// that group is still empty. A group that existed before, or that gained a
// member meanwhile, stays.
func TestASpawnWhoseNamingLosesDropsOnlyTheGroupItMadeEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// before runs before the spawn, during while it is held at naming.
		before, during func(t *testing.T, f *requestFixture, group string)
		kept           bool
	}{
		{name: "made by the spawn", kept: false},
		{name: "there before", kept: true, before: func(t *testing.T, f *requestFixture, group string) {
			if _, err := f.app.store.CreateThreadGroup(f.caller.ProjectID, group); err != nil {
				t.Fatalf("create the group: %v", err)
			}
		}},
		{name: "joined meanwhile", kept: true, during: func(t *testing.T, f *requestFixture, group string) {
			made, _, err := f.app.store.EnsureThreadGroup(f.caller.ProjectID, group)
			if err != nil {
				t.Fatalf("read the group the spawn made: %v", err)
			}
			if _, err := f.app.store.SetThreadGroup([]string{f.caller.ID}, made.ID); err != nil {
				t.Fatalf("move a thread into the group: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := initGitRepoWithOrigin(t)
			f := newRequestFixtureIn(t, repo)
			f.mockClaude(t, "never asked")
			const group = "sweep"
			if c.before != nil {
				c.before(t, f, group)
			}
			naming, release := make(chan struct{}), make(chan struct{})
			released := sync.OnceFunc(func() { close(release) })
			t.Cleanup(released)
			f.app.spawnNamingForTest = func(string) {
				close(naming)
				<-release
			}
			spawned := make(chan error, 1)
			go func() {
				_, err := f.adapter().Spawn(context.WithoutCancel(t.Context()), f.callerIdentity(),
					threadtools.SpawnCall{Prompt: "work in a group", Group: group})
				spawned <- err
			}()
			select {
			case <-naming:
			case err := <-spawned:
				t.Fatalf("the spawn ended before naming its thread: %v", err)
			}
			if c.during != nil {
				c.during(t, f, group)
			}
			rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 4, 0)
			if err != nil || len(rows) != 1 {
				t.Fatalf("the spawn's request: %d rows, %v", len(rows), err)
			}
			if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: rows[0].Token}); err != nil {
				t.Fatalf("cancel the spawn while it is named: %v", err)
			}
			released()
			if err := <-spawned; err == nil {
				t.Fatal("a spawn settled while its thread was made was accepted")
			}
			if kept := projectHasGroup(t, f.app.store, f.caller.ProjectID, group); kept != c.kept {
				t.Fatalf("group %q kept = %v, want %v", group, kept, c.kept)
			}
		})
	}
}

func projectHasGroup(t *testing.T, s *store.Store, projectID, name string) bool {
	t.Helper()
	groups, err := s.ListThreadGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for _, group := range groups {
		if group.ProjectID == projectID && group.Name == name {
			return true
		}
	}
	return false
}
