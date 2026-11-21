package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// hookedOperation is one git operation that runs a user's hook (or, for the
// forge CLIs' create, a process that pushes and so runs one): install puts
// script where the operation runs it, run performs the operation.
type hookedOperation struct {
	install func(script string)
	run     func(core *Core) error
	// options build the Core the operation runs on.
	options []CoreOption
}

func installHook(t *testing.T, repo, name, script string) {
	t.Helper()
	hook := filepath.Join(repo, ".git", "hooks", name)
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// withOrigin gives repo a bare origin holding main, tracked by main.
func withOrigin(t *testing.T, repo string) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t, repo, "remote", "add", "origin", remote)
	testutil.RunGit(t, repo, "push", "--quiet", "-u", "origin", "main")
	return remote
}

// hookedOperations are the operations a person waits on that run hooks.
func hookedOperations() map[string]func(t *testing.T) hookedOperation {
	return map[string]func(t *testing.T) hookedOperation{
		"commit all": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			writeRepoFile(t, repo, "README.txt", "hello\ncommitted\n")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "pre-commit", script) },
				run: func(core *Core) error {
					_, err := core.CommitSelected(repo, "all", "", CommitSelection{All: true})
					return err
				},
			}
		},
		"commit files": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			writeRepoFile(t, repo, "README.txt", "hello\ncommitted\n")
			selection := listedSelection(t, repo, "README.txt")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "pre-commit", script) },
				run: func(core *Core) error {
					_, err := core.CommitSelected(repo, "files", "", selection)
					return err
				},
			}
		},
		"push": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			withOrigin(t, repo)
			writeRepoFile(t, repo, "README.txt", "hello\npushed\n")
			testutil.RunGit(t, repo, "commit", "--quiet", "-am", "to push")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "pre-push", script) },
				run:     func(core *Core) error { return core.Push(repo) },
			}
		},
		"pull": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			remote := withOrigin(t, repo)
			other := t.TempDir()
			testutil.RunGit(t, other, "clone", "--quiet", "--branch", "main", remote, ".")
			testutil.RunGit(t, other, "-c", "user.name=O", "-c", "user.email=o@example.com", "commit", "--quiet", "--allow-empty", "-m", "upstream")
			testutil.RunGit(t, other, "push", "--quiet", "origin", "main")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "post-merge", script) },
				run:     func(core *Core) error { return core.Pull(repo) },
			}
		},
		"checkout": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			testutil.RunGit(t, repo, "branch", "topic")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "post-checkout", script) },
				run:     func(core *Core) error { return core.CheckoutRef(repo, BranchRef("topic")) },
			}
		},
		"create branch": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "post-checkout", script) },
				run:     func(core *Core) error { return core.CheckoutNewBranch(repo, "fresh") },
			}
		},
		"worktree add": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			path := filepath.Join(t.TempDir(), "wt")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "post-checkout", script) },
				run:     func(core *Core) error { return core.CreateWorktree(repo, path, "wt-branch") },
			}
		},
		"merge": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			testutil.RunGit(t, repo, "checkout", "--quiet", "-b", "topic")
			testutil.RunGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "topic work")
			testutil.RunGit(t, repo, "checkout", "--quiet", "main")
			return hookedOperation{
				install: func(script string) { installHook(t, repo, "post-merge", script) },
				run: func(core *Core) error {
					_, err := core.MergeBranch(repo, "main", "topic")
					return err
				},
			}
		},
		"forge create": func(t *testing.T) hookedOperation {
			repo := initGitRepo(t)
			testutil.RunGit(t, repo, "remote", "add", "origin", "https://github.com/acme/tool.git")
			fake := filepath.Join(t.TempDir(), "gh")
			op := hookedOperation{
				run: func(core *Core) error {
					_, err := core.CreatePR(context.Background(), repo, "Title", "", "", false)
					return err
				},
				options: []CoreOption{WithIsolatedForgeCLIs(fake, nil)},
			}
			op.install = func(script string) {
				if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+script+"echo https://github.com/acme/tool/pull/1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			return op
		},
	}
}

// A hook that outlasts the bound other git commands get does not fail the
// operation that runs it.
func TestHookedOperationsOutlastTheGitCommandTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	t.Parallel()
	for name, setup := range hookedOperations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := setup(t)
			op.install("sleep 2\n")
			core := NewCore(op.options...)
			core.timeout = time.Second
			if err := op.run(core); err != nil {
				t.Fatalf("%s with a hook past the command timeout: %v", name, err)
			}
		})
	}
}

// stoppedHookBound is the hook bound of the tests that stop a hook. The hook
// must record its pid before the bound ends, and macOS can take seconds to
// start a new shell script while the package's other tests start theirs.
const stoppedHookBound = 10 * time.Second

// An operation stopped at the hook bound says which command timed out and
// stops its hook too.
func TestAStoppedHookedOperationEndsItsHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	t.Parallel()
	for name, setup := range hookedOperations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := setup(t)
			pidFile := filepath.Join(t.TempDir(), "hook.pid")
			op.install("echo $$ > '" + pidFile + "'\nsleep 30\n")
			core := NewCore(op.options...)
			core.hookTimeout = stoppedHookBound
			err := op.run(core)
			if err == nil || !strings.Contains(err.Error(), "timed out after 10s") {
				t.Fatalf("%s past the hook bound = %v, want its command named as timed out", name, err)
			}
			if pid := recordedHookPID(t, pidFile); processExists(pid) {
				t.Fatal("the hook outlived the stopped operation")
			}
		})
	}
}

// A hook that ignores SIGTERM, and the child it started, are killed once the
// stop grace has passed, and the stopped operation returns only after both
// are gone.
func TestAStoppedHookedOperationKillsAHookThatIgnoresTerminate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	t.Parallel()
	const bound, grace = stoppedHookBound, time.Second
	for name, setup := range hookedOperations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := setup(t)
			dir := t.TempDir()
			hookPIDFile, childPIDFile := filepath.Join(dir, "hook.pid"), filepath.Join(dir, "child.pid")
			// The ignored SIGTERM is inherited by the child.
			op.install("trap '' TERM\nsleep 60 &\necho $! > '" + childPIDFile + "'\necho $$ > '" + hookPIDFile + "'\nwait\n")
			core := NewCore(op.options...)
			core.hookTimeout, core.hookStopGrace = bound, grace
			started := time.Now()
			err := op.run(core)
			elapsed := time.Since(started)
			child, hook := recordedHookPID(t, childPIDFile), recordedHookPID(t, hookPIDFile)
			hookAlive, childAlive := processExists(hook), processExists(child)
			if err == nil || !strings.Contains(err.Error(), "timed out after 10s") {
				t.Fatalf("%s past the hook bound = %v, want its command named as timed out", name, err)
			}
			if hookAlive || childAlive {
				t.Fatalf("after the stopped operation returned: hook alive %v, its child alive %v", hookAlive, childAlive)
			}
			if limit := bound + grace + 1500*time.Millisecond; elapsed > limit {
				t.Fatalf("the stopped operation returned after %s, want within %s", elapsed, limit)
			}
		})
	}
}

// A hook that handles SIGTERM gets it, and the grace to finish its cleanup,
// before the stopped operation returns; the operation does not wait out the
// grace once the hook is done.
func TestAStoppedHookedOperationLetsItsHookCleanUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	t.Parallel()
	const bound, grace = stoppedHookBound, 5 * time.Second
	for name, setup := range hookedOperations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := setup(t)
			dir := t.TempDir()
			pidFile, cleaned := filepath.Join(dir, "hook.pid"), filepath.Join(dir, "cleaned")
			op.install("trap 'sleep 0.3; echo done > \"" + cleaned + "\"; exit 0' TERM\necho $$ > '" + pidFile + "'\nwhile :; do sleep 0.05; done\n")
			core := NewCore(op.options...)
			core.hookTimeout, core.hookStopGrace = bound, grace
			started := time.Now()
			err := op.run(core)
			elapsed := time.Since(started)
			pid := recordedHookPID(t, pidFile)
			if err == nil || !strings.Contains(err.Error(), "timed out after 10s") {
				t.Fatalf("%s past the hook bound = %v, want its command named as timed out", name, err)
			}
			if _, statErr := os.Stat(cleaned); statErr != nil {
				t.Fatalf("the hook's SIGTERM cleanup did not finish before the operation returned: %v", statErr)
			}
			if processExists(pid) {
				t.Fatal("the hook outlived the stopped operation")
			}
			if elapsed >= bound+grace {
				t.Fatalf("the stopped operation waited out the grace (%s) after its hook exited", elapsed)
			}
		})
	}
}

// recordedHookPID reads the pid a hook wrote to path and, when the hook did
// write one, kills that process when the test ends so a failure leaves
// nothing running.
func recordedHookPID(t *testing.T, path string) int {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the hook did not record a pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
	if err != nil || pid <= 0 {
		t.Fatalf("recorded pid %q: %v", text, err)
	}
	t.Cleanup(func() {
		process, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill process %d: %v", pid, err)
		}
	})
	return pid
}

// processExists reports whether pid names a process that signal 0 reaches.
func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}
