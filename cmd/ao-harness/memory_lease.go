package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

const detachedHarnessLeaseTTL = 24 * time.Hour

// hostGovernorDir resolves the host-wide reservation directory. Package tests
// replace it so that no test can reach host-wide state.
var hostGovernorDir = governor.DefaultDir

// openGovernor opens the reservation store in dir, or the host-wide store
// when dir is empty.
func openGovernor(dir string) (*governor.Manager, error) {
	if dir == "" {
		var err error
		if dir, err = hostGovernorDir(); err != nil {
			return nil, err
		}
	}
	return governor.New(governor.Options{Dir: dir})
}

func (e *env) reserveDetachedHarness(dataRoot string, bs harnessclient.Bootstrap, limit uint64) (governor.Lease, error) {
	mgr, err := openGovernor(e.governorDir)
	if err != nil {
		return governor.Lease{}, err
	}
	worktree, err := instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		return governor.Lease{}, fmt.Errorf("canonicalize detached harness root: %w", err)
	}
	lease, err := mgr.Reserve(governor.Request{
		RunID:        "up-" + instanceinfo.ID(dataRoot),
		Worktree:     worktree,
		DataRoot:     dataRoot,
		OwnerPID:     bs.PID,
		OwnerBirthID: bs.ProcessStartTime,
		CeilingBytes: limit,
		TTL:          detachedHarnessLeaseTTL,
	})
	if err != nil {
		return governor.Lease{}, fmt.Errorf("reserve detached harness memory: %w", err)
	}
	return lease, nil
}

func (e *env) releaseDetachedHarnessLease(dataRoot string) error {
	mgr, err := openGovernor(e.governorDir)
	if err != nil {
		return err
	}
	root, err := instanceinfo.CanonicalPath(dataRoot)
	if err != nil {
		return fmt.Errorf("canonicalize detached harness root: %w", err)
	}
	snapshot, err := mgr.Snapshot()
	if err != nil {
		return err
	}
	for _, lease := range snapshot.Leases {
		if lease.DataRoot != root || !strings.HasPrefix(lease.RunID, "up-") {
			continue
		}
		if err := mgr.Release(lease); err != nil && !errors.Is(err, governor.ErrLeaseNotFound) {
			return fmt.Errorf("release detached harness memory reservation: %w", err)
		}
	}
	return nil
}

func (e *env) releaseDetachedHarnessLeaseByID(lease governor.Lease) error {
	mgr, err := openGovernor(e.governorDir)
	if err != nil {
		return err
	}
	return mgr.Release(lease)
}
