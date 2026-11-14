package gitdiff

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

// withoutVersions clears each file's Version, which names a blob, after
// checking it is set.
func withoutVersions(t *testing.T, files []ChangedFile) []ChangedFile {
	t.Helper()
	for i := range files {
		if len(files[i].Version) < 40 {
			t.Fatalf("%s: Version = %q", files[i].Path, files[i].Version)
		}
		files[i].Version = ""
	}
	return files
}

// worktreeChanges lists the repo's changes with their Versions cleared.
func worktreeChanges(t *testing.T, repo string) []ChangedFile {
	t.Helper()
	root := t.TempDir()
	files, err := WorktreeChanges(context.Background(), repo, root)
	if err != nil {
		t.Fatalf("WorktreeChanges: %v", err)
	}
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("snapshot directory not released: %v %v", left, err)
	}
	return withoutVersions(t, files)
}

func TestWorktreeChangesVersionFollowsContentNotLineCounts(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	list := func() ChangedFile {
		t.Helper()
		files, err := WorktreeChanges(context.Background(), repo, t.TempDir())
		if err != nil || len(files) != 1 {
			t.Fatalf("WorktreeChanges = %+v, %v", files, err)
		}
		return files[0]
	}
	writeFile(t, repo, "README.txt", "hello\nfirst\n")
	first := list()
	if again := list(); again != first {
		t.Fatalf("an unchanged file listed differently: %+v then %+v", first, again)
	}
	writeFile(t, repo, "README.txt", "hello\nother\n")
	second := list()
	if second.Additions != first.Additions || second.Deletions != first.Deletions {
		t.Fatalf("the edit was meant to keep the line counts: %+v then %+v", first, second)
	}
	if second.Version == first.Version {
		t.Fatalf("Version %q did not follow the edit", first.Version)
	}
}

func TestWorktreeChangesListsEveryKindOfChange(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "gone [x].txt", "bye\n", "add gone")
	commitFile(t, repo, "old.txt", "one\ntwo\nthree\nfour\n", "add old")
	commitFile(t, repo, "bin.dat", "a\x00b", "add bin")
	indexBefore := gitOutput(t, repo, "ls-files", "--stage")

	writeFile(t, repo, "README.txt", "hello\nedited\n")
	writeFile(t, repo, "new *.txt", "fresh\nlines\n")
	writeFile(t, repo, "bin.dat", "a\x00c")
	if err := os.Remove(filepath.Join(repo, "gone [x].txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "old.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}

	got := worktreeChanges(t, repo)
	want := []ChangedFile{
		{Path: "README.txt", Status: "M", Additions: 1},
		{Path: "bin.dat", Status: "M", Binary: true},
		{Path: "gone [x].txt", Status: "D", Deletions: 1},
		{Path: "new *.txt", Status: "A", Additions: 2},
		{Path: "renamed.txt", OldPath: "old.txt", Status: "R"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}
	if after := gitOutput(t, repo, "ls-files", "--stage"); after != indexBefore {
		t.Fatalf("the user's index changed:\n%s\nwas:\n%s", after, indexBefore)
	}
}

func TestWorktreeChangesMarksPartlyStagedFiles(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "README.txt", "hello\nstaged\n")
	testutil.RunGit(t, repo, "add", "README.txt")
	writeFile(t, repo, "README.txt", "hello\nstaged\nand more\n")
	writeFile(t, repo, "whole.txt", "staged whole\n")
	testutil.RunGit(t, repo, "add", "whole.txt")
	writeFile(t, repo, "loose.txt", "untracked\n")

	got := worktreeChanges(t, repo)
	want := []ChangedFile{
		{Path: "README.txt", Status: "M", Additions: 2, PartlyStaged: true},
		{Path: "loose.txt", Status: "A", Additions: 1},
		{Path: "whole.txt", Status: "A", Additions: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}
}

func TestWorktreeChangesCleanAndFreshRepos(t *testing.T) {
	t.Parallel()
	if got := worktreeChanges(t, testutil.InitGitRepo(t)); len(got) != 0 {
		t.Fatalf("clean repo listed %+v", got)
	}
	fresh := t.TempDir()
	testutil.RunGit(t, fresh, "init", "-b", "main")
	writeFile(t, fresh, "first.txt", "no commits yet\n")
	want := []ChangedFile{{Path: "first.txt", Status: "A", Additions: 1}}
	if got := worktreeChanges(t, fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh repo: got %+v want %+v", got, want)
	}
}

func TestParseRawNumstatRejectsMalformedOutput(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"raw without path":    ":100644 100644 a b M\x00",
		"missing counts":      ":100644 100644 a b M\x00f\x00",
		"counts for another":  ":100644 100644 a b M\x00f\x001\t0\tg\x00",
		"extra counts":        ":100644 100644 a b M\x00f\x001\t0\tf\x001\t0\tg\x00",
		"non-numeric counts":  ":100644 100644 a b M\x00f\x00x\t0\tf\x00",
		"rename without path": ":100644 100644 a b R100\x00f\x00",
	} {
		if _, err := parseRawNumstat(out); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestOpenWorktreeFilesDiffReadsOnlyTheNamedPaths(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, "old.txt", "one\ntwo\nthree\nfour\n", "add old")
	writeFile(t, repo, "README.txt", "hello\nedited\n")
	writeFile(t, repo, "a*.txt", "starred\n")
	writeFile(t, repo, "ab.txt", "must not match the star\n")
	if err := os.Rename(filepath.Join(repo, "old.txt"), filepath.Join(repo, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	read := func(paths ...string) string {
		t.Helper()
		diff, err := OpenWorktreeFilesDiff(context.Background(), repo, t.TempDir(), paths, Options{})
		patch, err := openAndRead(t, diff, err)
		if err != nil {
			t.Fatalf("read %v: %v", paths, err)
		}
		return patch
	}

	starred := read("a*.txt")
	if !strings.Contains(starred, "+starred") || strings.Contains(starred, "must not match") || strings.Contains(starred, "README") {
		t.Fatalf("a literal path matched as a pattern:\n%s", starred)
	}
	rename := read("old.txt", "renamed.txt")
	if !strings.Contains(rename, "rename from old.txt") || !strings.Contains(rename, "rename to renamed.txt") {
		t.Fatalf("expected one rename, got:\n%s", rename)
	}
	if _, err := OpenWorktreeFilesDiff(context.Background(), repo, t.TempDir(), nil, Options{}); err == nil {
		t.Fatal("opened a diff of no paths")
	}
	if _, err := OpenWorktreeFilesDiff(context.Background(), repo, t.TempDir(), []string{""}, Options{}); err == nil {
		t.Fatal("opened a diff of an empty path")
	}
}

// One snapshot answers the list and the patches, and what changes in the
// workspace afterwards is in neither.
func TestWorktreeSnapshotListAndPatchesDescribeOneState(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	writeFile(t, repo, "README.txt", "hello\nedited\n")
	writeFile(t, repo, "new.txt", "fresh\n")
	root := t.TempDir()
	ctx := context.Background()

	snapshot, err := CaptureWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("CaptureWorktree: %v", err)
	}
	writeFile(t, repo, "late.txt", "after the snapshot\n")
	writeFile(t, repo, "new.txt", "fresh\nand more\n")

	files, err := snapshot.Changes(ctx)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	files = withoutVersions(t, files)
	want := []ChangedFile{
		{Path: "README.txt", Status: "M", Additions: 1},
		{Path: "new.txt", Status: "A", Additions: 1},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", files, want)
	}
	whole, err := openAndRead(t, snapshot.Diff(Options{}), nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(whole, "+edited") || !strings.Contains(whole, "+fresh") ||
		strings.Contains(whole, "late.txt") || strings.Contains(whole, "and more") {
		t.Fatalf("whole patch is not the snapshot's:\n%s", whole)
	}
	one, err := snapshot.FilesDiff([]string{"new.txt"}, Options{})
	onePatch, err := openAndRead(t, one, err)
	if err != nil {
		t.Fatalf("FilesDiff: %v", err)
	}
	if !strings.Contains(onePatch, "+fresh") || strings.Contains(onePatch, "README") {
		t.Fatalf("file patch = %q", onePatch)
	}
	if _, err := snapshot.FilesDiff(nil, Options{}); err == nil {
		t.Fatal("opened a diff of no paths")
	}

	// Closing a diff leaves the snapshot for the next read; closing the
	// snapshot removes it.
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Fatalf("snapshot directory entries = %d, want 1", len(left))
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("snapshot directory not released: %v", left)
	}
}

// A submodule whose commit moved and an untracked nested repository list as
// one gitlink change each; a nested repository with no commit, which git
// cannot record, is left out. Before, any of them failed the whole listing.
func TestWorktreeChangesListsRepositoriesAsGitlinks(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	inner := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", inner, "sm")
	testutil.RunGit(t, repo, "commit", "-m", "add submodule")
	sm := filepath.Join(repo, "sm")
	testutil.RunGit(t, sm, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, sm, "config", "user.email", "agent-overflow@example.com")
	commitFile(t, sm, "README.txt", "moved\n", "move the submodule")
	nested := filepath.Join(repo, "nested")
	testutil.RunGit(t, repo, "init", "--quiet", "-b", "main", nested)
	testutil.RunGit(t, nested, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, nested, "config", "user.email", "agent-overflow@example.com")
	commitFile(t, nested, "n.txt", "n\n", "nested commit")
	testutil.RunGit(t, repo, "init", "--quiet", "-b", "main", filepath.Join(repo, "empty"))
	writeFile(t, repo, "README.txt", "hello\nedited\n")

	got := worktreeChanges(t, repo)
	want := []ChangedFile{
		{Path: "README.txt", Status: "M", Additions: 1},
		{Path: "nested", Status: "A", Additions: 1},
		{Path: "sm", Status: "M", Additions: 1, Deletions: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes:\n got %+v\nwant %+v", got, want)
	}
	patch, err := worktreePatch(t, repo, Options{})
	if err != nil {
		t.Fatalf("worktreePatch: %v", err)
	}
	if strings.Count(patch, "+Subproject commit ") != 2 {
		t.Fatalf("patch does not name the two repositories' commits:\n%s", patch)
	}
}
