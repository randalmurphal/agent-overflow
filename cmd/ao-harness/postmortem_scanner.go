package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agent-overflow/internal/harness/instanceinfo"
)

type postmortemScanner struct {
	report    postmortemReport
	opt       postmortemOptions
	seenBytes int64
	uiViews   []uiViewport
	bundles   map[string]map[string]bool
	// ownedExecutable is the executable path attested by the stopped
	// harness's instance marker. A basename-only exception would let an
	// arbitrary symlink named agent-overflow hide outside evidence.
	ownedExecutable string
}

const (
	postmortemCLIBinDirName  = "bin"
	postmortemCLICommandName = "agent-overflow"
)

func inspectPostmortem(root string, opt postmortemOptions) (postmortemReport, error) {
	if opt.MaxFiles <= 0 {
		opt.MaxFiles = postmortemDefaultMaxFiles
	}
	if opt.MaxFileBytes <= 0 {
		opt.MaxFileBytes = postmortemDefaultMaxFileBytes
	}
	if opt.MaxTotalBytes <= 0 {
		opt.MaxTotalBytes = postmortemDefaultMaxTotal
	}
	s := &postmortemScanner{opt: opt, bundles: map[string]map[string]bool{}, report: postmortemReport{
		Root: root, Requested: opt.UI, Live: map[string]string{
			"process": "n/a", "rpc": "n/a", "spawn": "n/a", "cursor": "n/a",
		},
	}}
	s.ownedExecutable = postmortemOwnedExecutable(root)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			s.find("unreadable", path, walkErr.Error(), "error")
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if s.isOwnedBinaryLink(path) {
				return nil
			}
			s.find("symlink", path, "artifact is a symlink and was not followed", "error")
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) == 2 && parts[0] == "bundles" {
				s.bundles[parts[1]] = map[string]bool{}
			}
			return nil
		}
		if path == root {
			return nil
		}
		if s.report.Files >= opt.MaxFiles {
			s.coverage("file limit reached; remaining files were not scanned")
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			s.find("stat", path, err.Error(), "error")
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		kind := classifyPostmortemFile(root, path)
		if kind == "" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		relParts := strings.Split(filepath.ToSlash(rel), "/")
		if len(relParts) >= 3 && relParts[0] == "bundles" {
			if s.bundles[relParts[1]] == nil {
				s.bundles[relParts[1]] = map[string]bool{}
			}
			s.bundles[relParts[1]][strings.ToLower(filepath.Base(path))] = true
		}
		s.scanFile(path, kind)
		return nil
	})
	if err != nil {
		return postmortemReport{}, fmt.Errorf("scan postmortem root %s: %w", root, err)
	}
	if opt.UI {
		s.requireUIEvidence(root)
	}
	s.validateBundles()
	if s.report.Evidence == 0 {
		s.find("no_evidence", root, "no recognized postmortem evidence was found", "error")
	}
	if len(s.report.Findings) > 0 {
		s.report.Status = "findings"
	} else if len(s.report.Coverage) > 0 {
		s.report.Status = "incomplete"
	} else {
		s.report.Status = "clean"
	}
	s.report.sort()
	return s.report, nil
}

// isOwnedBinaryLink permits the one symlink the app creates in a harness
// data root. The link name alone is not enough. Validate that the name stays
// under the inspected root and that its resolved target is a regular file.
// Any other link, including a broken or directory-valued link at this name,
// remains a finding and is never followed.
func (s *postmortemScanner) isOwnedBinaryLink(path string) bool {
	want := filepath.Join(s.opt.Root, appDataDirName, postmortemCLIBinDirName, postmortemCLICommandName)
	if filepath.Clean(path) != filepath.Clean(want) {
		return false
	}
	rel, err := filepath.Rel(s.opt.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	rawTarget, err := os.Readlink(path)
	if err != nil || strings.TrimSpace(rawTarget) == "" {
		return false
	}
	target := rawTarget
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || s.ownedExecutable == "" {
		return false
	}
	owned, err := filepath.EvalSymlinks(s.ownedExecutable)
	return err == nil && filepath.Clean(resolved) == filepath.Clean(owned)
}

func postmortemOwnedExecutable(root string) string {
	for _, path := range []string{
		filepath.Join(root, appDataDirName, instanceinfo.InstanceFileName),
		filepath.Join(root, instanceinfo.InstanceFileName),
	} {
		if hasSymlinkComponent(path, root) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var marker struct {
			ExecutablePath string `json:"executablePath"`
		}
		if json.Unmarshal(data, &marker) == nil && filepath.IsAbs(marker.ExecutablePath) && strings.TrimSpace(marker.ExecutablePath) != "" {
			return filepath.Clean(marker.ExecutablePath)
		}
	}
	return ""
}

func (s *postmortemScanner) validateBundles() {
	for name, files := range s.bundles {
		for _, required := range []string{"db.snapshot", "events.jsonl", "meta.json"} {
			if !files[required] {
				s.find("incomplete_bundle", filepath.Join(s.opt.Root, "bundles", name), "bundle is missing "+required, "error")
			}
		}
	}
}

func (s *postmortemScanner) coverage(detail string) {
	if !slicesContains(s.report.Coverage, detail) {
		s.report.Coverage = append(s.report.Coverage, detail)
	}
}

func (s *postmortemScanner) find(code, path, detail, severity string) {
	s.report.Findings = append(s.report.Findings, postmortemFinding{Code: code, Path: path, Detail: detail, Severity: severity})
}

func slicesContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func classifyPostmortemFile(root, path string) string {
	rel, _ := filepath.Rel(root, path)
	rel = filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(path))
	switch {
	case base == "agent-overflow.db":
		return "database"
	case strings.HasPrefix(base, "backend-stderr.log") || strings.HasSuffix(base, ".stderr.log") || strings.HasSuffix(base, ".stdout.log") || (strings.HasPrefix(rel, "logs/") && (strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".ndjson"))):
		return "backend-log"
	case strings.HasPrefix(base, "frontend-errors.jsonl"):
		return "frontend-log"
	case strings.HasPrefix(base, "ui-render.jsonl"):
		return "ui-log"
	case strings.HasPrefix(rel, "replay/") && (strings.HasSuffix(base, ".jsonl") || strings.HasSuffix(base, ".ndjson")):
		return "event-log"
	case base == "events.jsonl":
		return "event-log"
	case strings.HasPrefix(rel, "bundles/") && base == "meta.json":
		return "bundle-meta"
	case strings.HasPrefix(rel, "bundles/") && base == "db.snapshot":
		return "bundle-db"
	case strings.HasPrefix(rel, "bundles/") && base == "events.jsonl":
		return "bundle-events"
	case strings.HasPrefix(rel, "bench/") && strings.HasSuffix(base, ".json"):
		return "bench-report"
	case strings.HasPrefix(rel, "profiles/") && strings.HasSuffix(base, ".cpuprofile"):
		return "cpu-profile"
	case strings.HasPrefix(rel, "traces/") && (strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".trace")):
		return "trace"
	case strings.HasPrefix(rel, "ui-snapshots/") && strings.HasSuffix(base, ".json"):
		return "ui-snapshot"
	case base == "run-manifest.json", base == "compare-supervisor.json":
		return "run-manifest"
	case base == "harness-instance.json":
		return "metadata"
	}
	return ""
}

func (s *postmortemScanner) scanFile(path, kind string) {
	data, truncated, err := s.read(path)
	if err != nil {
		s.find("read", path, err.Error(), "error")
		return
	}
	info, _ := os.Stat(path)
	a := postmortemArtifact{Path: path, Kind: kind, Bytes: int64(len(data)), Validated: true}
	s.report.Files++
	s.report.Bytes += int64(len(data))
	if len(data) > 0 {
		s.report.Evidence++
	}
	if truncated {
		a.Validated = false
		s.coverage("file limit truncated " + path)
	}
	if truncated {
		s.report.Artifacts = append(s.report.Artifacts, a)
		return
	}
	if info != nil && info.Size() > int64(len(data)) {
		s.find("truncated", path, "file could not be read in full", "error")
		a.Validated = false
		s.report.Artifacts = append(s.report.Artifacts, a)
		return
	}
	findingsBefore := len(s.report.Findings)
	switch kind {
	case "backend-log":
		s.scanTextLog(path, data, &a)
	case "frontend-log", "ui-log", "event-log", "bundle-events":
		s.scanJSONLines(path, data, &a)
	case "bundle-meta", "bench-report", "metadata", "ui-snapshot":
		s.scanJSON(path, data, kind)
	case "bundle-db", "database":
		s.scanDatabase(path)
	case "cpu-profile":
		if _, err := decodeCPUProfile(data); err != nil {
			s.find("invalid_cpu_profile", path, err.Error(), "error")
		}
	case "trace":
		if _, err := parseTraceEvents(data); err != nil {
			s.find("invalid_trace", path, err.Error(), "error")
		}
	}
	if len(s.report.Findings) != findingsBefore {
		a.Validated = false
	}
	s.report.Artifacts = append(s.report.Artifacts, a)
}

func (s *postmortemScanner) read(path string) ([]byte, bool, error) {
	if s.seenBytes >= s.opt.MaxTotalBytes {
		s.coverage("total byte limit reached; remaining files were not scanned")
		return nil, true, nil
	}
	remaining := s.opt.MaxTotalBytes - s.seenBytes
	limit := s.opt.MaxFileBytes
	if remaining < limit {
		limit = remaining
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		s.seenBytes += limit
		return data[:limit], true, nil
	}
	s.seenBytes += int64(len(data))
	return data, false, nil
}

var postmortemErrorPattern = regexp.MustCompile(`(?i)\b(panic|fatal|uncaught|fault|error)\b`)

func (s *postmortemScanner) scanTextLog(path string, data []byte, a *postmortemArtifact) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		a.Lines++
		line := scanner.Text()
		if postmortemErrorPattern.MatchString(line) {
			s.find("log_error", path, truncate(line, 240), "error")
		}
	}
	if err := scanner.Err(); err != nil {
		s.find("log_line", path, err.Error(), "error")
	}
}

func (s *postmortemScanner) scanJSONLines(path string, data []byte, a *postmortemArtifact) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		a.Lines++
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			s.find("invalid_jsonl", path, fmt.Sprintf("line %d: %v", a.Lines, err), "error")
		}
	}
	if err := scanner.Err(); err != nil {
		s.find("jsonl_line", path, err.Error(), "error")
	}
}

func (s *postmortemScanner) scanJSON(path string, data []byte, kind string) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		s.find("invalid_json", path, err.Error(), "error")
		return
	}
	if len(value) == 0 {
		s.find("empty_json", path, "JSON object is empty", "error")
	}
	if kind == "ui-snapshot" {
		var snap uiSnapshotFile
		if err := json.Unmarshal(data, &snap); err != nil {
			s.find("invalid_ui_snapshot", path, err.Error(), "error")
			return
		}
		if snap.Viewport.V != 1 {
			s.find("invalid_ui_snapshot", path, fmt.Sprintf("viewport snapshot is version %d; this ao-harness reads v1", snap.Viewport.V), "error")
		} else {
			s.uiViews = append(s.uiViews, snap.Viewport)
		}
	}
	if kind == "bench-report" {
		var doc benchDocument
		if err := json.Unmarshal(data, &doc); err != nil {
			s.find("invalid_bench_report", path, err.Error(), "error")
		} else if doc.Workload == "" || len(doc.Runs) == 0 || len(doc.Aggregate) == 0 {
			s.find("invalid_bench_report", path, "bench report lacks workload, runs, or aggregate", "error")
		}
	}
}

func (s *postmortemScanner) scanDatabase(path string) {
	database := postmortemDatabase{Path: path}
	if info, err := os.Stat(path); err == nil {
		database.Bytes = info.Size()
	}
	db, cleanup, err := openPostmortemDatabase(path)
	if err != nil {
		s.find("database_open", path, err.Error(), "error")
		s.report.Databases = append(s.report.Databases, database)
		return
	}
	defer cleanup()
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		s.find("database_integrity", path, err.Error(), "error")
		database.Integrity = "error"
		s.report.Databases = append(s.report.Databases, database)
		return
	}
	database.Integrity = result
	if strings.ToLower(result) != "ok" {
		s.find("database_integrity", path, result, "error")
	}
	var queryOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		s.find("database_read_only", path, "connection is not query-only", "error")
	} else {
		database.ReadOnly = true
	}
	s.report.Databases = append(s.report.Databases, database)
}
