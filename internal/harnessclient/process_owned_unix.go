//go:build !windows

package harnessclient

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

type ownedGroupProof struct {
	rootPID      int
	pgid         int
	rootIdentity instanceinfo.ProcessIdentity
	members      []ownedGroupMember
}

var ownedGroupAliveProbe = ownedGroupAliveOS
var signalOwnedGroupProbe = signalOwnedGroup
var killProcessGroupProbe = requestKillProcessGroup
var memberGroupIDProbe = syscall.Getpgid

func captureUnverifiedProof(pid int) (ownedGroupProof, error) {
	members, err := snapshotProcessGroup(pid)
	if err != nil {
		return ownedGroupProof{}, err
	}
	return ownedGroupProof{pgid: pid, members: members}, nil
}

func emptyOwnedGroupProof(pid int) ownedGroupProof { return ownedGroupProof{pgid: pid} }

func terminateOwnedProcess(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	proof, err := captureOwnedGroupProof(pid, expected)
	if err != nil {
		return err
	}
	return terminateProcessGroup(ctx, pid, expected, proof, grace)
}

func killOwnedProcess(pid int, expected instanceinfo.ProcessIdentity) (ownedGroupProof, error) {
	proof, err := captureOwnedGroupProof(pid, expected)
	if err != nil {
		return ownedGroupProof{}, err
	}
	if !ownedGroupAliveProbe(proof.pgid) {
		return proof, nil
	}
	if ownedGroupMustBeVerified(proof) {
		if err := verifyOwnedGroup(proof); err != nil {
			return proof, fmt.Errorf("kill process group %d: %w", pid, err)
		}
	}
	if err := killProcessGroupProbe(proof.pgid); err != nil && ownedGroupAliveProbe(proof.pgid) {
		return proof, err
	}
	return proof, nil
}

func captureOwnedGroupProof(pid int, expected instanceinfo.ProcessIdentity) (ownedGroupProof, error) {
	if pid <= 0 {
		return ownedGroupProof{}, fmt.Errorf("terminate process group %d: invalid pid", pid)
	}
	rootAlive := processAlive(pid)
	if rootAlive {
		var err error
		rootAlive, err = verifyProcessIfAlive(pid, expected)
		if err != nil {
			return ownedGroupProof{}, fmt.Errorf("terminate %d: process identity changed: %w", pid, err)
		}
	}
	members, err := snapshotProcessGroup(pid)
	if err != nil {
		return ownedGroupProof{}, fmt.Errorf("capture process group %d: %w", pid, err)
	}
	if rootAlive && !groupMembersContainPID(members, pid) {
		return ownedGroupProof{}, fmt.Errorf("terminate %d: process group capture lost the root", pid)
	}
	currentRootAlive := processAlive(pid)
	if currentRootAlive != rootAlive {
		if currentRootAlive {
			return ownedGroupProof{}, fmt.Errorf("terminate %d: process identity changed during group capture", pid)
		}
	}
	if !processAlive(pid) && len(members) == 0 {
		if !ownedGroupAliveProbe(pid) {
			return ownedGroupProof{rootPID: pid, pgid: pid, rootIdentity: expected}, nil
		}
		return ownedGroupProof{}, fmt.Errorf("process group %d survives without an identifiable member", pid)
	}
	return ownedGroupProof{rootPID: pid, pgid: pid, rootIdentity: expected, members: members}, nil
}

func groupMembersContainPID(members []ownedGroupMember, pid int) bool {
	for _, member := range members {
		if member.pid == pid {
			return true
		}
	}
	return false
}

func terminateProcessGroup(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, proof ownedGroupProof, grace time.Duration) error {
	if !ownedGroupAliveProbe(proof.pgid) {
		return nil
	}
	if proof.rootPID > 0 {
		if _, err := verifyProcessIfAlive(proof.rootPID, proof.rootIdentity); err != nil {
			return fmt.Errorf("process identity changed before signal: %w", err)
		}
	}
	pidAlive, err := verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before signal: %w", pid, err)
	}
	if !pidAlive {
		if err := verifyOwnedGroup(proof); err != nil {
			return fmt.Errorf("terminate process group %d: %w", pid, err)
		}
	}
	if err := signalOwnedGroupProbe(proof.pgid, false); err != nil && (processAlive(pid) || ownedGroupAliveProbe(proof.pgid)) {
		return fmt.Errorf("terminate process group %d: %w", pid, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) && !ownedGroupAliveProbe(proof.pgid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if !ownedGroupAliveProbe(proof.pgid) {
		return nil
	}
	// The PID may have been recycled while the graceful stop was pending.
	// If the leader is gone, an unchanged member identity is the proof that
	// this PGID is still ours. A liveness check alone would turn a stale
	// launch record into a kill of an unrelated process group.
	pidAlive, err = verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate process group %d: process identity changed before kill: %w", pid, err)
	}
	if !pidAlive {
		if err := verifyOwnedGroup(proof); err != nil {
			return fmt.Errorf("terminate process group %d: %w", pid, err)
		}
	}
	if err := killProcessGroupProbe(proof.pgid); err != nil && ownedGroupAliveProbe(proof.pgid) {
		return fmt.Errorf("kill process group %d after %s grace: %w", pid, grace, err)
	}
	return waitForOwnedExit(ctx, pid, proof)
}

func waitForOwnedExit(ctx context.Context, pid int, proof ownedGroupProof) error {
	for {
		if proof.rootIdentity.StartTime != "" {
			if _, err := verifyProcessIfAlive(pid, proof.rootIdentity); err != nil {
				return fmt.Errorf("wait for process group %d: process identity changed: %w", pid, err)
			}
		}
		if !processAlive(pid) && !ownedGroupAliveProbe(proof.pgid) {
			return nil
		}
		if !processAlive(pid) && ownedGroupAliveProbe(proof.pgid) {
			if err := verifyOwnedGroup(proof); err != nil {
				return fmt.Errorf("wait for process group %d: %w", pid, err)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for process group %d to exit: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func ownedGroupMustBeVerified(proof ownedGroupProof) bool {
	return len(proof.members) > 0
}

func verifyOwnedGroup(proof ownedGroupProof) error {
	if proof.pgid <= 0 {
		return errors.New("process group proof has no group id")
	}
	if !ownedGroupAliveProbe(proof.pgid) {
		return nil
	}
	for _, member := range proof.members {
		if !processAlive(member.pid) {
			continue
		}
		if got, err := memberGroupIDProbe(member.pid); err == nil && got == proof.pgid {
			if err := verifyProcessIdentity(member.pid, member.identity); err == nil {
				return nil
			}
		}
	}
	return errors.New("no recorded member identity remains in the process group")
}

func ownedGroupAliveOS(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	// signal(0) reports zombies as present. They cannot execute work or
	// survive a group teardown, so only a non-zombie member keeps the group
	// live for ownership and lease-reuse purposes.
	return processGroupHasLiveMember(pgid)
}

func ownedProcessGroupAlive(pid int) bool {
	return ownedGroupAliveProbe(pid)
}
