package def

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxDefinitionBytes int64 = 1 << 20
	MaxPromptBytes     int64 = 4 << 20
)

func readLimitedFile(path, kind string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", kind, path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read %s %q: %w", kind, path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close %s %q: %w", kind, path, closeErr)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%s %q exceeds %d-byte limit", kind, path, maximum)
	}
	return data, nil
}

func confinedPath(base, relative string) (string, error) {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve definition directory %q: %w", base, err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(filepath.Join(base, relative))
	if err != nil {
		return "", fmt.Errorf("resolve relative path %q: %w", relative, err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil {
		return "", fmt.Errorf("compare relative path %q to definition directory: %w", relative, err)
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative path %q resolves outside the definition directory", relative)
	}
	return resolvedCandidate, nil
}
