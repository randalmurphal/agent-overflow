package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSupportedShellMutationCommandParsesDeleteRenameAndChains(t *testing.T) {
	parsed := parseSupportedShellMutationCommand(
		`/usr/bin/zsh -lc 'mv "src/old name.ts" "src/new name.ts" && rm src/remove.ts'`,
		"/repo",
	)
	if parsed == nil {
		t.Fatal("expected command to parse")
	}
	if parsed.NormalizedCommand != `mv "src/old name.ts" "src/new name.ts" && rm src/remove.ts` {
		t.Fatalf("unexpected normalized command: %q", parsed.NormalizedCommand)
	}
	if len(parsed.Operations) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(parsed.Operations))
	}
	if parsed.Operations[0].Kind != "rename" || parsed.Operations[0].OldPath != "src/old name.ts" || parsed.Operations[0].NewPath != "src/new name.ts" {
		t.Fatalf("unexpected rename operation: %+v", parsed.Operations[0])
	}
	if parsed.Operations[1].Kind != "delete" || parsed.Operations[1].Path != "src/remove.ts" {
		t.Fatalf("unexpected delete operation: %+v", parsed.Operations[1])
	}
}

func TestParseSupportedShellMutationCommandRejectsUnsupportedSyntax(t *testing.T) {
	cases := []string{
		"rm src/remove.ts | cat",
		"rm src/remove.ts || rm src/other.ts",
		"rm $TARGET",
		"rm src/*.ts",
		"(rm src/remove.ts)",
	}
	for _, command := range cases {
		if parsed := parseSupportedShellMutationCommand(command, "/repo"); parsed != nil {
			t.Fatalf("expected %q to be rejected, got %+v", command, parsed)
		}
	}
}

func TestBuildCommandExecutionInlineDiffArtifactFallsBackToSummaryOnly(t *testing.T) {
	inlineDiff, patch := buildCommandExecutionInlineDiffArtifact([]capturedShellMutationOperation{
		{
			Kind:    "rename",
			OldPath: "src/old.ts",
			NewPath: "src/new.ts",
			Exact:   false,
		},
		{
			Kind: "delete",
			Path: "src/remove.ts",
		},
	})
	if inlineDiff == nil {
		t.Fatal("expected inline diff")
	}
	if inlineDiff.Availability != "summary_only" {
		t.Fatalf("expected summary_only, got %+v", inlineDiff)
	}
	if patch != "" {
		t.Fatalf("expected empty patch, got %q", patch)
	}
}

func TestBuildCommandExecutionInlineDiffArtifactCapsPreviewFiles(t *testing.T) {
	operations := make([]capturedShellMutationOperation, 0, inlineDiffPreviewFileCount+4)
	for i := 0; i < inlineDiffPreviewFileCount+4; i++ {
		content := "old"
		operations = append(operations, capturedShellMutationOperation{
			Kind:            "delete",
			Path:            filepath.ToSlash(filepath.Join("src", fmt.Sprintf("remove-%02d.ts", i))),
			OriginalContent: &content,
			Exact:           true,
		})
	}

	inlineDiff, patch := buildCommandExecutionInlineDiffArtifact(operations)
	if inlineDiff == nil {
		t.Fatal("expected inline diff")
	}
	if inlineDiff.Availability != "exact_patch" {
		t.Fatalf("availability = %q, want exact_patch", inlineDiff.Availability)
	}
	if len(inlineDiff.Files) != inlineDiffPreviewFileCount {
		t.Fatalf("preview files = %d, want %d", len(inlineDiff.Files), inlineDiffPreviewFileCount)
	}
	if inlineDiff.TotalFiles != len(operations) {
		t.Fatalf("total files = %d, want %d", inlineDiff.TotalFiles, len(operations))
	}
	if inlineDiff.OmittedFiles != 4 || !inlineDiff.FilesTruncated {
		t.Fatalf("truncation metadata = omitted %d truncated %v, want omitted 4 truncated true", inlineDiff.OmittedFiles, inlineDiff.FilesTruncated)
	}
	if !strings.Contains(patch, "remove-") {
		t.Fatal("expected exact patch data to remain populated")
	}
}

func TestCaptureShellMutationOperationsRejectsUnsafePathsBeforeReading(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(workspace, "..", "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "config"), []byte("git-secret"), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
	}

	cases := []struct {
		name      string
		operation supportedShellMutationOperation
	}{
		{
			name: "delete traversal",
			operation: supportedShellMutationOperation{
				Kind: "delete",
				Path: "../outside-secret.txt",
			},
		},
		{
			name: "delete git metadata",
			operation: supportedShellMutationOperation{
				Kind: "delete",
				Path: ".git/config",
			},
		},
		{
			name: "rename source traversal",
			operation: supportedShellMutationOperation{
				Kind:    "rename",
				OldPath: "../outside-secret.txt",
				NewPath: "src/inside.txt",
			},
		},
		{
			name: "rename destination git metadata",
			operation: supportedShellMutationOperation{
				Kind:    "rename",
				OldPath: "src/inside.txt",
				NewPath: ".git/config",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured, ok := captureShellMutationOperations(
				[]supportedShellMutationOperation{tc.operation},
				workspace,
			)
			if ok {
				t.Fatalf("expected unsafe operation to be rejected, got %+v", captured)
			}
		})
	}
}

func TestCaptureShellMutationOperationsSkipsOversizedDeleteContent(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "huge.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("create huge file: %v", err)
	}
	if err := os.Truncate(path, commandInlineDiffDeleteCaptureMaxBytes+1); err != nil {
		t.Fatalf("grow huge file: %v", err)
	}

	captured, ok := captureShellMutationOperations(
		[]supportedShellMutationOperation{{Kind: "delete", Path: "huge.txt"}},
		workspace,
	)
	if !ok {
		t.Fatal("expected oversized delete to be captured as summary-only")
	}
	if len(captured) != 1 {
		t.Fatalf("captured operations = %d, want 1", len(captured))
	}
	if captured[0].Exact {
		t.Fatal("oversized delete should not be exact")
	}
	if captured[0].OriginalContent != nil {
		t.Fatal("oversized delete should not retain original content")
	}
}

func TestExtractRuntimeToolCommandNormalizesArrayArguments(t *testing.T) {
	workspace := t.TempDir()
	absoluteOld := filepath.Join(workspace, "src", "old.ts")
	absoluteNew := filepath.Join(workspace, "src", "new.ts")

	command := extractRuntimeToolCommand(map[string]any{
		"item": map[string]any{
			"command": []any{"mv", absoluteOld, absoluteNew},
		},
	})
	if command == "" {
		t.Fatal("expected array-form command to normalize")
	}

	parsed := parseSupportedShellMutationCommand(command, workspace)
	if parsed == nil {
		t.Fatal("expected array-form command to parse")
	}
	if parsed.Operations[0].OldPath != "src/old.ts" || parsed.Operations[0].NewPath != "src/new.ts" {
		t.Fatalf("unexpected normalized paths: %+v", parsed.Operations[0])
	}
}
