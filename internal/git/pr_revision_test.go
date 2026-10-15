package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

var prRevisionPR = PRReference{Forge: "github", Host: "github.com", Namespace: "owner", Repo: "repo", Number: 5}

// prOrigin builds an origin repository whose main has moved on after a
// feature branch published as refs/pull/5/head, and a clone made before
// either existed: the clone has neither the PR head nor the base tip.
func prOrigin(t *testing.T) (origin, clone string, rev PRRevision) {
	t.Helper()
	origin = testutil.InitGitRepo(t)
	clone = filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, origin, "clone", "-q", origin, clone)
	testutil.RunGit(t, origin, "checkout", "-q", "-b", "feature")
	commitFile(t, origin, "feature.txt", "feature\n")
	head := revParse(t, origin, "HEAD")
	testutil.RunGit(t, origin, "update-ref", "refs/pull/5/head", head)
	testutil.RunGit(t, origin, "checkout", "-q", "main")
	commitFile(t, origin, "base.txt", "base moved on\n")
	return origin, clone, PRRevision{BaseRef: "main", BaseSHA: revParse(t, origin, "main"), HeadSHA: head}
}

func commitFile(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "add", name)
	testutil.RunGit(t, repo, "commit", "-q", "-m", "add "+name)
}

func breakOrigin(t *testing.T, clone string) {
	t.Helper()
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
}

func TestEnsurePRObjectsFetchesMissingCommits(t *testing.T) {
	_, clone, rev := prOrigin(t)
	core := NewCore()
	if err := core.EnsurePRObjects(clone, prRevisionPR, rev); err != nil {
		t.Fatalf("EnsurePRObjects: %v", err)
	}
	missing, err := core.missingCommits(clone, rev.HeadSHA, rev.BaseSHA)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing after ensure = %v, %v", missing, err)
	}
	// The base branch is fetched by name, so origin/main follows it.
	if got := revParse(t, clone, "refs/remotes/origin/main"); got != rev.BaseSHA {
		t.Fatalf("origin/main = %s, want %s", got, rev.BaseSHA)
	}
}

// Commits already present need no network: with origin unreachable the
// call still succeeds.
func TestEnsurePRObjectsSkipsTheNetworkWhenCommitsAreLocal(t *testing.T) {
	_, clone, rev := prOrigin(t)
	core := NewCore()
	if err := core.EnsurePRObjects(clone, prRevisionPR, rev); err != nil {
		t.Fatal(err)
	}
	breakOrigin(t, clone)
	if err := core.EnsurePRObjects(clone, prRevisionPR, rev); err != nil {
		t.Fatalf("EnsurePRObjects with local commits and no origin: %v", err)
	}
}

func TestEnsurePRObjectsReportsTheFetchFailureWhenCommitsStayMissing(t *testing.T) {
	_, clone, rev := prOrigin(t)
	breakOrigin(t, clone)
	err := NewCore().EnsurePRObjects(clone, prRevisionPR, rev)
	if err == nil {
		t.Fatal("expected an error with the commits missing and origin unreachable")
	}
	// git's own diagnosis reaches the caller, not just an exit code.
	for _, want := range []string{"fetch pull request commits", "exited with code 128", "fatal:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
}

// A fetch that brought the commits but failed a ref update (another git
// process holds the ref) did its job.
func TestEnsurePRObjectsSucceedsWhenTheFetchFailsAfterTransferring(t *testing.T) {
	holdOriginMainLock := func(clone string) {
		lock := filepath.Join(clone, ".git", "refs", "remotes", "origin", "main.lock")
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	core := NewCore()
	// The fixture's fetch does fail under the held lock.
	_, probe, _ := prOrigin(t)
	holdOriginMainLock(probe)
	if _, _, err := core.Execute(probe, "fetch", "origin", "pull/5/head", "main"); err == nil {
		t.Fatal("fixture: expected the held ref lock to fail the fetch")
	}

	_, clone, rev := prOrigin(t)
	holdOriginMainLock(clone)
	if err := core.EnsurePRObjects(clone, prRevisionPR, rev); err != nil {
		t.Fatalf("EnsurePRObjects with a held ref lock: %v", err)
	}
}

func TestEnsurePRObjectsNamesACommitOriginDoesNotHave(t *testing.T) {
	_, clone, rev := prOrigin(t)
	rev.HeadSHA = strings.Repeat("d", 40)
	err := NewCore().EnsurePRObjects(clone, prRevisionPR, rev)
	if err == nil || !strings.Contains(err.Error(), "origin has no commit "+rev.HeadSHA) {
		t.Fatalf("err = %v, want the missing head named", err)
	}
}

func TestPRRevisionValidate(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		rev  PRRevision
		ok   bool
	}{
		{"valid", PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: sha}, true},
		{"sha256", PRRevision{BaseRef: "main", BaseSHA: strings.Repeat("b", 64), HeadSHA: sha}, true},
		{"no base ref", PRRevision{BaseSHA: sha, HeadSHA: sha}, false},
		{"flag base ref", PRRevision{BaseRef: "-main", BaseSHA: sha, HeadSHA: sha}, false},
		{"no base sha", PRRevision{BaseRef: "main", HeadSHA: sha}, false},
		{"short head", PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: "abc123"}, false},
		{"rev expression", PRRevision{BaseRef: "main", BaseSHA: sha, HeadSHA: "HEAD~1"}, false},
	} {
		if err := tc.rev.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
