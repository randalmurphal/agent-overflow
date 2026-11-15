package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/testutil"
)

func commitRepoFile(t *testing.T, repo, name, content, message string) {
	t.Helper()
	writeRepoFile(t, repo, name, content)
	testutil.RunGit(t, repo, "add", "--", name)
	testutil.RunGit(t, repo, "commit", "-m", message)
}

// listedSelection selects the rows a listing of repo has for paths, as the
// commit dialog sends them; a rename is selected by either of its paths. A
// path the listing does not have is sent as a bare row.
func listedSelection(t *testing.T, repo string, paths ...string) CommitSelection {
	t.Helper()
	files, err := gitdiff.WorktreeChanges(t.Context(), repo, t.TempDir())
	if err != nil {
		t.Fatalf("WorktreeChanges: %v", err)
	}
	selection := CommitSelection{SnapshotRoot: t.TempDir()}
	picked := make(map[string]bool)
	for _, path := range paths {
		found := false
		for _, file := range files {
			if path != "" && (file.Path == path || file.OldPath == path) {
				found = true
				if !picked[file.Path] {
					picked[file.Path] = true
					selection.Files = append(selection.Files, file)
				}
			}
		}
		if !found {
			selection.Files = append(selection.Files, gitdiff.ChangedFile{Path: path})
		}
	}
	return selection
}

func installPreCommitHook(t *testing.T, repo, script string) {
	t.Helper()
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A pre-commit hook that outlasts the bound other git commands get still
// commits.
func TestCommitOutlastsTheGitCommandTimeout(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	installPreCommitHook(t, repo, "sleep 2\n")
	core := NewCore()
	core.timeout = time.Second
	for name, selection := range map[string]func() CommitSelection{
		"all":   func() CommitSelection { return CommitSelection{All: true} },
		"files": func() CommitSelection { return listedSelection(t, repo, "README.txt") },
	} {
		writeRepoFile(t, repo, "README.txt", "hello\n"+name+"\n")
		if _, err := core.CommitSelected(repo, name, "", selection()); err != nil {
			t.Fatalf("%s: CommitSelected: %v", name, err)
		}
	}
}

// A commit stopped at its bound stops its hook too, and leaves no index lock
// behind to block the next git command.
func TestAStoppedCommitEndsItsHookAndLeavesNoIndexLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	t.Parallel()
	for name, selection := range map[string]func(repo string) CommitSelection{
		"all":   func(string) CommitSelection { return CommitSelection{All: true} },
		"files": func(repo string) CommitSelection { return listedSelection(t, repo, "README.txt") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := initGitRepo(t)
			pidFile := filepath.Join(t.TempDir(), "hook.pid")
			installPreCommitHook(t, repo, "echo $$ > '"+pidFile+"'\nsleep 30\n")
			core := NewCore()
			core.hookTimeout = time.Second
			writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
			if _, err := core.CommitSelected(repo, "stopped", "", selection(repo)); err == nil {
				t.Fatal("the commit finished")
			}
			pidText, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("the hook did not start: %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
			if err != nil {
				t.Fatal(err)
			}
			hook, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(HookStopGrace)
			for hook.Signal(syscall.Signal(0)) == nil {
				if time.Now().After(deadline) {
					t.Fatal("the hook outlived the stopped commit")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("index.lock after the stopped commit: %v", err)
			}
		})
	}
}

// A listed file rewritten before the commit reads it is refused by name,
// and nothing is committed.
func TestCommitSelectedRefusesAFileChangedSinceItWasListed(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	writeRepoFile(t, repo, "README.txt", "hello\nlisted\n")
	selection := listedSelection(t, repo, "README.txt")
	writeRepoFile(t, repo, "README.txt", "hello\nrewritten\n")
	head := runGitStdout(t, repo, "rev-parse", "HEAD")

	_, err := NewCore().CommitSelected(repo, "stale", "", selection)
	var changed *CommitFilesChangedError
	if !errors.As(err, &changed) || !errors.Is(err, ErrCommitFilesChanged) || len(changed.Paths) != 1 || changed.Paths[0] != "README.txt" {
		t.Fatalf("err = %v, want README.txt named as changed", err)
	}
	if got := runGitStdout(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %q", got)
	}
}

func TestCommitSelectedCommitsExactlyTheNamedPaths(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	core := NewCore()
	commitRepoFile(t, repo, "gone [x].txt", "bye\n", "add gone")
	commitRepoFile(t, repo, "old.txt", "one\ntwo\nthree\nfour\n", "add old")
	commitRepoFile(t, repo, "other.txt", "other\n", "add other")

	writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
	writeRepoFile(t, repo, "new *.txt", "fresh\n")
	writeRepoFile(t, repo, "new ab.txt", "must not match the star\n")
	writeRepoFile(t, repo, "-dash.txt", "leading dash\n")
	if err := os.Remove(filepath.Join(repo, "gone [x].txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "old.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	// Left out of the commit: a staged change with more on top, a file
	// staged whole, and an untracked file.
	writeRepoFile(t, repo, "other.txt", "other\nstaged\n")
	testutil.RunGit(t, repo, "add", "other.txt")
	writeRepoFile(t, repo, "other.txt", "other\nstaged\nunstaged\n")
	writeRepoFile(t, repo, "staged-new.txt", "staged\n")
	testutil.RunGit(t, repo, "add", "staged-new.txt")
	indexedOther := runGitStdout(t, repo, "ls-files", "--stage", "--", "other.txt", "staged-new.txt")

	sha, err := core.CommitSelected(repo, "pick some", "the body",
		listedSelection(t, repo, "README.txt", "new *.txt", "-dash.txt", "gone [x].txt", "old.txt", "renamed.txt"))
	if err != nil {
		t.Fatalf("CommitSelected: %v", err)
	}
	if head := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD")); sha != head {
		t.Fatalf("sha = %q, HEAD = %q", sha, head)
	}
	if msg := runGitStdout(t, repo, "log", "-1", "--format=%s%n%b"); !strings.HasPrefix(msg, "pick some\nthe body") {
		t.Fatalf("message = %q", msg)
	}
	committed := runGitStdout(t, repo, "show", "--format=", "--name-status", "-M", "-z", "HEAD")
	want := "A\x00-dash.txt\x00M\x00README.txt\x00D\x00gone [x].txt\x00A\x00new *.txt\x00R100\x00old.txt\x00renamed.txt\x00"
	if committed != want {
		t.Fatalf("committed %q, want %q", committed, want)
	}
	if got := runGitStdout(t, repo, "ls-files", "--stage", "--", "other.txt", "staged-new.txt"); got != indexedOther {
		t.Fatalf("the user's staged entries changed:\n%s\nwas:\n%s", got, indexedOther)
	}
	status := runGitStdout(t, repo, "status", "--porcelain")
	if status != "MM other.txt\nA  staged-new.txt\n?? \"new ab.txt\"\n" {
		t.Fatalf("status after commit = %q", status)
	}
}

func TestCommitSelectedAllStagesAndCommitsEverything(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
	writeRepoFile(t, repo, "new.txt", "fresh\n")

	if _, err := NewCore().CommitSelected(repo, "everything", "", CommitSelection{All: true}); err != nil {
		t.Fatalf("CommitSelected: %v", err)
	}
	if got := runGitStdout(t, repo, "show", "--format=", "--name-only", "HEAD"); got != "README.txt\nnew.txt\n" {
		t.Fatalf("committed %q", got)
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q", got)
	}
}

func TestCommitSelectedOnARepositoryWithNoCommits(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-b", "main")
	testutil.RunGit(t, repo, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")
	writeRepoFile(t, repo, "first.txt", "first\n")
	writeRepoFile(t, repo, "second.txt", "second\n")

	if _, err := NewCore().CommitSelected(repo, "first only", "", listedSelection(t, repo, "first.txt")); err != nil {
		t.Fatalf("CommitSelected: %v", err)
	}
	if got := runGitStdout(t, repo, "ls-tree", "--name-only", "HEAD"); got != "first.txt\n" {
		t.Fatalf("tree = %q", got)
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != "?? second.txt\n" {
		t.Fatalf("status = %q", got)
	}
}

func TestCommitSelectedLeavesNewFilesUntrackedWhenTheCommitFails(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho hook says no >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
	writeRepoFile(t, repo, "new.txt", "fresh\n")
	head := runGitStdout(t, repo, "rev-parse", "HEAD")

	_, err := NewCore().CommitSelected(repo, "refused", "", listedSelection(t, repo, "README.txt", "new.txt"))
	if err == nil || !strings.Contains(err.Error(), "hook says no") {
		t.Fatalf("err = %v, want the hook's refusal", err)
	}
	if got := runGitStdout(t, repo, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %q", got)
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != " M README.txt\n?? new.txt\n" {
		t.Fatalf("status after a refused commit = %q", got)
	}
}

// A refused commit leaves the user's index as it was: a deletion
// the user staged for the same path is still staged.
func TestCommitSelectedFailureKeepsAStagedDeletionOfTheSamePath(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "rm", "--cached", "--quiet", "README.txt")
	writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
	before := runGitStdout(t, repo, "status", "--porcelain")
	if before != "D  README.txt\n?? README.txt\n" {
		t.Fatalf("setup status = %q", before)
	}

	if _, err := NewCore().CommitSelected(repo, "refused", "", listedSelection(t, repo, "README.txt")); err == nil {
		t.Fatal("the hook did not refuse the commit")
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != before {
		t.Fatalf("status after a refused commit = %q, want %q", got, before)
	}
}

// A submodule whose commit moved and an untracked nested repository are
// each one path, committed as a gitlink; a refused commit leaves the nested
// repository untracked.
func TestCommitSelectedCommitsRepositoriesAsGitlinks(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	inner := initGitRepo(t)
	testutil.RunGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", inner, "sm")
	testutil.RunGit(t, repo, "commit", "-m", "add submodule")
	sm := filepath.Join(repo, "sm")
	testutil.RunGit(t, sm, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, sm, "config", "user.email", "agent-overflow@example.com")
	commitRepoFile(t, sm, "README.txt", "moved\n", "move the submodule")
	nested := filepath.Join(repo, "nested")
	testutil.RunGit(t, repo, "init", "--quiet", "-b", "main", nested)
	testutil.RunGit(t, nested, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, nested, "config", "user.email", "agent-overflow@example.com")
	commitRepoFile(t, nested, "n.txt", "n\n", "nested commit")
	core := NewCore()
	before := runGitStdout(t, repo, "status", "--porcelain")

	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := core.CommitSelected(repo, "refused", "", listedSelection(t, repo, "nested")); err == nil {
		t.Fatal("the hook did not refuse the commit")
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != before {
		t.Fatalf("status after a refused commit = %q, want %q", got, before)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}

	if _, err := core.CommitSelected(repo, "repositories", "", listedSelection(t, repo, "sm", "nested")); err != nil {
		t.Fatalf("CommitSelected: %v", err)
	}
	if got := runGitStdout(t, repo, "show", "--format=", "--raw", "--abbrev=1", "HEAD"); strings.Count(got, ":160000 160000") != 1 || strings.Count(got, ":000000 160000") != 1 {
		t.Fatalf("committed entries = %q", got)
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q", got)
	}
}

func TestCommitSelectedValidatesItsInput(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	core := NewCore()
	writeRepoFile(t, repo, "README.txt", "hello\nedited\n")
	for name, call := range map[string]func() (string, error){
		"blank subject": func() (string, error) {
			return core.CommitSelected(repo, "  ", "", listedSelection(t, repo, "README.txt"))
		},
		"nothing": func() (string, error) { return core.CommitSelected(repo, "s", "", CommitSelection{}) },
		"both forms": func() (string, error) {
			return core.CommitSelected(repo, "s", "", CommitSelection{All: true, Files: []gitdiff.ChangedFile{{Path: "README.txt"}}})
		},
		"empty path": func() (string, error) {
			return core.CommitSelected(repo, "s", "", listedSelection(t, repo, ""))
		},
		"NUL in path": func() (string, error) {
			return core.CommitSelected(repo, "s", "", listedSelection(t, repo, "a\x00b"))
		},
		"unknown path": func() (string, error) {
			return core.CommitSelected(repo, "s", "", listedSelection(t, repo, "missing.txt"))
		},
		"outside repo": func() (string, error) {
			return core.CommitSelected(repo, "s", "", listedSelection(t, repo, "../escape.txt"))
		},
	} {
		if _, err := call(); err == nil {
			t.Errorf("%s: committed", name)
		}
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != " M README.txt\n" {
		t.Fatalf("status after refused commits = %q", got)
	}
}

func TestCommitSelectedTakesMorePathsThanOneArgvHolds(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	const count = 600
	paths := make([]string, 0, count)
	for i := range count {
		// 600 paths of ~120 bytes: more than one argument list holds.
		name := fmt.Sprintf("dir/%s-%04d.txt", strings.Repeat("n", 100), i)
		writeRepoFile(t, repo, name, "x\n")
		paths = append(paths, name)
	}
	writeRepoFile(t, repo, "left.txt", "left\n")

	if _, err := NewCore().CommitSelected(repo, "many", "", listedSelection(t, repo, paths...)); err != nil {
		t.Fatalf("CommitSelected: %v", err)
	}
	committed := strings.Count(runGitStdout(t, repo, "show", "--format=", "--name-only", "HEAD"), "\n")
	if committed != count {
		t.Fatalf("committed %d files, want %d", committed, count)
	}
	if got := runGitStdout(t, repo, "status", "--porcelain"); got != "?? left.txt\n" {
		t.Fatalf("status = %q", got)
	}
}

// A commit of selected paths is refused in exactly the states
// CommitsWholeOnly reports, and the operation is left in progress, so the
// dialog that locks its selection on that answer cannot disagree with it.
func TestCommitsWholeOnlyMatchesTheRefusal(t *testing.T) {
	t.Parallel()
	conflict := func(t *testing.T) string {
		t.Helper()
		repo := initGitRepo(t)
		testutil.RunGit(t, repo, "checkout", "-b", "side")
		commitRepoFile(t, repo, "README.txt", "side\n", "side change")
		testutil.RunGit(t, repo, "checkout", "main")
		commitRepoFile(t, repo, "README.txt", "main\n", "main change")
		commitRepoFile(t, repo, "README.txt", "main again\n", "main again")
		return repo
	}
	resolve := func(t *testing.T, repo string) {
		t.Helper()
		writeRepoFile(t, repo, "README.txt", "resolved\n")
		testutil.RunGit(t, repo, "add", "README.txt")
		writeRepoFile(t, repo, "extra.txt", "extra\n")
	}
	for name, tc := range map[string]struct {
		start   []string
		head    string
		blocked bool
	}{
		"merge":       {[]string{"merge", "side"}, "MERGE_HEAD", true},
		"cherry-pick": {[]string{"cherry-pick", "side"}, "CHERRY_PICK_HEAD", true},
		"revert":      {[]string{"revert", "HEAD~1"}, "REVERT_HEAD", true},
		"rebase":      {[]string{"rebase", "side"}, "REBASE_HEAD", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := conflict(t)
			core := NewCore()
			if core.CommitsWholeOnly(repo) {
				t.Fatal("blocked before the operation started")
			}
			if err := testutil.RunGitAllowError(repo, tc.start...); err == nil {
				t.Fatalf("git %v did not stop on the conflict", tc.start)
			}
			resolve(t, repo)
			if got := core.CommitsWholeOnly(repo); got != tc.blocked {
				t.Fatalf("CommitsWholeOnly = %v, want %v", got, tc.blocked)
			}
			// The status the dialog's notice reads names the same operation.
			status, err := core.Status(t.Context(), repo)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.PendingOperation != name {
				t.Fatalf("PendingOperation = %q, want %q", status.PendingOperation, name)
			}
			head := runGitStdout(t, repo, "rev-parse", "HEAD")
			_, err = core.CommitSelected(repo, "partial", "", listedSelection(t, repo, "README.txt"))
			if refused := err != nil && strings.Contains(err.Error(), "is in progress"); refused != tc.blocked {
				t.Fatalf("CommitSelected err = %v, want refusal = %v", err, tc.blocked)
			}
			if got := runGitStdout(t, repo, "status", "--porcelain"); !strings.Contains(got, "?? extra.txt") {
				t.Fatalf("status afterwards = %q", got)
			}
			if !tc.blocked {
				return
			}
			if got := runGitStdout(t, repo, "rev-parse", "HEAD"); got != head {
				t.Fatalf("HEAD moved to %q", got)
			}
			if got := runGitStdout(t, repo, "rev-parse", "--verify", tc.head); got == "" {
				t.Fatalf("%s is gone", tc.head)
			}
			// The whole commit is the one that finishes the operation.
			if _, err := core.CommitSelected(repo, "whole", "", CommitSelection{All: true}); err != nil {
				t.Fatalf("CommitSelected All: %v", err)
			}
			if core.CommitsWholeOnly(repo) {
				t.Fatal("still blocked after the whole commit")
			}
		})
	}

	// A bisect stops nothing mid-commit, so a selection commits.
	repo := initGitRepo(t)
	commitRepoFile(t, repo, "a.txt", "a\n", "second")
	testutil.RunGit(t, repo, "bisect", "start")
	writeRepoFile(t, repo, "a.txt", "a\nb\n")
	writeRepoFile(t, repo, "b.txt", "b\n")
	core := NewCore()
	if core.CommitsWholeOnly(repo) {
		t.Fatal("a bisect reported as blocking")
	}
	if _, err := core.CommitSelected(repo, "during bisect", "", listedSelection(t, repo, "a.txt")); err != nil {
		t.Fatalf("CommitSelected during a bisect: %v", err)
	}
}
