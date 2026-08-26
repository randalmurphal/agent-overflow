// Package instanceinfo is the discovery layer for running harness and
// soak instances: the instance id derived from a data root, the
// registry row every isolated boot publishes under the user cache dir,
// and the liveness probe that lets a reader tell a live row from the
// leftovers of a killed process.
//
// It deliberately holds no token. A registry row answers "what is
// running and where does its data live"; attaching reads the token from
// <dataDir>/harness-instance.json, which is 0600 and inside the data
// root the reader must already be able to open. So a row planted by a
// foreign process can at worst point a reader at a path it cannot read.
package instanceinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/atomicfile"
)

// Mode names what a row describes. All three shapes boot through
// prepareHarness and differ only in shell and preset:
//
//   - ModeHarness is an isolated mocked instance nobody is driving yet —
//     the `--harness` browser shell AND the Windows launcher's harness
//     profile (`--soak` with no autopilot), which are the same instance
//     behind two bootstrap contracts.
//   - ModeSoak is that instance with the soak autopilot armed
//     (docs/architecture/soak-rig.md): seeded threads and a live turn
//     streaming forever.
//
// So "soak" on a row means the autopilot is running, never merely that
// the backend was spawned by the launcher.
type Mode string

const (
	ModeHarness Mode = "harness"
	ModeSoak    Mode = "soak"
)

// InstanceFileName is the per-instance file inside the data DIR (not
// the registry) that carries the full bootstrap payload, token
// included. Named here so writer and readers share one spelling.
const InstanceFileName = "harness-instance.json"

// idHexLen is how much of the data-root digest an instance id keeps.
// Eight hex chars is short enough to read in a window title and type on
// a command line, and 32 bits of digest over a filesystem path is far
// past what a single developer's set of checkouts can collide in.
const idHexLen = 8

// Identity is the block that describes WHICH instance a file is about.
// It is embedded by both discovery files — the registry row here and
// the data-dir instance file assembled in package main — so the two can
// never disagree about the field set.
type Identity struct {
	ID     string `json:"id"`
	Mode   Mode   `json:"mode"`
	Window bool   `json:"window"`
	// Worktree is the process's working directory at boot: the checkout
	// an instance belongs to, which is what "the instance for this
	// worktree" resolves against.
	Worktree string `json:"worktree"`
	// StartedAt is RFC3339. A string rather than a time.Time so a reader
	// that only prints it never has to care about parse failures.
	StartedAt string `json:"startedAt"`
	// LauncherPid is the Windows launcher process hosting this instance's
	// window, or 0 when nobody hosts one (a headless harness, a native
	// --window boot). It lives in the identity block because it is a fact
	// about the LAUNCH, and because a teardown reads it from whichever of
	// the two files it could open.
	//
	// It exists because the launcher deliberately outlives its backend
	// child — that is what preserves the evidence of a crash — so a
	// deliberate `ao-harness down` has to close the window itself, and
	// nothing else in the tree knows which Windows process it is.
	LauncherPid int `json:"launcherPid,omitempty"`
}

// Row is one registry entry: everything a discovery reader needs to
// pick an instance and reach it, minus the token.
type Row struct {
	Identity
	PID      int    `json:"pid"`
	Port     int    `json:"port"`
	DataRoot string `json:"dataRoot"`
	DataDir  string `json:"dataDir"`
	Version  string `json:"version"`
}

// Instance is a Row as a reader sees it: the parsed row, the file it
// came from, and whether its process is gone.
type Instance struct {
	Row
	// Path is the registry file this row was read from, so a reader that
	// prunes stale entries does not have to rebuild the name.
	Path string `json:"path"`
	// Stale means the recorded pid is not running. Readers decide what
	// to do about it; this package never deletes on their behalf.
	Stale bool `json:"stale"`
}

// ID derives the instance id from a data root: the first 8 hex chars of
// the SHA-256 of its canonical absolute path. Stable across restarts of
// the same instance, distinct per data root, and computable by any tool
// that knows only the path — which is what lets a CLI find the right
// registry row without having parsed anyone's stdout.
//
// Canonicalization resolves symlinks when the path exists, because two
// spellings of one directory must not produce two ids; a path that does
// not exist yet (the first boot on a fresh data root) falls back to
// Clean, and the two answers agree as long as no component is a link —
// which prepareHarness refuses for the roots it owns anyway.
func ID(dataRoot string) string {
	sum := sha256.Sum256([]byte(canonical(dataRoot)))
	return hex.EncodeToString(sum[:])[:idHexLen]
}

func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// RegistryDir is where rows live: <user cache dir>/agent-overflow/
// harness-instances. The cache dir rather than the config dir because
// these files are pure derived discovery state — losing them costs a
// listing, never data — and because the config dir is exactly the tree
// an isolated boot refuses to touch.
func RegistryDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("instanceinfo: resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "agent-overflow", "harness-instances"), nil
}

// Write publishes a row into the default registry directory.
func Write(row Row) error {
	dir, err := RegistryDir()
	if err != nil {
		return err
	}
	return WriteIn(dir, row)
}

// WriteIn is Write against an explicit registry directory (tests, and
// `ao-harness --registry-dir`).
func WriteIn(dir string, row Row) error {
	path, err := rowPath(dir, row.ID)
	if err != nil {
		return err
	}
	// atomicfile creates the directory 0700 and the file 0600, which is
	// what this registry wants: a row names another user's paths, and on
	// a shared machine that is nobody else's business.
	if err := atomicfile.WriteJSON(path, row); err != nil {
		return fmt.Errorf("instanceinfo: write %s: %w", path, err)
	}
	return nil
}

// Remove deletes a row from the default registry directory. A row that
// is already gone is not an error — graceful shutdown and a reader's
// stale prune both race for the same file, and both are right.
func Remove(id string) error {
	dir, err := RegistryDir()
	if err != nil {
		return err
	}
	return RemoveIn(dir, id)
}

// RemoveIn is Remove against an explicit registry directory.
func RemoveIn(dir, id string) error {
	path, err := rowPath(dir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("instanceinfo: remove %s: %w", path, err)
	}
	return nil
}

// List reads every row in the default registry directory, newest first,
// each marked with whether its process is still alive.
func List() ([]Instance, error) {
	dir, err := RegistryDir()
	if err != nil {
		return nil, err
	}
	return ListIn(dir, nil)
}

// ListIn is List against an explicit registry directory and liveness
// probe. A nil probe uses the real one; tests supply their own so the
// stale path is exercised without spawning anything.
//
// An unreadable or malformed row is skipped rather than failing the
// whole listing: the registry is discovery state written by other
// processes, and one corrupt file must not hide the healthy instances.
func ListIn(dir string, alive func(pid int) bool) ([]Instance, error) {
	if alive == nil {
		alive = ProcessAlive
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("instanceinfo: read registry %s: %w", dir, err)
	}
	out := make([]Instance, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var row Row
		if err := json.Unmarshal(data, &row); err != nil || row.ID == "" {
			continue
		}
		out = append(out, Instance{Row: row, Path: path, Stale: !alive(row.PID)})
	}
	// Newest first: a human listing instances is nearly always after the
	// one they just started.
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}

// rowPath composes the registry file for an id, refusing anything that
// is not an id. The id is the only caller-supplied component of this
// path, and a row is written with the process's own privileges — a
// value carrying separators or dots must never be able to aim the write
// (or the remove) outside the registry.
func rowPath(dir, id string) (string, error) {
	if dir == "" {
		return "", errors.New("instanceinfo: empty registry dir")
	}
	if !ValidID(id) {
		return "", fmt.Errorf("instanceinfo: %q is not an instance id (want %d lowercase hex chars)", id, idHexLen)
	}
	return filepath.Join(dir, id+".json"), nil
}

// ValidID reports whether a string has the shape ID returns: exactly
// idHexLen lowercase hex chars. Exported because the shape is a fact
// about this package's ids, and a caller that reimplemented it (to pick
// a better error message, say) would carry its own copy of the length.
func ValidID(id string) bool {
	if len(id) != idHexLen {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
