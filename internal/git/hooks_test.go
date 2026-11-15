package git

import (
	"context"
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
			core.hookTimeout = time.Second
			err := op.run(core)
			if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
				t.Fatalf("%s past the hook bound = %v, want its command named as timed out", name, err)
			}
			pidText, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatalf("the hook did not start: %v", readErr)
			}
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidText)))
			if convErr != nil {
				t.Fatal(convErr)
			}
			hook, findErr := os.FindProcess(pid)
			if findErr != nil {
				t.Fatal(findErr)
			}
			deadline := time.Now().Add(HookStopGrace)
			for hook.Signal(syscall.Signal(0)) == nil {
				if time.Now().After(deadline) {
					t.Fatal("the hook outlived the stopped operation")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
