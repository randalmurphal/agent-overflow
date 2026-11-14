package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
// the base branch's tip on origin, read now, does.
func TestRemoteBranchTipFindsConflictsTheRecordedBaseHides(t *testing.T) {
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

	tip, err := core.RemoteBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
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

// countUploadPack has the clone reach origin through a wrapper around
// git-upload-pack that appends one line to the returned log per connection
// (one per ls-remote, one per fetch). With moveTo set, the second
// connection first force-moves origin's main to it, so the branch moves
// between the tip read and the fetch.
func countUploadPack(t *testing.T, origin, clone, moveTo string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the upload-pack wrapper is a POSIX shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "connections")
	move := ""
	if moveTo != "" {
		move = fmt.Sprintf("if [ \"$(wc -l < '%s')\" -eq 2 ]; then git -C '%s' update-ref refs/heads/main %s; fi\n", log, origin, moveTo)
	}
	script := filepath.Join(dir, "upload-pack.sh")
	body := "#!/bin/sh\necho connection >> '" + log + "'\n" + move + "exec git upload-pack \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, clone, "config", "remote.origin.uploadpack", script)
	return log
}

func connections(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// A tip the clone already has costs the ls-remote alone: no fetch, and
// origin/main stays where it was.
func TestRemoteBranchTipUsesALocalTipWithoutFetching(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	// The commit arrives without origin/main following it: a fetch by path
	// writes FETCH_HEAD only.
	testutil.RunGit(t, clone, "fetch", "-q", origin, "main")
	tracking := revParse(t, clone, "refs/remotes/origin/main")
	log := countUploadPack(t, origin, clone, "")

	tip, err := NewCore().RemoteBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
	}
	if want := revParse(t, origin, "main"); tip != want {
		t.Fatalf("tip = %s, want %s", tip, want)
	}
	if n := connections(t, log); n != 1 {
		t.Fatalf("connections to origin = %d, want the ls-remote alone", n)
	}
	if got := revParse(t, clone, "refs/remotes/origin/main"); got != tracking {
		t.Fatalf("origin/main moved to %s, want it left at %s", got, tracking)
	}
}

// A tip the clone lacks is fetched once, and the tip read again later
// follows the branch.
func TestRemoteBranchTipFetchesAMissingTip(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	log := countUploadPack(t, origin, clone, "")
	core := NewCore()

	tip, err := core.RemoteBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
	}
	if want := revParse(t, origin, "main"); tip != want {
		t.Fatalf("tip = %s, want %s", tip, want)
	}
	if n := connections(t, log); n != 2 {
		t.Fatalf("connections to origin = %d, want ls-remote and one fetch", n)
	}
	if missing, err := core.missingCommits(clone, tip); err != nil || len(missing) != 0 {
		t.Fatalf("tip missing after the read: %v, %v", missing, err)
	}
	// The branch is fetched by name, so origin/main follows it.
	if got := revParse(t, clone, "refs/remotes/origin/main"); got != tip {
		t.Fatalf("origin/main = %s, want %s", got, tip)
	}

	commitFile(t, origin, "later.txt", "later\n")
	later, err := core.RemoteBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatal(err)
	}
	if want := revParse(t, origin, "main"); later != want || later == tip {
		t.Fatalf("later tip = %s (first %s), want origin's new main %s", later, tip, want)
	}
}

func TestRemoteBranchTipReportsABranchOriginLacks(t *testing.T) {
	_, clone, _ := staleBaseOrigin(t)
	_, err := NewCore().RemoteBranchTip(clone, "no-such-branch")
	if err == nil || !strings.Contains(err.Error(), "origin has no branch no-such-branch") {
		t.Fatalf("err = %v, want the absent branch named", err)
	}
}

func TestRemoteBranchTipReportsAnUnreachableOrigin(t *testing.T) {
	_, clone, rev := staleBaseOrigin(t)
	breakOrigin(t, clone)
	_, err := NewCore().RemoteBranchTip(clone, rev.BaseRef)
	if err == nil || !strings.Contains(err.Error(), "read main on origin") || !strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("err = %v, want the ls-remote failure with git's diagnosis", err)
	}
}

// A branch force-moved between the ls-remote and the fetch leaves the tip
// it named missing; that is an error, read once, not a loop.
func TestRemoteBranchTipReportsABranchThatMovedDuringTheRead(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	// A commit that does not contain main's tip, for main to jump to.
	testutil.RunGit(t, origin, "checkout", "-q", "--orphan", "elsewhere")
	commitFile(t, origin, "other.txt", "other\n")
	elsewhere := revParse(t, origin, "HEAD")
	testutil.RunGit(t, origin, "checkout", "-q", "-f", "main")
	tip := revParse(t, origin, "main")
	log := countUploadPack(t, origin, clone, elsewhere)

	_, err := NewCore().RemoteBranchTip(clone, rev.BaseRef)
	if err == nil || !strings.Contains(err.Error(), "main moved on origin during the read") || !strings.Contains(err.Error(), tip) {
		t.Fatalf("err = %v, want the move during the read named with tip %s", err, tip)
	}
	if n := connections(t, log); n != 2 {
		t.Fatalf("connections to origin = %d, want ls-remote and one fetch", n)
	}
}

// A fetch that brought the tip but lost the origin/main update to a held
// lock still answers the tip.
func TestRemoteBranchTipSucceedsWhenTheRefUpdateFails(t *testing.T) {
	origin, clone, rev := staleBaseOrigin(t)
	lock := filepath.Join(clone, ".git", "refs", "remotes", "origin", "main.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tip, err := NewCore().RemoteBranchTip(clone, rev.BaseRef)
	if err != nil {
		t.Fatalf("RemoteBranchTip with a held ref lock: %v", err)
	}
	if want := revParse(t, origin, "main"); tip != want {
		t.Fatalf("tip = %s, want %s", tip, want)
	}
}

func TestLSRemoteTip(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	const ref = "refs/heads/main"
	for _, tc := range []struct {
		name, stdout, want, err string
	}{
		{"one line", sha + "\t" + ref + "\n", sha, ""},
		{"other refs skipped", other + "\trefs/heads/x/refs/heads/main\n" + sha + "\t" + ref + "\n", sha, ""},
		{"none", "", "", "origin has no branch main"},
		{"only a suffix match", other + "\trefs/remotes/origin/refs/heads/main\n", "", "origin has no branch main"},
		{"twice", sha + "\t" + ref + "\n" + other + "\t" + ref + "\n", "", "more than once"},
		{"not an oid", "nope\t" + ref + "\n", "", "not an object ID"},
	} {
		got, err := lsRemoteTip(tc.stdout, ref)
		if tc.err == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.err)
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
