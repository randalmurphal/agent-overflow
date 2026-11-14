package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/textgen"
)

func writeWorkspaceFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func gitText(t *testing.T, workspace string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// pick is the selection of the named rows of a listing.
func pick(t *testing.T, listed CommitFiles, paths ...string) CommitSelection {
	t.Helper()
	var selection CommitSelection
	for _, p := range paths {
		i := slices.IndexFunc(listed.Files, func(f CommitFile) bool { return f.Path == p })
		if i < 0 {
			t.Fatalf("%s is not listed in %v", p, commitFilePaths(listed.Files))
		}
		selection.Files = append(selection.Files, listed.Files[i])
	}
	return selection
}

func commitFilePaths(files []CommitFile) []string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths
}

// The dialog's loop: list, commit a selection, list what is left, commit
// that. Each commit holds exactly its selection and the list empties.
func TestCommitFiles_SelectionsCommitUntilNothingIsLeft(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	ctx := context.Background()
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nedited\n")
	writeWorkspaceFile(t, workspace, "one.txt", "one\n")
	writeWorkspaceFile(t, workspace, "two.txt", "two\n")

	listed, err := app.ListCommitFiles(ctx, ref)
	if err != nil {
		t.Fatalf("ListCommitFiles: %v", err)
	}
	if got := commitFilePaths(listed.Files); !reflect.DeepEqual(got, []string{"README.txt", "one.txt", "two.txt"}) {
		t.Fatalf("listed %v", got)
	}
	if listed.Total != 3 || listed.WholeOnly {
		t.Fatalf("listed = %+v", listed)
	}

	first, err := app.GitCommit(ref, "first pick", "", pick(t, listed, "README.txt", "two.txt"))
	if err != nil {
		t.Fatalf("GitCommit selection: %v", err)
	}
	if got := gitText(t, workspace, "show", "--format=%H", "--name-only", "HEAD"); got != first.Commit+"\n\nREADME.txt\ntwo.txt\n" {
		t.Fatalf("first commit = %q", got)
	}

	left, err := app.ListCommitFiles(ctx, ref)
	if err != nil {
		t.Fatalf("ListCommitFiles after a commit: %v", err)
	}
	if got := commitFilePaths(left.Files); !reflect.DeepEqual(got, []string{"one.txt"}) {
		t.Fatalf("left %v", got)
	}
	if _, err := app.GitCommit(ref, "second pick", "", pick(t, left, "one.txt")); err != nil {
		t.Fatalf("GitCommit rest: %v", err)
	}
	done, err := app.ListCommitFiles(ctx, ref)
	if err != nil {
		t.Fatalf("ListCommitFiles when clean: %v", err)
	}
	if done.Files == nil || len(done.Files) != 0 || done.Total != 0 {
		t.Fatalf("clean list = %+v", done)
	}
	if got := gitText(t, workspace, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q", got)
	}
}

func TestCommitFiles_WholeOnlyDuringAMergeAndCommitWithoutPathsCompletesIt(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	testutil.RunGit(t, workspace, "checkout", "-b", "side")
	writeWorkspaceFile(t, workspace, "README.txt", "side\n")
	testutil.RunGit(t, workspace, "commit", "-am", "side change")
	testutil.RunGit(t, workspace, "checkout", "main")
	writeWorkspaceFile(t, workspace, "README.txt", "main\n")
	testutil.RunGit(t, workspace, "commit", "-am", "main change")
	if err := testutil.RunGitAllowError(workspace, "merge", "side"); err == nil {
		t.Fatal("merge did not stop on the conflict")
	}
	writeWorkspaceFile(t, workspace, "README.txt", "resolved\n")
	testutil.RunGit(t, workspace, "add", "README.txt")
	writeWorkspaceFile(t, workspace, "extra.txt", "extra\n")

	listed, err := app.ListCommitFiles(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListCommitFiles: %v", err)
	}
	if !listed.WholeOnly {
		t.Fatalf("a merge in progress was not reported whole-only: %+v", listed)
	}
	if _, err := app.GitCommit(ref, "partial", "", pick(t, listed, "README.txt")); err == nil || !strings.Contains(err.Error(), "merge is in progress") {
		t.Fatal("a selection committed during a merge")
	}
	if _, err := app.GitCommit(ref, "finish merge", "", CommitSelection{All: true}); err != nil {
		t.Fatalf("GitCommit of everything: %v", err)
	}
	if parents := strings.Fields(gitText(t, workspace, "rev-list", "--parents", "-n", "1", "HEAD")); len(parents) != 3 {
		t.Fatalf("HEAD parents = %v, want a merge commit", parents)
	}
	if got := gitText(t, workspace, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q", got)
	}
}

func TestCommitFiles_ListIsBoundedAndNonGitWorkspaceIsEmpty(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	for i := range maxCommitFiles + 3 {
		writeWorkspaceFile(t, workspace, fmt.Sprintf("f%05d.txt", i), "x\n")
	}
	listed, err := app.ListCommitFiles(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListCommitFiles: %v", err)
	}
	if len(listed.Files) != maxCommitFiles || listed.Total != maxCommitFiles+3 {
		t.Fatalf("listed %d of total %d", len(listed.Files), listed.Total)
	}
	if listed.Files[0].Path != "f00000.txt" {
		t.Fatalf("first listed = %q", listed.Files[0].Path)
	}
}

func TestOpenCommitFileDiff_ReadsOneFile(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nedited\n")
	writeWorkspaceFile(t, workspace, "other.txt", "other\n")

	opened, err := app.OpenCommitFileDiff(context.Background(), ref, []string{"other.txt"})
	if err != nil {
		t.Fatalf("OpenCommitFileDiff: %v", err)
	}
	if opened.ID != "" || !opened.Chunk.EOF {
		t.Fatalf("a small diff kept a handle: %+v", opened)
	}
	if !strings.Contains(opened.Chunk.Data, "+other") || strings.Contains(opened.Chunk.Data, "README") {
		t.Fatalf("patch = %q", opened.Chunk.Data)
	}
	if _, err := app.OpenCommitFileDiff(context.Background(), ref, nil); err == nil {
		t.Fatal("opened a diff of no paths")
	}
}

// Generate describes the selection only, and reads it without touching
// the workspace's index.
func TestGenerateCommitMessage_DescribesOnlyTheSelectionAndLeavesTheIndex(t *testing.T) {
	t.Parallel()
	app := newCommitMsgTestApp(t)
	workspace := initCommitMsgRepo(t)
	writeWorkspaceFile(t, workspace, "README", "hello\nworld\n")
	writeWorkspaceFile(t, workspace, "picked.txt", "picked line\n")
	writeWorkspaceFile(t, workspace, "skipped.txt", "skipped line\n")
	statusBefore := gitText(t, workspace, "status", "--porcelain")

	var prompts []string
	app.textGenerationExecutor = func(_ context.Context, spec textgen.CLISpec) (textgen.CLIResult, error) {
		prompts = append(prompts, spec.Stdin)
		if err := os.WriteFile(extractCodexOutputPath(spec.Args), []byte(`{"subject":"ok","body":""}`), 0o600); err != nil {
			return textgen.CLIResult{}, err
		}
		return textgen.CLIResult{ExitCode: 0}, nil
	}
	ref := testWorkspaceRef(t, app, workspace)

	if _, err := app.GenerateCommitMessage(ref, CommitSelection{Files: []CommitFile{{Path: "picked.txt"}}}); err != nil {
		t.Fatalf("GenerateCommitMessage selection: %v", err)
	}
	if _, err := app.GenerateCommitMessage(ref, CommitSelection{All: true}); err != nil {
		t.Fatalf("GenerateCommitMessage everything: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("executor ran %d times", len(prompts))
	}
	selection, everything := prompts[0], prompts[1]
	if !strings.Contains(selection, "A\tpicked.txt") || !strings.Contains(selection, "+picked line") {
		t.Fatalf("selection prompt misses the picked file:\n%s", selection)
	}
	for _, absent := range []string{"skipped", "README", "+world"} {
		if strings.Contains(selection, absent) {
			t.Fatalf("selection prompt mentions %q:\n%s", absent, selection)
		}
	}
	for _, present := range []string{"M\tREADME", "A\tpicked.txt", "A\tskipped.txt", "+world", "+skipped line"} {
		if !strings.Contains(everything, present) {
			t.Fatalf("whole prompt misses %q:\n%s", present, everything)
		}
	}
	if got := gitText(t, workspace, "status", "--porcelain"); got != statusBefore {
		t.Fatalf("generating changed the workspace:\n%s\nwas:\n%s", got, statusBefore)
	}

	if _, err := app.GenerateCommitMessage(ref, CommitSelection{Files: []CommitFile{{Path: "README-not-changed"}}}); err == nil ||
		!strings.Contains(err.Error(), "no uncommitted changes") {
		t.Fatalf("a selection with no changes: err = %v", err)
	}
}

// A caller that lost its selection gets an error from both methods, never a
// commit or a description of everything.
func TestCommitSelection_EmptyIsRefused(t *testing.T) {
	t.Parallel()
	app := newCommitMsgTestApp(t)
	workspace := initMainGitRepo(t)
	ref := testWorkspaceRef(t, app, workspace)
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nedited\n")
	head := gitText(t, workspace, "rev-parse", "HEAD")
	app.textGenerationExecutor = func(context.Context, textgen.CLISpec) (textgen.CLIResult, error) {
		t.Error("the text generator ran for an invalid selection")
		return textgen.CLIResult{}, nil
	}

	for name, selection := range map[string]CommitSelection{
		"nothing":    {},
		"both forms": {All: true, Files: []CommitFile{{Path: "README.txt"}}},
		"no path":    {Files: []CommitFile{{Status: "M"}}},
	} {
		if _, err := app.GitCommit(ref, "subject", "", selection); err == nil || !strings.Contains(err.Error(), "commit selection") {
			t.Errorf("%s: GitCommit err = %v", name, err)
		}
		if _, err := app.GenerateCommitMessage(ref, selection); err == nil || !strings.Contains(err.Error(), "commit selection") {
			t.Errorf("%s: GenerateCommitMessage err = %v", name, err)
		}
	}
	if got := gitText(t, workspace, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %q", got)
	}
	if got := gitText(t, workspace, "status", "--porcelain"); got != " M README.txt\n" {
		t.Fatalf("status = %q", got)
	}
}

// A commit records what was listed or nothing: a selected file edited,
// reverted or renamed since the listing fails the commit and names the
// file, and the same selection from a fresh listing commits.
func TestGitCommit_RefusesFilesChangedSinceTheyWereListed(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	ctx := context.Background()
	list := func() CommitFiles {
		t.Helper()
		listed, err := app.ListCommitFiles(ctx, ref)
		if err != nil {
			t.Fatalf("ListCommitFiles: %v", err)
		}
		return listed
	}
	head := gitText(t, workspace, "rev-parse", "HEAD")
	refused := func(what string, selection CommitSelection, names ...string) {
		t.Helper()
		_, err := app.GitCommit(ref, "subject", "", selection)
		if !errors.Is(err, ErrCommitFilesChanged) {
			t.Fatalf("%s: err = %v, want ErrCommitFilesChanged", what, err)
		}
		for _, name := range names {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("%s: error does not name %s: %v", what, name, err)
			}
		}
		if got := gitText(t, workspace, "rev-parse", "HEAD"); got != head {
			t.Fatalf("%s: HEAD moved to %q", what, got)
		}
	}

	writeWorkspaceFile(t, workspace, "README.txt", "hello\nfirst\n")
	writeWorkspaceFile(t, workspace, "kept.txt", "kept\n")
	writeWorkspaceFile(t, workspace, "gone.txt", "gone\n")
	listed := list()

	// Edited with the same line counts, so nothing but its content moved.
	writeWorkspaceFile(t, workspace, "README.txt", "hello\nother\n")
	refused("edited", pick(t, listed, "README.txt", "kept.txt"), "README.txt")
	if got := gitText(t, workspace, "status", "--porcelain"); got != " M README.txt\n?? gone.txt\n?? kept.txt\n" {
		t.Fatalf("a refused commit changed the workspace: %q", got)
	}

	// No longer a change at all.
	if err := os.Remove(filepath.Join(workspace, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	refused("removed", pick(t, listed, "gone.txt"), "gone.txt")

	// More changed files than the error names.
	var many []string
	for i := range maxNamedChangedFiles + 2 {
		name := fmt.Sprintf("m%d.txt", i)
		writeWorkspaceFile(t, workspace, name, "v1\n")
		many = append(many, name)
	}
	stale := list()
	for _, name := range many {
		writeWorkspaceFile(t, workspace, name, "v2\n")
	}
	refused("many", pick(t, stale, many...), "m0.txt", "m2.txt", "and 2 more")

	// The fresh listing of the same files commits them as they are now.
	fresh := list()
	result, err := app.GitCommit(ref, "now listed", "", pick(t, fresh, "README.txt", "kept.txt"))
	if err != nil {
		t.Fatalf("GitCommit from a fresh listing: %v", err)
	}
	if got := gitText(t, workspace, "show", "--format=%H", "--name-only", "HEAD"); got != result.Commit+"\n\nREADME.txt\nkept.txt\n" {
		t.Fatalf("commit = %q", got)
	}
	if got := gitText(t, workspace, "show", "HEAD:README.txt"); got != "hello\nother\n" {
		t.Fatalf("committed README = %q", got)
	}
}

// A listed rename is one selected row and commits both of its paths.
func TestGitCommit_CommitsAListedRenameWhole(t *testing.T) {
	t.Parallel()
	app, ref, workspace, _ := gitActionTestSetup(t)
	writeWorkspaceFile(t, workspace, "old.txt", "one\ntwo\nthree\nfour\n")
	testutil.RunGit(t, workspace, "add", "old.txt")
	testutil.RunGit(t, workspace, "commit", "-m", "add old")
	if err := os.Rename(filepath.Join(workspace, "old.txt"), filepath.Join(workspace, "new.txt")); err != nil {
		t.Fatal(err)
	}
	listed, err := app.ListCommitFiles(context.Background(), ref)
	if err != nil {
		t.Fatalf("ListCommitFiles: %v", err)
	}
	if len(listed.Files) != 1 || listed.Files[0].OldPath != "old.txt" {
		t.Fatalf("listed = %+v", listed.Files)
	}
	if _, err := app.GitCommit(ref, "rename", "", pick(t, listed, "new.txt")); err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	if got := gitText(t, workspace, "show", "--format=", "--name-status", "-M", "HEAD"); got != "R100\told.txt\tnew.txt\n" {
		t.Fatalf("commit = %q", got)
	}
	if got := gitText(t, workspace, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q", got)
	}
}
