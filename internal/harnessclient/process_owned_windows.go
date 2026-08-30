//go:build windows

package harnessclient

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"agent-overflow/internal/harness/instanceinfo"
	"golang.org/x/sys/windows"
)

// Windows has no process-group id. Keep creation-time identities for the
// root and descendants so a root-exit cannot turn a later taskkill into a
// PID-reuse kill.
type ownedGroupProof struct {
	rootPID      int
	rootIdentity instanceinfo.ProcessIdentity
	members      []ownedTreeMember
}

type ownedTreeMember struct {
	pid      int
	parent   int
	identity instanceinfo.ProcessIdentity
}

var snapshotProcessTree = snapshotProcessTreeOS
var runTaskkillProbe = runTaskkill

func captureUnverifiedProof(pid int) (ownedGroupProof, error) {
	members, err := snapshotProcessTree(pid)
	if err != nil {
		return ownedGroupProof{}, err
	}
	return ownedGroupProof{members: members}, nil
}

func emptyOwnedGroupProof(int) ownedGroupProof { return ownedGroupProof{} }

func terminateOwnedProcess(ctx context.Context, pid int, expected instanceinfo.ProcessIdentity, grace time.Duration) error {
	proof, err := captureOwnedTreeProof(pid, expected)
	if err != nil {
		return err
	}
	if _, err := verifyProcessIfAlive(pid, expected); err != nil {
		return fmt.Errorf("terminate %d: process identity changed after tree capture: %w", pid, err)
	}
	if !ownedTreeAlive(proof) {
		return nil
	}
	rootAlive, err := verifyProcessIfAlive(pid, expected)
	if err != nil {
		return fmt.Errorf("terminate %d: process identity changed before signal: %w", pid, err)
	}
	if rootAlive {
		_ = requestStopProcessTree(pid)
	} else if err := verifyOwnedTree(proof); err != nil {
		return fmt.Errorf("terminate process tree %d: %w", pid, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !ownedTreeAlive(proof) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	proof, err = killOwnedProcess(pid, expected)
	if err != nil {
		return err
	}
	return waitForOwnedExit(ctx, pid, proof)
}

func captureOwnedTreeProof(pid int, expected instanceinfo.ProcessIdentity) (ownedGroupProof, error) {
	if pid <= 0 {
		return ownedGroupProof{}, fmt.Errorf("terminate process tree %d: invalid pid", pid)
	}
	rootAlive := processAlive(pid)
	if rootAlive {
		var err error
		rootAlive, err = verifyProcessIfAlive(pid, expected)
		if err != nil {
			return ownedGroupProof{}, fmt.Errorf("terminate %d: process identity changed: %w", pid, err)
		}
	}
	members, err := snapshotProcessTree(pid)
	if err != nil {
		return ownedGroupProof{}, fmt.Errorf("capture process tree %d: %w", pid, err)
	}
	if rootAlive && !treeMembersContainPID(members, pid) {
		return ownedGroupProof{}, fmt.Errorf("terminate %d: process tree capture lost the root", pid)
	}
	if currentRootAlive := processAlive(pid); currentRootAlive && !rootAlive {
		return ownedGroupProof{}, fmt.Errorf("terminate %d: process identity changed during tree capture", pid)
	}
	return ownedGroupProof{rootPID: pid, rootIdentity: expected, members: members}, nil
}

func treeMembersContainPID(members []ownedTreeMember, pid int) bool {
	for _, member := range members {
		if member.pid == pid {
			return true
		}
	}
	return false
}

func killOwnedProcess(pid int, expected instanceinfo.ProcessIdentity) (ownedGroupProof, error) {
	proof, err := captureOwnedTreeProof(pid, expected)
	if err != nil {
		return proof, err
	}
	if _, err := verifyProcessIfAlive(pid, expected); err != nil {
		return proof, fmt.Errorf("kill %d: process identity changed after tree capture: %w", pid, err)
	}
	if !ownedTreeAlive(proof) {
		return proof, nil
	}
	rootAlive, err := verifyProcessIfAlive(pid, expected)
	if err != nil {
		return proof, fmt.Errorf("kill %d: process identity changed: %w", pid, err)
	}
	if rootAlive {
		if err := requestKillProcessTree(pid); err != nil && processAlive(pid) {
			return proof, err
		}
		return proof, nil
	}
	if err := verifyOwnedTree(proof); err != nil {
		return proof, fmt.Errorf("kill process tree %d: %w", pid, err)
	}
	// The root is already gone. Never run taskkill against its recycled PID.
	for i := len(proof.members) - 1; i >= 0; i-- {
		member := proof.members[i]
		if member.pid == pid || !processAlive(member.pid) {
			continue
		}
		if err := verifyProcessIdentity(member.pid, member.identity); err != nil {
			return proof, fmt.Errorf("kill descendant %d: process identity changed: %w", member.pid, err)
		}
		if err := requestKillProcessTree(member.pid); err != nil && processAlive(member.pid) {
			return proof, err
		}
	}
	return proof, nil
}

func requestStopProcessTree(pid int) error {
	return runTaskkillProbe("/PID", strconv.Itoa(pid), "/T")
}

func requestKillProcessTree(pid int) error {
	return runTaskkillProbe("/PID", strconv.Itoa(pid), "/T", "/F")
}

func runTaskkill(args ...string) error {
	cmd := exec.Command("taskkill.exe", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill process tree: %w (%s)", err, string(out))
	}
	return nil
}

func ownedTreeAlive(proof ownedGroupProof) bool {
	for _, member := range proof.members {
		if processAlive(member.pid) {
			return true
		}
	}
	return false
}

func verifyOwnedTree(proof ownedGroupProof) error {
	root := proof.rootPID
	if root > 0 {
		if _, err := verifyProcessIfAlive(root, proof.rootIdentity); err != nil {
			return fmt.Errorf("root process identity changed: %w", err)
		}
	}
	for _, member := range proof.members {
		if member.pid == root {
			continue
		}
		if _, err := verifyProcessIfAlive(member.pid, member.identity); err != nil {
			return fmt.Errorf("descendant %d identity changed: %w", member.pid, err)
		}
	}
	return nil
}

func waitForOwnedExit(ctx context.Context, pid int, proof ownedGroupProof) error {
	for {
		if proof.rootIdentity.StartTime != "" {
			if _, err := verifyProcessIfAlive(pid, proof.rootIdentity); err != nil {
				return fmt.Errorf("wait for process tree %d: process identity changed: %w", pid, err)
			}
		}
		if !ownedTreeAlive(proof) {
			return nil
		}
		if err := verifyOwnedTree(proof); err != nil {
			return fmt.Errorf("wait for process tree %d: %w", pid, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for process tree %d to exit: %w", pid, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

func snapshotProcessTreeOS(root int) ([]ownedTreeMember, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	parents := make(map[int]int)
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("read process snapshot: %w", err)
	}
	for {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("read process snapshot: %w", err)
		}
	}
	// Keep walking even when the leader has already exited. Windows leaves a
	// surviving child's parent PID in the snapshot, which is enough to prove
	// its ancestry back to the authenticated root. Returning no members here
	// would release a watchdog lease while that child still owns the launch.
	members := make([]ownedTreeMember, 0, 4)
	for pid := range parents {
		if pid != root && !isDescendant(pid, root, parents) {
			continue
		}
		identity, err := instanceinfo.CaptureProcessIdentity(pid)
		if err != nil {
			if !processAlive(pid) {
				continue
			}
			return nil, fmt.Errorf("capture process %d: %w", pid, err)
		}
		members = append(members, ownedTreeMember{pid: pid, parent: parents[pid], identity: identity})
	}
	return members, nil
}

func isDescendant(pid, root int, parents map[int]int) bool {
	seen := map[int]bool{}
	for pid != 0 && !seen[pid] {
		if pid == root {
			return true
		}
		seen[pid] = true
		pid = parents[pid]
	}
	return false
}
