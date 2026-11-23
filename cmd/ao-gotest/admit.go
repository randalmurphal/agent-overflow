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

// phase is one of a split run's two phases, each under a lease of its own:
// the list steps, one at a time, and then the buckets. They never overlap,
// so a lease sized for one would hold memory the other never uses.
type phase struct {
	// jobs names what runs in the phase, one names one of them, and runID
	// tells the phase's lease from the other's.
	jobs, one, runID string
}

var (
	listPhase   = phase{jobs: "the list steps", one: "a list step", runID: "list"}
	bucketPhase = phase{jobs: "the buckets", one: "one bucket", runID: "buckets"}
)

// admitter reserves memory for one phase: the largest of totals that fits,
// totals[k-1] being what k of the phase's jobs at once hold, and returns k
// and the release of that lease.
type admitter func(p phase, totals []uint64) (int, func() error, error)

// bucketTotals is what running 1, 2, ... of buckets whose units are units
// at once holds: the largest first, at most one per core and at most one
// package's bucket count, since the buckets start in declaration order and
// no more of the largest package's run at once.
func bucketTotals(units []uint64, cores int) []uint64 {
	sorted := slices.SortedFunc(slices.Values(units), func(a, b uint64) int { return cmp.Compare(b, a) })
	totals := make([]uint64, min(max(cores, 1), buckets, len(sorted)))
	var held uint64
	for k := range totals {
		held += sorted[k]
		totals[k] = held
	}
	return totals
}

// errStopped is an admitter's error when a signal ended the wait.
var errStopped = errors.New("stopped by a signal")

// admission is the admitter of the run whose binaries are in dir: each
// phase reserves in the host's governor.
func (r *runner) admission(dir string) (admitter, error) {
	m, err := governor.New(governor.Options{})
	if err != nil {
		return nil, fmt.Errorf("reserve memory for the run: %w", err)
	}
	worktree, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("reserve memory for the run: %w", err)
	}
	return r.admissionIn(m, governor.Request{RunID: "go-test-" + strconv.Itoa(os.Getpid()), Worktree: worktree, DataRoot: dir, OwnerPID: os.Getpid()}), nil
}

// admissionIn is the admitter whose phases reserve in m as req, each under
// a run ID of its own.
func (r *runner) admissionIn(m *governor.Manager, req governor.Request) admitter {
	return func(p phase, totals []uint64) (int, func() error, error) {
		req := req
		req.RunID += "-" + p.runID
		return r.reserve(m, req, p, totals)
	}
}

// reserve asks m for the largest of totals that fits, for W of p's jobs at
// once, and returns W and the lease's release. While m grants none it says
// once why and asks again every admitPoll; after admitWait it fails with
// the reason.
func (r *runner) reserve(m *governor.Manager, req governor.Request, p phase, totals []uint64) (int, func() error, error) {
	deadline := time.Now().Add(r.admitWait)
	told := false
	for {
		lease, granted, err := m.ReserveUpTo(req, totals)
		if err != nil {
			return 0, nil, fmt.Errorf("reserve memory for %s: %w", p.jobs, err)
		}
		if granted > 0 {
			return granted, func() error { return m.Release(lease) }, nil
		}
		why := refusal(m, p, totals[0])
		if !time.Now().Before(deadline) {
			return 0, nil, fmt.Errorf("no memory for %s after %v: %s", p.jobs, r.admitWait, why)
		}
		if !told {
			told = true
			r.out.Lock()
			fmt.Fprintf(r.stderr, "ao-gotest: waiting for memory for %s: %s\n", p.jobs, why)
			r.out.Unlock()
		}
		select {
		case <-r.stopping:
			return 0, nil, errStopped
		case <-time.After(r.admitPoll):
		}
	}
}

// refusal says why m grants not even need, what one of p's jobs holds:
// what is free, and who holds the rest.
func refusal(m *governor.Manager, p phase, need uint64) string {
	snap, err := m.Snapshot()
	if err != nil {
		return fmt.Sprintf("%s needs %d MiB, which is not free, and the reservations could not be read: %v", p.one, need>>20, err)
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
	return fmt.Sprintf("%s needs %d MiB and %d MiB is free: %d MiB available less the %d MiB floor the host keeps and the %d MiB other runs reserved (%s)",
		p.one, need>>20, free>>20, snap.AvailableBytes>>20, snap.AvailableFloorBytes>>20, snap.ReservedBytes>>20, holders)
}
