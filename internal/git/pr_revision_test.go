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

// staleBaseOrigin builds an origin whose main gained a commit conflicting
// with the PR head after the PR was recorded, as forges report it: the
// recorded base is the merge base, which main has since moved past. The
// clone predates the head and the conflicting commit.
func staleBaseOrigin(t *testing.T) (origin, clone string, rev PRRevision) {
	t.Helper()
	origin = testutil.InitGitRepo(t)
	commitFile(t, origin, "shared.txt", "original\n")
	clone = filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, origin, "clone", "-q", origin, clone)
	recorded := revParse(t, origin, "main")
	testutil.RunGit(t, origin, "checkout", "-q", "-b", "feature")
	commitFile(t, origin, "shared.txt", "from the feature\n")
	head := revParse(t, origin, "HEAD")
	testutil.RunGit(t, origin, "update-ref", "refs/pull/5/head", head)
	testutil.RunGit(t, origin, "checkout", "-q", "main")
	commitFile(t, origin, "shared.txt", "from main\n")
	return origin, clone, PRRevision{BaseRef: "main", BaseSHA: recorded, HeadSHA: head}
}

// Merging the head with the recorded base cannot conflict; merging it with
// the base branch's tip, fetched now, does.
func TestFetchBranchTipFindsConflictsTheRecordedBaseHides(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	core := NewCore()
	if err := core.EnsurePRObjects(clone, prRevisionPR, rev); err != nil {
		t.Fatal(err)
	}
	recorded, err := core.MergeTreeConflicts(clone, rev.BaseSHA, rev.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Conflicted {
		t.Fatalf("fixture: the recorded base conflicts: %+v", recorded)
	}

	tip, err := core.FetchBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("FetchBranchTip: %v", err)
	}
	if want := revParse(t, origin, "main"); tip != want {
		t.Fatalf("tip = %s, want origin's main %s", tip, want)
	}
	live, err := core.MergeTreeConflicts(clone, tip, rev.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !live.Conflicted || strings.Join(live.Paths, ",") != "shared.txt" {
		t.Fatalf("live merge = %+v, want shared.txt conflicted", live)
	}
	content, err := core.ShowTreeFile(clone, live.TreeOID, "shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<<<<<<< ", "from main", "=======", "from the feature", ">>>>>>> "} {
		if !strings.Contains(content, want) {
			t.Fatalf("content lacks %q:\n%s", want, content)
		}
	}
}

// The tip is fetched every time, even when the clone already has the
// recorded base and an older origin/main.
func TestFetchBranchTipFollowsTheBranchAcrossCalls(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	core := NewCore()
	first, err := core.FetchBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, origin, "later.txt", "later\n")
	second, err := core.FetchBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatal(err)
	}
	if want := revParse(t, origin, "main"); second != want || second == first {
		t.Fatalf("second tip = %s (first %s), want origin's new main %s", second, first, want)
	}
	// An unchanged branch still answers its tip.
	third, err := core.FetchBranchTip(clone, rev.BaseRef)
	if err != nil || third != second {
		t.Fatalf("unchanged tip = %s, %v; want %s", third, err, second)
	}
}

func TestFetchBranchTipReportsAnUnreachableOrigin(t *testing.T) {
	_, clone, rev := staleBaseOrigin(t)
	breakOrigin(t, clone)
	_, err := NewCore().FetchBranchTip(clone, rev.BaseRef)
	if err == nil || !strings.Contains(err.Error(), "fetch main from origin") || !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("err = %v, want the fetch failure with git's diagnosis", err)
	}
}

func TestFetchBranchTipReportsAMissingBranch(t *testing.T) {
	_, clone, _ := staleBaseOrigin(t)
	_, err := NewCore().FetchBranchTip(clone, "no-such-branch")
	if err == nil || !strings.Contains(err.Error(), "couldn't find remote ref") {
		t.Fatalf("err = %v, want git's missing-ref diagnosis", err)
	}
}

// A fetch that brought the tip but lost the origin/main update to a held
// lock still answers the tip.
func TestFetchBranchTipSucceedsWhenTheRefUpdateFails(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	lock := filepath.Join(clone, ".git", "refs", "remotes", "origin", "main.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tip, err := NewCore().FetchBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("FetchBranchTip with a held ref lock: %v", err)
	}
	if want := revParse(t, origin, "main"); tip != want {
		t.Fatalf("tip = %s, want %s", tip, want)
	}
}

func TestPorcelainFetchHead(t *testing.T) {
	sha := strings.Repeat("a", 40)
	zero := strings.Repeat("0", 40)
	for _, tc := range []struct {
		name, stdout, want string
	}{
		{"fetch head only", "* " + zero + " " + sha + " FETCH_HEAD\n", sha},
		{"with tracking update", "* " + zero + " " + sha + " FETCH_HEAD\n  " + zero + " " + sha + " refs/remotes/origin/main\n", sha},
		{"tracking only", "  " + zero + " " + sha + " refs/remotes/origin/main\n", ""},
		{"empty", "", ""},
		{"not an oid", "* " + zero + " nope FETCH_HEAD\n", ""},
	} {
		if got := porcelainFetchHead(tc.stdout); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
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
