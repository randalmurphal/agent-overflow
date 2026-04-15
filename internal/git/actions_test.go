package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitStagesAllChangesAndReturnsSHA(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("update tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	sha, err := core.Commit(repo, "add files", "body text")
	if err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("commit SHA length = %d, want 40", len(sha))
	}

	head := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	if sha != head {
		t.Fatalf("commit SHA = %q, want %q", sha, head)
	}

	status := strings.TrimSpace(runGitStdout(t, repo, "status", "--short"))
	if status != "" {
		t.Fatalf("expected clean working tree after commit, got %q", status)
	}
}

func TestCommitRequiresSubject(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	if _, err := core.Commit(repo, "   ", "body"); err == nil {
		t.Fatal("expected error for empty subject")
	}
}

func TestPushSetsUpstreamWhenMissing(t *testing.T) {
	repo := initGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	runGit(t, repo, "remote", "add", "origin", remote)

	core := NewCore()
	if err := core.Push(repo); err != nil {
		t.Fatalf("Push returned error: %v", err)
	}

	upstream := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"))
	if upstream != "origin/main" {
		t.Fatalf("upstream = %q, want origin/main", upstream)
	}
}

func TestPushWithoutRemoteReturnsError(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	err := core.Push(repo)
	if err == nil {
		t.Fatal("expected error when pushing without a remote")
	}
	if !strings.Contains(err.Error(), "no git remote is configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPullWithoutUpstreamReturnsError(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	err := core.Pull(repo)
	if err == nil {
		t.Fatal("expected error when pulling without upstream")
	}
	if !strings.Contains(err.Error(), "pull --ff-only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateBranchAndCheckout(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	if err := core.CreateBranch(repo, "feature/demo"); err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if err := core.Checkout(repo, "feature/demo"); err != nil {
		t.Fatalf("Checkout returned error: %v", err)
	}

	branch := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "feature/demo" {
		t.Fatalf("branch = %q, want feature/demo", branch)
	}
}

func TestCreateBranchRequiresName(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()

	if err := core.CreateBranch(repo, "  "); err == nil {
		t.Fatal("expected error for empty branch name")
	}
}

func runGitStdout(t *testing.T, cwd string, args ...string) string {
	t.Helper()

	output, err := runGitAllowErrorOutput(cwd, args...)
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return output
}

func runGitAllowErrorOutput(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, string(output))
	}
	return string(output), nil
}
