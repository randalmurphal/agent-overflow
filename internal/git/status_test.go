package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStatusOutput(t *testing.T) {
	status := parseStatusOutput(
		`# branch.oid abcdef
# branch.head feature/demo
# branch.upstream origin/feature/demo
# branch.ab +2 -1
1 .M N... 100644 100644 100644 abcdef abcdef tracked.txt
? untracked.txt`,
		"3\t1\ttracked.txt\n",
		"1\t0\tstaged.txt\n",
	)

	if status.Branch != "feature/demo" {
		t.Fatalf("Branch = %q, want feature/demo", status.Branch)
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true")
	}
	if status.AheadCount != 2 || status.BehindCount != 1 {
		t.Fatalf("ahead/behind = %d/%d, want 2/1", status.AheadCount, status.BehindCount)
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	if status.FileCount != 3 {
		t.Fatalf("FileCount = %d, want 3", status.FileCount)
	}
	if status.Insertions != 4 {
		t.Fatalf("Insertions = %d, want 4", status.Insertions)
	}
	if status.Deletions != 1 {
		t.Fatalf("Deletions = %d, want 1", status.Deletions)
	}
}

func TestParseBranchList(t *testing.T) {
	branches := parseBranchList(
		"main|*|/tmp/repo\nfeature/demo| |\norigin/main| |\norigin/feature/demo| |\norigin/HEAD| |\n",
		"main",
		[]string{"origin"},
	)

	if len(branches) != 4 {
		t.Fatalf("len(branches) = %d, want 4", len(branches))
	}
	if !branches[0].IsCurrent {
		t.Fatal("expected first branch to be current")
	}
	if !branches[0].IsDefault {
		t.Fatal("expected main to be default")
	}
	if !branches[2].IsRemote {
		t.Fatal("expected origin/main to be remote")
	}
	if !branches[2].IsDefault {
		t.Fatal("expected origin/main to be default")
	}
	if branches[1].IsRemote {
		t.Fatal("expected feature/demo to remain a local branch")
	}
}

func TestStatusReturnsNotRepoForNonGitDirectory(t *testing.T) {
	core := NewCore()

	status, err := core.Status(t.TempDir())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.IsRepo {
		t.Fatal("expected IsRepo=false")
	}
}

func TestListBranchesOnRepository(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	branches, err := core.ListBranches(repo)
	if err != nil {
		t.Fatalf("ListBranches returned error: %v", err)
	}
	if len(branches) == 0 {
		t.Fatal("expected at least one branch")
	}
	if branches[0].Name == "" {
		t.Fatal("expected branch name to be populated")
	}
}

func TestStatusOnRepositoryWithOrigin(t *testing.T) {
	repo := initGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")

	readmePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("update README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	core := NewCore()
	status, err := core.Status(repo)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}

	if !status.IsRepo {
		t.Fatal("expected IsRepo=true")
	}
	if status.Branch != "main" {
		t.Fatalf("Branch = %q, want main", status.Branch)
	}
	if !status.IsDefaultBranch {
		t.Fatal("expected default branch")
	}
	if !status.HasOriginRemote {
		t.Fatal("expected HasOriginRemote=true")
	}
	if !status.HasUpstream {
		t.Fatal("expected HasUpstream=true after push -u")
	}
	if !status.HasChanges {
		t.Fatal("expected HasChanges=true")
	}
	if status.FileCount < 2 {
		t.Fatalf("FileCount = %d, want at least 2", status.FileCount)
	}
	if status.Insertions == 0 {
		t.Fatal("expected insertions to be counted")
	}
}

func TestWorkingTreeDiff(t *testing.T) {
	repo := initGitRepo(t)
	readmePath := filepath.Join(repo, "README.txt")

	if err := os.WriteFile(readmePath, []byte("hello\nstaged\n"), 0o644); err != nil {
		t.Fatalf("write staged version: %v", err)
	}
	runGit(t, repo, "add", "README.txt")
	if err := os.WriteFile(readmePath, []byte("hello\nstaged\nunstaged\n"), 0o644); err != nil {
		t.Fatalf("write unstaged version: %v", err)
	}

	core := NewCore()
	diff, err := core.WorkingTreeDiff(repo)
	if err != nil {
		t.Fatalf("WorkingTreeDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "README.txt") {
		t.Fatalf("expected diff to mention README.txt, got %q", diff)
	}
}

func TestLookupOpenPRUsesGHWhenAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PATH override in short mode")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, number := core.lookupOpenPR(t.TempDir(), "main")
	if url != "https://example.com/pr/7" {
		t.Fatalf("url = %q, want https://example.com/pr/7", url)
	}
	if number != 7 {
		t.Fatalf("number = %d, want 7", number)
	}
}

func TestHelperFunctions(t *testing.T) {
	if got := normalizeNumstatPath("old/name.txt => new/name.txt"); got != "new/name.txt" {
		t.Fatalf("normalizeNumstatPath returned %q", got)
	}
	if got := parsePorcelainPath("? notes.txt"); got != "notes.txt" {
		t.Fatalf("parsePorcelainPath for untracked returned %q", got)
	}
	if got := parsePorcelainPath("2 R. N... 100644 100644 100644 abc abc R100\tnew.txt\told.txt"); got != "new.txt" {
		t.Fatalf("parsePorcelainPath for rename returned %q", got)
	}
	if got := combineDiffs("one", "two"); got != "one\n\ntwo" {
		t.Fatalf("combineDiffs returned %q", got)
	}
	if got := parseNumstatCount("-"); got != 0 {
		t.Fatalf("parseNumstatCount('-') = %d, want 0", got)
	}
	if !isDefaultBranchName("origin/main", "") {
		t.Fatal("expected origin/main to be treated as default when origin HEAD is unavailable")
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	if err := runGitAllowError(repo, "init", "-b", "main"); err != nil {
		runGit(t, repo, "init")
		runGit(t, repo, "checkout", "-b", "main")
	}
	runGit(t, repo, "config", "user.name", "Agent Overflow")
	runGit(t, repo, "config", "user.email", "agent-overflow@example.com")

	filePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(filePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	runGit(t, repo, "add", "README.txt")
	runGit(t, repo, "commit", "-m", "initial commit")

	return repo
}

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()

	if err := runGitAllowError(cwd, args...); err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
}

func runGitAllowError(cwd string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, string(output))
	}
	return nil
}
