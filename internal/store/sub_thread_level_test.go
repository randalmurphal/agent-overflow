package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// spawnOwned makes worker an owned spawn of caller, as a local
// thread_spawn's naming write does.
func spawnOwned(t *testing.T, s *Store, token, caller, worker string) {
	t.Helper()
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: ThreadRequestSpawn, State: ThreadRequestUnconfirmed,
	}, ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: ThreadRequestSpawn, SourceThreadID: caller}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NameLocalThreadRequestTarget(token, worker, true); err != nil {
		t.Fatal(err)
	}
}

func requireLevel(t *testing.T, s *Store, threadID string, want int) {
	t.Helper()
	got, err := s.SubThreadLevel(threadID)
	if err != nil || got != want {
		t.Fatalf("SubThreadLevel(%s) = %d, %v; want %d", threadID, got, err, want)
	}
}

// A level counts owned links up to the nearest thread that is not an owned
// worker: the person's thread is 0, each owned worker one more than its
// spawner, and a worker the person placed in the sidebar is 0 again with
// its own workers counted from it.
func TestSubThreadLevelCountsTheOwnedLineage(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lead", "one", "two", "three"} {
		createDraftCleanupThread(t, s, id, "chat")
	}
	spawnOwned(t, s, "t1", "lead", "one")
	spawnOwned(t, s, "t2", "one", "two")
	spawnOwned(t, s, "t3", "two", "three")
	requireLevel(t, s, "lead", 0)
	requireLevel(t, s, "one", 1)
	requireLevel(t, s, "two", 2)
	requireLevel(t, s, "three", 3)

	if _, err := s.SetThreadPlacement([]string{"one"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "one", 0)
	requireLevel(t, s, "three", 2)

	if _, err := s.SetThreadPlacement([]string{"one"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "three", 3)

	if _, err := s.SubThreadLevel("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SubThreadLevel(missing) = %v, want sql.ErrNoRows", err)
	}
}

// A paired computer's spawn carries its level, and the threads it spawns
// here count from it.
func TestSubThreadLevelAddsTheLevelAPairedSpawnCarried(t *testing.T) {
	s := newTestStore(t)
	remote := makeThread("remote", "claude")
	remote.SpawnLevel = 2
	if err := s.CreateThread(remote); err != nil {
		t.Fatal(err)
	}
	createDraftCleanupThread(t, s, "below", "chat")
	spawnOwned(t, s, "t1", "remote", "below")
	requireLevel(t, s, "remote", 2)
	requireLevel(t, s, "below", 3)
}

// A thread's spawn instructions and carried level are written with it,
// read back, kept by a fork, which adds its own instructions on top in the
// app, and travel with a transfer, which writes the row anew.
func TestSpawnInstructionsAndLevelLiveWithTheThread(t *testing.T) {
	s := newTestStore(t)
	worker := makeThread("worker", "claude")
	worker.SpawnInstructions, worker.SpawnLevel = "Review only.", 2
	if err := s.CreateThread(worker); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetThread("worker")
	if err != nil || got.SpawnInstructions != "Review only." || got.SpawnLevel != 2 {
		t.Fatalf("the stored worker = %q, %d, %v", got.SpawnInstructions, got.SpawnLevel, err)
	}
	if fork := BuildForkedThread(got); fork.SpawnInstructions != "Review only." || fork.SpawnLevel != 0 {
		t.Fatalf("the fork = %q, %d; want the instructions and no carried level", fork.SpawnInstructions, fork.SpawnLevel)
	}

	destination := newTestStore(t)
	request := transferRequest("worker", "copy", "incoming")
	if _, err := destination.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	if err := s.ExportThreadHistory(context.Background(), "worker", &history); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, destination, request.ID, "prepared")
	if _, err := destination.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), got, &history); err != nil {
		t.Fatal(err)
	}
	moved, err := destination.GetThread("worker")
	if err != nil || moved.SpawnInstructions != "Review only." || moved.SpawnLevel != 2 {
		t.Fatalf("the transferred worker = %q, %d, %v", moved.SpawnInstructions, moved.SpawnLevel, err)
	}
}
