package codex

import "testing"

// collab_agents_generations_test.go — the per-child resume-generation counter
// is session-scoped state on a long-lived process, so it has to be bounded by
// the same ownership teardown as every other collab map.
//
// The counter exists because a mailbox delivery's row identity mixes it in
// (triage's codexMailboxCompletionID): a child that legitimately answers the
// same thing twice needs two rows. It is keyed by the child's CANONICAL name,
// which is why the teardown has to resolve that name before it drops the
// mapping that resolves it.

// TestClosingAChildDropsItsTurnGenerationCounter: `close_agent` tears the
// child's ownership down, and the counter it minted ordinals from goes with
// it. Without this a v1 session that spawns and closes uniquely named children
// grows this map for the life of the process — the unbounded growth every
// other collab map is cleaned to avoid, and a doc comment that promised a
// boundedness the code did not provide.
func TestClosingAChildDropsItsTurnGenerationCounter(t *testing.T) {
	s := &Session{}
	if !s.registerChildOwnership("root-thread", "child-1", "/root/reviewer", "spawn-1") {
		t.Fatal("registerChildOwnership refused the fixture")
	}

	s.advanceChildTurnGeneration("child-1")
	s.advanceChildTurnGeneration("child-1")
	if got := s.childTurnGeneration("/root/reviewer"); got != 2 {
		t.Fatalf("generation = %d, want 2 before the teardown", got)
	}

	s.deleteParentToolUseForProviderThread("child-1")

	s.mu.Lock()
	remaining := len(s.collab.childTurnGenerations)
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("childTurnGenerations retains %d entries after close_agent: %v",
			remaining, s.collab.childTurnGenerations)
	}
	// Zero is the documented legal answer for a child this session never
	// watched start, and after the teardown there is no parent card left for
	// a later delivery to reach anyway.
	if got := s.childTurnGeneration("/root/reviewer"); got != 0 {
		t.Fatalf("generation = %d after the teardown, want 0", got)
	}
}

// The teardown has to resolve the counter's key BEFORE it drops
// agentPathByThread. The counter is keyed on the canonical agent path where
// there is one, so deleting the mapping first leaves the entry unreachable —
// permanent, and invisible to any later lookup, which is the worst version of
// the leak because nothing can ever name it again.
func TestClosingAChildResolvesTheGenerationKeyBeforeDroppingItsPath(t *testing.T) {
	s := &Session{}
	if !s.registerChildOwnership("root-thread", "child-1", "/root/reviewer", "spawn-1") {
		t.Fatal("registerChildOwnership refused the fixture")
	}
	s.advanceChildTurnGeneration("child-1")

	s.mu.Lock()
	_, keyedByPath := s.collab.childTurnGenerations["/root/reviewer"]
	s.mu.Unlock()
	if !keyedByPath {
		t.Fatalf("counter is not keyed on the canonical agent path: %v", s.collab.childTurnGenerations)
	}

	s.deleteParentToolUseForProviderThread("child-1")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, orphaned := s.collab.childTurnGenerations["/root/reviewer"]; orphaned {
		t.Fatal("the path-keyed counter outlived the path mapping that names it")
	}
}

// A child with no canonical agent path (older unnamed-agent builds name the
// thread id in both places) has to be torn down too.
func TestClosingAnUnnamedChildDropsItsTurnGenerationCounter(t *testing.T) {
	s := &Session{}
	if !s.registerChildOwnership("root-thread", "child-1", "", "spawn-1") {
		t.Fatal("registerChildOwnership refused the fixture")
	}
	s.advanceChildTurnGeneration("child-1")
	if got := s.childTurnGeneration("child-1"); got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}

	s.deleteParentToolUseForProviderThread("child-1")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.collab.childTurnGenerations) != 0 {
		t.Fatalf("childTurnGenerations retains %v for an unnamed child", s.collab.childTurnGenerations)
	}
}
