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

// createCarried creates a thread with the level a paired computer's spawn
// or ask carried to it, as the destination's creation writes it.
func createCarried(t *testing.T, s *Store, id, mode string, level int) {
	t.Helper()
	thread := makeThread(id, "claude")
	thread.Mode, thread.SpawnLevel = mode, level
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
}

// A paired computer's owned spawn carries its level, and the threads it
// spawns here count from it.
func TestSubThreadLevelAddsTheLevelAPairedSpawnCarried(t *testing.T) {
	s := newTestStore(t)
	createCarried(t, s, "remote", "chat", 2)
	acceptLink(t, s, "link", "dev-s", "remote", 1000)
	createDraftCleanupThread(t, s, "below", "chat")
	spawnOwned(t, s, "t1", "remote", "below")
	requireLevel(t, s, "remote", 2)
	requireLevel(t, s, "below", 3)
}

// A thread's spawn instructions are written with it, read back and kept by
// a fork, which adds its own on top in the app, and by a transfer, which
// writes the row anew. The carried level is the delegation's: neither a
// fork nor a transfer, which makes the thread a conversation of its own
// there, keeps it.
func TestSpawnInstructionsLiveWithTheThreadAndTheCarriedLevelWithItsDelegation(t *testing.T) {
	s := newTestStore(t)
	worker := makeThread("worker", "claude")
	worker.SpawnInstructions, worker.SpawnLevel = "Review only.", 2
	if err := s.CreateThread(worker); err != nil {
		t.Fatal(err)
	}
	acceptLink(t, s, "link", "dev-s", "worker", 1000)
	requireLevel(t, s, "worker", 2)
	got, err := s.GetThread("worker")
	if err != nil || got.SpawnInstructions != "Review only." {
		t.Fatalf("the stored worker = %q, %v", got.SpawnInstructions, err)
	}
	if fork := BuildForkedThread(got); fork.SpawnInstructions != "Review only." || fork.SpawnLevel != 0 {
		t.Fatalf("the fork = %q, %d; want the instructions and no carried level", fork.SpawnInstructions, fork.SpawnLevel)
	}

	for _, kind := range []string{"copy", "move"} {
		destination := newTestStore(t)
		request := transferRequest("worker", kind, "incoming")
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
		if err != nil || moved.SpawnInstructions != "Review only." || moved.SpawnLevel != 0 {
			t.Fatalf("%s: the transferred worker = %q, %d, %v; want the instructions and no carried level", kind, moved.SpawnInstructions, moved.SpawnLevel, err)
		}
		requireLevel(t, destination, "worker", 0)
	}
}

// askFrom records an ask from caller about source, answered by the scratch
// copy, as a local thread_ask's acceptance, naming write and scratch record
// do.
func askFrom(t *testing.T, s *Store, token, caller, source, copy string) {
	t.Helper()
	createDraftCleanupThread(t, s, copy, "scratch")
	if _, err := s.AcceptLocalThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: ThreadRequestAsk, State: ThreadRequestUnconfirmed, OriginThreadID: source,
	}, ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: ThreadRequestAsk, SourceThreadID: caller}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NameLocalThreadRequestTarget(token, copy, false); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertScratchThread(ScratchThread{ThreadID: copy, SourceThreadID: source, ReturnMode: "chat", RequestToken: token}); err != nil {
		t.Fatal(err)
	}
}

// An agent's ask copy continues its asker's delegation: one level below the
// thread that asked, by the ask that links them, and a copy that asks again
// one below that. A person's side chat names no request and is level 0,
// and a side chat the person keeps is a thread of its own.
func TestAnAgentsAskCopyIsOneLevelBelowItsAsker(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lead", "worker", "consulted"} {
		createDraftCleanupThread(t, s, id, "chat")
	}
	spawnOwned(t, s, "spawn", "lead", "worker")
	askFrom(t, s, "ask-1", "worker", "consulted", "copy-1")
	requireLevel(t, s, "copy-1", 2)
	askFrom(t, s, "ask-2", "copy-1", "consulted", "copy-2")
	requireLevel(t, s, "copy-2", 3)
	if chain, err := s.DelegationChain("copy-2"); err != nil || strings.Join(chain, ",") != "copy-2,copy-1,worker,lead" {
		t.Fatalf("DelegationChain(copy-2) = %v, %v", chain, err)
	}

	createDraftCleanupThread(t, s, "side-chat", "scratch")
	if err := s.InsertScratchThread(ScratchThread{ThreadID: "side-chat", SourceThreadID: "worker", ReturnMode: "chat"}); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "side-chat", 0)
	if _, err := s.PromoteScratchThread("side-chat"); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "side-chat", 0)
}

// A paired computer's spawn or ask carries its level, and the level is the
// link's: it counts while the thread is owned through that computer's
// link, or is an ask copy, and not while the thread has a sidebar row or a
// group. Show in Sidebar and a group end it, Remove from Sidebar restores
// it, and a released link ends it for good, since nothing can own the
// thread again. A carried level on a thread no link owns counts nothing.
func TestACarriedLevelCountsWhileItsDelegationLasts(t *testing.T) {
	s := newTestStore(t)
	createCarried(t, s, "worker", "chat", 2)
	acceptLink(t, s, "link", "dev-s", "worker", 1000)
	requireLevel(t, s, "worker", 2)

	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "worker", 0)
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "worker", 2)

	group, err := s.CreateThreadGroup(defaultTestProjectID, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"worker"}, group.ID); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "worker", 0)
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementOwned); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "worker", 2)

	if _, err := s.ReleaseSpawnLink("link", "dev-s", "lead-link"); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "worker", 0)
	if _, err := s.SetThreadPlacement([]string{"worker"}, PlacementOwned); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owning a thread whose link ended = %v", err)
	}
	requireLevel(t, s, "worker", 0)

	createCarried(t, s, "unlinked", "chat", 2)
	requireLevel(t, s, "unlinked", 0)

	createCarried(t, s, "remote-ask", "scratch", 2)
	if err := s.InsertScratchThread(ScratchThread{ThreadID: "remote-ask", SourceThreadID: "worker", ReturnMode: "chat", RequestToken: "their-token"}); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "remote-ask", 2)
}
