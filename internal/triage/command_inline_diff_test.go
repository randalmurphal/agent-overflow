package triage

import (
	"path/filepath"
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
