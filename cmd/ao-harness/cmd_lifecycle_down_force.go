package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
	"agent-overflow/internal/procrss"
)

// noManifestError marks the ONE refusal `down --force` may override: the
// data root claims no instance AT ALL, so nothing contradicts the registry
// row — the confirming evidence is missing, not conflicting. Every other
// confirmation failure names a DIFFERENT claimant (a second pid, a
// mismatched identity, another build), and force must never override
// those: there the row is the thing that is wrong.
//
// The message is carried verbatim rather than wrapped, so an unforced
// refusal reads exactly as it did before this type existed.
type noManifestError struct {
	msg   string
	cause error
}

func (e *noManifestError) Error() string { return e.msg }
func (e *noManifestError) Unwrap() error { return e.cause }

func isNoManifest(err error) bool {
	var target *noManifestError
	return errors.As(err, &target)
}

// harnessProcessName is what a harness backend's own process must look
// like before --force may signal a pid nothing else confirms.
const harnessProcessName = "agent-overflow"

// forceProbe is the process-inspection surface --force reads. Indirected
// so the decision table runs against canned /proc answers instead of
// against processes a test would have to create in order to judge.
type forceProbe struct {
	alive    func(pid int) bool
	identity func(pid int) (instanceinfo.ProcessIdentity, error)
	comm     func(pid int) (string, error)
}

// forcedProbe is the live probe. A var only so tests can stand in for it.
var forcedProbe = forceProbe{
	alive:    instanceinfo.ProcessAlive,
	identity: instanceinfo.CaptureProcessIdentity,
	comm:     readProcessComm,
}

// readProcessComm reports the kernel's name for a pid, read from
// /proc/<pid>/stat through the same parser the RSS sampler uses — comm
// can contain spaces and parentheses, so splitting on whitespace is
// wrong (`procrss.ParseStat` has the detail). Elsewhere than Linux there
// is no /proc, the read fails, and --force refuses: an unreadable
// process is not a confirmed one.
func readProcessComm(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", fmt.Errorf("read process %d name: %w", pid, err)
	}
	name, _, ok := procrss.ParseStat(data)
	if !ok {
		return "", fmt.Errorf("process %d stat is not in the expected form", pid)
	}
	return name, nil
}

type forceVerdict int

const (
	// forceRefuse: the pid is not verifiably ours. Refuse, even forced.
	forceRefuse forceVerdict = iota
	// forcePruneOnly: nothing is running under that pid. Drop the row.
	forcePruneOnly
	// forceStop: /proc confirms an agent-overflow process. Signal it.
	forceStop
)

// decideForcedStop is the whole authority of --force: it turns a registry
// row whose data root confirms nothing into one of three answers, and it
// is pure apart from the injected probe.
//
// --force is emphatically NOT "kill whatever pid the file says". The row
// is only discovery state, and a pid is a number the OS recycles, so the
// pid must independently look like an agent-overflow process before
// anything is signalled: same namespace, whatever recorded birth identity
// the row carries, and a name and executable that are ours.
func decideForcedStop(row instanceinfo.Row, probe forceProbe) (forceVerdict, instanceinfo.ProcessIdentity, error) {
	var none instanceinfo.ProcessIdentity
	if row.PID <= 0 {
		return forceRefuse, none, fmt.Errorf("registry row %q names pid %d, which is not a pid", row.ID, row.PID)
	}
	current := instanceinfo.CurrentPIDNamespace()
	if row.PIDNamespace != "" && row.PIDNamespace != current {
		return forceRefuse, none, fmt.Errorf("registry row %q names pid namespace %q, not this CLI's %q; --force cannot inspect a pid it cannot see", row.ID, row.PIDNamespace, current)
	}
	if !probe.alive(row.PID) {
		return forcePruneOnly, none, nil
	}
	actual, err := probe.identity(row.PID)
	if err != nil {
		return forceRefuse, none, fmt.Errorf("--force cannot inspect pid %d: %w", row.PID, err)
	}
	if actual.Namespace == "" || actual.Namespace != current {
		return forceRefuse, none, fmt.Errorf("pid %d lives in pid namespace %q, not this CLI's %q; refusing to signal it", row.PID, actual.Namespace, current)
	}
	// A row that recorded a birth marker is the strongest evidence there
	// is, and it outranks the name check: a mismatch here means the pid
	// was recycled, however much the new occupant looks like us.
	if row.ProcessStartTime != "" && row.ProcessStartTime != actual.StartTime {
		return forceRefuse, none, fmt.Errorf("pid %d was born at %q, not the %q the registry row recorded; the pid has been recycled", row.PID, actual.StartTime, row.ProcessStartTime)
	}
	if row.ExecutablePath != "" && row.ExecutablePath != actual.Executable {
		return forceRefuse, none, fmt.Errorf("pid %d is running %q, not the %q the registry row recorded; refusing to signal it", row.PID, actual.Executable, row.ExecutablePath)
	}
	comm, err := probe.comm(row.PID)
	if err != nil {
		return forceRefuse, none, fmt.Errorf("--force cannot read what pid %d is: %w", row.PID, err)
	}
	if !looksLikeHarnessProcess(comm, actual.Executable) {
		return forceRefuse, none, fmt.Errorf("pid %d is %q (%s), not an %s process; refusing to signal it even under --force", row.PID, comm, actual.Executable, harnessProcessName)
	}
	return forceStop, actual, nil
}

// looksLikeHarnessProcess matches by PREFIX on both the kernel's name for
// the process and its executable's base name.
//
// Prefix, not equality, and that is load-bearing: the kernel caps comm at
// 15 characters, so a longer build name reads back truncated and an exact
// match would silently find nothing (the same trap `internal/procrss`
// documents). Both halves must agree, because comm is inherited from the
// path handed to execve while the executable is the file actually
// running: a symlink named for us in front of /bin/sh satisfies one and
// not the other.
func looksLikeHarnessProcess(comm, executable string) bool {
	if !strings.HasPrefix(comm, harnessProcessName) {
		return false
	}
	// procfs marks a replaced-on-disk binary; the name before it is real.
	base := filepath.Base(strings.TrimSuffix(executable, " (deleted)"))
	base = strings.TrimSuffix(base, ".exe")
	return strings.HasPrefix(base, harnessProcessName)
}

// terminateForced is the escalation itself, indirected so a test can
// assert that a verdict reached it WITHOUT a test process ever signalling
// something it did not start.
var terminateForced = harnessclient.TerminateProcessVerified

// stopForcedVictim applies the verdict: the same TERM-then-KILL
// escalation the unauthenticated path of an ordinary `down` uses, which
// re-verifies the identity captured here before the signal AND before the
// escalation, so a pid that dies and is recycled inside the grace window
// is never killed twice over.
func stopForcedVictim(ctx context.Context, row instanceinfo.Row, probe forceProbe) (bool, error) {
	verdict, identity, err := decideForcedStop(row, probe)
	switch verdict {
	case forcePruneOnly:
		return false, nil
	case forceStop:
		return true, terminateForced(ctx, row.PID, identity, stopGrace)
	default:
		return false, err
	}
}
