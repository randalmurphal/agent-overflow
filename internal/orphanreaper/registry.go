package orphanreaper

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Record identifies a provider process durably enough to re-find it after
// a crash despite PID reuse: a PID alone is ambiguous once recycled, but
// PID + start time is not. PGID is the group the sweep kills (== PID,
// since the provider is its own group leader). CreateUnix is the
// process start time in milliseconds (gopsutil's CreateTime); 0 means it
// couldn't be captured, in which case the sweep falls back to the
// orphaned-parent check alone.
type Record struct {
	UUID       string `json:"uuid"`
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	CreateUnix int64  `json:"create_unix"`
}

// Registry is a small JSON file tracking live provider processes for the
// startup sweep. Writes are atomic (temp + rename) and serialized by a
// mutex; the volume is a handful of entries, so a full rewrite per change
// keeps the format trivially correct.
type Registry struct {
	mu   sync.Mutex
	path string
}

func NewRegistry(path string) *Registry { return &Registry{path: path} }

// Add records (or replaces, keyed by pgid) a tracked process. A corrupt
// existing file is logged and reset rather than blocking new tracking —
// losing stale backstop entries is preferable to failing a session start.
func (r *Registry) Add(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := r.loadLocked()
	if err != nil {
		log.Printf("orphanreaper: resetting unreadable registry on add: %v", err)
		recs = nil
	}
	out := make([]Record, 0, len(recs)+1)
	for _, e := range recs {
		if e.PGID != rec.PGID {
			out = append(out, e)
		}
	}
	out = append(out, rec)
	return r.saveLocked(out)
}

// Remove drops the record for pgid (its session ended cleanly).
func (r *Registry) Remove(pgid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := r.loadLocked()
	if err != nil {
		log.Printf("orphanreaper: registry remove read failed (leaving entry for next sweep): %v", err)
		return nil
	}
	out := make([]Record, 0, len(recs))
	for _, e := range recs {
		if e.PGID != pgid {
			out = append(out, e)
		}
	}
	return r.saveLocked(out)
}

// Load returns the current records (empty slice when the file is absent).
func (r *Registry) Load() ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

// Clear removes the registry file entirely.
func (r *Registry) Clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked(nil)
}

func (r *Registry) loadLocked() ([]Record, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("orphanreaper: read registry: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("orphanreaper: parse registry: %w", err)
	}
	return recs, nil
}

func (r *Registry) saveLocked(recs []Record) error {
	if len(recs) == 0 {
		if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("orphanreaper: clear registry: %w", err)
		}
		return nil
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].PGID < recs[j].PGID })
	data, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("orphanreaper: marshal registry: %w", err)
	}
	// Atomic write: temp + fsync + rename, matching the repo's durable-JSON
	// convention (internal/wsldistro, settings). The fsync is load-bearing
	// here — the registry's whole job is to survive a crash / power loss, so
	// the bytes must reach disk before the rename publishes them.
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, "orphan-registry-*.tmp")
	if err != nil {
		return fmt.Errorf("orphanreaper: create registry tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("orphanreaper: chmod registry tempfile: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("orphanreaper: write registry tempfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("orphanreaper: sync registry tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("orphanreaper: close registry tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		cleanup()
		return fmt.Errorf("orphanreaper: rename registry: %w", err)
	}
	return nil
}
