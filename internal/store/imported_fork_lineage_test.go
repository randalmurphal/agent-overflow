package store

import (
	"strings"
	"testing"
)

func seedImportedSession(
	t *testing.T, s *Store, threadID, providerName, sessionID, parentSessionID string,
) {
	t.Helper()
	thread := importedThread(threadID, providerName)
	thread.SessionRef = sessionID
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create imported thread %s: %v", threadID, err)
	}
	if err := s.SetThreadImportState(ThreadImportState{
		ThreadID:              threadID,
		Provider:              providerName,
		SourceSessionID:       sessionID,
		SourceParentSessionID: parentSessionID,
		LastTurnIndex:         -1,
		LastItemIndex:         -1,
		ImportedAt:            1,
	}); err != nil {
		t.Fatalf("set import state for %s: %v", threadID, err)
	}
}

func requireForkParent(t *testing.T, s *Store, threadID, want string) {
	t.Helper()
	thread, err := s.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	if thread.ForkedFromThreadID != want {
		t.Fatalf("thread %s fork parent = %q, want %q", threadID, thread.ForkedFromThreadID, want)
	}
}

func TestReconcileImportedForkLineageIsImportOrderIndependent(t *testing.T) {
	s := newTestStore(t)

	// Import the deepest child first. Its unresolved provider id is durable,
	// but it must not point at an invented AO thread.
	seedImportedSession(t, s, "thread-c", "claude", "session-c", "session-b")
	warnings, err := s.ReconcileImportedForkLineage("claude", "session-c")
	if err != nil {
		t.Fatalf("reconcile child first: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("child-first warnings = %+v", warnings)
	}
	requireForkParent(t, s, "thread-c", "")

	// A root and two sibling forks may arrive in any worker order.
	seedImportedSession(t, s, "thread-a", "claude", "session-a", "")
	seedImportedSession(t, s, "thread-b2", "claude", "session-b2", "session-a")
	seedImportedSession(t, s, "thread-b", "claude", "session-b", "session-a")
	warnings, err = s.ReconcileImportedForkLineage("claude", "session-b")
	if err != nil {
		t.Fatalf("reconcile complete family: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("complete-family warnings = %+v", warnings)
	}
	requireForkParent(t, s, "thread-a", "")
	requireForkParent(t, s, "thread-b", "thread-a")
	requireForkParent(t, s, "thread-b2", "thread-a")
	requireForkParent(t, s, "thread-c", "thread-b")

	// Idempotence matters because every session imported by Import All runs
	// reconciliation, including calls after the family is already complete.
	warnings, err = s.ReconcileImportedForkLineage("claude", "session-b")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("idempotent reconcile = warnings:%+v err:%v", warnings, err)
	}
	requireForkParent(t, s, "thread-c", "thread-b")

	// Deletion clears SQLite's resolved FK, while provider provenance remains.
	// Re-importing the parent under a new AO id restores every descendant edge.
	if err := s.DeleteThread("thread-a"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if _, err := s.ReconcileImportedForkLineage("claude", "session-a"); err != nil {
		t.Fatalf("reconcile after parent deletion: %v", err)
	}
	requireForkParent(t, s, "thread-b", "")
	requireForkParent(t, s, "thread-b2", "")
	seedImportedSession(t, s, "thread-a-new", "claude", "session-a", "")
	if _, err := s.ReconcileImportedForkLineage("claude", "session-a"); err != nil {
		t.Fatalf("reconcile re-imported parent: %v", err)
	}
	requireForkParent(t, s, "thread-b", "thread-a-new")
	requireForkParent(t, s, "thread-b2", "thread-a-new")
}

func TestReconcileImportedForkLineageResolvesAONativeClaudeParent(t *testing.T) {
	s := newTestStore(t)
	parent := makeThread("native-parent", "claude-tui")
	parent.SessionRef = "provider-parent"
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("create AO-native parent: %v", err)
	}
	seedImportedSession(t, s, "imported-child", "claude", "provider-child", "provider-parent")

	warnings, err := s.ReconcileImportedForkLineage("claude", "provider-child")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("reconcile native parent = warnings:%+v err:%v", warnings, err)
	}
	requireForkParent(t, s, "imported-child", "native-parent")
}

func TestReconcileImportedForkLineageRejectsUnsafeMetadataWithoutLosingHistory(t *testing.T) {
	t.Run("self parent", func(t *testing.T) {
		s := newTestStore(t)
		seedImportedSession(t, s, "self", "claude", "session-self", "session-self")
		warnings, err := s.ReconcileImportedForkLineage("claude", "session-self")
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(warnings) != 1 || warnings[0].Code != ImportedForkLineageSelf {
			t.Fatalf("warnings = %+v, want self warning", warnings)
		}
		requireForkParent(t, s, "self", "")
	})

	t.Run("cycle", func(t *testing.T) {
		s := newTestStore(t)
		seedImportedSession(t, s, "cycle-a", "codex", "session-a", "session-b")
		seedImportedSession(t, s, "cycle-b", "codex", "session-b", "session-a")
		warnings, err := s.ReconcileImportedForkLineage("codex", "session-b")
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(warnings) != 2 || warnings[0].Code != ImportedForkLineageCycle ||
			warnings[1].Code != ImportedForkLineageCycle {
			t.Fatalf("warnings = %+v, want two cycle warnings", warnings)
		}
		requireForkParent(t, s, "cycle-a", "")
		requireForkParent(t, s, "cycle-b", "")
	})

	t.Run("ambiguous parent", func(t *testing.T) {
		s := newTestStore(t)
		for _, threadID := range []string{"claim-a", "claim-b"} {
			claim := makeThread(threadID, "claude")
			claim.SessionRef = "shared-parent"
			if err := s.CreateThread(claim); err != nil {
				t.Fatalf("create claimant %s: %v", threadID, err)
			}
		}
		seedImportedSession(t, s, "ambiguous-child", "claude", "child", "shared-parent")
		warnings, err := s.ReconcileImportedForkLineage("claude", "child")
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if len(warnings) != 1 || warnings[0].Code != ImportedForkLineageAmbiguous {
			t.Fatalf("warnings = %+v, want ambiguous warning", warnings)
		}
		requireForkParent(t, s, "ambiguous-child", "")
		// The warning concerns only the edge: all three histories remain.
		if threads, err := s.ListThreads(); err != nil || len(threads) != 3 {
			t.Fatalf("threads after ambiguous lineage = %d, err %v", len(threads), err)
		}
	})
}

func TestSetThreadImportStatePreventsNewDuplicateProviderSessionClaims(t *testing.T) {
	s := newTestStore(t)
	seedImportedSession(t, s, "first", "claude", "same-session", "")

	second := importedThread("second", "claude")
	second.SessionRef = "different-session"
	if err := s.CreateThread(second); err != nil {
		t.Fatalf("create second thread: %v", err)
	}
	err := s.SetThreadImportState(ThreadImportState{
		ThreadID: "second", Provider: "claude", SourceSessionID: "same-session", ImportedAt: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "provider session is already claimed") {
		t.Fatalf("duplicate source claim error = %v", err)
	}

	// Provider is part of the identity: UUID/string collisions across homes
	// must not make one provider's conversation hide the other's.
	codex := importedThread("codex", "codex")
	codex.SessionRef = "same-session"
	if err := s.CreateThread(codex); err != nil {
		t.Fatalf("create cross-provider thread: %v", err)
	}
	if err := s.SetThreadImportState(ThreadImportState{
		ThreadID: "codex", Provider: "codex", SourceSessionID: "same-session", ImportedAt: 2,
	}); err != nil {
		t.Fatalf("cross-provider source claim: %v", err)
	}

	// A cursor refresh updates the existing claim rather than looking like a
	// second import of the same source.
	state, ok, err := s.GetThreadImportState("first")
	if err != nil || !ok {
		t.Fatalf("get first state = ok:%v err:%v", ok, err)
	}
	state.LastSourceUUID = "later"
	if err := s.SetThreadImportState(state); err != nil {
		t.Fatalf("refresh existing claim: %v", err)
	}
}
