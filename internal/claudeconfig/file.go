package claudeconfig

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
)

// writeIfUnchanged is atomicfile.WriteIfUnchanged; a test replaces it to
// report a committed write whose directory sync failed.
var writeIfUnchanged = atomicfile.WriteIfUnchanged

// ErrConcurrentWrite reports that another writer kept changing the file
// between our read and our write across every attempt.
var ErrConcurrentWrite = errors.New("claudeconfig: concurrent write detected")

// Store reads and writes ~/.claude.json, plus the read-only slices of
// the sibling ~/.claude directory (settings.json, plugin manifests)
// that MCP enumeration needs. Tests inject a temp path via the
// constructor — the package never reads $HOME directly.
type Store struct {
	path string
	// home is the ~/.claude directory derived from path's parent —
	// claude.json lives at $HOME/.claude.json, next to $HOME/.claude/.
	home string
}

// New returns a Store bound to the given file path. A non-existent
// file is treated as an empty config on Load; Save creates the file
// (and its parent directory) on first write.
func New(path string) *Store {
	return &Store{
		path: path,
		home: filepath.Join(filepath.Dir(path), ".claude"),
	}
}

// PathForHome returns `<home>/.claude.json`.
//
// The home is INJECTED, never resolved here (the same rule
// internal/sessionimport states): this package writes, and the app layer
// owns the one seam that decides whether a write lands in the user's real
// provider home or in an isolated boot's pinned one. There is deliberately
// no $HOME-reading variant for a caller to reach for by accident.
func PathForHome(home string) string {
	return filepath.Join(home, ".claude.json")
}

// snapshot is the in-memory view used for read+mutate+write cycles.
// stat captures the file metadata at read time so save can detect a
// concurrent write before clobbering. raw is nil if the file was
// missing at read time.
type snapshot struct {
	raw  *orderedJSON
	stat os.FileInfo
}

func (s *Store) load() (*snapshot, error) {
	data, statBefore, err := atomicfile.ReadWithStat(s.path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &snapshot{raw: newOrderedJSON()}, nil
	}
	obj, err := parseOrderedJSON(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return &snapshot{raw: obj, stat: statBefore}, nil
}

// modifyAttempts bounds the read-mutate-write retry loop. Claude Code
// rewrites ~/.claude.json on a sub-second cadence (metrics,
// lastSessionId, fps), so losing a race is routine rather than
// exceptional and a single retry is thin cover. Each attempt re-reads
// first, so fn always runs against current content and retrying is
// idempotent. Same reasoning and same bound as atomicfile.ReadWithStat.
const modifyAttempts = 5

// modify runs fn against the parsed snapshot, then atomically writes
// the result back to disk. If the file changed between our read and our
// write attempt (atomicfile.WriteIfUnchanged), the call retries. After
// modifyAttempts it returns ErrConcurrentWrite.
func (s *Store) modify(fn func(*orderedJSON) error) error {
	_, err := s.modifyReporting(func(root *orderedJSON) (bool, error) {
		return true, fn(root)
	})
	return err
}

// modifyReporting is modify for mutations that may be no-ops. fn
// reports whether it changed anything; a false return skips the write
// entirely (no rename, no mtime bump) and reports changed=false to the
// caller. Claude Code watches ~/.claude.json's mtime, so a no-op write
// is not free.
func (s *Store) modifyReporting(fn func(*orderedJSON) (bool, error)) (bool, error) {
	for attempt := 0; attempt < modifyAttempts; attempt++ {
		snap, err := s.load()
		if err != nil {
			return false, err
		}
		changed, err := fn(snap.raw)
		if err != nil {
			return false, err
		}
		if !changed {
			return false, nil
		}
		out, err := snap.raw.marshalIndented()
		if err != nil {
			return false, fmt.Errorf("marshal: %w", err)
		}
		committed, err := writeIfUnchanged(s.path, out, snap.stat)
		if committed {
			if err != nil {
				// The new config is in place; only its directory entry may
				// not survive a power loss. The caller goes on as written.
				log.Printf("claudeconfig: %v", err)
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, ErrConcurrentWrite
}
