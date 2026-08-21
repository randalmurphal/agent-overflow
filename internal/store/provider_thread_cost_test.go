package store

import "testing"

// mustCreateThreadWithSessionRef seeds a thread already pointing at a provider
// thread. Every provider-cost row names the provider thread it describes, and
// a read only answers while that still matches the thread's own session ref
// (migration v68), so a session-ref-less thread can hold no readable row.
func mustCreateThreadWithSessionRef(t *testing.T, s *Store, id, sessionRef string) {
	t.Helper()
	thread := makeThread(id, "codex")
	thread.SessionRef = sessionRef
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

// mustRepointThread moves a thread onto a different provider thread (or off
// one entirely, with ""), the way a Codex rollback does.
func mustRepointThread(t *testing.T, s *Store, id, sessionRef string) {
	t.Helper()
	thread, err := s.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", id, err)
	}
	thread.SessionRef = sessionRef
	if err := s.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread(%s): %v", id, err)
	}
}

// TestProviderThreadCost_UpsertReadsBackTheNewestAnswer covers the table's
// whole contract: one row per thread, replaced in place, micros round-tripped
// exactly, and a miss reported as a state (found=false) rather than an error.
func TestProviderThreadCost_UpsertReadsBackTheNewestAnswer(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-1", "codex-thread-1")

	if _, found, err := s.GetProviderThreadCost("thread-cost-1"); err != nil || found {
		t.Fatalf("GetProviderThreadCost before any write = (found %v, err %v), want (false, nil)", found, err)
	}

	first := ProviderThreadCost{
		ThreadID:      "thread-cost-1",
		SessionRef:    "codex-thread-1",
		Provider:      "codex",
		CostSource:    ProviderThreadCostSourceEstimate,
		CostUSDMicros: 137500,
		CreditsMicros: 4200000,
		UpdatedAt:     1000,
	}
	if err := s.PutProviderThreadCost(first); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}
	got, found, err := s.GetProviderThreadCost("thread-cost-1")
	if err != nil || !found {
		t.Fatalf("GetProviderThreadCost = (found %v, err %v), want (true, nil)", found, err)
	}
	if got != first {
		t.Fatalf("read back %+v, want %+v", got, first)
	}
	if got.CostUSD() != 0.1375 {
		t.Fatalf("CostUSD() = %v, want 0.1375", got.CostUSD())
	}

	// A later read of the same thread restates a larger cumulative total. It
	// must REPLACE, never accumulate: adding a cumulative figure to itself is
	// exactly the arithmetic this table exists to keep out of usage_ledger.
	second := first
	second.CostUSDMicros = 290000
	second.CreditsMicros = 8800000
	second.UpdatedAt = 2000
	if err := s.PutProviderThreadCost(second); err != nil {
		t.Fatalf("PutProviderThreadCost (second): %v", err)
	}
	got, _, err = s.GetProviderThreadCost("thread-cost-1")
	if err != nil {
		t.Fatalf("GetProviderThreadCost (second): %v", err)
	}
	if got != second {
		t.Fatalf("read back %+v, want %+v", got, second)
	}
}

// TestProviderThreadCost_DefaultsTheCostSource keeps "provider-estimate" from
// having to be spelled at every call site while the CHECK still forbids a
// third value.
func TestProviderThreadCost_DefaultsTheCostSource(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-2", "codex-thread-2")

	if err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-2",
		SessionRef:    "codex-thread-2",
		Provider:      "codex",
		CostUSDMicros: 1,
		UpdatedAt:     1,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}
	got, _, err := s.GetProviderThreadCost("thread-cost-2")
	if err != nil {
		t.Fatalf("GetProviderThreadCost: %v", err)
	}
	if got.CostSource != ProviderThreadCostSourceEstimate {
		t.Fatalf("CostSource = %q, want %q", got.CostSource, ProviderThreadCostSourceEstimate)
	}
}

// TestProviderThreadCost_UnknownThreadIsNotAnError pins the deletion race: the
// estimate is read asynchronously after a turn settles, so the thread can be
// gone by the time it lands. The write must no-op rather than surface an FK
// violation a caller would have to pattern-match to ignore.
func TestProviderThreadCost_UnknownThreadIsNotAnError(t *testing.T) {
	s := newTestStore(t)

	if err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-that-was-deleted",
		SessionRef:    "codex-thread-gone",
		Provider:      "codex",
		CostUSDMicros: 500,
		UpdatedAt:     1,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost for a missing thread: %v", err)
	}
	if _, found, err := s.GetProviderThreadCost("thread-that-was-deleted"); err != nil || found {
		t.Fatalf("wrote a row for a thread that does not exist (found %v, err %v)", found, err)
	}
}

// TestProviderThreadCost_CascadesWithTheThread is the other half of the
// lifecycle decision: unlike usage_ledger (deliberately FK-free so lifetime
// account totals survive a deletion), this row only ever renders ONE thread's
// own cost, so it has nothing to say once that thread is gone.
func TestProviderThreadCost_CascadesWithTheThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-3", "codex-thread-3")
	if err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-3",
		SessionRef:    "codex-thread-3",
		Provider:      "codex",
		CostUSDMicros: 42,
		UpdatedAt:     1,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}
	if err := s.DeleteThread("thread-cost-3"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if _, found, err := s.GetProviderThreadCost("thread-cost-3"); err != nil || found {
		t.Fatalf("row survived its thread (found %v, err %v)", found, err)
	}
}

// TestProviderThreadCost_RefusesAnUnknownCostSource is what forces a future
// second producer to declare itself rather than borrowing this one's label.
func TestProviderThreadCost_RefusesAnUnknownCostSource(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-4", "codex-thread-4")

	err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-4",
		SessionRef:    "codex-thread-4",
		Provider:      "codex",
		CostSource:    "wire",
		CostUSDMicros: 1,
		UpdatedAt:     1,
	})
	if err == nil {
		t.Fatal("PutProviderThreadCost accepted a cost_source outside the CHECK")
	}
}

// TestProviderThreadCost_EmptyThreadIDIsRefused keeps the empty string from
// becoming a real key. It is the unattributed marker everywhere else in this
// schema, and a row under it would be read as some thread's cost.
func TestProviderThreadCost_EmptyThreadIDIsRefused(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutProviderThreadCost(ProviderThreadCost{Provider: "codex", SessionRef: "codex-thread-x"}); err == nil {
		t.Fatal("PutProviderThreadCost accepted an empty thread id")
	}
	if _, found, err := s.GetProviderThreadCost(""); err != nil || found {
		t.Fatalf("GetProviderThreadCost(\"\") = (found %v, err %v), want (false, nil)", found, err)
	}
}

// TestProviderThreadCost_IgnoresARowForAnotherProviderThread is the v68 fix
// (defect L1): a row that names a provider thread the AO thread no longer
// points at is not an answer about this thread, whether or not the rollback's
// delete ever landed.
//
// Both rollback shapes are covered, because they invalidate for different
// reasons. A FORK repoints the thread onto a new Codex thread carrying a
// shorter history; a rollback to turn 0 clears the reference entirely and the
// thread has no provider cost at all until it starts one. Neither may serve
// the old thread's lifetime total.
//
// Before the fix this row was readable and only an in-memory "the delete
// failed" mark stood in front of it, so the stale number came back the moment
// the process restarted.
func TestProviderThreadCost_IgnoresARowForAnotherProviderThread(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repointTo  string
		threadID   string
		sessionRef string
	}{
		{"fork repoints the thread", "codex-thread-forked", "thread-cost-fork", "codex-thread-original"},
		{"rollback to turn 0 clears the ref", "", "thread-cost-cleared", "codex-thread-original"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			mustCreateThreadWithSessionRef(t, s, tc.threadID, tc.sessionRef)
			if err := s.PutProviderThreadCost(ProviderThreadCost{
				ThreadID:      tc.threadID,
				SessionRef:    tc.sessionRef,
				Provider:      "codex",
				CostUSDMicros: 9_000_000,
				UpdatedAt:     1,
			}); err != nil {
				t.Fatalf("PutProviderThreadCost: %v", err)
			}
			if _, found, err := s.GetProviderThreadCost(tc.threadID); err != nil || !found {
				t.Fatalf("row unreadable before the rollback (found %v, err %v)", found, err)
			}

			// The rollback repoints the thread. The row is deliberately NOT
			// deleted here: this is the failed-delete case, which is the whole
			// point of storing the identity.
			mustRepointThread(t, s, tc.threadID, tc.repointTo)

			got, found, err := s.GetProviderThreadCost(tc.threadID)
			if err != nil {
				t.Fatalf("GetProviderThreadCost after the rollback: %v", err)
			}
			if found {
				t.Fatalf("served $%v read from %q against a thread now on %q",
					got.CostUSD(), got.SessionRef, tc.repointTo)
			}
		})
	}
}

// TestProviderThreadCost_ANewReadReclaimsTheRow completes the lifecycle the
// test above leaves open: once the thread's next settled turn prices the NEW
// provider thread, the same row is rewritten under the new identity and reads
// again. The stale row costs one rate-table answer, not a permanent one.
func TestProviderThreadCost_ANewReadReclaimsTheRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-reclaim", "codex-thread-before")
	if err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-reclaim",
		SessionRef:    "codex-thread-before",
		Provider:      "codex",
		CostUSDMicros: 9_000_000,
		UpdatedAt:     1,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost: %v", err)
	}
	mustRepointThread(t, s, "thread-cost-reclaim", "codex-thread-after")

	if err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-reclaim",
		SessionRef:    "codex-thread-after",
		Provider:      "codex",
		CostUSDMicros: 25_000,
		UpdatedAt:     2,
	}); err != nil {
		t.Fatalf("PutProviderThreadCost (after the fork): %v", err)
	}
	got, found, err := s.GetProviderThreadCost("thread-cost-reclaim")
	if err != nil || !found {
		t.Fatalf("GetProviderThreadCost after the re-read = (found %v, err %v)", found, err)
	}
	if got.CostUSDMicros != 25_000 || got.SessionRef != "codex-thread-after" {
		t.Fatalf("read back %+v, want the new provider thread's own total", got)
	}
}

// TestProviderThreadCost_EmptySessionRefIsRefused pins the one comparison that
// must never succeed. A blank stored identity would equal the cleared
// session_ref of every thread rolled back to turn 0, which is exactly the row
// v68 exists to stop serving — so the writer refuses a row it cannot name
// rather than storing an unattributed one.
func TestProviderThreadCost_EmptySessionRefIsRefused(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithSessionRef(t, s, "thread-cost-unnamed", "")

	err := s.PutProviderThreadCost(ProviderThreadCost{
		ThreadID:      "thread-cost-unnamed",
		Provider:      "codex",
		CostUSDMicros: 1,
		UpdatedAt:     1,
	})
	if err == nil {
		t.Fatal("PutProviderThreadCost stored a row that names no provider thread")
	}
	if _, found, err := s.GetProviderThreadCost("thread-cost-unnamed"); err != nil || found {
		t.Fatalf("an unnamed row became readable (found %v, err %v)", found, err)
	}
}
