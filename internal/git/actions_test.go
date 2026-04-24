package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestCommitOnlyStagedChanges(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("update tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	// Stage only README.txt -- new.txt should remain untracked.
	testutil.RunGit(t, repo, "add", "README.txt")

	sha, err := core.Commit(repo, "update readme", "body text")
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

	// new.txt should still be untracked.
	status := strings.TrimSpace(runGitStdout(t, repo, "status", "--short"))
	if !strings.Contains(status, "new.txt") {
		t.Fatalf("expected new.txt to remain untracked, status = %q", status)
	}
}

func TestStageAllThenCommit(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("update tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	if err := core.StageAll(repo); err != nil {
		t.Fatalf("StageAll returned error: %v", err)
	}

	sha, err := core.Commit(repo, "add files", "body text")
	if err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("commit SHA length = %d, want 40", len(sha))
	}

	status := strings.TrimSpace(runGitStdout(t, repo, "status", "--short"))
	if status != "" {
		t.Fatalf("expected clean working tree after stage-all + commit, got %q", status)
	}
}

func TestCommitRequiresSubject(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if _, err := core.Commit(repo, "   ", "body"); err == nil {
		t.Fatal("expected error for empty subject")
	}
}

func TestPushSetsUpstreamWhenMissing(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t, repo, "remote", "add", "origin", remote)

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
	repo := testutil.InitGitRepo(t)
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
	repo := testutil.InitGitRepo(t)
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
	repo := testutil.InitGitRepo(t)
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

func TestRenameBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := core.CreateBranch(repo, "forge/1234abcd"); err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if err := core.Checkout(repo, "forge/1234abcd"); err != nil {
		t.Fatalf("Checkout returned error: %v", err)
	}

	renamed, err := core.RenameBranch(repo, "forge/1234abcd", "forge/reconnect-backoff")
	if err != nil {
		t.Fatalf("RenameBranch returned error: %v", err)
	}
	if renamed != "forge/reconnect-backoff" {
		t.Fatalf("RenameBranch() = %q, want forge/reconnect-backoff", renamed)
	}

	branch := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "forge/reconnect-backoff" {
		t.Fatalf("branch = %q, want forge/reconnect-backoff", branch)
	}
}

func TestRenameBranchAddsSuffixWhenTargetExists(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	testutil.RunGit(t, repo, "branch", "forge/reconnect-backoff")
	if err := core.CreateBranch(repo, "forge/1234abcd"); err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if err := core.Checkout(repo, "forge/1234abcd"); err != nil {
		t.Fatalf("Checkout returned error: %v", err)
	}

	renamed, err := core.RenameBranch(repo, "forge/1234abcd", "forge/reconnect-backoff")
	if err != nil {
		t.Fatalf("RenameBranch returned error: %v", err)
	}
	if renamed != "forge/reconnect-backoff-1" {
		t.Fatalf("RenameBranch() = %q, want forge/reconnect-backoff-1", renamed)
	}
}

func TestCreateBranchRequiresName(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := core.CreateBranch(repo, "  "); err == nil {
		t.Fatal("expected error for empty branch name")
	}
}

func TestCheckoutRequiresBranchName(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := core.Checkout(repo, "  "); err == nil {
		t.Fatal("expected error for empty branch name")
	}
}

func TestCheckoutRejectsInvalidBranchName(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := core.Checkout(repo, "--flag"); err == nil {
		t.Fatal("expected error for flag-like branch name")
	}
}

func TestCheckoutRejectsInvalidLocalNameDerivedFromRemote(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	testutil.RunGit(t, repo, "remote", "add", "origin", repo)
	testutil.RunGit(t, repo, "update-ref", "refs/heads/-f", "HEAD")

	err := core.Checkout(repo, "origin/-f")
	if err == nil || !strings.Contains(err.Error(), "must not start with -") {
		t.Fatalf("Checkout(origin/-f) error = %v, want invalid local branch rejection", err)
	}
}

func TestCreateBranchRejectsInvalidName(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	if err := core.CreateBranch(repo, "--malicious"); err == nil {
		t.Fatal("expected error for flag-like branch name")
	}
}

func TestValidateBranchName(t *testing.T) {
	tests := []struct {
		name    string
		branch  string
		wantErr bool
	}{
		{"valid simple name", "feature", false},
		{"valid with slash", "feature/demo", false},
		{"starts with dash", "-bad", true},
		{"contains double dot", "a..b", true},
		{"contains NUL", "a\x00b", true},
		{"contains tab control char", "a\tb", true},
		{"valid with hyphen", "my-branch", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranchName(tt.branch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateBranchName(%q) error = %v, wantErr = %v", tt.branch, err, tt.wantErr)
			}
		})
	}
}

func TestCommitBodyOmittedWhenEmpty(t *testing.T) {
	args := commitArgs("subject only", "")
	if len(args) != 3 {
		t.Fatalf("len(args) = %d, want 3 (no body -m flag)", len(args))
	}
}

func TestCommitBodyIncludedWhenPresent(t *testing.T) {
	args := commitArgs("subject", "body text")
	if len(args) != 5 {
		t.Fatalf("len(args) = %d, want 5 (subject + body -m flags)", len(args))
	}
	if args[4] != "body text" {
		t.Fatalf("body arg = %q, want body text", args[4])
	}
}

func TestPushUsesExistingUpstream(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	testutil.RunGit(t, t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t, repo, "remote", "add", "origin", remote)
	testutil.RunGit(t, repo, "push", "-u", "origin", "main")

	// Second push should use the existing upstream, not set-upstream again.
	core := NewCore()
	if err := core.Push(repo); err != nil {
		t.Fatalf("Push with existing upstream returned error: %v", err)
	}
}

func TestPushSelectsNonOriginRemote(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	remote := filepath.Join(t.TempDir(), "upstream.git")
	testutil.RunGit(t, t.TempDir(), "init", "--bare", remote)
	testutil.RunGit(t, repo, "remote", "add", "upstream", remote)

	core := NewCore()
	if err := core.Push(repo); err != nil {
		t.Fatalf("Push with non-origin remote returned error: %v", err)
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
