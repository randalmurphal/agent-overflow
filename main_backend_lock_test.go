package main

import (
	"path/filepath"
	"testing"
)

// TestHoldBackendLockKeepsTheLockThisBootTook: the desktop boot takes the
// lock to reconcile its update record, and bootTransport keeps that one
// rather than refusing on its own lock.
func TestHoldBackendLockKeepsTheLockThisBootTook(t *testing.T) {
	previous := heldBackendLock
	heldBackendLock = nil
	t.Cleanup(func() {
		if heldBackendLock != nil {
			heldBackendLock.releaseForTest(t)
		}
		heldBackendLock = previous
	})
	root := filepath.Join(t.TempDir(), "agent-overflow")
	if err := holdBackendLock(root); err != nil {
		t.Fatal(err)
	}
	held := heldBackendLock
	if err := holdBackendLock(root); err != nil || heldBackendLock != held {
		t.Fatalf("the second hold = %v, lock kept %v", err, heldBackendLock == held)
	}
	if other, err := acquireBackendInstanceLock(root); err == nil {
		other.releaseForTest(t)
		t.Fatal("the held lock admitted another backend")
	}
}

func TestBackendDataRootHasOneOwnerAcrossBootModesAndRecoversAfterExit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent-overflow")
	first, err := acquireBackendInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireBackendInstanceLock(root); err == nil {
		second.releaseForTest(t)
		t.Fatal("a second backend entered the same root")
	}
	first.releaseForTest(t)
	next, err := acquireBackendInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	next.releaseForTest(t)
}
