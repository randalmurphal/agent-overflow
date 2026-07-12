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
