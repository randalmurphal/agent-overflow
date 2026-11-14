package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

// The PR's head and the base tip edit one line differently; the merge is
// computed at the revision, from commits fetched into the clone.
func TestGetPRMergeConflictsHappyPath(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	origin := initMainGitRepo(t)
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(origin, "README.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.RunGit(t, origin, "commit", "-q", "-am", content)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, origin, "clone", "-q", origin, clone)
	testutil.RunGit(t, origin, "checkout", "-q", "-b", "feature")
	write("feature\n")
	head := revParseIn(t, origin, "HEAD")
	testutil.RunGit(t, origin, "update-ref", "refs/pull/9/head", head)
	testutil.RunGit(t, origin, "checkout", "-q", "main")
	write("main\n")
	ref := testWorkspaceRef(t, app, clone)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: revParseIn(t, origin, "main"), HeadSHA: head}

	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature-branch", true)
	if err != nil {
		t.Fatalf("GetPRMergeConflicts: %v", err)
	}
	if !result.Conflicted || result.BaseLabel != "origin/main" || result.HeadLabel != "feature-branch" {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(result.Paths, ",") != "README.txt" {
		t.Fatalf("paths = %v", result.Paths)
	}

	content, err := app.GetMergeConflictFile(ref, result.TreeOID, "README.txt")
	if err != nil {
		t.Fatalf("GetMergeConflictFile: %v", err)
	}
	for _, want := range []string{"<<<<<<< ", "main", "=======", "feature", ">>>>>>> "} {
		if !strings.Contains(content, want) {
			t.Fatalf("content lacks %q:\n%s", want, content)
		}
	}
}

// staleBasePR builds the PR state forges report for a conflicted merge
// request: the recorded base is the merge base, and main has since gained a
// commit that edits the head's line differently.
func staleBasePR(t *testing.T) (origin, clone string, rev gitops.PRRevision) {
	t.Helper()
	origin = initMainGitRepo(t)
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(origin, "README.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		testutil.RunGit(t, origin, "commit", "-q", "-am", content)
	}
	clone = filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, origin, "clone", "-q", origin, clone)
	recorded := revParseIn(t, origin, "main")
	testutil.RunGit(t, origin, "checkout", "-q", "-b", "feature")
	write("feature\n")
	head := revParseIn(t, origin, "HEAD")
	testutil.RunGit(t, origin, "update-ref", "refs/pull/9/head", head)
	testutil.RunGit(t, origin, "checkout", "-q", "main")
	write("main moved\n")
	return origin, clone, gitops.PRRevision{BaseRef: "main", BaseSHA: recorded, HeadSHA: head}
}

// The conflict read merges the head with the base branch's tip on origin
// now, not with the base the forge recorded, which the head already
// contains and so can never conflict.
func TestGetPRMergeConflictsMergesWithTheLiveBaseTip(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	_, clone, rev := staleBasePR(t)
	ref := testWorkspaceRef(t, app, clone)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", true)
	if err != nil {
		t.Fatalf("GetPRMergeConflicts: %v", err)
	}
	if !result.Conflicted || strings.Join(result.Paths, ",") != "README.txt" {
		t.Fatalf("result = %+v, want README.txt conflicted against the live tip", result)
	}
	content, err := app.GetMergeConflictFile(ref, result.TreeOID, "README.txt")
	if err != nil {
		t.Fatalf("GetMergeConflictFile: %v", err)
	}
	for _, want := range []string{"<<<<<<< ", "main moved", "=======", "feature", ">>>>>>> "} {
		if !strings.Contains(content, want) {
			t.Fatalf("content lacks %q:\n%s", want, content)
		}
	}
}

// A base tip that cannot be read is an error, never a clean merge.
func TestGetPRMergeConflictsFailsWhenTheBaseTipCannotBeRead(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	origin, clone, rev := staleBasePR(t)
	// The clone has the PR's commits, so only the tip read needs origin.
	testutil.RunGit(t, clone, "fetch", "-q", origin, "refs/pull/9/head")
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	ref := testWorkspaceRef(t, app, clone)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", true)
	if err == nil {
		t.Fatalf("result = %+v, want the base tip read failure", result)
	}
	if !strings.Contains(err.Error(), "get PR merge conflicts") || !strings.Contains(err.Error(), "read main on origin") {
		t.Fatalf("err = %v, want the action and the read failure named", err)
	}
}

// Only a read the user asked for arms the pump's forced emit after a
// failure. An automatic re-read that fails reports its error and arms
// nothing, so a failure cannot chain into another automatic read on every
// poll (read fails, forced emit, reconcile, read fails, ...).
func TestGetPRMergeConflictsOnlyAUserReadArmsTheRetry(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	origin, clone, rev := staleBasePR(t)
	testutil.RunGit(t, clone, "fetch", "-q", origin, "refs/pull/9/head")
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	ref := testWorkspaceRef(t, app, clone)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	pump := &prUpdatePump{}
	app.prUpdates.mu.Lock()
	if app.prUpdates.pumps == nil {
		app.prUpdates.pumps = make(map[string]*prUpdatePump)
	}
	app.prUpdates.pumps[prUpdateKey(pr)] = pump
	app.prUpdates.mu.Unlock()
	armed := func() bool {
		app.prUpdates.mu.Lock()
		defer app.prUpdates.mu.Unlock()
		return pump.readFailed
	}

	if _, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", false); err == nil || !strings.Contains(err.Error(), "read main on origin") {
		t.Fatalf("automatic read err = %v, want the base tip read failure", err)
	}
	if armed() {
		t.Fatal("a failed automatic read armed the pump's forced emit")
	}
	if _, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", true); err == nil {
		t.Fatal("user read: want the base tip read failure")
	}
	if !armed() {
		t.Fatal("a failed user read did not arm the pump's forced emit")
	}
}

// The conflict read carries its intent to git: against a remote that asks
// for credentials, an automatic read never launches the user's askpass
// helper and a read the user asked for does. Serial: it sets the process
// environment.
func TestGetPRMergeConflictsPromptsOnlyForAUserRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script askpass helper is unix-only")
	}
	app := newTestAppWithStore(t)
	origin, clone, rev := staleBasePR(t)
	// The clone has the PR's commits, so the tip read is the network step.
	testutil.RunGit(t, clone, "fetch", "-q", origin, "refs/pull/9/head")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	testutil.RunGit(t, clone, "remote", "set-url", "origin", server.URL+"/repo.git")
	ref := testWorkspaceRef(t, app, clone)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}

	// A clean config chain: a real user's credential.helper would answer
	// before git reaches the askpass helper this test is about.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	askpassLog := filepath.Join(dir, "askpass.log")
	askpass := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho invoked >> '"+askpassLog+"'\necho someone\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	asked := func() bool {
		_, err := os.Stat(askpassLog)
		return err == nil
	}

	if _, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", false); err == nil {
		t.Fatal("automatic read against a credential-requiring remote should fail")
	}
	if asked() {
		t.Fatal("an automatic conflict read launched the askpass helper")
	}
	if _, err := app.GetPRMergeConflicts(ref, pr, rev, "feature", true); err == nil {
		t.Fatal("user read against a remote refusing every credential should fail")
	}
	if !asked() {
		t.Fatal("a user's conflict read did not reach the askpass helper")
	}
}

func readAppTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestGetPRMergeConflictsRequiresAGitWorkspace(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	// A zero ref names no project, so resolution refuses it outright.
	sha := strings.Repeat("a", 40)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: sha}
	if _, err := app.GetPRMergeConflicts(WorkspaceRef{}, pr, rev, "", true); err == nil {
		t.Fatal("zero ref: expected a workspace resolution error")
	}
	noClone := testWorkspaceRef(t, app, t.TempDir())
	_, err := app.GetPRMergeConflicts(noClone, pr, rev, "", true)
	if err == nil || !strings.Contains(err.Error(), "workspace is not a git repository") {
		t.Fatalf("error = %v, want the not-a-repository refusal", err)
	}
}
