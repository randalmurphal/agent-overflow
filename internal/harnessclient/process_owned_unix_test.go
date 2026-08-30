//go:build !windows

package harnessclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

func TestOwnedGroupKillsRecordedSurvivorAfterLeaderExit(t *testing.T) {
	oldSnapshot := snapshotProcessGroup
	oldAlive := processAlive
	oldVerify := verifyProcessIdentity
	oldGroupAlive := ownedGroupAliveProbe
	oldSignal := signalOwnedGroupProbe
	oldKill := killProcessGroupProbe
	oldMemberGroupID := memberGroupIDProbe
	t.Cleanup(func() {
		snapshotProcessGroup = oldSnapshot
		processAlive = oldAlive
		verifyProcessIdentity = oldVerify
		ownedGroupAliveProbe = oldGroupAlive
		signalOwnedGroupProbe = oldSignal
		killProcessGroupProbe = oldKill
		memberGroupIDProbe = oldMemberGroupID
	})
	const root, child = 101, 202
	expected := instanceinfo.ProcessIdentity{StartTime: "root", Executable: "/backend", Namespace: "ns"}
	memberID := instanceinfo.ProcessIdentity{StartTime: "child", Executable: "/backend", Namespace: "ns"}
	snapshotProcessGroup = func(int) ([]ownedGroupMember, error) {
		return []ownedGroupMember{{pid: child, identity: memberID}}, nil
	}
	processAlive = func(pid int) bool { return pid == child }
	verifyProcessIdentity = func(pid int, got instanceinfo.ProcessIdentity) error {
		if pid != child || got != memberID {
			return fmt.Errorf("unexpected identity check for pid %d", pid)
		}
		return nil
	}
	groupAlive := true
	ownedGroupAliveProbe = func(int) bool { return groupAlive }
	memberGroupIDProbe = func(int) (int, error) { return root, nil }
	gracefulSignals, forceSignals := 0, 0
	signalOwnedGroupProbe = func(int, bool) error { gracefulSignals++; return nil }
	killProcessGroupProbe = func(int) error { forceSignals++; groupAlive = false; return nil }

	err := terminateOwnedProcess(context.Background(), root, expected, 0)
	if err != nil {
		t.Fatalf("terminate owned survivor: %v", err)
	}
	if gracefulSignals != 1 || forceSignals != 1 {
		t.Fatalf("signals = graceful %d force %d, want one of each", gracefulSignals, forceSignals)
	}
}

func TestOwnedGroupRefusesRecordedSurvivorAfterPIDReuse(t *testing.T) {
	oldSnapshot := snapshotProcessGroup
	oldAlive := processAlive
	oldVerify := verifyProcessIdentity
	oldGroupAlive := ownedGroupAliveProbe
	oldSignal := signalOwnedGroupProbe
	oldMemberGroupID := memberGroupIDProbe
	t.Cleanup(func() {
		snapshotProcessGroup = oldSnapshot
		processAlive = oldAlive
		verifyProcessIdentity = oldVerify
		ownedGroupAliveProbe = oldGroupAlive
		signalOwnedGroupProbe = oldSignal
		memberGroupIDProbe = oldMemberGroupID
	})
	const root, child = 303, 404
	expected := instanceinfo.ProcessIdentity{StartTime: "root", Executable: "/backend", Namespace: "ns"}
	oldChild := instanceinfo.ProcessIdentity{StartTime: "old", Executable: "/backend", Namespace: "ns"}
	snapshotProcessGroup = func(int) ([]ownedGroupMember, error) {
		return []ownedGroupMember{{pid: child, identity: oldChild}}, nil
	}
	processAlive = func(pid int) bool { return pid == child }
	verifyProcessIdentity = func(int, instanceinfo.ProcessIdentity) error { return errors.New("pid was recycled") }
	ownedGroupAliveProbe = func(int) bool { return true }
	memberGroupIDProbe = func(int) (int, error) { return root, nil }
	signals := 0
	signalOwnedGroupProbe = func(int, bool) error { signals++; return nil }

	err := terminateOwnedProcess(context.Background(), root, expected, 0)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("terminate after PID reuse = %v, want identity refusal", err)
	}
	if signals != 0 {
		t.Fatalf("signals = %d, want no signal after PID reuse", signals)
	}
}

func TestOwnedGroupAcceptsLeaderExitDuringIdentityVerification(t *testing.T) {
	oldSnapshot := snapshotProcessGroup
	oldAlive := processAlive
	oldVerify := verifyProcessIdentity
	oldGroupAlive := ownedGroupAliveProbe
	t.Cleanup(func() {
		snapshotProcessGroup = oldSnapshot
		processAlive = oldAlive
		verifyProcessIdentity = oldVerify
		ownedGroupAliveProbe = oldGroupAlive
	})

	const root = 505
	expected := instanceinfo.ProcessIdentity{StartTime: "root", Executable: "/backend", Namespace: "ns"}
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
	snapshotProcessGroup = func(int) ([]ownedGroupMember, error) { return nil, nil }
	ownedGroupAliveProbe = func(int) bool { return false }

	proof, err := captureOwnedGroupProof(root, expected)
	if err != nil {
		t.Fatalf("capture after leader exit: %v", err)
	}
	if proof.rootPID != root || proof.pgid != root {
		t.Fatalf("proof = %+v, want exited root %d", proof, root)
	}
}
