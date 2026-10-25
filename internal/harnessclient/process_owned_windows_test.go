//go:build windows

package harnessclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
)

func TestOwnedTreeKillsProviderAfterLeaderExit(t *testing.T) {
	oldSnapshot, oldAlive, oldVerify, oldTaskkill := snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe
	t.Cleanup(func() {
		snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe = oldSnapshot, oldAlive, oldVerify, oldTaskkill
	})
	const root, provider = 101, 202
	rootID := instanceinfo.ProcessIdentity{StartTime: "root", Executable: `C:\backend.exe`, Namespace: "windows"}
	childID := instanceinfo.ProcessIdentity{StartTime: "provider", Executable: `C:\provider.exe`, Namespace: "windows"}
	snapshotProcessTree = func(int) ([]ownedTreeMember, error) {
		return []ownedTreeMember{{pid: provider, parent: root, identity: childID}}, nil
	}
	alive := true
	processAlive = func(pid int) bool { return pid == provider && alive }
	verifyProcessIdentity = func(pid int, got instanceinfo.ProcessIdentity) error {
		if pid != provider || got != childID {
			return errors.New("unexpected process identity")
		}
		return nil
	}
	kills := 0
	runTaskkillProbe = func(args ...string) error {
		kills++
		if len(args) < 2 || args[1] != "202" {
			t.Fatalf("taskkill args = %v, want provider only", args)
		}
		alive = false
		return nil
	}

	if err := terminateOwnedProcess(context.Background(), root, rootID, 0, nil); err != nil {
		t.Fatalf("terminate owned tree: %v", err)
	}
	if kills != 1 {
		t.Fatalf("taskkill calls = %d, want one provider termination", kills)
	}
}

func TestOwnedTreeRefusesPIDReuseBeforeTaskkill(t *testing.T) {
	oldSnapshot, oldAlive, oldVerify, oldTaskkill := snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe
	t.Cleanup(func() {
		snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe = oldSnapshot, oldAlive, oldVerify, oldTaskkill
	})
	const root, child = 303, 404
	rootID := instanceinfo.ProcessIdentity{StartTime: "root", Executable: `C:\backend.exe`, Namespace: "windows"}
	childID := instanceinfo.ProcessIdentity{StartTime: "old", Executable: `C:\backend.exe`, Namespace: "windows"}
	snapshotProcessTree = func(int) ([]ownedTreeMember, error) {
		return []ownedTreeMember{{pid: child, parent: root, identity: childID}}, nil
	}
	processAlive = func(pid int) bool { return pid == child }
	verifyProcessIdentity = func(int, instanceinfo.ProcessIdentity) error { return errors.New("process identity changed") }
	kills := 0
	runTaskkillProbe = func(...string) error { kills++; return nil }

	err := terminateOwnedProcess(context.Background(), root, rootID, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("terminate after PID reuse = %v, want identity refusal", err)
	}
	if kills != 0 {
		t.Fatalf("taskkill calls = %d, want none", kills)
	}
}

func TestOwnedTreeAcceptsLeaderExitDuringIdentityVerification(t *testing.T) {
	oldSnapshot, oldAlive, oldVerify := snapshotProcessTree, processAlive, verifyProcessIdentity
	t.Cleanup(func() {
		snapshotProcessTree, processAlive, verifyProcessIdentity = oldSnapshot, oldAlive, oldVerify
	})

	const root = 505
	expected := instanceinfo.ProcessIdentity{StartTime: "root", Executable: `C:\backend.exe`, Namespace: "windows"}
	aliveChecks := 0
	processAlive = func(pid int) bool {
		if pid != root {
			t.Fatalf("liveness checked unexpected pid %d", pid)
		}
		aliveChecks++
		return aliveChecks <= 2
	}
	verifyProcessIdentity = func(pid int, got instanceinfo.ProcessIdentity) error {
		if pid != root || got != expected {
			t.Fatalf("identity check = %d %+v, want root identity", pid, got)
		}
		return errors.New("process exited before executable read")
	}
	snapshotProcessTree = func(int) ([]ownedTreeMember, error) { return nil, nil }

	proof, err := captureOwnedTreeProof(root, expected)
	if err != nil {
		t.Fatalf("capture after leader exit: %v", err)
	}
	if proof.rootPID != root || len(proof.members) != 0 {
		t.Fatalf("proof = %+v, want exited root %d", proof, root)
	}
}

// A child the root starts after the stop request is missing from the first
// capture. When the root exits, the tree is captured again by parent links
// and the child gets a stop request of its own before termination reports
// success.
func TestOwnedTreeStopsAChildStartedAfterTheStopRequest(t *testing.T) {
	oldSnapshot, oldAlive, oldVerify, oldTaskkill := snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe
	t.Cleanup(func() {
		snapshotProcessTree, processAlive, verifyProcessIdentity, runTaskkillProbe = oldSnapshot, oldAlive, oldVerify, oldTaskkill
	})
	const root, late = 505, 606
	rootID := instanceinfo.ProcessIdentity{StartTime: "root", Executable: `C:\backend.exe`, Namespace: "windows"}
	lateID := instanceinfo.ProcessIdentity{StartTime: "late", Executable: `C:\provider.exe`, Namespace: "windows"}
	rootAlive, lateAlive, lateStarted := true, false, false
	snapshotProcessTree = func(int) ([]ownedTreeMember, error) {
		members := []ownedTreeMember{}
		if rootAlive {
			members = append(members, ownedTreeMember{pid: root, identity: rootID})
		}
		if lateAlive {
			members = append(members, ownedTreeMember{pid: late, parent: root, identity: lateID})
		}
		return members, nil
	}
	processAlive = func(pid int) bool { return (pid == root && rootAlive) || (pid == late && lateAlive) }
	verifyProcessIdentity = func(pid int, got instanceinfo.ProcessIdentity) error {
		if (pid == root && got == rootID) || (pid == late && got == lateID) {
			return nil
		}
		return errors.New("unexpected process identity")
	}
	lateStops := 0
	runTaskkillProbe = func(args ...string) error {
		switch {
		case len(args) >= 2 && args[1] == "505" && !lateStarted:
			// The root starts its child, then exits on the request.
			lateStarted, lateAlive, rootAlive = true, true, false
		case len(args) >= 2 && args[1] == "606":
			lateStops++
			lateAlive = false
		default:
			t.Fatalf("taskkill args = %v", args)
		}
		return nil
	}

	if err := terminateOwnedProcess(context.Background(), root, rootID, time.Second, nil); err != nil {
		t.Fatalf("terminate owned tree: %v", err)
	}
	if lateStops != 1 || lateAlive {
		t.Fatalf("late child stops = %d alive = %v, want one stop and exited", lateStops, lateAlive)
	}
}
