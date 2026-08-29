package governor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	stateFileName = "reservations.json"
	lockFileName  = "reservations.lock"
	defaultTTL    = 2 * time.Minute
	// Defaults are deliberately conservative for a visible multi-pane
	// workload. They are options so a machine-specific harness profile can
	// tighten them without changing this package.
	DefaultCeilingBytes        uint64 = 600 << 20
	// Keep enough unreserved memory for a large allocation burst between the
	// 100 ms watchdog samples. A sub-gigabyte floor can still let the host OOM
	// before the next exact process-tree measurement.
	DefaultAvailableFloorBytes uint64 = 2 << 30
)

type persisted struct {
	Leases []Lease `json:"leases"`
}

type Manager struct {
	dir            string
	defaultCeiling uint64
	floor          uint64
	ttl            time.Duration
	clock          func() time.Time
	memory         MemoryReader
	processes      ProcessReader
	local          sync.Mutex
}

func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("harness governor: resolve cache dir: %w", err)
	}
	return filepath.Join(base, "agent-overflow", "harness-governor"), nil
}

func New(opts Options) (*Manager, error) {
	dir := opts.Dir
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("harness governor: directory must be absolute: %q", dir)
	}
	defaultCeiling := opts.DefaultCeilingBytes
	if defaultCeiling == 0 {
		defaultCeiling = DefaultCeilingBytes
	}
	floor := opts.AvailableFloorBytes
	if floor == 0 {
		floor = DefaultAvailableFloorBytes
	}
	ttl := opts.LeaseTTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	mem := opts.Memory
	if mem == nil {
		mem = defaultMemory()
	}
	proc := opts.Processes
	if proc == nil {
		proc = defaultProcesses()
	}
	return &Manager{dir: filepath.Clean(dir), defaultCeiling: defaultCeiling, floor: floor, ttl: ttl, clock: clock, memory: mem, processes: proc}, nil
}

// Reserve atomically adds a capacity claim after pruning only expired leases
// whose recorded owner is verified dead. The available-memory sample is taken
// while the OS lock is held, so concurrent worktrees cannot pass the same
// capacity check.
func (m *Manager) Reserve(req Request) (Lease, error) {
	if err := validateRequest(req); err != nil {
		return Lease{}, err
	}
	req.Worktree = canonicalPath(req.Worktree)
	req.DataRoot = canonicalPath(req.DataRoot)
	m.local.Lock()
	defer m.local.Unlock()
	lock, err := m.lock()
	if err != nil {
		return Lease{}, err
	}
	defer lock.Close()
	now := m.clock()
	state, err := m.load()
	if err != nil {
		return Lease{}, err
	}
	state.Leases = m.pruneDead(state.Leases)
	for _, existing := range state.Leases {
		if existing.RunID == req.RunID && existing.Worktree == req.Worktree && existing.DataRoot == req.DataRoot {
			return Lease{}, fmt.Errorf("%w: run=%s worktree=%s root=%s", ErrAlreadyReserved, req.RunID, req.Worktree, req.DataRoot)
		}
	}
	available, err := m.memory.AvailableMemory()
	if err != nil {
		return Lease{}, fmt.Errorf("harness governor: sample available memory: %w", err)
	}
	ceiling := req.CeilingBytes
	if ceiling == 0 {
		ceiling = m.defaultCeiling
	}
	reserved := sumCeilings(state.Leases)
	if available < m.floor || available-m.floor < reserved || ceiling > available-m.floor-reserved {
		return Lease{}, fmt.Errorf("%w: available=%d floor=%d reserved=%d requested=%d", ErrCapacityExceeded, available, m.floor, reserved, ceiling)
	}
	birthID := req.OwnerBirthID
	owner, probeErr := m.processes.State(req.OwnerPID)
	if probeErr != nil {
		return Lease{}, fmt.Errorf("harness governor: identify owner %d: %w", req.OwnerPID, probeErr)
	}
	if !owner.Alive || owner.BirthID == "" {
		return Lease{}, fmt.Errorf("harness governor: owner %d is not live or has no birth identity", req.OwnerPID)
	}
	if birthID != "" && birthID != owner.BirthID {
		return Lease{}, fmt.Errorf("%w: owner %d birth identity changed", ErrLeaseOwnerMismatch, req.OwnerPID)
	}
	birthID = owner.BirthID
	id, err := randomID()
	if err != nil {
		return Lease{}, err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = m.ttl
	}
	lease := Lease{ID: id, RunID: req.RunID, Worktree: req.Worktree, DataRoot: req.DataRoot, OwnerPID: req.OwnerPID, OwnerBirthID: birthID, CeilingBytes: ceiling, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	state.Leases = append(state.Leases, lease)
	if err := m.save(state); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

func (m *Manager) Renew(lease Lease) (Lease, error) {
	if lease.ID == "" {
		return Lease{}, errors.New("harness governor: lease id is required")
	}
	m.local.Lock()
	defer m.local.Unlock()
	lock, err := m.lock()
	if err != nil {
		return Lease{}, err
	}
	defer lock.Close()
	state, err := m.load()
	if err != nil {
		return Lease{}, err
	}
	for i := range state.Leases {
		if state.Leases[i].ID != lease.ID {
			continue
		}
		if !sameOwner(state.Leases[i], lease) {
			return Lease{}, ErrLeaseOwnerMismatch
		}
		now := m.clock()
		state.Leases[i].ExpiresAt = now.Add(m.ttl)
		if err := m.save(state); err != nil {
			return Lease{}, err
		}
		return state.Leases[i], nil
	}
	return Lease{}, ErrLeaseNotFound
}

func (m *Manager) Release(lease Lease) error {
	if lease.ID == "" {
		return errors.New("harness governor: lease id is required")
	}
	m.local.Lock()
	defer m.local.Unlock()
	lock, err := m.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := m.load()
	if err != nil {
		return err
	}
	for i := range state.Leases {
		if state.Leases[i].ID != lease.ID {
			continue
		}
		if !sameOwner(state.Leases[i], lease) {
			return ErrLeaseOwnerMismatch
		}
		state.Leases = append(state.Leases[:i], state.Leases[i+1:]...)
		return m.save(state)
	}
	return ErrLeaseNotFound
}

// Snapshot reads leases and prunes verified-dead owners. Alive owners are
// never removed merely because their TTL elapsed. Removing a dead owner even
// before TTL expiry is what lets a crashed detached `up` release capacity.
func (m *Manager) Snapshot() (Snapshot, error) {
	m.local.Lock()
	defer m.local.Unlock()
	lock, err := m.lock()
	if err != nil {
		return Snapshot{}, err
	}
	defer lock.Close()
	state, err := m.load()
	if err != nil {
		return Snapshot{}, err
	}
	state.Leases = m.pruneDead(state.Leases)
	if err := m.save(state); err != nil {
		return Snapshot{}, err
	}
	available, err := m.memory.AvailableMemory()
	if err != nil {
		return Snapshot{}, fmt.Errorf("harness governor: sample available memory: %w", err)
	}
	sort.Slice(state.Leases, func(i, j int) bool { return state.Leases[i].ID < state.Leases[j].ID })
	return Snapshot{Leases: state.Leases, AvailableBytes: available, ReservedBytes: sumCeilings(state.Leases), AvailableFloorBytes: m.floor}, nil
}

func (m *Manager) pruneDead(leases []Lease) []Lease {
	kept := leases[:0]
	for _, lease := range leases {
		if lease.OwnerPID <= 0 {
			kept = append(kept, lease)
			continue
		}
		state, err := m.processes.State(lease.OwnerPID)
		// Any probe error is an unknown owner and must preserve the lease.
		// A live owner with a different birth marker is a reused PID, not the
		// original run. Drop it so the old reservation cannot strand capacity
		// or be mistaken for the new process. Alive owners remain reserved
		// after TTL when their birth marker still matches.
		if err != nil || (state.Alive && (lease.OwnerBirthID == "" || state.BirthID == lease.OwnerBirthID)) {
			kept = append(kept, lease)
			continue
		}
	}
	return kept
}

func (m *Manager) lock() (*fileLock, error) {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("harness governor: create state dir: %w", err)
	}
	return acquireLock(filepath.Join(m.dir, lockFileName))
}

func (m *Manager) load() (persisted, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, stateFileName))
	if os.IsNotExist(err) {
		return persisted{}, nil
	}
	if err != nil {
		return persisted{}, fmt.Errorf("harness governor: read state: %w", err)
	}
	var state persisted
	if err := json.Unmarshal(data, &state); err != nil {
		return persisted{}, fmt.Errorf("harness governor: decode state: %w", err)
	}
	return state, nil
}

func (m *Manager) save(state persisted) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("harness governor: encode state: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, ".reservations-*.tmp")
	if err != nil {
		return fmt.Errorf("harness governor: create state temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("harness governor: chmod state temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("harness governor: write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("harness governor: sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("harness governor: close state: %w", err)
	}
	if err := os.Rename(name, filepath.Join(m.dir, stateFileName)); err != nil {
		return fmt.Errorf("harness governor: install state: %w", err)
	}
	return nil
}

func validateRequest(req Request) error {
	for name, value := range map[string]string{"run id": req.RunID, "worktree": req.Worktree, "data root": req.DataRoot} {
		if value == "" {
			return fmt.Errorf("harness governor: %s is required", name)
		}
	}
	if req.OwnerPID <= 0 {
		return errors.New("harness governor: owner pid must be positive")
	}
	return nil
}
func sameOwner(a, b Lease) bool { return a.OwnerPID == b.OwnerPID && a.OwnerBirthID == b.OwnerBirthID }
func sumCeilings(leases []Lease) uint64 {
	var sum uint64
	for _, l := range leases {
		if ^uint64(0)-sum < l.CeilingBytes {
			return ^uint64(0)
		}
		sum += l.CeilingBytes
	}
	return sum
}
func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("harness governor: generate lease id: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
