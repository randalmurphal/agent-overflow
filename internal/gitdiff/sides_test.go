package gitdiff

import (
	"context"
	"errors"
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

// Past maxListedCommits the selector lists the newest 300: a range may end
// anywhere in them and start at the oldest listed commit (it is not the
// whole PR, so it is read), and a commit past the list is refused.
func TestCommitRangeOfATruncatedList(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	base := headSHA(t, repo)
	fastImportCommits(t, repo, maxListedCommits+2)
	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "feature"))
	listed, err := ListCommitsRange(context.Background(), repo, base, head)
	if err != nil || len(listed) != maxListedCommits {
		t.Fatalf("listed %d commits (err %v), want %d", len(listed), err, maxListedCommits)
	}
	oldestListed := listed[len(listed)-1].SHA
	diff, err := OpenCommitRangeDiff(context.Background(), repo, base, head, oldestListed, head, Options{})
	patch, err := openAndRead(t, diff, err)
	if err != nil || !strings.Contains(patch, "+302") || !strings.Contains(patch, "-2") {
		t.Fatalf("the oldest listed commit through the head: err %v, patch:\n%s", err, patch)
	}
	pastTheList := strings.TrimSpace(gitOutput(t, repo, "rev-parse", oldestListed+"~1"))
	if _, err := rangePatch(t, prHistory{repo: repo, base: base, c4: head}, pastTheList, head); err == nil || !strings.Contains(err.Error(), "is not one of the pull request's commits") {
		t.Fatalf("a commit past the list: %v", err)
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

func TestPinnedSidesRefuseWhatIsNotAFullCommitOrTree(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	head := headSHA(t, repo)
	blob := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD:README.txt"))
	tree := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD^{tree}"))
	if _, err := PinnedSides(context.Background(), repo, Pin{Old: tree, New: head}); err != nil {
		t.Fatalf("a tree and a commit: %v", err)
	}
	for name, pin := range map[string]Pin{
		"abbreviated":        {Old: head[:12]},
		"a blob":             {Old: blob},
		"missing":            {Old: strings.Repeat("e", 40)},
		"a ref name":         {Old: "HEAD"},
		"a missing new":      {Old: head, New: strings.Repeat("e", 40)},
		"a blob new side":    {Old: head, New: blob},
		"an abbreviated new": {Old: head, New: head[:7]},
	} {
		if _, err := PinnedSides(context.Background(), repo, pin); err == nil {
			t.Errorf("%s: %+v was accepted", name, pin)
		}
	}
}

// Each requested path is one literal entry: a directory is its own entry,
// never its contents, a glob matches only its own name, and a path git
// would read differently is refused before git runs.
func TestEntriesReadOneLiteralPathEach(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeBytes(t, repo, "a*b", []byte("glob\n"))
	writeBytes(t, repo, "axb", []byte("x\n"))
	writeBytes(t, repo, "dir/inner/deep.png", []byte("deep\n"))
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "files")
	sides, err := PinnedSides(context.Background(), repo, Pin{Old: headSHA(t, repo), New: headSHA(t, repo)})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sides.OldEntries(context.Background(), []string{"a*b", "dir", "dir/inner/deep.png", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries %+v, want one per path", entries)
	}
	if e := entries["a*b"]; !e.IsFile() || e.Size != 5 {
		t.Errorf("a*b = %+v", e)
	}
	if e := entries["dir"]; !e.Exists || e.IsFile() || e.Mode != "040000" {
		t.Errorf("dir = %+v, want its own tree entry", e)
	}
	if e := entries["dir/inner/deep.png"]; !e.IsFile() || e.Size != 5 {
		t.Errorf("nested = %+v", e)
	}
	if e := entries["missing"]; e.Exists {
		t.Errorf("missing = %+v", e)
	}
	many := make([]string, MaxEntryPaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	for name, paths := range map[string][]string{
		"too many": many, "too long": {strings.Repeat("x", MaxEntryPathBytes+1)},
		"trailing slash": {"dir/"}, "parent": {"../x"}, "absolute": {"/etc/passwd"},
		"dot segment": {"dir/./inner"}, "empty": {""}, "nul": {"a\x00b"},
	} {
		if _, err := sides.OldEntries(context.Background(), paths); err == nil {
			t.Errorf("%s: accepted %q", name, paths[0])
		}
		if _, err := sides.NewEntries(context.Background(), paths); err == nil {
			t.Errorf("%s: new side accepted %q", name, paths[0])
		}
	}
}

// A working-tree side pins each changed regular file by the snapshot's
// blob ID and size; an ignored file the diff leaves out is not pinned.
func TestWorktreeBlobsPinTheSnapshot(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, ".gitignore", "secret.png\n")
	testutil.RunGit(t, repo, "add", ".gitignore")
	testutil.RunGit(t, repo, "commit", "-qm", "ignore")
	writeFile(t, repo, "README.txt", "changed\n")
	writeBytes(t, repo, "new.png", []byte{1, 2, 3})
	writeBytes(t, repo, "secret.png", []byte{4})
	testutil.RunGit(t, repo, "rm", "-q", ".gitignore", "--cached")
	writeFile(t, repo, ".gitignore", "secret.png\n")

	sides, err := WorktreeSides(context.Background(), repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sides.Close() }()
	if pin := sides.Pin(); pin.New != "" || !IsFullOID(pin.Old) {
		t.Fatalf("pin %+v, want the HEAD commit and the working tree", pin)
	}
	blobs, truncated, err := sides.WorktreeBlobs(context.Background(), 10)
	if err != nil || truncated {
		t.Fatalf("WorktreeBlobs: %v %v", err, truncated)
	}
	got := map[string]WorktreeBlob{}
	for _, blob := range blobs {
		got[blob.Path] = blob
	}
	for path, content := range map[string][]byte{"README.txt": []byte("changed\n"), "new.png": {1, 2, 3}} {
		if b := got[path]; b.Blob != BlobID(content, 40) || b.Size != int64(len(content)) {
			t.Errorf("%s pinned %+v, want %s at %d bytes", path, b, BlobID(content, 40), len(content))
		}
	}
	if _, ok := got["secret.png"]; ok {
		t.Error("an ignored file is pinned")
	}
	blobs, truncated, err = sides.WorktreeBlobs(context.Background(), 1)
	if err != nil || !truncated || len(blobs) != 1 {
		t.Fatalf("limit 1: %d blobs, truncated %v, err %v", len(blobs), truncated, err)
	}
}

// A working-tree read serves exactly the pinned blob: same bytes or
// nothing, never a file rewritten since, whatever its size.
func TestReadWorktreeFileProvesThePinnedBlob(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	content := []byte("pinned image bytes")
	writeBytes(t, repo, "img/a.png", content)
	sides, err := PinnedSides(context.Background(), repo, Pin{Old: headSHA(t, repo)})
	if err != nil {
		t.Fatal(err)
	}
	blob := BlobID(content, 40)
	buf := make([]byte, len(content))
	if err := sides.ReadWorktreeFile("img/a.png", blob, buf); err != nil || string(buf) != string(content) {
		t.Fatalf("read %q, err %v", buf, err)
	}
	writeBytes(t, repo, "img/a.png", []byte("PINNED image bytes"))
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("same size, other bytes: %v", err)
	}
	writeBytes(t, repo, "img/a.png", append(content, '!'))
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("grown: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "img/a.png")); err != nil {
		t.Fatal(err)
	}
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestWorktreeEntriesRefuseASymlinkedDirectory(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	outside := t.TempDir()
	writeBytes(t, outside, "x.png", []byte("outside"))
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	sides, err := PinnedSides(context.Background(), repo, Pin{Old: headSHA(t, repo)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sides.NewEntries(context.Background(), []string{"link/x.png"}); err == nil {
		t.Fatal("a path through a symlinked directory was read")
	}
	if err := sides.ReadWorktreeFile("link/x.png", BlobID([]byte("outside"), 40), make([]byte, 7)); err == nil {
		t.Fatal("a file through a symlinked directory was read")
	}
	// A symlink to a directory inside the workspace is refused too: the
	// path git would read is not the one the link names.
	writeBytes(t, repo, "real/y.png", []byte("inside"))
	if err := os.Symlink("real", filepath.Join(repo, "inner")); err != nil {
		t.Fatal(err)
	}
	if _, err := sides.NewEntries(context.Background(), []string{"inner/y.png"}); err == nil {
		t.Fatal("a path through a symlinked directory inside the workspace was read")
	}
	if err := sides.ReadWorktreeFile("inner/y.png", BlobID([]byte("inside"), 40), make([]byte, 6)); err == nil {
		t.Fatal("a file through a symlinked directory inside the workspace was read")
	}
}

// ReadBlob writes exactly the buffer the caller reserved and refuses a
// blob of another size; git's output never grows the buffer.
func TestReadBlobFillsExactlyTheReservedBuffer(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	sides, err := PinnedSides(context.Background(), repo, Pin{Old: headSHA(t, repo), New: headSHA(t, repo)})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sides.OldEntries(context.Background(), []string{"README.txt"})
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["README.txt"]
	buf := make([]byte, entry.Size)
	if err := sides.ReadBlob(context.Background(), entry.OID, buf); err != nil || string(buf) != "hello\n" {
		t.Fatalf("read %q, err %v", buf, err)
	}
	short := make([]byte, entry.Size-1, entry.Size+16)
	if err := sides.ReadBlob(context.Background(), entry.OID, short); err == nil {
		t.Fatal("a short buffer was accepted")
	}
	if len(short) != int(entry.Size-1) {
		t.Fatalf("the buffer grew to %d", len(short))
	}
	if err := sides.ReadBlob(context.Background(), entry.OID, make([]byte, entry.Size+1)); err == nil {
		t.Fatal("a long buffer was accepted")
	}
}

// core.abbrev plays no part: pins and reads use full IDs.
func TestPinsIgnoreCoreAbbrev(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "config", "core.abbrev", "4")
	commitFile(t, repo, "a.txt", "a\n", "a")
	sides, err := CommitSides(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	pin := sides.Pin()
	if !IsFullOID(pin.Old) || !IsFullOID(pin.New) {
		t.Fatalf("pin %+v is not full IDs", pin)
	}
	reopened, err := PinnedSides(context.Background(), repo, pin)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.NewEntries(context.Background(), []string{"a.txt"})
	if err != nil || !IsFullOID(entries["a.txt"].OID) {
		t.Fatalf("entry %+v, err %v", entries["a.txt"], err)
	}
}
