package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agent-overflow/internal/observability/goroutinedump"
)

// logKinds are the daily-file prefixes this package mints. Every kind is
// minted by dailyLogPath and pruned by PruneOlderThan, so a new log stream
// cannot ship with retention that silently ignores it.
var logKinds = []string{"provider-events", "engine"}

// Dir is the log directory under an app data root: every file this package
// mints lives here, and so does anything else that wants to land next to the
// logs (the SIGUSR1 goroutine dump). Exported so no caller has to re-spell
// the subdirectory name.
func Dir(baseDir string) string {
	return filepath.Join(baseDir, "logs")
}

// dailyLogPath is where a log of the given kind for the current day lands.
// The date stub uses local time, and PruneOlderThan formats `now` the same
// way — that agreement is what keeps the active file off the prune list
// across a UTC/local day boundary.
func dailyLogPath(baseDir, kind string) string {
	return filepath.Join(Dir(baseDir), fmt.Sprintf(
		"%s-%s.ndjson", kind, time.Now().Format("2006-01-02"),
	))
}

// logFilePattern matches an active daily log file of any minted kind
// (<kind>-YYYY-MM-DD.ndjson) and its size-based rotations (.ndjson.1, .2,
// .3). Anchored so unrelated files in the same directory aren't touched.
var logFilePattern = regexp.MustCompile(
	`^(` + strings.Join(logKinds, "|") + `)-(\d{4}-\d{2}-\d{2})\.ndjson(\.[123])?$`,
)

// isDumpFile reports the second stream that lands in this directory: the SIGUSR1
// goroutine dumps (`internal/observability/goroutinedump`, which names `Dir` as
// its home). They are not daily logs — one file per signal, named by the moment
// it was taken — so they have no kind stub, no rotation suffix, and no active
// file to protect. What they DO have is the same retention question and no
// answer of their own: a dump is a full stack listing of a wedged process, taken
// exactly when that listing is largest, and a directory this package prunes must
// not be the one place that accumulates forever.
//
// The prefix is imported rather than re-spelled. `goroutinedump` is stdlib-only
// by design and imports nothing from here, so the dependency has one direction
// and the two cannot drift.
func isDumpFile(name string) bool {
	return strings.HasPrefix(name, goroutinedump.FilePrefix)
}

// PruneOlderThan removes files under `<baseDir>/logs` whose modification time is
// strictly older than `cutoff`. TWO streams live there and both are swept: the
// daily logs this package mints, and the SIGUSR1 goroutine dumps
// (`isDumpFile`). The current day's active file of each log
// kind (matching `now`'s date stub in the same local timezone dailyLogPath
// uses to mint the filename) is never removed even if its mtime somehow
// predates the cutoff — that's the file a running logger has open for append,
// and removing it under SQLite-like semantics is fine on Linux but corrupts
// the active handle on Windows.
//
// `now` is passed in so callers driving deterministic clocks (the retention
// sweep injects `App.retentionNowFn`) can also pin the active-file guard.
//
// mtime is the single source of truth for "is this old". The date in the
// filename only marks the day the file was created; rotated backups inherit
// the original creation date in their name but their mtimes track when the
// rotation happened.
//
// Returns the count of removed files and an aggregated error covering all
// failures so a single permission-denied doesn't hide a disk-full further
// down the directory. Non-existent log dir returns (0, nil).
func PruneOlderThan(baseDir string, now, cutoff time.Time) (int, error) {
	dir := Dir(baseDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("logging: read dir %s: %w", dir, err)
	}

	todayStub := now.Format("2006-01-02")

	var (
		removed int
		errs    []error
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch match := logFilePattern.FindStringSubmatch(name); {
		case match != nil:
			// match[2] is the YYYY-MM-DD date stub; match[3] is the optional
			// .1/.2/.3 rotation suffix. Active file = today's date stub AND
			// no rotation suffix.
			if match[2] == todayStub && match[3] == "" {
				continue
			}
		case isDumpFile(name):
			// No active-file guard: a dump is written once, closed, and never
			// appended to, so mtime alone decides. Today's dump is newer than any
			// cutoff a retention window produces anyway.
		default:
			continue
		}

		info, err := entry.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", name, err))
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
			continue
		}
		removed++
	}

	return removed, errors.Join(errs...)
}
