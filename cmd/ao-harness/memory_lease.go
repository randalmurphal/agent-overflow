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

func reserveDetachedHarness(dataRoot string, bs harnessclient.Bootstrap, limit uint64) (governor.Lease, error) {
	mgr, err := governor.New(governor.Options{})
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

func releaseDetachedHarnessLease(dataRoot string) error {
	mgr, err := governor.New(governor.Options{})
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

func releaseDetachedHarnessLeaseByID(lease governor.Lease) error {
	mgr, err := governor.New(governor.Options{})
	if err != nil {
		return err
	}
	return mgr.Release(lease)
}
