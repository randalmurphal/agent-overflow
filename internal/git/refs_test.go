package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// emptyCommit makes an empty commit in repo and returns its id.
func emptyCommit(t *testing.T, repo, message string) string {
	t.Helper()
	testutil.RunGit(t, repo, "commit", "-q", "--allow-empty", "-m", message)
	return revParse(t, repo, "HEAD")
}

// annotatedTag tags rev with a pinned tagger date.
func annotatedTag(t *testing.T, repo, name, rev, date string) {
	t.Helper()
	cmd := exec.Command("git", "tag", "-a", "-m", name, name, rev)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag %s: %v\n%s", name, err, out)
	}
}

// collisionRepo has a branch "dup" at the initial commit and a tag "dup"
// at a later commit on main, so every operation that lets git guess picks
// the wrong one. It returns the repo and the branch and tag commits.
func collisionRepo(t *testing.T) (repo, branchTip, tagTip string) {
	t.Helper()
	repo = initGitRepo(t)
	testutil.RunGit(t, repo, "branch", "dup")
	branchTip = revParse(t, repo, "HEAD")
	tagTip = emptyCommit(t, repo, "tagged")
	testutil.RunGit(t, repo, "tag", "dup", tagTip)
	return repo, branchTip, tagTip
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wire    string
		want    Ref
		wantErr bool
	}{
		{"", Ref{}, false},
		{"  ", Ref{}, false},
		{"main", BranchRef("main"), false},
		{"feature/x", BranchRef("feature/x"), false},
		{"refs/tags/v1.2", TagRef("v1.2"), false},
		{"refs/tags/", Ref{}, true},
		{"refs/tags/-f", Ref{}, true},
		{"-f", Ref{}, true},
		{"refs/heads/main", BranchRef("main"), false},
		{"refs/heads/refs/tags/v1.0", BranchRef("refs/tags/v1.0"), false},
		{"refs/heads/refs/heads/x", BranchRef("refs/heads/x"), false},
		{"refs/heads/", Ref{}, true},
		{"refs/heads/-f", Ref{}, true},
		{"refs/remotes/origin/x", BranchRef("refs/remotes/origin/x"), false},
	} {
		got, err := ParseRef(tc.wire)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v, err %v", tc.wire, got, err, tc.want, tc.wantErr)
		}
	}
	if wire := TagRef("v1").Wire(); wire != "refs/tags/v1" {
		t.Fatalf("TagRef wire = %q", wire)
	}
	if wire := BranchRef("main").Wire(); wire != "main" {
		t.Fatalf("BranchRef wire = %q", wire)
	}
}

// Every branch and tag name is addressable: Wire then ParseRef gives the
// same Ref back.
func TestRefWireRoundTrips(t *testing.T) {
	t.Parallel()
	for _, ref := range []Ref{
		BranchRef("main"),
		BranchRef("feature/x"),
		TagRef("v1.0"),
		BranchRef("refs/tags/v1.0"),
		BranchRef("refs/heads/x"),
		BranchRef("refs/remotes/origin/x"),
		TagRef("refs/heads/x"),
	} {
		got, err := ParseRef(ref.Wire())
		if err != nil || got != ref {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", ref.Wire(), got, err, ref)
		}
	}
	if wire := BranchRef("refs/tags/v1.0").Wire(); wire != "refs/heads/refs/tags/v1.0" {
		t.Errorf("branch refs/tags/v1.0 wire = %q", wire)
	}
}

func TestListBranchesNamesABranchThatSharesItsNameWithATag(t *testing.T) {
	t.Parallel()
	repo, _, _ := collisionRepo(t)
	branches, err := NewCore().ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, branch := range branches {
		names = append(names, branch.Name)
		if branch.Date <= 0 {
			t.Fatalf("branch %s has no date: %+v", branch.Name, branch)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "dup,main" {
		t.Fatalf("branches = %v, want dup and main", names)
	}
}

// A detached HEAD is no branch: the listing has no "(HEAD detached at ...)"
// row and no current branch.
func TestListBranchesOnADetachedHeadListsOnlyBranches(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "tag", "v1")
	testutil.RunGit(t, repo, "branch", "feature")
	testutil.RunGit(t, repo, "checkout", "-q", "--detach", "v1")
	branches, err := NewCore().ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, branch := range branches {
		names = append(names, branch.Name)
		if branch.IsCurrent {
			t.Fatalf("branch %s is current on a detached HEAD: %+v", branch.Name, branches)
		}
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "feature,main" {
		t.Fatalf("branches = %v, want feature and main", names)
	}
}

func TestListTagsOrdersNewestFirstThenByName(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	annotatedTag(t, repo, "v1", "HEAD", "2020-01-01T00:00:00Z")
	annotatedTag(t, repo, "v3", "HEAD", "2024-01-01T00:00:00Z")
	annotatedTag(t, repo, "b-same", "HEAD", "2022-01-01T00:00:00Z")
	annotatedTag(t, repo, "a-same", "HEAD", "2022-01-01T00:00:00Z")

	tags, err := NewCore().ListTags(repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	if strings.Join(names, ",") != "v3,a-same,b-same,v1" {
		t.Fatalf("tags = %v, want [v3 a-same b-same v1]", names)
	}
	if tags[0].Date != 1704067200 {
		t.Fatalf("v3 date = %d, want 1704067200", tags[0].Date)
	}

	empty, err := NewCore().ListTags(initGitRepo(t))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("ListTags of a repo with no tags = %#v, %v; want an empty list", empty, err)
	}
}

func TestStatusTagIsSetOnlyOnADetachedHeadAtATag(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	annotatedTag(t, repo, "older", "HEAD", "2020-01-01T00:00:00Z")
	annotatedTag(t, repo, "newer", "HEAD", "2024-01-01T00:00:00Z")
	core := NewCore()

	status, err := core.StatusFast(repo)
	if err != nil || status.Tag != "" || status.HeadCommit != "" || status.Branch != "main" {
		t.Fatalf("on main: status = branch %q tag %q head %q, %v", status.Branch, status.Tag, status.HeadCommit, err)
	}
	tagged := revParse(t, repo, "HEAD")
	if err := core.CheckoutRef(repo, TagRef("older")); err != nil {
		t.Fatal(err)
	}
	status, err = core.StatusFast(repo)
	if err != nil || status.Branch != "" || status.Tag != "newer" || status.HeadCommit != tagged {
		t.Fatalf("detached at two tags: status = branch %q tag %q head %q, %v; want the newest tag at %s", status.Branch, status.Tag, status.HeadCommit, err, tagged)
	}
	past := emptyCommit(t, repo, "past the tags")
	status, err = core.StatusFast(repo)
	if err != nil || status.Branch != "" || status.Tag != "" || status.HeadCommit != past {
		t.Fatalf("detached at no tag: status = branch %q tag %q head %q, %v; want head %s", status.Branch, status.Tag, status.HeadCommit, err, past)
	}
}

func TestStatusOfAnUnbornBranchHasNoHeadCommit(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-q", "-b", "main")
	status, err := NewCore().StatusFast(repo)
	if err != nil || status.Branch != "main" || status.HeadCommit != "" || status.Tag != "" {
		t.Fatalf("unborn main: status = branch %q tag %q head %q, %v", status.Branch, status.Tag, status.HeadCommit, err)
	}
}

func TestCurrentRef(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "tag", "v1")
	core := NewCore()
	if ref, err := core.CurrentRef(repo); err != nil || ref != BranchRef("main") {
		t.Fatalf("on main: CurrentRef = %+v, %v", ref, err)
	}
	testutil.RunGit(t, repo, "checkout", "-q", "--detach", "v1")
	if ref, err := core.CurrentRef(repo); err != nil || ref != TagRef("v1") {
		t.Fatalf("at v1: CurrentRef = %+v, %v", ref, err)
	}
	head := emptyCommit(t, repo, "detached work")
	if ref, err := core.CurrentRef(repo); err != nil || ref != CommitRef(head) {
		t.Fatalf("past v1: CurrentRef = %+v, %v; want commit %s", ref, err, head)
	}
}

func TestCheckoutRefPicksTheNamedKindWhenABranchAndATagShareAName(t *testing.T) {
	t.Parallel()
	repo, branchTip, tagTip := collisionRepo(t)
	core := NewCore()

	if err := core.CheckoutRef(repo, TagRef("dup")); err != nil {
		t.Fatal(err)
	}
	if branch := core.CurrentBranch(repo); branch != "" || revParse(t, repo, "HEAD") != tagTip {
		t.Fatalf("tag checkout: branch %q at %s, want detached at %s", branch, revParse(t, repo, "HEAD"), tagTip)
	}
	if err := core.CheckoutRef(repo, BranchRef("dup")); err != nil {
		t.Fatal(err)
	}
	if branch := core.CurrentBranch(repo); branch != "dup" || revParse(t, repo, "HEAD") != branchTip {
		t.Fatalf("branch checkout: branch %q at %s, want dup at %s", branch, revParse(t, repo, "HEAD"), branchTip)
	}
}

// A branch only origin has, whose name is also a local tag, becomes a local
// branch tracking origin's; `git checkout <name>` would detach at the tag.
func TestCheckoutRefOfAnOriginOnlyBranchIgnoresASameNamedTag(t *testing.T) {
	t.Parallel()
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	originOnlyBranch(t, bare, "remonly")
	testutil.RunGit(t, repo, "fetch", "-q", "origin")
	testutil.RunGit(t, repo, "tag", "remonly", "main")
	core := NewCore()

	if err := core.Checkout(repo, "remonly"); err != nil {
		t.Fatal(err)
	}
	if branch := core.CurrentBranch(repo); branch != "remonly" {
		t.Fatalf("branch = %q, want remonly", branch)
	}
	if head, want := revParse(t, repo, "HEAD"), revParse(t, repo, "refs/remotes/origin/remonly"); head != want {
		t.Fatalf("HEAD = %s, want origin's remonly %s", head, want)
	}
	if upstream := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "--abbrev-ref", "remonly@{upstream}")); upstream != "origin/remonly" {
		t.Fatalf("upstream = %q, want origin/remonly", upstream)
	}
	if err := core.Checkout(repo, "nowhere"); err == nil || !strings.Contains(err.Error(), `branch "nowhere" not found`) {
		t.Fatalf("Checkout(nowhere) = %v, want not found", err)
	}
}

func TestAttachWorktreeAtATagDetachesAndAllowsSeveral(t *testing.T) {
	t.Parallel()
	repo, branchTip, tagTip := collisionRepo(t)
	core := NewCore()
	for _, name := range []string{"one", "two"} {
		path := filepath.Join(t.TempDir(), name)
		created, err := core.AttachWorktreeAt(repo, path, TagRef("dup"))
		if err != nil || created {
			t.Fatalf("%s: AttachWorktreeAt(tag) = %v, %v", name, created, err)
		}
		if branch := core.CurrentBranch(path); branch != "" || revParse(t, path, "HEAD") != tagTip {
			t.Fatalf("%s: worktree on %q at %s, want detached at %s", name, branch, revParse(t, path, "HEAD"), tagTip)
		}
	}
	path := filepath.Join(t.TempDir(), "branch")
	if _, err := core.AttachWorktreeAt(repo, path, BranchRef("dup")); err != nil {
		t.Fatal(err)
	}
	if branch := core.CurrentBranch(path); branch != "dup" || revParse(t, path, "HEAD") != branchTip {
		t.Fatalf("branch attach: worktree on %q, want dup at %s", branch, branchTip)
	}
}

// A failed tag attach removes its own registration and deletes no branch.
func TestAttachWorktreeAtATagUndoesAFailedCheckout(t *testing.T) {
	t.Parallel()
	repo, _, _ := collisionRepo(t)
	failCheckoutHook(t, repo)
	created, err := NewCore().AttachWorktreeAt(repo, filepath.Join(t.TempDir(), "tag"), TagRef("dup"))
	if err == nil || created || strings.Contains(err.Error(), "undo") {
		t.Fatalf("AttachWorktreeAt = %v, %v; want a clean failure", created, err)
	}
	assertAddUndone(t, repo, "dup", true)
}

func TestCreateWorktreeFromRefCutsTheNamedKind(t *testing.T) {
	t.Parallel()
	repo, branchTip, tagTip := collisionRepo(t)
	core := NewCore()

	fromBranch := filepath.Join(t.TempDir(), "from-branch")
	if _, err := core.CreateWorktreeFromFreshRef(context.Background(), repo, fromBranch, BranchRef("dup"), "cut-branch"); err != nil {
		t.Fatalf("cut from branch dup: %v", err)
	}
	if head := revParse(t, repo, "refs/heads/cut-branch"); head != branchTip {
		t.Fatalf("cut-branch at %s, want branch dup's %s", head, branchTip)
	}

	fromTag := filepath.Join(t.TempDir(), "from-tag")
	if _, err := core.CreateWorktreeFromFreshRef(context.Background(), repo, fromTag, TagRef("dup"), "cut-tag"); err != nil {
		t.Fatalf("cut from tag dup: %v", err)
	}
	if head := revParse(t, repo, "refs/heads/cut-tag"); head != tagTip {
		t.Fatalf("cut-tag at %s, want tag dup's %s", head, tagTip)
	}
	if core.CurrentBranch(fromTag) != "cut-tag" {
		t.Fatalf("tag cut worktree is on %q", core.CurrentBranch(fromTag))
	}

	local := filepath.Join(t.TempDir(), "local-tag")
	if err := core.CreateWorktreeFromRef(repo, local, TagRef("dup"), "cut-local"); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"cut-tag", "cut-local"} {
		if err := testutil.RunGitAllowError(repo, "config", "--get", "branch."+branch+".merge"); err == nil {
			t.Fatalf("%s has an upstream; a tag cut has none", branch)
		}
	}
}

// A tag base is local: nothing is fetched and origin's same-named branch is
// not preferred.
func TestCreateWorktreeFromFreshRefDoesNotFetchForATag(t *testing.T) {
	t.Parallel()
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	testutil.RunGit(t, repo, "tag", "v1")
	tagTip := revParse(t, repo, "v1")
	core := NewCore()
	fetched := false
	core.fetchFn = func(context.Context, string) error { fetched = true; return nil }
	testutil.AdvanceOriginMain(t, bare)

	seed, err := core.CreateWorktreeFromFreshRef(context.Background(), repo, filepath.Join(t.TempDir(), "wt"), TagRef("v1"), "from-v1")
	if err != nil {
		t.Fatal(err)
	}
	if fetched || seed.FromRemote || seed.Ref != "refs/tags/v1" {
		t.Fatalf("seed = %+v, fetched = %v; want a local cut from the tag", seed, fetched)
	}
	if head := revParse(t, repo, "refs/heads/from-v1"); head != tagTip {
		t.Fatalf("from-v1 at %s, want %s", head, tagTip)
	}
}

func TestResolveRefPrefersABranchThenATag(t *testing.T) {
	t.Parallel()
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	originOnlyBranch(t, bare, "remote-only")
	testutil.RunGit(t, repo, "fetch", "-q", "origin")
	testutil.RunGit(t, repo, "branch", "dup")
	testutil.RunGit(t, repo, "tag", "dup")
	testutil.RunGit(t, repo, "tag", "v1")
	core := NewCore()
	for _, tc := range []struct {
		name      string
		localOnly bool
		want      Ref
		found     bool
	}{
		{"dup", false, BranchRef("dup"), true},
		{"refs/tags/dup", false, TagRef("dup"), true},
		{"v1", false, TagRef("v1"), true},
		{"refs/tags/v1", true, TagRef("v1"), true},
		{"remote-only", false, BranchRef("remote-only"), true},
		{"remote-only", true, Ref{}, false},
		{"refs/tags/main", false, Ref{}, false},
		{"refs/heads/dup", false, BranchRef("dup"), true},
		{"refs/heads/v1", false, Ref{}, false},
		{"refs/heads/remote-only", false, BranchRef("remote-only"), true},
		{"missing", false, Ref{}, false},
	} {
		got, found, err := core.ResolveRef(repo, tc.name, tc.localOnly)
		if err != nil || found != tc.found || got != tc.want {
			t.Errorf("ResolveRef(%q, localOnly %v) = %+v, %v, %v; want %+v, %v", tc.name, tc.localOnly, got, found, err, tc.want, tc.found)
		}
	}
	if exists, err := core.TagExists(repo, "v1"); err != nil || !exists {
		t.Fatalf("TagExists(v1) = %v, %v", exists, err)
	}
	if exists, err := core.TagExists(repo, "main"); err != nil || exists {
		t.Fatalf("TagExists(main) = %v, %v", exists, err)
	}
}

func TestOrphanCommitsCountsOnlyWhatNoRefReaches(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	testutil.RunGit(t, repo, "tag", "v1")
	core := NewCore()
	if left, err := core.DetachedOrphans(repo); err != nil || left != nil {
		t.Fatalf("on a branch: DetachedOrphans = %+v, %v", left, err)
	}
	testutil.RunGit(t, repo, "checkout", "-q", "--detach", "v1")
	if left, err := core.DetachedOrphans(repo); err != nil || left != nil {
		t.Fatalf("at a tag: DetachedOrphans = %+v, %v", left, err)
	}
	emptyCommit(t, repo, "one")
	head := emptyCommit(t, repo, "two")
	left, err := core.DetachedOrphans(repo)
	if err != nil || left == nil || left.Count != 2 || left.Commit != head {
		t.Fatalf("two commits past v1: DetachedOrphans = %+v, %v; want 2 at %s", left, err, head)
	}
	if count, err := core.OrphanCommits(repo, head); err != nil || count != 2 {
		t.Fatalf("OrphanCommits = %d, %v", count, err)
	}
}
