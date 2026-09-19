package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateRepoBuildsHistoryAndDirtyState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	err := CreateRepo(dir, RepoSpec{
		Commits: []CommitSpec{
			{Message: "first", Files: map[string]string{"src/main.go": "package main\n"}},
			{Message: "second", Files: map[string]string{"src/main.go": "package main // v2\n", "README.md": "hi\n"}},
		},
		Dirty: map[string]string{"src/main.go": "package main // dirty\n"},
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	log := gitOut(t, dir, "log", "--format=%s")
	if !strings.Contains(log, "second") || !strings.Contains(log, "first") {
		t.Fatalf("log = %q", log)
	}
	branch := gitOut(t, dir, "branch", "--show-current")
	if strings.TrimSpace(branch) != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	status := gitOut(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "src/main.go") {
		t.Fatalf("dirty file missing from status: %q", status)
	}
	data, err := os.ReadFile(filepath.Join(dir, "src/main.go"))
	if err != nil || !strings.Contains(string(data), "dirty") {
		t.Fatalf("dirty content = %q, %v", data, err)
	}
}

func TestCreateRepoExtraBranchesExistUncheckedOut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	err := CreateRepo(dir, RepoSpec{
		Commits:  []CommitSpec{{Message: "first", Files: map[string]string{"a.txt": "a\n"}}},
		Branches: []string{"existing-work", "feature/nested"},
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	branches := gitOut(t, dir, "branch", "--format=%(refname:short)")
	for _, want := range []string{"main", "existing-work", "feature/nested"} {
		if !strings.Contains(branches, want) {
			t.Fatalf("branch %q missing from %q", want, branches)
		}
	}
	// The point of the field: the extra branches are attachable, which
	// means nothing may have them checked out.
	if current := strings.TrimSpace(gitOut(t, dir, "branch", "--show-current")); current != "main" {
		t.Fatalf("current branch = %q, want main", current)
	}
}

func TestCreateRepoRejectsBlankBranchName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := CreateRepo(dir, RepoSpec{Branches: []string{"  "}}); err == nil {
		t.Fatal("CreateRepo accepted a blank branch name")
	}
}

func TestCreateRepoEmptySpecStillHasHead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := CreateRepo(dir, RepoSpec{}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if out := gitOut(t, dir, "rev-parse", "HEAD"); strings.TrimSpace(out) == "" {
		t.Fatal("no HEAD commit")
	}
}

func TestCreateRepoRejectsEscapingPathsAndExistingRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	err := CreateRepo(dir, RepoSpec{Commits: []CommitSpec{{Files: map[string]string{"../escape.txt": "x"}}}})
	if err == nil {
		t.Fatal("CreateRepo accepted a parent-escaping path")
	}
	if err := CreateRepo(dir, RepoSpec{}); err == nil {
		// dir now has a .git from the failed attempt's init; a second
		// CreateRepo must refuse rather than layer histories.
		t.Fatal("CreateRepo re-initialised an existing repository")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// Git exports GIT_DIR to the processes it spawns from a linked worktree
// (hooks, aliases, `git bisect run`). A fixture created under that
// environment must still be its own repository: the enclosing worktree's
// history and shared config stay untouched.
func TestCreateRepoIgnoresInheritedGitDir(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	if err := CreateRepo(outer, RepoSpec{Commits: []CommitSpec{{Message: "outer", Files: map[string]string{"outer.txt": "x"}}}}); err != nil {
		t.Fatal(err)
	}
	outerHead := strings.TrimSpace(gitOut(t, outer, "rev-parse", "HEAD"))
	outerConfig, err := os.ReadFile(filepath.Join(outer, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	t.Setenv("GIT_WORK_TREE", outer)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(outer, ".git", "index"))

	dir := filepath.Join(t.TempDir(), "fixture")
	if err := CreateRepo(dir, RepoSpec{Commits: []CommitSpec{{Message: "init", Files: map[string]string{"README.md": "# fixture\n"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "HEAD")); err != nil {
		t.Fatalf("fixture has no repository of its own: %v", err)
	}
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	os.Unsetenv("GIT_INDEX_FILE")
	if got := strings.TrimSpace(gitOut(t, outer, "rev-parse", "HEAD")); got != outerHead {
		t.Fatalf("fixture committed onto the enclosing repository: %s -> %s", outerHead, got)
	}
	if got, err := os.ReadFile(filepath.Join(outer, ".git", "config")); err != nil || string(got) != string(outerConfig) {
		t.Fatalf("fixture rewrote the enclosing repository config:\n%s", got)
	}
	if log := gitOut(t, dir, "log", "--format=%s"); strings.TrimSpace(log) != "init" {
		t.Fatalf("fixture history = %q, want init", log)
	}
}

func TestAddWorktreeLinksToTheSourceRepository(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "workspaces", "app")
	if err := CreateRepo(repo, RepoSpec{}); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktrees", "app", "feature-x")
	if err := AddWorktree(repo, worktree, "feature-x"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	common := strings.TrimSpace(gitOut(t, worktree, "rev-parse", "--git-common-dir"))
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatal(err)
	}
	wantCommon, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wantCommon {
		t.Fatalf("--git-common-dir = %s, want %s", resolved, wantCommon)
	}
	if branch := strings.TrimSpace(gitOut(t, worktree, "branch", "--show-current")); branch != "feature-x" {
		t.Fatalf("worktree branch = %q, want feature-x", branch)
	}
	if branch := strings.TrimSpace(gitOut(t, repo, "branch", "--show-current")); branch != "main" {
		t.Fatalf("source repo moved off main: %q", branch)
	}
	if head := strings.TrimSpace(gitOut(t, worktree, "rev-parse", "HEAD")); head == "" {
		t.Fatal("worktree has no HEAD commit")
	}
}

func TestAddWorktreeRefusesBlankBranchExistingPathAndNonRepo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := CreateRepo(repo, RepoSpec{}); err != nil {
		t.Fatal(err)
	}
	if err := AddWorktree(repo, filepath.Join(root, "wt"), "  "); err == nil {
		t.Fatal("AddWorktree accepted a blank branch name")
	}
	occupied := filepath.Join(root, "occupied")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AddWorktree(repo, occupied, "b1"); err == nil {
		t.Fatal("AddWorktree accepted an existing worktree path")
	}
	if err := AddWorktree(filepath.Join(root, "missing"), filepath.Join(root, "wt2"), "b2"); err == nil {
		t.Fatal("AddWorktree accepted a directory that is not a repository")
	}
}
