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
	got, err := s.GetThread("worker")
	if err != nil || got.SpawnInstructions != "Review only." || got.SpawnLevel != 2 {
		t.Fatalf("the stored worker = %q, %d, %v", got.SpawnInstructions, got.SpawnLevel, err)
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

// A paired computer's spawn or ask carries its level to a thread that has
// no link here, and that level ends where its delegation does: when the
// thread is filed in a group or made standalone. Nothing puts it back:
// owning the thread needs a spawn link here, which it does not have. An ask
// copy's carried level counts like a spawn's.
func TestACarriedLevelEndsWhereItsDelegationEnds(t *testing.T) {
	s := newTestStore(t)
	carried := func(id, mode string) {
		thread := makeThread(id, "claude")
		thread.Mode, thread.SpawnLevel = mode, 2
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		requireLevel(t, s, id, 2)
	}
	carried("grouped", "chat")
	group, err := s.CreateThreadGroup(defaultTestProjectID, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetThreadGroup([]string{"grouped"}, group.ID); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "grouped", 0)

	carried("standalone", "chat")
	if _, err := s.SetThreadPlacement([]string{"standalone"}, PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "standalone", 0)
	if _, err := s.SetThreadPlacement([]string{"standalone"}, PlacementOwned); !errors.Is(err, ErrThreadNotSpawned) {
		t.Fatalf("owning a thread no spawn here made = %v", err)
	}
	requireLevel(t, s, "standalone", 0)

	carried("remote-ask", "scratch")
	if err := s.InsertScratchThread(ScratchThread{ThreadID: "remote-ask", SourceThreadID: "standalone", ReturnMode: "chat", RequestToken: "their-token"}); err != nil {
		t.Fatal(err)
	}
	requireLevel(t, s, "remote-ask", 2)
}
