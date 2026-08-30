package main

// This file is deliberately independent of instance.go's attach path. A
// postmortem is for a stopped run, and opening its wire would both make the
// answer time-dependent and risk talking to the wrong process.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/appdirs"
	"agent-overflow/internal/harness/instanceinfo"
)

const (
	postmortemDefaultMaxFiles     = 10000
	postmortemDefaultMaxFileBytes = 64 << 20
	postmortemDefaultMaxTotal     = 512 << 20
)

type postmortemFinding struct {
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Detail   string `json:"detail"`
	Severity string `json:"severity"`
}

type postmortemArtifact struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Bytes     int64  `json:"bytes"`
	Lines     int    `json:"lines,omitempty"`
	Validated bool   `json:"validated"`
}

type postmortemDatabase struct {
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Integrity string `json:"integrity"`
	ReadOnly  bool   `json:"readOnly"`
}

type postmortemReport struct {
	Root      string               `json:"root"`
	Status    string               `json:"status"`
	Requested bool                 `json:"uiRequested"`
	Live      map[string]string    `json:"live"`
	Coverage  []string             `json:"coverageWarnings,omitempty"`
	Findings  []postmortemFinding  `json:"findings,omitempty"`
	Artifacts []postmortemArtifact `json:"artifacts,omitempty"`
	Databases []postmortemDatabase `json:"databases,omitempty"`
	Files     int                  `json:"files"`
	Evidence  int                  `json:"evidenceFiles"`
	Bytes     int64                `json:"bytes"`
}

type postmortemOptions struct {
	Root          string
	UI            bool
	UIDiff        bool
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

func runPostmortem(e *env, args []string) error {
	flags := e.newFlagSet("postmortem --root <absolute-canonical-root>")
	root := flags.String("root", "", "explicit absolute canonical harness data root to inspect")
	ui := flags.Bool("ui", false, "require and validate UI snapshots and their diff evidence")
	uiSnapshot := flags.Bool("ui-snapshot", false, "require UI snapshot evidence")
	uiDiff := flags.Bool("ui-diff", false, "require at least two UI snapshots for an offline diff")
	maxFiles := flags.Int("max-files", postmortemDefaultMaxFiles, "maximum files to scan")
	maxFileBytes := flags.Int64("max-file-bytes", postmortemDefaultMaxFileBytes, "maximum bytes read from one file")
	maxTotal := flags.Int64("max-total-bytes", postmortemDefaultMaxTotal, "maximum bytes read from the root")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("postmortem takes no positional arguments (got %v)", rest)
	}
	if strings.TrimSpace(*root) == "" {
		return usagef("postmortem requires --root <absolute-canonical-root>")
	}
	if *maxFiles < 1 || *maxFileBytes < 1 || *maxTotal < 1 {
		return usagef("postmortem limits must be positive")
	}
	canonical, err := validatePostmortemRoot(*root)
	if err != nil {
		return err
	}
	if err := refusePostmortemOwner(e, canonical); err != nil {
		return err
	}
	report, err := inspectPostmortem(canonical, postmortemOptions{
		Root: canonical, UI: *ui || *uiSnapshot || *uiDiff, UIDiff: *ui || *uiDiff, MaxFiles: *maxFiles,
		MaxFileBytes: *maxFileBytes, MaxTotalBytes: *maxTotal,
	})
	if err != nil {
		return err
	}
	if e.jsonOutput() {
		if err := e.writeJSON(report); err != nil {
			return err
		}
	} else {
		e.printf("postmortem %s\n", report.Status)
		e.printf("root      %s\n", report.Root)
		e.printf("evidence  %d recognized file(s), %d non-empty file(s), %d bytes\n", report.Files, report.Evidence, report.Bytes)
		for _, warning := range report.Coverage {
			e.printf("coverage  %s\n", warning)
		}
		for _, finding := range report.Findings {
			e.printf("%s       %s%s\n", finding.Severity, finding.Detail, formatFindingPath(finding.Path))
		}
	}
	if len(report.Findings) > 0 || len(report.Coverage) > 0 {
		return exitCodeError{code: exitBadNews, err: fmt.Errorf("postmortem is %s", report.Status)}
	}
	return nil
}

func formatFindingPath(path string) string {
	if path == "" {
		return ""
	}
	return " (" + path + ")"
}

// validatePostmortemRoot rejects lexical tricks and all symlinked components.
// The scanner never gets a path that can escape the operator's named tree.
func validatePostmortemRoot(raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		return "", usagef("postmortem refuses %q: --root must be absolute", raw)
	}
	if filepath.Clean(raw) != raw {
		return "", usagef("postmortem refuses %q: --root must be canonical (no . or .. or trailing separator)", raw)
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", fmt.Errorf("postmortem root %s: %w", raw, err)
	}
	if !info.IsDir() {
		return "", usagef("postmortem refuses %s: --root must be a directory", raw)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", usagef("postmortem refuses %s: --root must not be a symlink", raw)
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize postmortem root %s: %w", raw, err)
	}
	if filepath.Clean(resolved) != raw {
		return "", usagef("postmortem refuses %s: --root resolves through a symlink to %s", raw, resolved)
	}
	return raw, nil
}

func refusePostmortemOwner(e *env, root string) error {
	// Never inspect the app-managed tree, even though all reads below are
	// read-only. This prevents a mistaken postmortem from becoming a data dump.
	realRoot, err := appdirs.Root()
	if err != nil {
		return usagef("postmortem refuses %s: cannot resolve the real app data root: %v", root, err)
	}
	if underDir(root, realRoot) || underDir(realRoot, root) {
		return usagef("postmortem refuses %s: it overlaps the real app data root %s", root, realRoot)
	}
	// A scratch root can alias the live database through a hard link, which
	// has no symlink or path spelling for the containment check to catch.
	if realInfo, realErr := os.Stat(filepath.Join(realRoot, "agent-overflow.db")); realErr == nil {
		for _, candidate := range []string{filepath.Join(root, "agent-overflow.db"), filepath.Join(root, appDataDirName, "agent-overflow.db")} {
			if candidateInfo, candidateErr := os.Stat(candidate); candidateErr == nil && os.SameFile(realInfo, candidateInfo) {
				return usagef("postmortem refuses %s: it contains the real app database via hard link %s", root, candidate)
			}
		}
	}
	rows, err := e.listInstances()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Stale || row.DataRoot == "" {
			continue
		}
		rowRoot := filepath.Clean(row.DataRoot)
		if resolved, resolveErr := filepath.EvalSymlinks(rowRoot); resolveErr == nil {
			rowRoot = resolved
		}
		if underDir(rowRoot, root) || underDir(root, rowRoot) {
			return usagef("postmortem refuses %s: live harness %s owns overlapping root %s", root, row.ID, row.DataRoot)
		}
	}
	instanceFile := filepath.Join(root, appDataDirName, instanceinfo.InstanceFileName)
	if hasSymlinkComponent(instanceFile, root) {
		return usagef("postmortem refuses %s: ownership marker path escapes through a symlink", root)
	}
	if _, err := os.Stat(instanceFile); errors.Is(err, os.ErrNotExist) {
		instanceFile = filepath.Join(root, instanceinfo.InstanceFileName)
	}
	if hasSymlinkComponent(instanceFile, root) {
		return usagef("postmortem refuses %s: ownership marker path escapes through a symlink", root)
	}
	data, err := os.ReadFile(instanceFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read postmortem ownership marker %s: %w", instanceFile, err)
	}
	var marker struct {
		PID          int    `json:"pid"`
		PIDNamespace string `json:"pidNamespace"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return usagef("postmortem refuses %s: malformed ownership marker: %v", root, err)
	}
	if marker.PID > 0 && instanceinfo.ProcessAliveInNamespace(marker.PID, marker.PIDNamespace) {
		return usagef("postmortem refuses %s: ownership marker names live pid %d", root, marker.PID)
	}
	return nil
}

func hasSymlinkComponent(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	current := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
