package main

import (
	"path/filepath"
	"testing"
)

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
