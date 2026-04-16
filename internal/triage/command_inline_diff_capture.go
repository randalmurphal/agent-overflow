package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type capturedShellMutationOperation struct {
	Kind            string
	Path            string
	OldPath         string
	NewPath         string
	OriginalContent *string
	Exact           bool
}

func captureShellMutationOperations(
	operations []supportedShellMutationOperation,
	workspaceRoot string,
) ([]capturedShellMutationOperation, bool) {
	captured := make([]capturedShellMutationOperation, 0, len(operations))
	for _, operation := range operations {
		if operation.Kind == "delete" {
			next, ok := captureDeleteOperation(operation, workspaceRoot)
			if !ok {
				return nil, false
			}
			captured = append(captured, next)
			continue
		}

		next, ok := captureRenameOperation(operation, workspaceRoot)
		if !ok {
			return nil, false
		}
		captured = append(captured, next)
	}
	return captured, true
}

func captureDeleteOperation(
	operation supportedShellMutationOperation,
	workspaceRoot string,
) (capturedShellMutationOperation, bool) {
	absolutePath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.Path))
	stat, err := os.Lstat(absolutePath)
	if err == nil && stat.IsDir() {
		return capturedShellMutationOperation{}, false
	}

	result := capturedShellMutationOperation{
		Kind:  "delete",
		Path:  operation.Path,
		Exact: true,
	}
	if err == nil && stat.Mode().IsRegular() {
		content, readErr := os.ReadFile(absolutePath)
		if readErr == nil {
			original := string(content)
			result.OriginalContent = &original
			return result, true
		}
	}
	result.Exact = false
	return result, true
}

func captureRenameOperation(
	operation supportedShellMutationOperation,
	workspaceRoot string,
) (capturedShellMutationOperation, bool) {
	oldPath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.OldPath))
	newPath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.NewPath))

	sourceStat, sourceErr := os.Lstat(oldPath)
	if sourceErr == nil && sourceStat.IsDir() {
		return capturedShellMutationOperation{}, false
	}
	if destinationStat, err := os.Lstat(newPath); err == nil && destinationStat != nil {
		return capturedShellMutationOperation{}, false
	}

	return capturedShellMutationOperation{
		Kind:    "rename",
		OldPath: operation.OldPath,
		NewPath: operation.NewPath,
		Exact:   sourceErr == nil && sourceStat.Mode().IsRegular(),
	}, true
}

func buildCommandExecutionInlineDiffArtifact(
	operations []capturedShellMutationOperation,
) (*ToolInlineDiff, string) {
	if len(operations) == 0 {
		return nil, ""
	}

	files := summarizeCapturedCommandFiles(operations)
	if len(files) == 0 {
		return nil, ""
	}

	fragments := make([]string, 0, len(operations))
	exact := true
	deletions := 0
	for _, operation := range operations {
		switch operation.Kind {
		case "delete":
			if operation.OriginalContent == nil {
				exact = false
				continue
			}
			deletions += len(splitRawFileContentLines(*operation.OriginalContent))
			fragments = append(fragments, buildDeletedFileUnifiedDiff(operation.Path, *operation.OriginalContent))
		case "rename":
			if !operation.Exact {
				exact = false
				continue
			}
			fragments = append(fragments, buildRenamedFileUnifiedDiff(operation.OldPath, operation.NewPath))
		}
	}

	if !exact || len(fragments) != len(operations) {
		return &ToolInlineDiff{
			Availability: "summary_only",
			Files:        files,
		}, ""
	}

	return &ToolInlineDiff{
		Availability: "exact_patch",
		Files:        files,
		Deletions:    deletions,
	}, strings.Join(fragments, "\n\n")
}

func summarizeCapturedCommandFiles(operations []capturedShellMutationOperation) []ToolInlineDiffFile {
	byPath := make(map[string]ToolInlineDiffFile, len(operations))
	for _, operation := range operations {
		if operation.Kind == "delete" {
			file := ToolInlineDiffFile{
				Path: operation.Path,
				Kind: "deleted",
			}
			if operation.OriginalContent != nil {
				file.Deletions = len(splitRawFileContentLines(*operation.OriginalContent))
			}
			byPath[file.Path] = file
			continue
		}

		byPath[operation.NewPath] = ToolInlineDiffFile{
			Path: operation.NewPath,
			Kind: "renamed",
		}
	}

	files := make([]ToolInlineDiffFile, 0, len(byPath))
	for _, file := range byPath {
		files = append(files, file)
	}
	slices.SortFunc(files, func(left, right ToolInlineDiffFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	return files
}

func buildDeletedFileUnifiedDiff(path, rawContent string) string {
	lines := splitRawFileContentLines(rawContent)
	section := []string{
		fmt.Sprintf("diff --git a/%s b/%s", path, path),
		"deleted file mode 100644",
		fmt.Sprintf("--- a/%s", path),
		"+++ /dev/null",
	}
	if len(lines) == 0 {
		return strings.Join(section, "\n")
	}

	section = append(section, fmt.Sprintf("@@ -1,%d +0,0 @@", len(lines)))
	for _, line := range lines {
		section = append(section, "-"+line)
	}
	return strings.Join(section, "\n")
}

func buildRenamedFileUnifiedDiff(oldPath, newPath string) string {
	return strings.Join([]string{
		fmt.Sprintf("diff --git a/%s b/%s", oldPath, newPath),
		fmt.Sprintf("rename from %s", oldPath),
		fmt.Sprintf("rename to %s", newPath),
		fmt.Sprintf("--- a/%s", oldPath),
		fmt.Sprintf("+++ b/%s", newPath),
	}, "\n")
}

func splitRawFileContentLines(content string) []string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if normalized == "" {
		return nil
	}
	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}
