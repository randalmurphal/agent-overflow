package supervise

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Layout is where a supervised install keeps its launch state, rooted at the
// app's own data directory (`<configRoot>/agent-overflow`, the directory that
// already holds agent-overflow.db).
//
// It lives beside the database on purpose: the snapshot is a copy of that
// database and must land on the same filesystem, so a restore is a rename-free
// local copy rather than a cross-device move that could tear.
//
//	<root>/runtime/service-state.json     the durable selection
//	<root>/runtime/versions/<v>/agent-overflow   immutable staged binaries
//	<root>/runtime/snapshot/              the SQLite triple, while a trial runs
//	<root>/runtime/restore-marker.json    written before a restore begins
type Layout struct{ root string }

// BinaryName is the file a staged version directory holds. One name, because
// the supervisor spawns it by path and never searches.
const BinaryName = "agent-overflow"

const (
	runtimeDirName = "runtime"
	versionsDir    = "versions"
	snapshotDir    = "snapshot"
	stateFileName  = "service-state.json"
	markerFileName = "restore-marker.json"
)

// NewLayout roots a layout at an app data directory.
func NewLayout(dataDir string) (Layout, error) {
	if strings.TrimSpace(dataDir) == "" {
		return Layout{}, errors.New("supervise: the data directory is required")
	}
	if !filepath.IsAbs(dataDir) {
		return Layout{}, fmt.Errorf("supervise: the data directory must be absolute, got %q", dataDir)
	}
	return Layout{root: filepath.Join(dataDir, runtimeDirName)}, nil
}

// Root is the runtime directory itself.
func (l Layout) Root() string { return l.root }

// StatePath is the durable selection file.
func (l Layout) StatePath() string { return filepath.Join(l.root, stateFileName) }

// MarkerPath is the restore marker.
func (l Layout) MarkerPath() string { return filepath.Join(l.root, markerFileName) }

// SnapshotDir is where the quiescent SQLite copy lives.
func (l Layout) SnapshotDir() string { return filepath.Join(l.root, snapshotDir) }

// VersionsDir holds one directory per staged version.
func (l Layout) VersionsDir() string { return filepath.Join(l.root, versionsDir) }

// VersionDir is one staged version's directory.
func (l Layout) VersionDir(version string) (string, error) {
	if err := ValidVersion(version); err != nil {
		return "", err
	}
	return filepath.Join(l.VersionsDir(), version), nil
}

// VersionBinary is the executable a staged version directory holds.
func (l Layout) VersionBinary(version string) (string, error) {
	dir, err := l.VersionDir(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, BinaryName), nil
}

// ValidVersion refuses a version string that could name something other than a
// child of the versions directory.
//
// The check is here rather than at each join because a version reaches this
// package from three directions — the state file on disk, a `service update`
// argument, and (next wave) a remote request — and only one of them is written
// by code in this tree. A name is a single path component, and that is the
// whole rule: no separator, no `.` or `..`, nothing a filesystem treats as a
// reference to somewhere else.
func ValidVersion(version string) error {
	if strings.TrimSpace(version) == "" {
		return errors.New("supervise: a version is required")
	}
	if version != strings.TrimSpace(version) {
		return fmt.Errorf("supervise: version %q has surrounding whitespace", version)
	}
	if version == "." || version == ".." {
		return fmt.Errorf("supervise: %q is not a version", version)
	}
	if strings.ContainsRune(version, '/') || strings.ContainsRune(version, '\\') ||
		strings.ContainsRune(version, filepath.Separator) {
		return fmt.Errorf("supervise: version %q contains a path separator", version)
	}
	if strings.ContainsAny(version, "\x00\n\r") {
		return fmt.Errorf("supervise: version %q contains a control character", version)
	}
	if version != filepath.Base(version) {
		return fmt.Errorf("supervise: version %q is not a single path component", version)
	}
	return nil
}

// DatabaseFiles are the three files one SQLite database is: the database, its
// write-ahead log, and its shared-memory index. They are snapshotted and
// restored as a SET, because a database restored without its WAL — or beside a
// WAL from a different moment — is not the database that was copied.
//
// The names are the ones internal/app opens (`app_startup.go`); they are
// restated rather than imported because this package must stay clear of the
// App graph, and a drift test pins them.
func DatabaseFiles() []string {
	return []string{"agent-overflow.db", "agent-overflow.db-wal", "agent-overflow.db-shm"}
}
