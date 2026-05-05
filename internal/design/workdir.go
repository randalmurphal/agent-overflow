package design

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// SnapshotRetentionLimit caps how many auto-snapshots survive GC per
// thread. Manual / labeled snapshots and any snapshot with descendants
// are kept regardless. See PruneSnapshots.
const SnapshotRetentionLimit = 20

const (
	subdirMain      = "main"
	subdirOptions   = "options"
	subdirSnapshots = "snapshots"
	indexHTMLName   = "index.html"
)

// SnapshotSpec captures the metadata for a new snapshot.
type SnapshotSpec struct {
	Label string
	Auto  bool
	// ParentSnapshotID, if non-empty, records the lineage. Auto snapshots
	// taken at user-turn-start record the previous snapshot id; manual
	// snapshots leave it blank.
	ParentSnapshotID string
}

// WorkDirManager owns per-thread design working directories.
type WorkDirManager struct {
	baseDir string
	store   *store.Store
}

// NewWorkDirManager constructs a manager rooted at baseDir.
func NewWorkDirManager(baseDir string, st *store.Store) *WorkDirManager {
	return &WorkDirManager{
		baseDir: strings.TrimSpace(baseDir),
		store:   st,
	}
}

// BaseDir returns the root directory the file server hosts. The HTTP
// path /design/{threadId}/{main|options|snapshots}/... maps directly
// into this tree, so the base dir is the StripPrefix target.
func (m *WorkDirManager) BaseDir() string { return m.baseDir }

// EnsureThread creates the per-thread directory layout if it doesn't
// exist already. Idempotent.
func (m *WorkDirManager) EnsureThread(threadID string) error {
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return err
	}
	for _, sub := range []string{subdirMain, subdirOptions, subdirSnapshots} {
		if err := os.MkdirAll(filepath.Join(threadDir, sub), 0o755); err != nil {
			return fmt.Errorf("design: ensure %s/%s: %w", threadID, sub, err)
		}
	}
	// Seed an empty index.html so the file server has something to load
	// before the agent's first edit. The agent overwrites it via Write
	// or Edit on its first turn.
	indexPath := filepath.Join(threadDir, subdirMain, indexHTMLName)
	if _, err := os.Stat(indexPath); errors.Is(err, fs.ErrNotExist) {
		if err := writeBytes(indexPath, []byte(emptyDesignHTML)); err != nil {
			return err
		}
	}
	return nil
}

// MainPath returns the absolute path to the main working directory for
// a thread.
func (m *WorkDirManager) MainPath(threadID string) (string, error) {
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return "", err
	}
	return filepath.Join(threadDir, subdirMain), nil
}

// ThreadDir returns the per-thread root directory — the parent of the
// `main/`, `options/`, and `snapshots/` subdirs the design system
// prompt teaches the agent to operate inside. Used by app_session.go
// to set the provider subprocess's CWD so Read/Edit/Write resolve
// paths relative to the workdir, not the project root.
func (m *WorkDirManager) ThreadDir(threadID string) (string, error) {
	return m.threadDir(threadID)
}

// ListOptions returns the option ids inside `options/{setId}/` for a
// thread, sorted lexically. Returns an empty slice (no error) when the
// set directory does not exist — the watcher fires options-update on
// the first write into a set, and the frontend racing the agent's
// initial mkdir is expected.
func (m *WorkDirManager) ListOptions(threadID, setID string) ([]string, error) {
	setID = sanitizeSegment(setID)
	if setID == "" {
		return nil, fmt.Errorf("design: options set id required")
	}
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(threadDir, subdirOptions, setID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("design: list options for %s: %w", setID, err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "" || name == "." || name == ".." {
			continue
		}
		out = append(out, name)
	}
	// os.ReadDir returns lexically sorted, but reaffirm by trimming
	// hidden entries (anything starting with `.`) — agents shouldn't
	// emit dotfiles into options/, but defense in depth.
	cleaned := out[:0]
	for _, name := range out {
		if strings.HasPrefix(name, ".") {
			continue
		}
		cleaned = append(cleaned, name)
	}
	return cleaned, nil
}

// OptionsPath returns the directory path for an option set, creating
// parents as needed. The agent writes its component fragments into
// `options/{setId}/{optionId}/`; the file watcher fires the
// design:options-update event when files appear there.
func (m *WorkDirManager) OptionsPath(threadID, setID, optionID string) (string, error) {
	setID = sanitizeSegment(setID)
	optionID = sanitizeSegment(optionID)
	if setID == "" || optionID == "" {
		return "", fmt.Errorf("design: options set/option id required")
	}
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return "", err
	}
	full := filepath.Join(threadDir, subdirOptions, setID, optionID)
	if !pathWithinBase(threadDir, full) {
		return "", fmt.Errorf("design: options path escapes thread dir")
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return "", fmt.Errorf("design: ensure options dir: %w", err)
	}
	return full, nil
}

// Snapshot copies the current main/ directory into snapshots/{id}/ and
// records the metadata row. Returns the persisted snapshot record.
func (m *WorkDirManager) Snapshot(threadID string, spec SnapshotSpec) (Snapshot, error) {
	if m.store == nil {
		return Snapshot{}, fmt.Errorf("design: snapshot store unavailable")
	}
	mainPath, err := m.MainPath(threadID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := m.EnsureThread(threadID); err != nil {
		return Snapshot{}, err
	}
	snapshotID := uuid.NewString()
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return Snapshot{}, err
	}
	dest := filepath.Join(threadDir, subdirSnapshots, snapshotID)
	if err := copyTreeAtomic(mainPath, dest); err != nil {
		return Snapshot{}, fmt.Errorf("design: snapshot copy: %w", err)
	}
	snap := Snapshot{
		ID:               snapshotID,
		ThreadID:         threadID,
		Label:            strings.TrimSpace(spec.Label),
		DirPath:          dest,
		ParentSnapshotID: strings.TrimSpace(spec.ParentSnapshotID),
		Auto:             spec.Auto,
		CreatedAt:        time.Now().UnixMilli(),
	}
	if err := m.store.InsertDesignSnapshot(snap); err != nil {
		_ = os.RemoveAll(dest)
		return Snapshot{}, err
	}
	return snap, nil
}

// RestoreFromSnapshot replaces main/ with a copy of the given
// snapshot's contents. Atomic via tmp dir then RemoveAll+Rename.
func (m *WorkDirManager) RestoreFromSnapshot(threadID, snapshotID string) error {
	if m.store == nil {
		return fmt.Errorf("design: snapshot store unavailable")
	}
	snap, err := m.store.GetDesignSnapshot(threadID, snapshotID)
	if err != nil {
		return err
	}
	mainPath, err := m.MainPath(threadID)
	if err != nil {
		return err
	}
	if err := m.EnsureThread(threadID); err != nil {
		return err
	}
	tmp := mainPath + ".restore-" + uuid.NewString()
	if err := copyTreeAtomic(snap.DirPath, tmp); err != nil {
		return fmt.Errorf("design: restore copy: %w", err)
	}
	if err := os.RemoveAll(mainPath); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("design: restore clear main: %w", err)
	}
	if err := os.Rename(tmp, mainPath); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("design: restore swap main: %w", err)
	}
	return nil
}

// Wipe removes the per-thread directory tree. Used by thread deletion.
func (m *WorkDirManager) Wipe(threadID string) error {
	threadDir, err := m.threadDir(threadID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(threadDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("design: wipe thread %s: %w", threadID, err)
	}
	return nil
}

// PruneSnapshots GCs older auto-snapshots. Manual (labeled-or-non-auto)
// snapshots are kept regardless; any snapshot with descendants is kept
// to preserve branch lineage. Returns the IDs of the snapshots that
// were pruned.
func (m *WorkDirManager) PruneSnapshots(threadID string) ([]string, error) {
	if m.store == nil {
		return nil, fmt.Errorf("design: snapshot store unavailable")
	}
	all, err := m.store.ListDesignSnapshots(threadID)
	if err != nil {
		return nil, err
	}
	// Walk newest-first. Keep all manual snapshots (Auto == false), all
	// snapshots with children, and the most-recent SnapshotRetentionLimit
	// auto snapshots. Prune the rest.
	autoKept := 0
	var pruned []string
	for _, snap := range all {
		if !snap.Auto {
			continue
		}
		hasChildren, err := m.store.HasDesignSnapshotChildren(snap.ID)
		if err != nil {
			return pruned, err
		}
		if hasChildren {
			continue
		}
		if autoKept < SnapshotRetentionLimit {
			autoKept++
			continue
		}
		// Drop.
		if err := os.RemoveAll(snap.DirPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return pruned, fmt.Errorf("design: prune snapshot %s: %w", snap.ID, err)
		}
		if err := m.store.DeleteDesignSnapshot(threadID, snap.ID); err != nil {
			return pruned, err
		}
		pruned = append(pruned, snap.ID)
	}
	return pruned, nil
}

func (m *WorkDirManager) threadDir(threadID string) (string, error) {
	threadID = sanitizeSegment(threadID)
	if threadID == "" {
		return "", fmt.Errorf("design: thread id required")
	}
	base := strings.TrimSpace(m.baseDir)
	if base == "" {
		return "", fmt.Errorf("design: base directory required")
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("design: resolve base directory: %w", err)
	}
	full, err := filepath.Abs(filepath.Join(absBase, threadID))
	if err != nil {
		return "", fmt.Errorf("design: resolve thread directory: %w", err)
	}
	if !pathWithinBase(absBase, full) {
		return "", fmt.Errorf("design: thread path escapes base directory")
	}
	return full, nil
}

// sanitizeSegment trims whitespace and rejects path-traversal-flavored
// segments. Callers pass thread / set / option ids that the user does
// not control directly, but defense in depth keeps a future caller from
// shipping a path that breaks out.
func sanitizeSegment(seg string) string {
	seg = strings.TrimSpace(seg)
	if seg == "" || seg == "." || seg == ".." {
		return ""
	}
	if strings.ContainsAny(seg, `/\`) {
		return ""
	}
	return seg
}

func pathWithinBase(basePath, targetPath string) bool {
	basePath = filepath.Clean(basePath)
	targetPath = filepath.Clean(targetPath)
	return targetPath == basePath || strings.HasPrefix(targetPath, basePath+string(os.PathSeparator))
}

// writeBytes is the atomic file write pattern: tmp file + rename.
// Pulled out of the deleted artifacts.go so workdir.go and screenshot.go
// share the implementation.
func writeBytes(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("design: create dir for %s: %w", path, err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("design: create temp file %s: %w", tmp, err)
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("design: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("design: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("design: close %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("design: rename %s: %w", path, err)
	}
	return nil
}

// copyTreeAtomic copies src to dst recursively. Existing dst (if any)
// is replaced atomically: the copy lands in a tmp dir alongside, then
// rename swaps it in.
func copyTreeAtomic(src, dst string) error {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("design: ensure parent for %s: %w", dst, err)
	}
	tmp := dst + ".tmp-" + uuid.NewString()
	if err := copyTree(src, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	// If dst exists, swap via rename → old; remove old after.
	if _, err := os.Stat(dst); err == nil {
		old := dst + ".old-" + uuid.NewString()
		if err := os.Rename(dst, old); err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("design: stash old %s: %w", dst, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			// Try to roll back; either way report the original error.
			_ = os.Rename(old, dst)
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("design: swap %s: %w", dst, err)
		}
		_ = os.RemoveAll(old)
		return nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("design: rename %s: %w", dst, err)
	}
	return nil
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("design: mkdir %s: %w", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Empty source — leaving dst as an empty dir is fine.
			return nil
		}
		return fmt.Errorf("design: read %s: %w", src, err)
	}
	for _, entry := range entries {
		s := filepath.Join(src, entry.Name())
		d := filepath.Join(dst, entry.Name())
		switch {
		case entry.IsDir():
			if err := copyTree(s, d); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			if err := copyFile(s, d); err != nil {
				return err
			}
		default:
			// Skip symlinks / specials. The agent only writes regular
			// files via Edit/Write, so this branch shouldn't fire in
			// practice.
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("design: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("design: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("design: copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("design: sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("design: close %s: %w", dst, err)
	}
	return nil
}

const emptyDesignHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Design preview</title>
<style>
  html, body { margin: 0; padding: 0; height: 100%; }
  body { display: grid; place-items: center; font-family: system-ui, sans-serif; color: #94a3b8; background: #0b0b0c; }
</style>
</head>
<body>
<p>Waiting for the agent to draft this design.</p>
</body>
</html>
`
