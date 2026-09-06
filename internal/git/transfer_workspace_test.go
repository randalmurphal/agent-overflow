package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func writeWorkspaceFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTransferWorkspacePreservesIndexWorkingFilesAndHiddenFlags(t *testing.T) {
	ctx, core := context.Background(), NewCore()
	source := testutil.InitGitRepo(t)
	for _, path := range []string{"both", "skipped", "assumed", "delete", "staged-delete", "becomes-directory", "becomes-file/nested/old"} {
		writeWorkspaceFile(t, source, path, "base\n")
	}
	writeWorkspaceFile(t, source, ".gitignore", "ignored\n")
	if err := os.Symlink("both", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, source, "add", ".")
	testutil.RunGit(t, source, "commit", "-m", "workspace base")
	writeWorkspaceFile(t, source, "both", "staged\n")
	testutil.RunGit(t, source, "add", "both")
	testutil.RunGit(t, source, "rm", "staged-delete")
	writeWorkspaceFile(t, source, "both", "working\n")
	writeWorkspaceFile(t, source, "untracked binary", string([]byte{0, 1, 2, 255}))
	writeWorkspaceFile(t, source, "intent", "new, not staged\n")
	if err := os.Symlink("both", filepath.Join(source, "intent-link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, source, "add", "-N", "intent", "intent-link")
	testutil.RunGit(t, source, "update-index", "--skip-worktree", "skipped")
	testutil.RunGit(t, source, "update-index", "--assume-unchanged", "assumed")
	writeWorkspaceFile(t, source, "assumed", "hidden working edit\n")
	for _, path := range []string{"skipped", "delete", "link", "becomes-directory"} {
		if err := os.Remove(filepath.Join(source, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("intent", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, source, "becomes-directory/new", "new child\n")
	if err := os.RemoveAll(filepath.Join(source, "becomes-file")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, source, "becomes-file", "now a file\n")
	writeWorkspaceFile(t, source, "ignored", "must stay local\n")
	beforeIndex, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	beforeRefs, _, err := core.Execute(source, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	capture, err := core.CaptureTransferWorkspace(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var pack bytes.Buffer
	if err := core.WriteTransferObjects(ctx, source, capture.Workspace.Head, capture.Workspace.Index, nil, &pack); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "workspace.tar")
	digest, err := transferfiles.Create(ctx, archive, capture.Sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.VerifyTransferWorkspace(ctx, source, capture); err != nil {
		t.Fatal(err)
	}
	afterIndex, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatalf("capture mutated source index: %v", err)
	}
	afterRefs, _, err := core.Execute(source, "show-ref")
	if err != nil || beforeRefs != afterRefs {
		t.Fatalf("capture mutated source refs: %v", err)
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
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete pack", true: "destination clone"}[shared], func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "checkout")
			base := testutil.InitGitRepo(t)
			if shared {
				base = source
			}
			plan, err := core.PrepareTransferWorktree(ctx, TransferWorktreeRequest{OperationID: uuid.NewString(), Repository: base, Path: destination, Workspace: capture.Workspace, ArchiveRoot: stage}, bytes.NewReader(pack.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := core.PublishTransferWorktree(ctx, plan); err != nil {
				t.Fatal(err)
			}
			if err := core.PublishTransferWorktree(ctx, plan); err != nil {
				t.Fatalf("publication retry: %v", err)
			}
			for _, args := range [][]string{{"status", "--porcelain=v2", "-z"}, {"ls-files", "--stage", "-z"}, {"ls-files", "-v", "-z"}, {"diff", "--cached", "--binary"}} {
				want, _, err := core.Execute(source, args...)
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := core.Execute(destination, args...)
				if err != nil || got != want {
					t.Fatalf("%v mismatch\nwant %q\ngot  %q\n%v", args, want, got, err)
				}
			}
			for _, path := range []string{"both", "assumed", "untracked binary", "intent", "becomes-directory/new", "becomes-file"} {
				want, _ := os.ReadFile(filepath.Join(source, path))
				got, err := os.ReadFile(filepath.Join(destination, path))
				if err != nil || !bytes.Equal(want, got) {
					t.Fatalf("working bytes %s: %q %v", path, got, err)
				}
			}
			for _, path := range []string{"ignored", "skipped", "delete", "staged-delete"} {
				if _, err := os.Lstat(filepath.Join(destination, path)); !os.IsNotExist(err) {
					t.Fatalf("unexpected restored %s: %v", path, err)
				}
			}
			for _, path := range []string{"link", "intent-link"} {
				want, _ := os.Readlink(filepath.Join(source, path))
				got, err := os.Readlink(filepath.Join(destination, path))
				if err != nil || got != want {
					t.Fatalf("symbolic link %s: %q %v", path, got, err)
				}
			}
		})
	}
}

func TestTransferWorkspaceRefusesSourceChangesAndExistingDestination(t *testing.T) {
	ctx, core := context.Background(), NewCore()
	source := testutil.InitGitRepo(t)
	writeWorkspaceFile(t, source, "untracked", "initial")
	capture, err := core.CaptureTransferWorkspace(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, source, "untracked", "new external edit")
	if err := core.VerifyTransferWorkspace(ctx, source, capture); err == nil {
		t.Fatal("accepted changing source")
	}
	destination := t.TempDir()
	writeWorkspaceFile(t, destination, "keep", "existing checkout")
	if _, err := core.PrepareTransferWorktree(ctx, TransferWorktreeRequest{OperationID: uuid.NewString(), Repository: source, Path: destination, Workspace: capture.Workspace, ArchiveRoot: t.TempDir()}, strings.NewReader("")); err == nil {
		t.Fatal("overwrote existing destination")
	}
	if got, err := os.ReadFile(filepath.Join(destination, "keep")); err != nil || string(got) != "existing checkout" {
		t.Fatalf("existing data touched: %q %v", got, err)
	}
}

func TestTransferWorkspaceRejectsUnsafeMetadataBeforeCreatingDirectory(t *testing.T) {
	ctx, core := context.Background(), NewCore()
	source := testutil.InitGitRepo(t)
	capture, err := core.CaptureTransferWorkspace(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".git/config", ".GiT/config", ".g\u200dit/config", "git~1/config", "../outside"} {
		w := capture.Workspace
		w.Working = []TransferWorkingFile{{Path: path, Kind: "file"}}
		destination := filepath.Join(t.TempDir(), "checkout")
		if _, err := core.PrepareTransferWorktree(ctx, TransferWorktreeRequest{OperationID: uuid.NewString(), Repository: source, Path: destination, Workspace: w, ArchiveRoot: t.TempDir()}, strings.NewReader("")); err == nil {
			t.Fatalf("accepted %s", path)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("invalid metadata created a directory")
		}
	}
	w := capture.Workspace
	w.Working = []TransferWorkingFile{{Path: "parent", Kind: "symlink", Link: "/outside"}, {Path: "parent/child", Kind: "file"}}
	if err := validateTransferWorkspace(w); err == nil {
		t.Fatal("allowed a write through a working symlink")
	}
	if capture.Workspace.Working != nil {
		t.Fatal("validation mutated metadata")
	}
}
