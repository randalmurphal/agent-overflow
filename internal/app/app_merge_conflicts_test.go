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
