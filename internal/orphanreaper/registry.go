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

// Record is one recorded group as the registry decoded it. It identifies a
// group leader durably enough to re-find it after a crash despite PID
// reuse: a PID alone is ambiguous once recycled, but PID + Identity is not.
// Leader is the group's leader and the group the sweep signals: a leader
// leads its own group, so its pid is the pgid. Identity is the leader's,
// read when the group was recorded. A record an earlier version wrote has
// no Identity, only CreateUnix, gopsutil's start time in ms; the sweep
// compares that through ProcInfo.LegacyStartMillis. A record this version
// writes carries both, and CreateUnix only for earlier versions' readers:
// this version never compares it when Identity is set. Parent is the
// process that started the leader; a leader whose parent is no longer that
// process was orphaned, whichever process it was reparented to: init,
// launchd, or a subreaper such as systemd's user manager. 0 is a record
// written before the parent was recorded, which counts as orphaned only
// under init. Owner names what started the group.
type Record struct {
	Owner      string
	Leader     int
	Identity   *Identity
	CreateUnix int64
	Parent     int
}

// diskRecord is a record as the registry file holds it. pid and pgid are
// both written from Record.Leader, as earlier versions read both.
type diskRecord struct {
	Owner      string    `json:"owner"`
	PID        int       `json:"pid"`
	PGID       int       `json:"pgid"`
	Identity   *Identity `json:"identity,omitempty"`
	CreateUnix int64     `json:"create_unix,omitempty"`
	ParentPID  int       `json:"parent_pid,omitempty"`
}

// decode validates an entry read from the file. A record whose pid and
// pgid differ, whose leader is out of range, or that names no identity
// cannot be matched to a leader safely.
func (d diskRecord) decode() (Record, error) {
	if d.PID != d.PGID {
		return Record{}, fmt.Errorf("pid %d and pgid %d differ", d.PID, d.PGID)
	}
	if err := validLeader(d.PID); err != nil {
		return Record{}, err
	}
	if d.Identity != nil && d.Identity.Start == "" {
		return Record{}, fmt.Errorf("its identity %+v has no start", *d.Identity)
	}
	if d.Identity == nil && d.CreateUnix <= 0 {
		return Record{}, fmt.Errorf("it names no process identity")
	}
	return Record{Owner: d.Owner, Leader: d.PID, Identity: d.Identity, CreateUnix: d.CreateUnix, Parent: d.ParentPID}, nil
}

func (rec Record) encode() diskRecord {
	return diskRecord{Owner: rec.Owner, PID: rec.Leader, PGID: rec.Leader, Identity: rec.Identity, CreateUnix: rec.CreateUnix, ParentPID: rec.Parent}
}

// Registry is a small JSON file tracking live process groups for the
// startup sweep. Writes are atomic (temp + rename) and serialized by a
// mutex; the volume is a handful of entries, so a full rewrite per change
// keeps the format trivially correct.
type Registry struct {
	mu   sync.Mutex
	path string
}

func NewRegistry(path string) *Registry { return &Registry{path: path} }

// Add records owner's group as c captured its leader, replacing any record
// of the same group. A corrupt existing file is logged and reset rather
// than blocking new tracking — losing stale backstop entries is preferable
// to failing a session start.
func (r *Registry) Add(owner string, c Capture) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recs, err := r.loadLocked()
	if err != nil {
		log.Printf("orphanreaper: resetting unreadable registry on add: %v", err)
		recs = nil
	}
	out := make([]Record, 0, len(recs)+1)
	for _, e := range recs {
		if e.Leader != c.leader {
			out = append(out, e)
		}
	}
	identity := c.identity
	out = append(out, Record{Owner: owner, Leader: c.leader, Identity: &identity, CreateUnix: c.createUnix, Parent: c.parent})
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
		if e.Leader != pgid {
			out = append(out, e)
		}
	}
	return r.saveLocked(out)
}

// Load returns the current records (empty slice when the file is absent).
// An entry that fails validation is logged and left out; the next write
// drops it.
func (r *Registry) Load() ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

// replace makes recs the registry's whole content: the sweep's kept
// records. No records removes the file.
func (r *Registry) replace(recs []Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked(recs)
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
	var entries []diskRecord
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("orphanreaper: parse registry: %w", err)
	}
	recs := make([]Record, 0, len(entries))
	for _, entry := range entries {
		rec, err := entry.decode()
		if err != nil {
			log.Printf("orphanreaper: registry entry for group %d (%s) rejected: %v", entry.PGID, entry.Owner, err)
			continue
		}
		recs = append(recs, rec)
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
	sort.Slice(recs, func(i, j int) bool { return recs[i].Leader < recs[j].Leader })
	entries := make([]diskRecord, len(recs))
	for i, rec := range recs {
		entries[i] = rec.encode()
	}
	data, err := json.Marshal(entries)
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
