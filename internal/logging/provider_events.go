package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ProviderEventEntry is the raw provider I/O log schema.
type ProviderEventEntry struct {
	Timestamp string `json:"ts"`
	ThreadID  string `json:"threadId"`
	Direction string `json:"direction"`
	Provider  string `json:"provider"`
	Data      string `json:"data"`
}

// LogProviderEvent writes a raw provider stdin/stdout event as one NDJSON line.
// Timestamps use RFC3339Nano so events arriving in the same second can still
// be ordered — provider streams burst hundreds of frames in a few milliseconds
// during a turn, and ordering is what this log is for.
func (l *Logger) LogProviderEvent(entry ProviderEventEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return l.logValue(entry)
}

// NewProviderEventLogger returns a provider-events Logger when the
// AGENT_OVERFLOW_DEBUG env var enables the "provider" topic, or
// (nil, nil) when logging is disabled. The log lands under
// <baseDir>/logs/provider-events-YYYY-MM-DD.ndjson with default rotation.
func NewProviderEventLogger(baseDir string) (*Logger, error) {
	if !providerEventLoggingEnabled(os.Getenv("AGENT_OVERFLOW_DEBUG")) {
		return nil, nil
	}

	path := filepath.Join(baseDir, "logs", fmt.Sprintf(
		"provider-events-%s.ndjson",
		time.Now().Format("2006-01-02"),
	))
	return NewLogger(path, 0)
}

func providerEventLoggingEnabled(value string) bool {
	for _, topic := range strings.Split(value, ",") {
		switch strings.TrimSpace(strings.ToLower(topic)) {
		case "all", "provider":
			return true
		}
	}
	return false
}

// providerEventFilePattern matches the active log file
// (provider-events-YYYY-MM-DD.ndjson) and its size-based rotations
// (.ndjson.1, .ndjson.2, .ndjson.3). Anchored so unrelated files in
// the same directory aren't touched.
var providerEventFilePattern = regexp.MustCompile(
	`^provider-events-(\d{4}-\d{2}-\d{2})\.ndjson(\.[123])?$`,
)

// PruneOlderThan removes provider-events log files under
// `<baseDir>/logs` whose modification time is strictly older than
// `cutoff`. The current day's active file (matching `now`'s date stub
// in the same local timezone NewProviderEventLogger uses to mint the
// filename) is never removed even if its mtime somehow predates the
// cutoff — that's the file a running logger has open for append, and
// removing it under SQLite-like semantics is fine on Linux but
// corrupts the active handle on Windows.
//
// `now` is passed in so callers driving deterministic clocks (the
// retention sweep injects `App.retentionNowFn`) can also pin the
// active-file guard. Use the same local-time formatter
// NewProviderEventLogger uses so the stub here matches the on-disk
// filename across UTC/local boundaries.
//
// mtime is the single source of truth for "is this old". The date in
// the filename only marks the day the file was created; rotated
// backups inherit the original creation date in their name but their
// mtimes track when the rotation happened.
//
// Returns the count of removed files and an aggregated error covering
// all failures so a single permission-denied doesn't hide a disk-full
// further down the directory. Non-existent log dir returns (0, nil).
func PruneOlderThan(baseDir string, now, cutoff time.Time) (int, error) {
	dir := filepath.Join(baseDir, "logs")
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
		match := providerEventFilePattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		// match[1] is the YYYY-MM-DD date stub; match[2] is the optional
		// .1/.2/.3 rotation suffix. Active file = today's date stub AND
		// no rotation suffix.
		if match[1] == todayStub && match[2] == "" {
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
