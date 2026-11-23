package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/harness/governor"
)

// admitWait is how long a split run asks the governor for memory before it
// fails, and admitPoll how often it asks.
const (
	admitWait = 10 * time.Minute
	admitPoll = 5 * time.Second
)

// reservationTotals is what a run of declared reserves to run 1, 2, ...
// buckets at once on cores: at most one per core and at most one package's
// bucket count, since the buckets start in declaration order and no more of
// the largest package's run at once. The list steps run first, one at a
// time, under the same lease, and no bucket starts until they end, so k
// buckets need the larger of the largest list step's unit and the k
// largest bucket units together.
func reservationTotals(declared []split, cores int) []uint64 {
	var units []uint64
	var list uint64
	for _, s := range declared {
		list = max(list, s.listUnit)
		for range buckets {
			units = append(units, s.bucketUnit)
		}
	}
	slices.SortFunc(units, func(a, b uint64) int { return cmp.Compare(b, a) })
	totals := make([]uint64, min(max(cores, 1), buckets, len(units)))
	var held uint64
	for k := range totals {
		held += units[k]
		totals[k] = max(list, held)
	}
	return totals
}

// errStopped is admit's error when a signal ended the wait.
var errStopped = errors.New("stopped by a signal")

// admit reserves the largest of totals that fits in the host's governor,
// for the run whose binaries are in dir.
func (r *runner) admit(dir string, totals []uint64) (int, func() error, error) {
	manager, err := governor.New(governor.Options{})
	if err != nil {
		return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
	}
	worktree, err := os.Getwd()
	if err != nil {
		return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
	}
	return r.reserve(manager, governor.Request{
		RunID:    "go-test-" + strconv.Itoa(os.Getpid()),
		Worktree: worktree,
		DataRoot: dir,
		OwnerPID: os.Getpid(),
	}, totals)
}

// reserve asks m for the largest of totals that fits, W buckets' (see
// reservationTotals), and returns W and the lease's release. While m grants
// none it says once why and asks again every admitPoll; after admitWait it
// fails with the reason.
func (r *runner) reserve(m *governor.Manager, req governor.Request, totals []uint64) (int, func() error, error) {
	deadline := time.Now().Add(r.admitWait)
	told := false
	for {
		lease, granted, err := m.ReserveUpTo(req, totals)
		if err != nil {
			return 0, nil, fmt.Errorf("reserve memory for the buckets: %w", err)
		}
		if granted > 0 {
			return granted, func() error { return m.Release(lease) }, nil
		}
		why := refusal(m, totals[0])
		if !time.Now().Before(deadline) {
			return 0, nil, fmt.Errorf("no memory for the buckets after %v: %s", r.admitWait, why)
		}
		if !told {
			told = true
			r.out.Lock()
			fmt.Fprintf(r.stderr, "ao-gotest: waiting for memory for the buckets: %s\n", why)
			r.out.Unlock()
		}
		select {
		case <-r.stopping:
			return 0, nil, errStopped
		case <-time.After(r.admitPoll):
		}
	}
}

// refusal says why m grants not even need, one bucket's total: what is
// free, and who holds the rest.
func refusal(m *governor.Manager, need uint64) string {
	snap, err := m.Snapshot()
	if err != nil {
		return fmt.Sprintf("one bucket and the list steps before it need %d MiB, which is not free, and the reservations could not be read: %v", need>>20, err)
	}
	var free uint64
	if kept := snap.AvailableFloorBytes + snap.ReservedBytes; snap.AvailableBytes > kept {
		free = snap.AvailableBytes - kept
	}
	holders := "none"
	if len(snap.Leases) > 0 {
		var held []string
		for _, lease := range snap.Leases {
			held = append(held, fmt.Sprintf("%s in %s holds %d MiB", lease.RunID, lease.Worktree, lease.CeilingBytes>>20))
		}
		holders = strings.Join(held, "; ")
	}
	return fmt.Sprintf("one bucket and the list steps before it need %d MiB and %d MiB is free: %d MiB available less the %d MiB floor the host keeps and the %d MiB other runs reserved (%s)",
		need>>20, free>>20, snap.AvailableBytes>>20, snap.AvailableFloorBytes>>20, snap.ReservedBytes>>20, holders)
}
