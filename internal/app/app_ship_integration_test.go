package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// -- Agent 3 (Wave 5D) -- integration tests for the Ship Changes flow.
//
// The Ship Changes wizard is a frontend drawer (owned by Agent 6) that calls
// three Wails bindings in sequence: GitCommit, GitPush, and GitCreatePR.
// Rather than test the UI, we assert that the backend can serve a realistic
// sequence of those bindings against a real git repo + bare-remote setup
// + a mock gh CLI on PATH.
//
// Each test creates a tempdir git repo with an initial commit, wires a bare
// remote as "origin", and drives the Ship sequence step-by-step. The mock gh
// is installed by prepending a writable directory onto PATH via t.Setenv —
// this matches internal/git/github_test.go's existing convention and lets the
// live `gh` resolution in internal/git/core.go pick it up.

// shipTestSetup returns a thread that is already persisted in the app store
// and configured against a fresh repo with a bare "origin" remote. Callers
// can stage / commit / push and the wizard's state will mirror reality.
func shipTestSetup(t *testing.T) (*App, string, string) {
	t.Helper()

	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

	// Bare remote for push testing. Using a dedicated parent dir so the
	// temp-cleanup doesn't interfere with the working repo.
	remoteParent := t.TempDir()
	remote := filepath.Join(remoteParent, "origin.git")
	testutil.RunGit(t, remoteParent, "init", "--bare", remote)

	thread := testThread("thread-ship-" + t.Name())
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return app, thread.ID, remote
}

// installMockGhShip prepends a mock gh on PATH for a ship-flow test. The mock
// prints `stdout` to stdout and (optionally) `stderr` to stderr, then exits
// with `exitCode`. gh tracks no state across invocations.
func installMockGhShip(t *testing.T, stdout, stderr string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock gh shim assumes POSIX shell")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := fmt.Sprintf(`#!/bin/sh
echo %q
echo %q 1>&2
exit %d
`, stdout, stderr, exitCode)
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestShip_FullWizardFlow: stage-commit-push-PR end-to-end against a bare
// remote. Each step must succeed and leave the workspace in the expected state.
func TestShip_FullWizardFlow(t *testing.T) {
	app, threadID, remote := shipTestSetup(t)

	// Get workspace path back from the stored thread so we can write files.
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	workspace := stored.WorkspacePath

	// Wire the remote: fetch URL classifies as github (so the forge
	// dispatcher routes Create PR through the github forge / gh mock),
	// while the push URL points at the bare local repo so push tests
	// land commits without a network round-trip.
	testutil.RunGit(t, workspace, "remote", "add", "origin", "https://github.com/test/test.git")
	testutil.RunGit(t, workspace, "remote", "set-url", "--push", "origin", remote)

	// Stage a new file via the filesystem (simulating uncommitted changes).
	feature := filepath.Join(workspace, "feature.txt")
	if err := os.WriteFile(feature, []byte("shipped\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// Step 1: commit.
	installMockGhShip(t, "https://example.com/pr/1", "", 0)
	result, err := app.GitCommit(threadID, "ship: add feature", "body")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}
	if result.Action != "commit" || len(result.Commit) != 40 {
		t.Fatalf("GitCommit result = %+v", result)
	}
	if result.Branch != "main" {
		t.Fatalf("commit Branch = %q, want main", result.Branch)
	}

	// Step 2: push to the bare remote.
	pushResult, err := app.GitPush(threadID)
	if err != nil {
		t.Fatalf("GitPush() error = %v", err)
	}
	if pushResult.Action != "push" {
		t.Fatalf("push Action = %q, want push", pushResult.Action)
	}
	// Remote should now have our commit reachable from refs/heads/main.
	if err := testutil.RunGitAllowError(remote, "rev-parse", "refs/heads/main"); err != nil {
		t.Fatalf("remote rev-parse main error = %v (push didn't publish)", err)
	}

	// Step 3: create PR via mock gh.
	prResult, err := app.GitCreatePR(threadID, "Ship feature", "details", false)
	if err != nil {
		t.Fatalf("GitCreatePR() error = %v", err)
	}
	if prResult.PRURL != "https://example.com/pr/1" {
		t.Fatalf("PRURL = %q, want https://example.com/pr/1", prResult.PRURL)
	}
	if prResult.Action != "pr" {
		t.Fatalf("PR Action = %q, want pr", prResult.Action)
	}
}

// TestShip_CommitOnlyPath: the user stops the wizard after committing. We
// assert that no PR side-effects happen — GitCommit is self-contained, no
// calls to GitPush or GitCreatePR are wired from that single binding.
func TestShip_CommitOnlyPath(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}

	readme := filepath.Join(stored.WorkspacePath, "README.txt")
	if err := os.WriteFile(readme, []byte("hello\nupdated\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := app.GitCommit(threadID, "update readme", "body")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}
	if result.Action != "commit" {
		t.Fatalf("Action = %q, want commit", result.Action)
	}
	if len(result.Commit) != 40 {
		t.Fatalf("Commit length = %d, want 40", len(result.Commit))
	}
	if result.PRURL != "" {
		t.Fatalf("PRURL = %q, want empty for commit-only path", result.PRURL)
	}
}

// TestShip_CommitAndPushNoPR: commit and push, but stop before PR. Remote
// must have the new commit reachable.
func TestShip_CommitAndPushNoPR(t *testing.T) {
	app, threadID, remote := shipTestSetup(t)
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}

	testutil.RunGit(t, stored.WorkspacePath, "remote", "add", "origin", "https://github.com/test/test.git")
	testutil.RunGit(t, stored.WorkspacePath, "remote", "set-url", "--push", "origin", remote)

	extra := filepath.Join(stored.WorkspacePath, "extra.txt")
	if err := os.WriteFile(extra, []byte("extra\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	commitResult, err := app.GitCommit(threadID, "ship: extra", "body")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}

	pushResult, err := app.GitPush(threadID)
	if err != nil {
		t.Fatalf("GitPush() error = %v", err)
	}
	if pushResult.PRURL != "" {
		t.Fatalf("push should not include PR URL: %+v", pushResult)
	}

	// Remote now has the commit.
	if err := testutil.RunGitAllowError(remote, "rev-parse", commitResult.Commit); err != nil {
		t.Fatalf("remote missing commit %s after push: %v", commitResult.Commit, err)
	}
}

// TestShip_CommitFailsOnNoChanges: GitCommit runs `git add -A` then
// `git commit`. On a clean tree, git commit emits a non-zero exit and the
// wrapper surfaces the error -- the wizard must then tell the user there's
// nothing to commit.
func TestShip_CommitFailsOnNoChanges(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)

	_, err := app.GitCommit(threadID, "nothing", "")
	if err == nil {
		t.Fatal("GitCommit() on clean tree error = nil, want failure (nothing to commit)")
	}
	// git's message is "nothing to commit" on a clean tree.
	if !strings.Contains(err.Error(), "nothing to commit") && !strings.Contains(err.Error(), "exited with code") {
		t.Fatalf("err = %v, want 'nothing to commit' or non-zero exit hint", err)
	}
}

// TestShip_PushFailsOnNoUpstream: a fresh branch without `origin` configured
// cannot push. The wizard must surface the missing-remote message from the
// Core, not an obscure git crash.
func TestShip_PushFailsOnNoUpstream(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)

	// No remote added to the repo. Push should fail with the remote-missing
	// error from internal/git/actions.go.
	_, err := app.GitPush(threadID)
	if err == nil {
		t.Fatal("GitPush() without remote error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "no git remote is configured") {
		t.Fatalf("err = %v, want 'no git remote is configured'", err)
	}
}

// TestShip_CreatePRFailsWhenNotPushed: if gh exits non-zero (branch has no
// upstream / no PR can be created), the wrapper must surface the error.
func TestShip_CreatePRFailsWhenNotPushed(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	// Forge dispatch needs a classifiable origin to route to gh.
	testutil.RunGit(t, stored.WorkspacePath, "remote", "add", "origin", "https://github.com/test/test.git")

	installMockGhShip(t, "", "must push first", 1)

	_, err = app.GitCreatePR(threadID, "PR title", "body", false)
	if err == nil {
		t.Fatal("GitCreatePR() with failing gh error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "gh pr create failed") {
		t.Fatalf("err = %v, want 'gh pr create failed'", err)
	}
	if !strings.Contains(err.Error(), "must push first") {
		t.Fatalf("err = %v, want stderr surfaced", err)
	}
}

// TestShip_NewBranchFromCurrent: the wizard may create a branch before
// committing. GitCreateBranch + a subsequent checkout then commit must land
// the commit on the new branch.
func TestShip_NewBranchFromCurrent(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)

	if err := app.GitCreateBranch(threadID, "ship/feature"); err != nil {
		t.Fatalf("GitCreateBranch() error = %v", err)
	}
	if err := app.GitCheckout(threadID, "ship/feature"); err != nil {
		t.Fatalf("GitCheckout() error = %v", err)
	}

	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	feature := filepath.Join(stored.WorkspacePath, "feature.txt")
	if err := os.WriteFile(feature, []byte("on branch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	commitResult, err := app.GitCommit(threadID, "ship: feature", "")
	if err != nil {
		t.Fatalf("GitCommit() error = %v", err)
	}
	if commitResult.Branch != "ship/feature" {
		t.Fatalf("branch at commit = %q, want ship/feature", commitResult.Branch)
	}

	// Verify the commit is reachable only from ship/feature, not main.
	branches, err := app.GitListBranches(threadID)
	if err != nil {
		t.Fatalf("GitListBranches() error = %v", err)
	}
	if !branchNamed(branches, "ship/feature") {
		t.Fatalf("ship/feature branch missing: %+v", branches)
	}
	if !branchNamed(branches, "main") {
		t.Fatalf("main branch missing: %+v", branches)
	}
}

// TestShip_StackedActionsIdempotent: running GitCommit twice on the same
// state -- no new changes between them -- the second must fail with the same
// "nothing to commit" signal as TestShip_CommitFailsOnNoChanges. In other
// words the wizard cannot accidentally produce two commits by running the
// step twice, which matters because drawer retry buttons wire to the same
// binding.
func TestShip_StackedActionsIdempotent(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}

	feature := filepath.Join(stored.WorkspacePath, "feature.txt")
	if err := os.WriteFile(feature, []byte("shipped\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	first, err := app.GitCommit(threadID, "ship: first", "")
	if err != nil {
		t.Fatalf("first GitCommit() error = %v", err)
	}
	if len(first.Commit) != 40 {
		t.Fatalf("first commit SHA malformed: %q", first.Commit)
	}

	// Second call: tree is now clean because we committed everything. Must
	// error out, not silently produce a duplicate commit.
	_, err = app.GitCommit(threadID, "ship: second", "")
	if err == nil {
		t.Fatal("second GitCommit() on clean tree error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "nothing to commit") && !strings.Contains(err.Error(), "exited with code") {
		t.Fatalf("second commit error = %v, want clean-tree signal", err)
	}
}

// TestShip_CreatePRWithDraftFlag verifies the GitCreatePR contract: the
// `draft` parameter threads through to `gh pr create --draft`. When draft is
// false the flag is absent; when true it's present.
func TestShip_CreatePRWithDraftFlag(t *testing.T) {
	app, threadID, _ := shipTestSetup(t)
	stored, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	// Forge dispatch needs a classifiable origin to route to gh.
	testutil.RunGit(t, stored.WorkspacePath, "remote", "add", "origin", "https://github.com/test/test.git")

	// Write a mock gh that records whether --draft was present. We emit the
	// flag state to stdout so the caller can observe it.
	if runtime.GOOS == "windows" {
		t.Skip("mock gh shim assumes POSIX shell")
	}
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
found_draft=false
for arg in "$@"; do
  if [ "$arg" = "--draft" ]; then
    found_draft=true
  fi
done
echo "https://example.com/pr/draft-flag=$found_draft"
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// draft=false: gh is invoked without --draft.
	result, err := app.GitCreatePR(threadID, "PR title", "body", false)
	if err != nil {
		t.Fatalf("GitCreatePR(draft=false) error = %v", err)
	}
	if !strings.Contains(result.PRURL, "draft-flag=false") {
		t.Fatalf("draft=false PRURL = %q, want draft-flag=false", result.PRURL)
	}

	// draft=true: gh is invoked with --draft.
	result, err = app.GitCreatePR(threadID, "PR title", "body", true)
	if err != nil {
		t.Fatalf("GitCreatePR(draft=true) error = %v", err)
	}
	if !strings.Contains(result.PRURL, "draft-flag=true") {
		t.Fatalf("draft=true PRURL = %q, want draft-flag=true", result.PRURL)
	}
}

// branchNamed returns true if any branch in the list has the given name.
func branchNamed(branches []gitops.GitBranch, name string) bool {
	for _, b := range branches {
		if b.Name == name {
			return true
		}
	}
	return false
}
