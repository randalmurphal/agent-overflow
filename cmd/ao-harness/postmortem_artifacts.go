package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// openPostmortemDatabase reads a private copy of the database and its WAL
// companions. SQLite may create or update -shm while opening a WAL database,
// so the original evidence tree must never be the connection's directory.
func openPostmortemDatabase(path string) (*sql.DB, func(), error) {
	tmp, err := os.MkdirTemp("", ".ao-postmortem-db-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create read-only database staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	if err := os.Chmod(tmp, 0o700); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("protect read-only database staging directory: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		source := path + suffix
		info, statErr := os.Lstat(source)
		if errors.Is(statErr, os.ErrNotExist) {
			if suffix == "" {
				cleanup()
				return nil, func() {}, fmt.Errorf("database %s does not exist", path)
			}
			continue
		}
		if statErr != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("inspect database companion %s: %w", source, statErr)
		}
		if !info.Mode().IsRegular() {
			cleanup()
			return nil, func() {}, fmt.Errorf("database companion %s is not a regular file", source)
		}
		if err := copyPostmortemDBFile(source, filepath.Join(tmp, filepath.Base(source))); err != nil {
			cleanup()
			return nil, func() {}, err
		}
	}
	db, err := openReadOnly(filepath.Join(tmp, filepath.Base(path)))
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return db, cleanup, nil
}

func copyPostmortemDBFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open database copy source %s: %w", source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create database copy %s: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy database companion %s: %w", source, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close database copy %s: %w", target, err)
	}
	return nil
}

func (s *postmortemScanner) requireUIEvidence(root string) {
	dir := filepath.Join(root, "ui-snapshots")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		s.find("missing_ui_evidence", dir, "UI evidence was requested but ui-snapshots is missing", "error")
		return
	}
	if err != nil {
		s.find("missing_ui_evidence", dir, err.Error(), "error")
		return
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count == 0 {
		s.find("missing_ui_evidence", dir, "UI evidence was requested but no snapshots were found", "error")
	} else if s.opt.UIDiff && len(s.uiViews) < 2 {
		s.find("missing_ui_diff", dir, "an offline UI diff requires at least two snapshots", "error")
	} else if s.opt.UIDiff {
		// Run the same pure geometry reader used by `ui diff`. Differences are
		// evidence, not errors. A malformed snapshot was already rejected above.
		for i := 1; i < len(s.uiViews); i++ {
			_ = diffViewports(s.uiViews[i-1], s.uiViews[i], uiGeometryThresholdPx)
		}
	}
}

// Keep JSON output deterministic for scripts and review diffs.
func (r *postmortemReport) sort() {
	for i := range r.Artifacts {
		for j := i + 1; j < len(r.Artifacts); j++ {
			if r.Artifacts[j].Path < r.Artifacts[i].Path {
				r.Artifacts[i], r.Artifacts[j] = r.Artifacts[j], r.Artifacts[i]
			}
		}
	}
	for i := range r.Findings {
		for j := i + 1; j < len(r.Findings); j++ {
			if r.Findings[j].Path < r.Findings[i].Path || (r.Findings[j].Path == r.Findings[i].Path && r.Findings[j].Code < r.Findings[i].Code) {
				r.Findings[i], r.Findings[j] = r.Findings[j], r.Findings[i]
			}
		}
	}
}
