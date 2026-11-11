package app

import (
	"os"
	"path/filepath"
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
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature-branch")
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
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature")
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

// A base tip that cannot be fetched is an error, never a clean merge.
func TestGetPRMergeConflictsFailsWhenTheBaseTipCannotBeFetched(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	origin, clone, rev := staleBasePR(t)
	// The clone has the PR's commits, so only the tip read needs origin.
	testutil.RunGit(t, clone, "fetch", "-q", origin, "refs/pull/9/head")
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	ref := testWorkspaceRef(t, app, clone)
	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 9}
	result, err := app.GetPRMergeConflicts(ref, pr, rev, "feature")
	if err == nil {
		t.Fatalf("result = %+v, want the base tip fetch failure", result)
	}
	if !strings.Contains(err.Error(), "get PR merge conflicts") || !strings.Contains(err.Error(), "fetch main from origin") {
		t.Fatalf("err = %v, want the action and the fetch failure named", err)
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
	if _, err := app.GetPRMergeConflicts(WorkspaceRef{}, pr, rev, ""); err == nil {
		t.Fatal("zero ref: expected a workspace resolution error")
	}
	noClone := testWorkspaceRef(t, app, t.TempDir())
	_, err := app.GetPRMergeConflicts(noClone, pr, rev, "")
	if err == nil || !strings.Contains(err.Error(), "workspace is not a git repository") {
		t.Fatalf("error = %v, want the not-a-repository refusal", err)
	}
}
