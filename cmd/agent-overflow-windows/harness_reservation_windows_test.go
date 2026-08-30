//go:build windows

package main

import (
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/harness/governor"
)

func TestWSLHomeFromBinaryRequiresCanonicalPayloadPath(t *testing.T) {
	if got, ok := wslHomeFromBinary("/home/dev/.local/bin/agent-overflow"); !ok || got != "/home/dev" {
		t.Fatalf("home = %q, ok=%v", got, ok)
	}
	if _, ok := wslHomeFromBinary("/home/dev/bin/agent-overflow"); ok {
		t.Fatal("non-canonical payload path accepted")
	}
}

func TestWSLContainmentEvidenceScriptIsAtomicAndPrivate(t *testing.T) {
	for _, fragment := range []string{"chmod 600", "mv -f", "cat \"$logdir/harness-containment.json\"", "trap 'rm -f"} {
		if !strings.Contains(wslContainmentEvidenceScript, fragment) {
			t.Fatalf("evidence script missing %q", fragment)
		}
	}
}

type reservationTestMemory struct{}

func (reservationTestMemory) AvailableMemory() (uint64, error) { return 200, nil }

type reservationTestProcesses struct{}

func (reservationTestProcesses) State(int) (governor.ProcessState, error) {
	return governor.ProcessState{Alive: true, BirthID: "birth"}, nil
}

func TestLauncherReservationAllowsOnlyOneCombinedBudget(t *testing.T) {
	manager, err := governor.New(governor.Options{
		Dir:                 t.TempDir(),
		DefaultCeilingBytes: 150,
		AvailableFloorBytes: 10,
		Memory:              reservationTestMemory{},
		Processes:           reservationTestProcesses{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := manager.Reserve(governor.Request{
				RunID:        "wsl-run",
				Worktree:     t.TempDir(),
				DataRoot:     t.TempDir(),
				OwnerPID:     1,
				OwnerBirthID: "birth",
				CeilingBytes: 150,
			})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("reservation successes = %d, want exactly one", successes)
	}
}

func TestLauncherReservationReleaseRemovesOnlyOwnedLease(t *testing.T) {
	manager, err := governor.New(governor.Options{
		Dir:                 t.TempDir(),
		DefaultCeilingBytes: 100,
		AvailableFloorBytes: 10,
		Memory:              reservationTestMemory{},
		Processes:           reservationTestProcesses{},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Reserve(governor.Request{RunID: "wsl-run", Worktree: t.TempDir(), DataRoot: t.TempDir(), OwnerPID: 1, OwnerBirthID: "birth", CeilingBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(lease); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Leases) != 0 {
		t.Fatalf("leases after cleanup = %d, want zero", len(snapshot.Leases))
	}
}
