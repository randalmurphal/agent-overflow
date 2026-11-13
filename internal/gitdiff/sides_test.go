package gitdiff

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	commits, err := ListCommitsRange(context.Background(), repo, h.base, h.c4)
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

// pngHeader is enough of a PNG for git to call the file binary.
func pngBytes(seed byte) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), bytes.Repeat([]byte{seed, 0}, 64)...)
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

func readSide(t *testing.T, sides *Sides, blob Blob) []byte {
	t.Helper()
	data, err := sides.ReadBlob(context.Background(), blob, 1<<20)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	return data
}

// Each side of an added, deleted, changed and renamed image is the blob at
// that side, with its size; a rename is its old path deleted and its new
// path added.
func TestChangedPathsReadsBothSidesOfEachImage(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeBytes(t, repo, "deleted.png", pngBytes(1))
	writeBytes(t, repo, "changed.png", pngBytes(2))
	writeBytes(t, repo, "old/name.png", pngBytes(3))
	writeBytes(t, repo, "same.png", pngBytes(4))
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "images")
	testutil.RunGit(t, repo, "rm", "-q", "deleted.png")
	writeBytes(t, repo, "added.png", pngBytes(5))
	writeBytes(t, repo, "changed.png", pngBytes(6))
	testutil.RunGit(t, repo, "mv", "old/name.png", "new name.png")
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "change images")

	sides, err := CommitSides(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatalf("CommitSides: %v", err)
	}
	changed, err := sides.ChangedPaths(context.Background(), []string{"added.png", "deleted.png", "changed.png", "old/name.png", "new name.png", "same.png", "missing.png"})
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	expect := func(path string, old, current []byte) {
		t.Helper()
		entry, ok := changed[path]
		if !ok {
			t.Fatalf("%s is not reported", path)
		}
		for _, side := range []struct {
			name string
			blob Blob
			want []byte
		}{{"old", entry.Old, old}, {"new", entry.New, current}} {
			if side.blob.Exists != (side.want != nil) {
				t.Fatalf("%s %s side exists = %v", path, side.name, side.blob.Exists)
			}
			if side.want == nil {
				continue
			}
			if side.blob.Size != int64(len(side.want)) || !side.blob.IsFile() {
				t.Fatalf("%s %s side = %+v, want a %d byte file", path, side.name, side.blob, len(side.want))
			}
			if got := readSide(t, sides, side.blob); !bytes.Equal(got, side.want) {
				t.Fatalf("%s %s side read %q", path, side.name, got)
			}
		}
	}
	expect("added.png", nil, pngBytes(5))
	expect("deleted.png", pngBytes(1), nil)
	expect("changed.png", pngBytes(2), pngBytes(6))
	expect("old/name.png", pngBytes(3), nil)
	expect("new name.png", nil, pngBytes(3))
	for _, unchanged := range []string{"same.png", "missing.png"} {
		if _, ok := changed[unchanged]; ok {
			t.Errorf("%s is reported, but the diff does not change it", unchanged)
		}
	}
}

// A worktree side reads the snapshot, untracked files included, and never
// an ignored file the diff leaves out.
func TestWorktreeSidesReadTheSnapshot(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, ".gitignore", "secret.png\n")
	testutil.RunGit(t, repo, "add", ".gitignore")
	testutil.RunGit(t, repo, "commit", "-qm", "ignore")
	writeBytes(t, repo, "untracked.png", pngBytes(7))
	writeBytes(t, repo, "secret.png", pngBytes(8))

	sides, err := WorktreeSides(context.Background(), repo, t.TempDir())
	if err != nil {
		t.Fatalf("WorktreeSides: %v", err)
	}
	defer func() {
		if err := sides.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	changed, err := sides.ChangedPaths(context.Background(), []string{"untracked.png", "secret.png"})
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if _, ok := changed["secret.png"]; ok {
		t.Fatal("an ignored file is reported")
	}
	entry := changed["untracked.png"]
	if entry.Old.Exists || !entry.New.Exists {
		t.Fatalf("untracked.png = %+v, want an added file", entry)
	}
	if got := readSide(t, sides, entry.New); !bytes.Equal(got, pngBytes(7)) {
		t.Fatalf("read %q", got)
	}
}

func TestReadBlobRefusesOverTheCapBeforeReading(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeBytes(t, repo, "big.png", pngBytes(9))
	testutil.RunGit(t, repo, "add", "big.png")
	testutil.RunGit(t, repo, "commit", "-qm", "big")
	sides, err := CommitSides(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := sides.ChangedPaths(context.Background(), []string{"big.png"})
	if err != nil {
		t.Fatal(err)
	}
	blob := changed["big.png"].New
	if _, err := sides.ReadBlob(context.Background(), blob, blob.Size-1); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("ReadBlob under the size = %v, want the cap refusal", err)
	}
	// A size the caller misstates is caught by the read, not trusted.
	lied := blob
	lied.Size = 3
	if _, err := sides.ReadBlob(context.Background(), lied, 1<<20); err == nil {
		t.Fatal("ReadBlob served a blob whose size disagrees with its bytes")
	}
}

func TestChangedPathsBoundsItsArguments(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	sides, err := CommitSides(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxChangedPaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	for name, paths := range map[string][]string{
		"too many": many,
		"too long": {strings.Repeat("x", MaxChangedPathBytes+1)},
		"empty":    {""},
		"nul":      {"a\x00b"},
	} {
		if _, err := sides.ChangedPaths(context.Background(), paths); err == nil {
			t.Errorf("%s: ChangedPaths accepted it", name)
		}
	}
	// A pattern is a literal path, never a glob over the tree.
	changed, err := sides.ChangedPaths(context.Background(), []string{"*.txt"})
	if err != nil || len(changed) != 0 {
		t.Fatalf("a glob matched %v (err %v)", changed, err)
	}
}
