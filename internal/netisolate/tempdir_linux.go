//go:build linux

package netisolate

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"agent-overflow/internal/filelock"
	"agent-overflow/internal/hostmemory"
	"agent-overflow/internal/ownedtemp"
)

// A run's temp directory is a tmpfs when the run is admitted to one
// (reserveRAMTemp), and otherwise a per-run directory on disk
// (diskTempPrefix) mounted at the same TempDir, created, removed and swept
// like the Go work directory.
//
// tempDirBytes caps the tmpfs, so a runaway test fails with ENOSPC instead
// of filling the host's memory; pages are allocated as they are written and
// the cap reserves nothing. It is over three times the largest peak
// measured on 24 cores, 1.2 GiB both for ./internal/store alone and for
// the whole tree (./internal/app: 0.3 GiB), which grows with the core
// count. ramTempReserve is what MemAvailable must hold beyond that cap:
// the developer's own processes and the memory the run's compilers, test
// binaries and browsers take besides temp files, for which the e2e gate's
// 8 GiB memory ceiling is the largest any one run is given. A run that
// starts below the threshold could not fill its cap without pushing the
// host toward swap, so it writes to disk.
//
// Since a tmpfs reserves nothing, MemAvailable cannot see runs that started
// a moment ago, so it alone does not bound them. Each RAM run also holds a
// reservation, and all of them together may cap at most
// ramTempBudgetFraction of the memory the launcher may use: the smaller
// of MemTotal and its cgroup's memory limit. That is the share the kernel
// gives a tmpfs mounted without a size, and it leaves the rest for what
// the runs do besides writing temp files.
const (
	tempDirBytes          = 4 << 30
	ramTempReserve        = 8 << 30
	ramTempThreshold      = tempDirBytes + ramTempReserve
	ramTempBudgetFraction = 0.5
	// diskTempPrefix names a run's temp directory on disk.
	diskTempPrefix = "ao-tmp-"
	// reservationPrefix names a RAM run's reservation (ownedtemp) in
	// reservationsDir, and admissionLockName the lock file there under
	// which a launcher counts the reservations and takes one.
	reservationPrefix = "ram-"
	admissionLockName = "admission.lock"
	// admissionWait bounds the wait for that lock, which a launcher holds
	// only to count a few directories and create one.
	admissionWait = 30 * time.Second
)

// memoryLimits are the numbers the admission rule reads, in bytes.
type memoryLimits struct {
	// available and total are /proc/meminfo's MemAvailable and MemTotal.
	available, total uint64
	// cgroupLimit is the lowest memory limit of the launcher's cgroup and
	// its ancestors, or 0 when none is set.
	cgroupLimit uint64
}

// ramTempBudget is what the RAM temp directories of all runs may cap
// together.
func ramTempBudget(m memoryLimits) uint64 {
	usable := m.total
	if m.cgroupLimit != 0 && m.cgroupLimit < usable {
		usable = m.cgroupLimit
	}
	return uint64(float64(usable) * ramTempBudgetFraction)
}

// ramTempAdmit decides whether one more run may use a RAM temp directory
// while live runs hold one, given the memory limits or the error reading
// them. When it may not, reason says why. Unknown limits mean disk: nothing
// then shows the host can hold the cap, and disk costs only speed.
func ramTempAdmit(m memoryLimits, readErr error, live int) (ram bool, reason string) {
	if readErr != nil {
		return false, fmt.Sprintf("the host's memory is unknown (%v)", readErr)
	}
	if m.available < ramTempThreshold {
		return false, fmt.Sprintf("the host has %s of memory available, under the %s a RAM one needs (its %s cap plus %s for everything else)",
			gib(m.available), gib(ramTempThreshold), gib(tempDirBytes), gib(ramTempReserve))
	}
	if budget := ramTempBudget(m); uint64(live+1)*tempDirBytes > budget {
		return false, fmt.Sprintf("the %s budget for RAM temp directories (%.0f%% of %s) has no room for another %s one beside the %d other runs holding one",
			gib(budget), ramTempBudgetFraction*100, usableMemory(m), gib(tempDirBytes), live)
	}
	return true, ""
}

func usableMemory(m memoryLimits) string {
	if m.cgroupLimit != 0 && m.cgroupLimit < m.total {
		return "the " + gib(m.cgroupLimit) + " cgroup memory limit"
	}
	return "the host's " + gib(m.total)
}

func gib(n uint64) string { return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB" }

// reservationsDir is the fixed per-user directory that holds the RAM
// runs' reservations, on the host's /tmp, which every namespace Command
// builds shares. Tests stand in for the host through it.
var reservationsDir = func() string { return TempDir() + "-ram" }

// readMemoryLimits reads the limits the admission rule needs. Tests stand
// in for the host through it.
var readMemoryLimits = func() (memoryLimits, error) {
	r, err := hostmemory.Read()
	return memoryLimits{available: r.Available, total: r.Total, cgroupLimit: r.CgroupLimit}, err
}

// reserveRAMTemp admits this run to a RAM temp directory or says why not.
// Admitted, it returns the run's reservation, which the run's processes
// hold like its directories on disk, so it counts until the last of them
// has exited. Under the admission lock it counts the live reservations,
// removing those nobody holds, applies ramTempAdmit and takes one.
func reserveRAMTemp(stderr io.Writer) (reservation *ownedtemp.Shared, reason string) {
	dir := reservationsDir()
	unreserved := func(err error) (*ownedtemp.Shared, string) {
		return nil, fmt.Sprintf("its RAM reservation could not be taken (%v)", err)
	}
	if err := ensureMountPoint(dir); err != nil {
		return unreserved(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), admissionWait)
	defer cancel()
	lock, err := filelock.Acquire(ctx, filepath.Join(dir, admissionLockName))
	if err != nil {
		return unreserved(fmt.Errorf("take the admission lock in %s: %w", dir, err))
	}
	defer func() {
		if err := filelock.Release(lock); err != nil {
			warn(stderr, "netisolate: release the RAM admission lock:", err)
		}
	}()
	live, err := ownedtemp.Held(dir, reservationPrefix)
	if err != nil {
		return unreserved(fmt.Errorf("count the RAM temp directories in use: %w", err))
	}
	limits, readErr := readMemoryLimits()
	if ram, reason := ramTempAdmit(limits, readErr, live); !ram {
		return nil, reason
	}
	reservation, err = ownedtemp.MakeShared(dir, reservationPrefix)
	if err != nil {
		return unreserved(err)
	}
	return reservation, ""
}
