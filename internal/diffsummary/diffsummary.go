package diffsummary

import (
	"path/filepath"
	"strconv"
	"strings"
)

// File is the compact per-file summary persisted with checkpoint rows.
type File struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

func cleanDiffPath(raw string) string {
	path := strings.Trim(strings.TrimSpace(raw), `"`)
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	if path == "/dev/null" || path == "" {
		return path
	}
	return filepath.ToSlash(filepath.Clean(path))
}

// ParseGitNameStatusNumstat combines `git diff --name-status -z --no-renames`
// and `git diff --numstat -z --no-renames` output into compact file summaries.
func ParseGitNameStatusNumstat(nameStatusZ string, numstatZ string) []File {
	filesByPath := make(map[string]*File)
	var ordered []string

	ensure := func(path string) *File {
		path = cleanDiffPath(path)
		if path == "" {
			return nil
		}
		if existing := filesByPath[path]; existing != nil {
			return existing
		}
		file := &File{Path: path, Kind: "modified"}
		filesByPath[path] = file
		ordered = append(ordered, path)
		return file
	}

	nameStatusTokens := splitNULTokens(nameStatusZ)
	for i := 0; i+1 < len(nameStatusTokens); i += 2 {
		status := nameStatusTokens[i]
		path := nameStatusTokens[i+1]
		file := ensure(path)
		if file == nil {
			continue
		}
		file.Kind = statusToKind(status)
	}

	for _, record := range splitNULTokens(numstatZ) {
		parts := strings.Split(record, "\t")
		if len(parts) < 3 {
			continue
		}
		file := ensure(parts[2])
		if file == nil {
			continue
		}
		file.Additions = parseNumstatCount(parts[0])
		file.Deletions = parseNumstatCount(parts[1])
	}

	out := make([]File, 0, len(ordered))
	for _, path := range ordered {
		if file := filesByPath[path]; file != nil {
			out = append(out, *file)
		}
	}
	return out
}

func splitNULTokens(value string) []string {
	if value == "" {
		return nil
	}
	raw := strings.Split(value, "\x00")
	out := make([]string, 0, len(raw))
	for _, token := range raw {
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

func statusToKind(status string) string {
	switch {
	case strings.HasPrefix(status, "A"):
		return "added"
	case strings.HasPrefix(status, "D"):
		return "deleted"
	case strings.HasPrefix(status, "R"):
		return "renamed"
	default:
		return "modified"
	}
}

func parseNumstatCount(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return n
}
