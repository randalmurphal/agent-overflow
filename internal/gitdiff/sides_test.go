package gitdiff

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// commitAt commits what is staged with both dates fixed, so `git log`
// orders the fixture's commits the same way on every run.
func commitAt(t *testing.T, repo, message string, second int) string {
	t.Helper()
	date := fmt.Sprintf("2026-01-01T00:00:%02dZ", second)
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	if _, _, _, err := runGit(context.Background(), repo, env, false, "commit", "--quiet", "--allow-empty", "-m", message); err != nil {
		t.Fatalf("commit %s: %v", message, err)
	}
	return headSHA(t, repo)
}

// prHistory is a pull request's history on feature, off main:
//
//	c1 adds a.txt; s1 (on a side branch off c1) adds side.txt; c2 changes
//	a.txt; m merges the side branch into feature; c4 adds b.txt.
//
// ListCommitsRange(main, c4) lists it newest first: c4, m, c2, s1, c1.
type prHistory struct {
	repo, base            string
	c1, s1, c2, merge, c4 string
}

func newPRHistory(t *testing.T) prHistory {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	h := prHistory{repo: repo, base: headSHA(t, repo)}
	testutil.RunGit(t, repo, "checkout", "--quiet", "-b", "feature")
	writeFile(t, repo, "a.txt", "a1\n")
	testutil.RunGit(t, repo, "add", "a.txt")
	h.c1 = commitAt(t, repo, "c1", 1)
	testutil.RunGit(t, repo, "checkout", "--quiet", "-b", "side")
	writeFile(t, repo, "side.txt", "side\n")
	testutil.RunGit(t, repo, "add", "side.txt")
	h.s1 = commitAt(t, repo, "s1", 2)
	testutil.RunGit(t, repo, "checkout", "--quiet", "feature")
	writeFile(t, repo, "a.txt", "a2\n")
	testutil.RunGit(t, repo, "add", "a.txt")
	h.c2 = commitAt(t, repo, "c2", 3)
	date := "2026-01-01T00:00:04Z"
	if _, _, _, err := runGit(context.Background(), repo, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, false,
		"merge", "--quiet", "--no-ff", "-m", "merge side", "side"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	h.merge = headSHA(t, repo)
	writeFile(t, repo, "b.txt", "b\n")
	testutil.RunGit(t, repo, "add", "b.txt")
	h.c4 = commitAt(t, repo, "c4", 5)

	commits, err := commitsOf(ListCommitsRange(context.Background(), repo, h.base, h.c4))
	if err != nil {
		t.Fatalf("ListCommitsRange: %v", err)
	}
	var order []string
	for _, c := range commits {
		order = append(order, c.SHA)
	}
	if want := []string{h.c4, h.merge, h.c2, h.s1, h.c1}; strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("fixture lists %v, want c4 m c2 s1 c1 %v", order, want)
	}
	return h
}

func rangePatch(t *testing.T, h prHistory, first, last string) (string, error) {
	t.Helper()
	diff, err := OpenCommitRangeDiff(context.Background(), h.repo, h.base, h.c4, first, last, Options{})
	return openAndRead(t, diff, err)
}

// twoTreePatch is `git diff old new` with the package's patch flags.
func twoTreePatch(t *testing.T, repo, old, new string) string {
	t.Helper()
	return gitOutput(t, repo, Options{}.gitArgs("diff", old, new, "--")...)
}

// A range is the two-tree diff from its first commit's parent to its last
// commit, whatever lies between: a merge inside it, the PR's first commit
// (whose parent is the base) or its last.
func TestCommitRangeDiffIsTheTwoTreeDiffOfItsEnds(t *testing.T) {
	t.Parallel()
	h := newPRHistory(t)
	for _, tc := range []struct {
		name        string
		first, last string
		old         string
		contains    []string
		excludes    []string
	}{
		{"a merge inside", h.c2, h.c4, h.c1, []string{"+a2", "+side", "+b"}, []string{"+a1"}},
		{"from the first commit", h.c1, h.merge, h.base, []string{"+a2", "+side"}, []string{"+b"}},
		{"to the last commit", h.merge, h.c4, h.c2, []string{"+side", "+b"}, []string{"a2"}},
		{"a merge commit alone", h.merge, h.merge, h.c2, []string{"+side"}, []string{"a2", "+b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			patch, err := rangePatch(t, h, tc.first, tc.last)
			if err != nil {
				t.Fatalf("OpenCommitRangeDiff: %v", err)
			}
			if want := twoTreePatch(t, h.repo, tc.old, tc.last); patch != want {
				t.Fatalf("patch:\n%s\nwant git diff of the ends:\n%s", patch, want)
			}
			for _, text := range tc.contains {
				if !strings.Contains(patch, text) {
					t.Errorf("patch lacks %q:\n%s", text, patch)
				}
			}
			for _, text := range tc.excludes {
				if strings.Contains(patch, text) {
					t.Errorf("patch has %q:\n%s", text, patch)
				}
			}
		})
	}
}

// A range of one commit is that commit's own diff, the single-commit read.
func TestCommitRangeOfOneCommitIsTheCommitDiff(t *testing.T) {
	t.Parallel()
	h := newPRHistory(t)
	for _, sha := range []string{h.c1, h.s1, h.merge, h.c4} {
		ranged, err := rangePatch(t, h, sha, sha)
		if err != nil {
			t.Fatalf("range %s: %v", sha, err)
		}
		single, err := commitPatch(t, h.repo, sha, Options{})
		if err != nil {
			t.Fatalf("commit %s: %v", sha, err)
		}
		if ranged != single || ranged == "" {
			t.Fatalf("range %s..%s:\n%s\nsingle-commit diff:\n%s", sha, sha, ranged, single)
		}
	}
}

func TestCommitRangeRefusesEndsOutsideTheListOrOutOfOrder(t *testing.T) {
	t.Parallel()
	h := newPRHistory(t)
	for _, tc := range []struct {
		name, first, last, want string
	}{
		{"first not in the list", h.base, h.c2, "is not one of the pull request's commits"},
		{"last not in the list", h.c1, h.base, "is not one of the pull request's commits"},
		{"out of order", h.c4, h.c1, "out of order"},
		{"the whole list", h.c1, h.c4, "covers every commit"},
		{"not a sha", "HEAD", h.c4, "invalid first commit"},
		{"a flag", h.c1, "--all", "invalid last commit"},
	} {
		if _, err := rangePatch(t, h, tc.first, tc.last); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The single commit of a one-commit pull request is its own view, not a
// range covering the whole list.
func TestCommitRangeAllowsTheOnlyCommit(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	base := headSHA(t, repo)
	commitFile(t, repo, "only.txt", "only\n", "only")
	only := headSHA(t, repo)
	diff, err := OpenCommitRangeDiff(context.Background(), repo, base, only, only, only, Options{})
	patch, err := openAndRead(t, diff, err)
	if err != nil || !strings.Contains(patch, "+only") {
		t.Fatalf("patch = %q, err = %v", patch, err)
	}
}

// A range holding a merge of the base branch shows what that merge brought
// in, as the merge's own single-commit view does: the range is a two-tree
// diff, not a diff against the base.
func TestCommitRangeOverAMergeOfTheBaseShowsTheMergedBaseChanges(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	base := headSHA(t, repo)
	testutil.RunGit(t, repo, "checkout", "--quiet", "-b", "feature")
	writeFile(t, repo, "feature.txt", "feature\n")
	testutil.RunGit(t, repo, "add", "feature.txt")
	first := commitAt(t, repo, "feature work", 1)
	testutil.RunGit(t, repo, "checkout", "--quiet", "main")
	writeFile(t, repo, "upstream.txt", "upstream\n")
	testutil.RunGit(t, repo, "add", "upstream.txt")
	commitAt(t, repo, "upstream work", 2)
	testutil.RunGit(t, repo, "checkout", "--quiet", "feature")
	date := "2026-01-01T00:00:03Z"
	if _, _, _, err := runGit(context.Background(), repo, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, false,
		"merge", "--quiet", "--no-ff", "-m", "merge main", "main"); err != nil {
		t.Fatal(err)
	}
	merge := headSHA(t, repo)
	writeFile(t, repo, "after.txt", "after\n")
	testutil.RunGit(t, repo, "add", "after.txt")
	head := commitAt(t, repo, "after", 4)

	diff, err := OpenCommitRangeDiff(context.Background(), repo, base, head, merge, head, Options{})
	patch, err := openAndRead(t, diff, err)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "+upstream") || !strings.Contains(patch, "+after") || strings.Contains(patch, "+feature") {
		t.Fatalf("merge..head:\n%s\nwant the merged base change and the later commit only", patch)
	}
	_ = first
}

// fastImportCommits makes count commits on feature off main in one
// fast-import, each changing n.txt, dated one second apart.
func fastImportCommits(t *testing.T, repo string, count int) {
	t.Helper()
	var stream strings.Builder
	fmt.Fprintf(&stream, "reset refs/heads/feature\nfrom refs/heads/main\n")
	for i := 1; i <= count; i++ {
		content := fmt.Sprintf("%d\n", i)
		fmt.Fprintf(&stream, "commit refs/heads/feature\ncommitter A <a@example.com> %d +0000\ndata %d\nc%d\nM 100644 inline n.txt\ndata %d\n%s\n",
			1_700_000_000+i, len(fmt.Sprintf("c%d", i)), i, len(content), content)
	}
	if _, _, _, err := runGitWithStdin(context.Background(), repo, nil, []byte(stream.String()), false, "fast-import", "--quiet"); err != nil {
		t.Fatalf("fast-import: %v", err)
	}
}

// The list and its count read one range: a branch reset between them
// (HEAD moved back to the base) leaves the total the listed range's.
func TestCommitListAndCountReadOneRange(t *testing.T) {
	t.Parallel()
	const limit = 3
	repo := testutil.InitGitRepo(t)
	base := headSHA(t, repo)
	fastImportCommits(t, repo, limit+2)
	gitOutput(t, repo, "checkout", "--quiet", "feature")
	list, err := listCommitRange(context.Background(), repo, "main", "HEAD", limit, time.Minute, func() {
		gitOutput(t, repo, "reset", "--quiet", "--hard", base)
	})
	if err != nil || len(list.Commits) != limit || list.Total == nil || *list.Total != limit+2 {
		t.Fatalf("listed %d commits of %v (err %v), want %d of %d", len(list.Commits), list.Total, err, limit, limit+2)
	}
}

// commitsOf answers a list's commits.
func commitsOf(list CommitList, err error) ([]Commit, error) { return list.Commits, err }

// Past the cap the selector lists the newest commits and the range's real
// total: a range may end anywhere in them and start at the oldest listed
// commit (it is not the whole PR, so that run is read), and a commit past
// the list is refused. A count that cannot be had is no total, never the
// listed number.
func TestCommitRangeOfATruncatedList(t *testing.T) {
	t.Parallel()
	const limit = 3
	repo := testutil.InitGitRepo(t)
	base := headSHA(t, repo)
	fastImportCommits(t, repo, limit+2)
	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "feature"))
	list, err := listCommitRange(context.Background(), repo, base, head, limit, time.Minute, nil)
	if err != nil || len(list.Commits) != limit || list.Total == nil || *list.Total != limit+2 {
		t.Fatalf("listed %d commits of %v (err %v), want %d of %d", len(list.Commits), list.Total, err, limit, limit+2)
	}
	whole, err := listCommitRange(context.Background(), repo, base, head, limit+2, time.Minute, nil)
	if err != nil || len(whole.Commits) != limit+2 || whole.Total == nil || *whole.Total != limit+2 {
		t.Fatalf("a list at the cap = %d commits of %v (err %v), want it complete", len(whole.Commits), whole.Total, err)
	}
	unknown, err := listCommitRange(context.Background(), repo, base, head, limit, time.Nanosecond, nil)
	if err != nil || len(unknown.Commits) != limit || unknown.Total != nil {
		t.Fatalf("a count past its bound = %d commits of %v (err %v), want the list with no total", len(unknown.Commits), unknown.Total, err)
	}
	oldestListed := list.Commits[limit-1].SHA
	sides, err := commitRangeSides(context.Background(), repo, base, head, oldestListed, head, limit)
	if err != nil {
		t.Fatalf("the oldest listed commit through the head: %v", err)
	}
	patch, err := openAndRead(t, sides.OpenDiff(Options{}), nil)
	if err != nil || !strings.Contains(patch, "+5") || !strings.Contains(patch, "-2") {
		t.Fatalf("the oldest listed commit through the head: err %v, patch:\n%s", err, patch)
	}
	if pin := sides.Pin(); pin.Old != strings.TrimSpace(gitOutput(t, repo, "rev-parse", oldestListed+"~1")) || pin.New != head {
		t.Fatalf("the run is %s..%s, not the oldest listed commit's parent to the head", pin.Old, pin.New)
	}
	pastTheList := strings.TrimSpace(gitOutput(t, repo, "rev-parse", oldestListed+"~1"))
	if _, err := commitRangeSides(context.Background(), repo, base, head, pastTheList, head, limit); err == nil || !strings.Contains(err.Error(), "is not one of the pull request's commits") {
		t.Fatalf("a commit past the list: %v", err)
	}
	if _, err := commitRangeSides(context.Background(), repo, base, head, oldestListed, head, limit+2); err != nil {
		t.Fatalf("a run inside a complete list: %v", err)
	}
	firstOfAll := whole.Commits[limit+1].SHA
	if _, err := commitRangeSides(context.Background(), repo, base, head, firstOfAll, head, limit+2); err == nil || !strings.Contains(err.Error(), "covers every commit") {
		t.Fatalf("the whole complete list as a run: %v", err)
	}
}

func writeBytes(t *testing.T, repo, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
