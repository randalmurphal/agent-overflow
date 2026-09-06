package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func TestTransferPreservesCleanConversionsWithoutDestinationFilters(t *testing.T) {
	ctx, core := context.Background(), NewCore()
	source := testutil.InitGitRepo(t)
	writeWorkspaceFile(t, source, ".gitattributes", "asset.txt filter=portable\ncrlf.txt text eol=crlf\nident.txt ident\nencoded.txt text working-tree-encoding=UTF-16\nintent.txt filter=portable working-tree-encoding=UTF-16\n")
	testutil.RunGit(t, source, "config", "filter.portable.clean", "git stripspace")
	writeWorkspaceFile(t, source, "asset.txt", "source working content\n\n\n")
	writeWorkspaceFile(t, source, "crlf.txt", "source\r\nline endings\r\n")
	writeWorkspaceFile(t, source, "ident.txt", "$Id$\n")
	encoded := string([]byte{0xff, 0xfe, 'h', 0, 'i', 0, '\n', 0})
	writeWorkspaceFile(t, source, "encoded.txt", encoded)
	testutil.RunGit(t, source, "add", ".")
	testutil.RunGit(t, source, "commit", "-qm", "converted files")
	if err := os.Remove(filepath.Join(source, "ident.txt")); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, source, "checkout-index", "-f", "ident.txt")
	// Canonical Git comparisons report clean even though the raw blob differs.
	// diff-files can report the freshly expanded ident's stale stat entry;
	// diff performs the canonical clean conversion and confirms the contents.
	if status, _, err := core.Execute(source, "diff", "--exit-code"); err != nil || status != "" {
		t.Fatalf("fixture is not clean: %q %v", status, err)
	}
	writeWorkspaceFile(t, source, "intent.txt", encoded)
	testutil.RunGit(t, source, "add", "-N", "intent.txt")
	capture, err := core.CaptureTransferWorkspace(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, file := range capture.Workspace.Working {
		seen[file.Path] = true
	}
	for _, path := range []string{"asset.txt", "crlf.txt", "ident.txt", "encoded.txt", "intent.txt"} {
		if !seen[path] {
			t.Fatalf("lost clean conversion for %s", path)
		}
	}
	var pack bytes.Buffer
	if err := core.WriteTransferObjects(ctx, source, capture.Workspace.Head, capture.Workspace.Index, nil, &pack); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar")
	digest, err := transferfiles.Create(ctx, archive, capture.Sources)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	stage := filepath.Join(t.TempDir(), "extracted")
	if _, err := transferfiles.Extract(ctx, input, digest, stage); err != nil {
		t.Fatal(err)
	}
	destination := testutil.InitGitRepo(t)
	for _, key := range []string{"clean", "smudge", "process"} {
		testutil.RunGit(t, destination, "config", "filter.portable."+key, "ao-filter-must-never-run")
	}
	testutil.RunGit(t, destination, "config", "filter.portable.required", "true")
	plan, err := core.PrepareTransferWorktree(ctx, TransferWorktreeRequest{OperationID: uuid.NewString(), Repository: destination, Path: filepath.Join(t.TempDir(), "received"), Workspace: capture.Workspace, ArchiveRoot: stage}, &pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.PublishTransferWorktree(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for path := range seen {
		wanted, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(filepath.Join(plan.Path, path))
		if err != nil || !bytes.Equal(actual, wanted) {
			t.Fatalf("%s conversion lost: want=%q got=%q err=%v", path, wanted, actual, err)
		}
	}
	want, _, err := core.Execute(source, "ls-files", "--stage")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := core.Execute(plan.Path, "ls-files", "--stage")
	if err != nil || want != got {
		t.Fatalf("index changed: %v\nwant=%s\ngot=%s", err, want, got)
	}
}

func TestTransferBlobDecoderHandlesSplitHeadersEmptyFilesAndLinks(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries := []TransferIndexEntry{{OID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: "100644", Path: "empty"}, {OID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Mode: "100755", Path: "nested/body"}, {OID: "cccccccccccccccccccccccccccccccccccccccc", Mode: "120000", Path: "link"}}
	w := &transferBlobWriter{root: root, entries: entries, sizes: []int64{0, 4, 11}}
	defer w.close()
	data := []byte(entries[0].OID + " blob 0\n\n" + entries[1].OID + " blob 4\nA\x00\nZ\n" + entries[2].OID + " blob 11\nnested/body\n")
	for _, b := range data {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if w.next != 3 || w.separator || w.file != nil {
		t.Fatalf("unfinished decode: %+v", w)
	}
	got, err := root.ReadFile("nested/body")
	if err != nil || !bytes.Equal(got, []byte{'A', 0, '\n', 'Z'}) {
		t.Fatalf("body=%q %v", got, err)
	}
	link, err := root.Readlink("link")
	if err != nil || link != "nested/body" {
		t.Fatalf("link=%q %v", link, err)
	}
}
