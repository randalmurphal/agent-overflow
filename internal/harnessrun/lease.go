package harnessrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harness/instanceinfo"
)

const DefaultLeaseStaleAfter = 24 * time.Hour

// LeaseOptions controls stale-owner recovery. A stale lease is removed only
// when its timestamp is older than StaleAfter. Malformed leases are refused.
type LeaseOptions struct{ StaleAfter time.Duration }

// Lease is an ownership token. Release checks the token so a late cleanup
// cannot remove a newer owner's lease.
type Lease struct {
	mu     sync.Mutex
	root   string
	record LeaseRecord
}

func leasePath(root string) string { return filepath.Join(root, LeaseFileName) }

// LeasePath returns the exclusive lease path for a data root.
func LeasePath(root string) string { return leasePath(root) }

// AcquireLease takes the data-root lease using O_EXCL. Existing stale leases
// are removed and retried. The operation is safe against concurrent callers:
// only one O_EXCL create can win after stale cleanup.
func AcquireLease(root, runID string, now time.Time) (*Lease, error) {
	return AcquireLeaseWithOptions(root, runID, now, LeaseOptions{StaleAfter: DefaultLeaseStaleAfter})
}

func AcquireLeaseWithOptions(root, runID string, now time.Time, opts LeaseOptions) (*Lease, error) {
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) || strings.TrimSpace(runID) == "" {
		return nil, errors.New("lease: invalid root or run id")
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultLeaseStaleAfter
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("lease: create root: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	pid := os.Getpid()
	identity, err := instanceinfo.CaptureProcessIdentity(pid)
	if err != nil {
		return nil, fmt.Errorf("lease: capture owner identity: %w", err)
	}
	record := LeaseRecord{Token: token, RunID: runID, PID: pid, OwnerBirthID: leaseBirthID(identity), AcquiredAt: now.UTC(), HeartbeatAt: now.UTC()}
	path := leasePath(root)
	for attempt := 0; attempt < 2; attempt++ {
		f, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			data, marshalErr := json.MarshalIndent(record, "", "  ")
			if marshalErr == nil {
				_, marshalErr = f.Write(data)
			}
			if marshalErr == nil {
				marshalErr = f.Sync()
			}
			if closeErr := f.Close(); marshalErr == nil {
				marshalErr = closeErr
			}
			if marshalErr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("lease: publish: %w", marshalErr)
			}
			if err := syncDir(root); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("lease: durable publish: %w", err)
			}
			return &Lease{root: root, record: record}, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("lease: create: %w", createErr)
		}
		old, readErr := readLease(path)
		// O_EXCL makes the lease name visible before the creator has finished
		// publishing its JSON. A concurrent acquirer may observe that short
		// window as EOF. Retry only that transient state. A malformed non-empty
		// lease remains a hard error.
		for retry := 0; errors.Is(readErr, io.EOF) && retry < 20; retry++ {
			time.Sleep(time.Millisecond)
			old, readErr = readLease(path)
		}
		if readErr != nil {
			return nil, readErr
		}
		if now.UTC().Sub(old.heartbeat()) <= opts.StaleAfter || leaseOwnerAlive(old) {
			return nil, fmt.Errorf("data root is leased by run %q (pid %d)", old.RunID, old.PID)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("lease: remove stale lease: %w", err)
		}
	}
	return nil, errors.New("lease: concurrent owner won stale-lease race")
}

func readLease(path string) (LeaseRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LeaseRecord{}, fmt.Errorf("lease: read existing lease: %w", err)
	}
	var record LeaseRecord
	if err := decodeStrict(data, &record); err != nil {
		return LeaseRecord{}, fmt.Errorf("lease: malformed existing lease: %w", err)
	}
	if record.Token == "" || record.RunID == "" || record.AcquiredAt.IsZero() {
		return LeaseRecord{}, errors.New("lease: malformed existing lease: missing identity")
	}
	return record, nil
}

func leaseBirthID(identity instanceinfo.ProcessIdentity) string {
	if identity.StartTime != "" {
		return identity.StartTime
	}
	return identity.Executable
}

func leaseOwnerAlive(record LeaseRecord) bool {
	if record.PID <= 0 {
		return false
	}
	// Legacy leases predate owner identity. Their timestamp is the only
	// evidence available, so retain the historical stale-age behavior.
	if record.OwnerBirthID == "" {
		return false
	}
	identity, err := instanceinfo.CaptureProcessIdentity(record.PID)
	if err != nil {
		return true
	}
	return leaseBirthID(identity) == record.OwnerBirthID
}

func (r LeaseRecord) heartbeat() time.Time {
	if !r.HeartbeatAt.IsZero() {
		return r.HeartbeatAt
	}
	return r.AcquiredAt
}

func (l *Lease) Record() LeaseRecord {
	if l == nil {
		return LeaseRecord{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.record
}

// Heartbeat refreshes the lease timestamp after proving that this token still
// owns the root. Long-running runs can therefore remain live without relying
// on the stale-age fallback.
func (l *Lease) Heartbeat(now time.Time) error {
	if l == nil {
		return errors.New("lease: nil lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, err := readLease(leasePath(l.root))
	if err != nil {
		return err
	}
	if current.Token != l.record.Token {
		return errors.New("lease: refusing to refresh a different owner's lease")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	current.HeartbeatAt = now
	if err := atomicfile.WriteJSON(leasePath(l.root), current); err != nil {
		return fmt.Errorf("lease: heartbeat: %w", err)
	}
	if err := syncDir(l.root); err != nil {
		return fmt.Errorf("lease: durable heartbeat: %w", err)
	}
	l.record.HeartbeatAt = now
	return nil
}

// Verify proves that this lease token still owns the root. It does not
// refresh the timestamp or otherwise change durable state. Callers use this
// immediately before a destructive operation so a late supervisor cannot
// act after another owner has replaced the lease.
func (l *Lease) Verify() error {
	if l == nil {
		return errors.New("lease: nil lease")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, err := readLease(leasePath(l.root))
	if err != nil {
		return err
	}
	if current.Token != l.record.Token || current.RunID != l.record.RunID {
		return errors.New("lease: refusing operation for a different owner's lease")
	}
	return nil
}

func (l *Lease) rehome(root string) {
	l.mu.Lock()
	l.root = root
	l.mu.Unlock()
}

func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, err := readLease(leasePath(l.root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if current.Token != l.record.Token {
		return errors.New("lease: refusing to remove a different owner's lease")
	}
	if err := os.Remove(leasePath(l.root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
