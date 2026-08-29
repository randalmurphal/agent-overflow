package governor

import (
	"errors"
	"time"
)

var (
	ErrUnsupported        = errors.New("harness governor: host measurement is unsupported on this platform")
	ErrAlreadyReserved    = errors.New("harness governor: run is already reserved")
	ErrCapacityExceeded   = errors.New("harness governor: host memory reservation exceeds available capacity")
	ErrLeaseNotFound      = errors.New("harness governor: lease not found")
	ErrLeaseOwnerMismatch = errors.New("harness governor: lease owner identity does not match")
)

const ReasonSafetyCeiling = "safety-ceiling"

// MemoryReader returns the host memory that can safely be allocated. It must
// be the OS's available-memory value, not total minus process RSS.
type MemoryReader interface{ AvailableMemory() (uint64, error) }

// ProcessState is an owner liveness and birth-identity observation. BirthID
// distinguishes a reused PID from the process that made a reservation.
type ProcessState struct {
	Alive   bool
	BirthID string
}

type ProcessReader interface {
	State(pid int) (ProcessState, error)
}

// Request identifies one harness run. All three keys are required so a stale
// record cannot accidentally reserve a different checkout's run.
type Request struct {
	RunID        string
	Worktree     string
	DataRoot     string
	OwnerPID     int
	OwnerBirthID string
	CeilingBytes uint64
	TTL          time.Duration
}

// Lease is the caller's capability to renew and release a reservation.
type Lease struct {
	ID           string    `json:"id"`
	RunID        string    `json:"runId"`
	Worktree     string    `json:"worktree"`
	DataRoot     string    `json:"dataRoot"`
	OwnerPID     int       `json:"ownerPid"`
	OwnerBirthID string    `json:"ownerBirthId"`
	CeilingBytes uint64    `json:"ceilingBytes"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type Options struct {
	// Dir is a host-global directory, never a worktree directory. Empty uses
	// the user's cache directory.
	Dir                 string
	DefaultCeilingBytes uint64
	AvailableFloorBytes uint64
	LeaseTTL            time.Duration
	Clock               func() time.Time
	Memory              MemoryReader
	Processes           ProcessReader
}

// Snapshot is a read-only view of all active reservations.
type Snapshot struct {
	Leases              []Lease `json:"leases"`
	AvailableBytes      uint64  `json:"availableBytes"`
	ReservedBytes       uint64  `json:"reservedBytes"`
	AvailableFloorBytes uint64  `json:"availableFloorBytes"`
}

// Event is emitted by Monitor when the observed owner exceeds its lease.
type Event struct {
	RunID        string    `json:"runId"`
	Worktree     string    `json:"worktree"`
	DataRoot     string    `json:"dataRoot"`
	Reason       string    `json:"reason"`
	RSSBytes     uint64    `json:"rssBytes"`
	CeilingBytes uint64    `json:"ceilingBytes"`
	At           time.Time `json:"at"`
}

type ProcessMemoryReader interface{ RSS(pid int) (uint64, error) }
