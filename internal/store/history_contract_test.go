package store

import (
	"database/sql"
	"errors"
	"testing"
)

// historyStampOf reads a thread's stamps straight from SQL so the
// contract tests never depend on the accessor they are checking.
func historyStampOf(t *testing.T, s *Store, threadID string) HistoryStamp {
	t.Helper()
	var stamp HistoryStamp
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = ?`, threadID,
	).Scan(&stamp.Rev, &stamp.Epoch); err != nil {
		t.Fatalf("read history stamps for %s: %v", threadID, err)
	}
	return stamp
}

func seedContractThread(t *testing.T, s *Store, threadID string) {
	t.Helper()
	mustCreateThread(t, s, threadID)
	if err := s.InsertTurn(Turn{
		TurnID: threadID + ":0", ThreadID: threadID, TurnIndex: 0, StartedAt: 1000,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
}

func contractItem(threadID, id string, itemIndex int) Item {
	return Item{
		ID: id, ThreadID: threadID, TurnIndex: 0, ItemIndex: itemIndex,
		Kind: "assistant_text", Role: "assistant", Status: "completed",
		Summary: "seed", CreatedAt: 1000, UpdatedAt: 1000,
	}
}

// TestHistoryContractTransitions walks each operation class of design
// §3.2 as a SEQUENCE — insert, then update the same row's content, then
// reposition it, then delete it — asserting the (rev, epoch) DELTA each
// step produces. State coverage would miss exactly the bug this exists to
// prevent: an operation that reaches the right end state without ever
// advancing the counter a client compares against.
func TestHistoryContractTransitions(t *testing.T) {
	type step struct {
		name      string
		run       func(t *testing.T, s *Store)
		wantRev   int64
		wantEpoch int64
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, s *Store)
		steps []step
	}{
		{
			name:  "item lifecycle: insert, in-place update, reposition, delete",
			setup: func(t *testing.T, s *Store) { seedContractThread(t, s, "t") },
			steps: []step{
				{
					name: "AppendItem insert",
					run: func(t *testing.T, s *Store) {
						if _, err := s.AppendItem(contractItem("t", "i1", 0)); err != nil {
							t.Fatalf("append item: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "AppendItem second insert",
					run: func(t *testing.T, s *Store) {
						if _, err := s.AppendItem(contractItem("t", "i2", 1)); err != nil {
							t.Fatalf("append second item: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "UpsertItem content update stays in place",
					run: func(t *testing.T, s *Store) {
						item := contractItem("t", "i1", 0)
						item.Summary = "rewritten"
						if _, err := s.UpsertItem(item, nil); err != nil {
							t.Fatalf("upsert item: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "AppendItemSummary streaming append",
					run: func(t *testing.T, s *Store) {
						status := "streaming"
						if err := s.UpdateItemFields("t", "i1", ItemPartialUpdate{Status: &status}); err != nil {
							t.Fatalf("flip to streaming: %v", err)
						}
						if _, err := s.AppendItemSummary("t", "i1", " more", 2000); err != nil {
							t.Fatalf("append summary: %v", err)
						}
					},
					// Two item UPDATEs, neither of them a move.
					wantRev: 2,
				},
				{
					name: "BumpItemToTurnEnd repositions",
					run: func(t *testing.T, s *Store) {
						if _, err := s.BumpItemToTurnEnd("t", "i1", nil, 3000); err != nil {
							t.Fatalf("bump item: %v", err)
						}
					},
					wantRev:   1,
					wantEpoch: 1,
				},
				{
					name: "DeleteThreadItem",
					run: func(t *testing.T, s *Store) {
						if err := s.DeleteThreadItem("t", "i1"); err != nil {
							t.Fatalf("delete item: %v", err)
						}
					},
					wantRev:   1,
					wantEpoch: 1,
				},
			},
		},
		{
			name:  "head insert is additive, not a reposition",
			setup: func(t *testing.T, s *Store) { seedContractThread(t, s, "t") },
			steps: []step{
				{
					name: "AppendItem",
					run: func(t *testing.T, s *Store) {
						if _, err := s.AppendItem(contractItem("t", "i1", 0)); err != nil {
							t.Fatalf("append item: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "UpsertItemAtTurnHead inserts below existing indices",
					run: func(t *testing.T, s *Store) {
						if _, err := s.UpsertItemAtTurnHead(contractItem("t", "head", 0)); err != nil {
							t.Fatalf("upsert at head: %v", err)
						}
					},
					// A row appearing below the window is additive: a stale
					// paint merely lacks it until the next reconcile.
					wantRev: 1,
				},
			},
		},
		{
			name: "payload mutators bump rev through the threadID parameter",
			setup: func(t *testing.T, s *Store) {
				seedContractThread(t, s, "t")
				if _, err := s.UpsertItem(Item{
					ID: "i1", ThreadID: "t", TurnIndex: 0, ItemIndex: 0,
					Kind: "tool_call", Role: "assistant", Status: "completed",
					Summary: "cmd", PayloadID: "p1", CreatedAt: 1000, UpdatedAt: 1000,
				}, &Payload{ID: "p1", Kind: "command_output", Meta: "{}", Data: []byte("out"), CreatedAt: 1000}); err != nil {
					t.Fatalf("seed item+payload: %v", err)
				}
			},
			steps: []step{
				{
					name: "AppendPayloadData",
					run: func(t *testing.T, s *Store) {
						if err := s.AppendPayloadData("t", "p1", []byte(" more"), "{}", 2000); err != nil {
							t.Fatalf("append payload: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "ReplacePayloadData",
					run: func(t *testing.T, s *Store) {
						if err := s.ReplacePayloadData("t", "p1", []byte("final"), "{}", 3000); err != nil {
							t.Fatalf("replace payload: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "UpdatePayloadMeta",
					run: func(t *testing.T, s *Store) {
						if err := s.UpdatePayloadMeta("t", "p1", `{"lines":3}`); err != nil {
							t.Fatalf("update payload meta: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "UpdatePayloadSpans (async span backfill)",
					run: func(t *testing.T, s *Store) {
						if err := s.UpdatePayloadSpans("t", "p1", "preview", "full"); err != nil {
							t.Fatalf("update payload spans: %v", err)
						}
					},
					wantRev: 1,
				},
				{
					name: "UpsertItemWithPayloadAppend does not double-bump",
					run: func(t *testing.T, s *Store) {
						item := contractItem("t", "i1", 0)
						item.Kind = "tool_call"
						item.PayloadID = "p1"
						if _, err := s.UpsertItemWithPayloadAppend(item, "p1", []byte("x"), "{}", 4000); err != nil {
							t.Fatalf("upsert with payload append: %v", err)
						}
					},
					// The item UPDATE's trigger covers the whole
					// transaction; the combined writers must not add an
					// explicit bump on top.
					wantRev: 1,
				},
			},
		},
		{
			name: "conversation cuts bump epoch",
			setup: func(t *testing.T, s *Store) {
				seedContractThread(t, s, "t")
				if err := s.InsertTurn(Turn{TurnID: "t:1", ThreadID: "t", TurnIndex: 1, StartedAt: 2000}); err != nil {
					t.Fatalf("insert turn 1: %v", err)
				}
				for _, spec := range []struct {
					id        string
					turnIndex int
				}{{"i0", 0}, {"i1", 1}} {
					item := contractItem("t", spec.id, 0)
					item.TurnIndex = spec.turnIndex
					if _, err := s.AppendItem(item); err != nil {
						t.Fatalf("seed item %s: %v", spec.id, err)
					}
				}
			},
			steps: []step{
				{
					name: "DeleteConversationFromTurn",
					run: func(t *testing.T, s *Store) {
						deleted, stamp, err := s.DeleteConversationFromTurn("t", 1)
						if err != nil {
							t.Fatalf("delete conversation from turn: %v", err)
						}
						if deleted != 1 {
							t.Fatalf("deleted = %d, want 1", deleted)
						}
						if got := historyStampOf(t, s, "t"); stamp != got {
							t.Fatalf("returned stamp %+v, want the post-cut row %+v", stamp, got)
						}
					},
					wantRev:   1,
					wantEpoch: 1,
				},
				{
					name: "DeleteConversationFromItem",
					run: func(t *testing.T, s *Store) {
						_, stamp, err := s.DeleteConversationFromItem("t", "i0")
						if err != nil {
							t.Fatalf("delete conversation from item: %v", err)
						}
						if got := historyStampOf(t, s, "t"); stamp != got {
							t.Fatalf("returned stamp %+v, want the post-cut row %+v", stamp, got)
						}
					},
					wantRev:   1,
					wantEpoch: 1,
				},
			},
		},
		{
			name: "import batch and crash recovery advance the contract",
			setup: func(t *testing.T, s *Store) {
				mustCreateThread(t, s, "t")
			},
			steps: []step{
				{
					name: "ApplyImportBatch appends two rows",
					run: func(t *testing.T, s *Store) {
						completedAt := int64(1500)
						if err := s.ApplyImportBatch("t", ImportBatch{
							Turns: []Turn{{TurnID: "t:0", ThreadID: "t", TurnIndex: 0, StartedAt: 1000, CompletedAt: &completedAt}},
							Rows: []ImportRow{
								{Item: contractItem("t", "imp0", 0)},
								{Item: contractItem("t", "imp1", 1)},
							},
						}); err != nil {
							t.Fatalf("apply import batch: %v", err)
						}
					},
					wantRev: 2,
				},
				{
					name: "RecoverCrashedTurns settles a stranded streaming row",
					run: func(t *testing.T, s *Store) {
						if err := s.InsertTurn(Turn{TurnID: "t:1", ThreadID: "t", TurnIndex: 1, StartedAt: 3000}); err != nil {
							t.Fatalf("insert crashed turn: %v", err)
						}
						stranded := contractItem("t", "crashed", 0)
						stranded.TurnIndex = 1
						stranded.Status = "streaming"
						if _, err := s.AppendItem(stranded); err != nil {
							t.Fatalf("insert stranded item: %v", err)
						}
						before := historyStampOf(t, s, "t")
						if _, err := s.RecoverCrashedTurns(func(sum string) string { return sum + " — stopped" }, 4000); err != nil {
							t.Fatalf("recover crashed turns: %v", err)
						}
						after := historyStampOf(t, s, "t")
						if after.Rev <= before.Rev {
							t.Fatalf("crash recovery did not advance rev: %d -> %d", before.Rev, after.Rev)
						}
						if after.Epoch != before.Epoch {
							t.Fatalf("crash recovery moved epoch (%d -> %d); it settles rows in place", before.Epoch, after.Epoch)
						}
					},
					// The insert above (+1) plus the recovery flip; the
					// step asserts the recovery half itself.
					wantRev: 2,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			tc.setup(t, s)
			previous := historyStampOf(t, s, "t")
			for _, st := range tc.steps {
				st.run(t, s)
				current := historyStampOf(t, s, "t")
				gotRev := current.Rev - previous.Rev
				gotEpoch := current.Epoch - previous.Epoch
				if gotRev != st.wantRev || gotEpoch != st.wantEpoch {
					t.Fatalf("%s: (rev, epoch) delta = (%d, %d), want (%d, %d)",
						st.name, gotRev, gotEpoch, st.wantRev, st.wantEpoch)
				}
				if gotEpoch > 0 && gotRev == 0 {
					t.Fatalf("%s: epoch advanced without rev; every epoch bump must bump rev", st.name)
				}
				previous = current
			}
		})
	}
}

// seedContractPlan puts a proposed_plan item and its state row on a
// thread, returning the stamps AFTER the seed so a caller measures only
// the mutator under test.
func seedContractPlan(t *testing.T, s *Store, threadID, planItemID string) {
	t.Helper()
	item := contractItem(threadID, planItemID, 0)
	item.Kind = "tool_call"
	item.PayloadID = "payload-" + planItemID
	if _, err := s.UpsertItem(item, &Payload{
		ID: item.PayloadID, Kind: "proposed_plan", Meta: "{}",
		Data: []byte("# plan"), CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("seed plan item: %v", err)
	}
	if _, err := s.EnsureProposedPlanState(threadID, planItemID, 1000); err != nil {
		t.Fatalf("seed plan state: %v", err)
	}
}

// TestHistoryContractProposedPlanMutatorsBumpRev covers the operation
// class §3.2 calls window-visible DECORATION: `proposed_plans` and
// `proposed_plan_comments` never touch an `items` row, so no trigger sees
// them — but decorateProposedPlanItems rewrites Item.Meta from both on
// EVERY window read, SyncThreadWindow included. A mutator that skipped
// the bump would leave a client's stale plan card reading as fresh
// forever.
//
// Sequenced rather than state-checked, and every step asserts an exact
// (rev, epoch) delta: the failure this guards against is a mutator that
// reaches the right row state without ever advancing the counter.
func TestHistoryContractProposedPlanMutatorsBumpRev(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	seedContractPlan(t, s, "t", "plan-1")

	previous := historyStampOf(t, s, "t")
	step := func(name string, wantRev int64, run func()) {
		t.Helper()
		run()
		current := historyStampOf(t, s, "t")
		gotRev := current.Rev - previous.Rev
		gotEpoch := current.Epoch - previous.Epoch
		if gotRev != wantRev || gotEpoch != 0 {
			t.Fatalf("%s: (rev, epoch) delta = (%d, %d), want (%d, 0)", name, gotRev, gotEpoch, wantRev)
		}
		previous = current
	}

	// The seed's EnsureProposedPlanState already bumped once (measured by
	// the plan-state case below); a replay of it writes nothing.
	step("EnsureProposedPlanState replay is idempotent", 0, func() {
		if _, err := s.EnsureProposedPlanState("t", "plan-1", 2000); err != nil {
			t.Fatalf("ensure plan state replay: %v", err)
		}
	})

	step("EnsureProposedPlanState insert", 1, func() {
		item := contractItem("t", "plan-2", 1)
		item.Kind = "tool_call"
		if _, err := s.UpsertItem(item, nil); err != nil {
			t.Fatalf("seed second plan item: %v", err)
		}
		previous = historyStampOf(t, s, "t") // discount the item write
		if _, err := s.EnsureProposedPlanStateWithParent("t", "plan-2", "plan-1", 2000); err != nil {
			t.Fatalf("ensure plan state with parent: %v", err)
		}
	})

	step("CreateProposedPlanComment", 1, func() {
		if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
			ID: "c1", ThreadID: "t", PlanItemID: "plan-1",
			StartLine: 1, EndLine: 2, Body: "tighten this",
			CreatedAt: 2000, UpdatedAt: 2000,
		}); err != nil {
			t.Fatalf("create plan comment: %v", err)
		}
	})

	step("UpdateProposedPlanComment", 1, func() {
		if _, err := s.UpdateProposedPlanComment("t", "c1", ProposedPlanCommentUpdate{Body: "revised"}, 3000); err != nil {
			t.Fatalf("update plan comment: %v", err)
		}
	})

	step("MarkProposedPlanCommentsSent", 1, func() {
		if err := s.MarkProposedPlanCommentsSent("t", "plan-1", []string{"c1"}, 4000, "turn-1"); err != nil {
			t.Fatalf("mark comments sent: %v", err)
		}
	})

	step("MarkProposedPlanCommentsSent replay writes nothing", 0, func() {
		if err := s.MarkProposedPlanCommentsSent("t", "plan-1", []string{"c1"}, 4000, "turn-1"); err != nil {
			t.Fatalf("replay mark comments sent: %v", err)
		}
	})

	step("DeleteOrResolveProposedPlanComment resolves a sent comment", 1, func() {
		if err := s.DeleteOrResolveProposedPlanComment("t", "c1", 5000); err != nil {
			t.Fatalf("resolve plan comment: %v", err)
		}
	})

	step("DeleteOrResolveProposedPlanComment deletes a draft", 1, func() {
		if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
			ID: "c2", ThreadID: "t", PlanItemID: "plan-1",
			StartLine: 3, EndLine: 3, Body: "draft note",
			CreatedAt: 5000, UpdatedAt: 5000,
		}); err != nil {
			t.Fatalf("create draft comment: %v", err)
		}
		previous = historyStampOf(t, s, "t") // discount the create
		if err := s.DeleteOrResolveProposedPlanComment("t", "c2", 6000); err != nil {
			t.Fatalf("delete draft comment: %v", err)
		}
	})

	step("MarkProposedPlanImplemented", 1, func() {
		if err := s.MarkProposedPlanImplemented("t", "plan-1", "t", "impl-item", 7000); err != nil {
			t.Fatalf("mark plan implemented: %v", err)
		}
	})

	step("MarkProposedPlanImplemented replay writes nothing", 0, func() {
		if err := s.MarkProposedPlanImplemented("t", "plan-1", "t", "impl-item", 8000); err != nil {
			t.Fatalf("replay mark plan implemented: %v", err)
		}
	})
}

// TestHistoryContractProposedPlanImplementedBumpsPlanThread pins the
// cross-thread half: app_send.go marks a plan implemented from ANOTHER
// thread (the one doing the work), passing the plan's own thread id. The
// decoration lands on the plan item, so the plan's thread is the one that
// must move — and the implementing thread must not, or every pane holding
// it re-fetches for nothing.
func TestHistoryContractProposedPlanImplementedBumpsPlanThread(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	seedContractPlan(t, s, "t", "plan-1")
	seedContractThread(t, s, "worker")

	plan := historyStampOf(t, s, "t")
	worker := historyStampOf(t, s, "worker")

	if err := s.MarkProposedPlanImplemented("t", "plan-1", "worker", "worker-item", 5000); err != nil {
		t.Fatalf("mark plan implemented cross-thread: %v", err)
	}

	after := historyStampOf(t, s, "t")
	if after.Rev-plan.Rev != 1 || after.Epoch != plan.Epoch {
		t.Fatalf("plan thread (rev, epoch) delta = (%d, %d), want (1, 0)",
			after.Rev-plan.Rev, after.Epoch-plan.Epoch)
	}
	if got := historyStampOf(t, s, "worker"); got != worker {
		t.Fatalf("implementing thread stamps moved: %+v -> %+v", worker, got)
	}
}

// TestHistoryContractProposedPlanRefusesUnknownThread — the thread id
// these mutators carry is the enforcement, exactly as it is for the
// payload mutators, so a caller naming a thread that isn't there must
// fail loudly rather than write decoration state no stamp describes.
func TestHistoryContractProposedPlanRefusesUnknownThread(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	seedContractPlan(t, s, "t", "plan-1")

	err := s.MarkProposedPlanImplemented("no-such-thread", "plan-1", "t", "i", 5000)
	if err == nil {
		t.Fatal("MarkProposedPlanImplemented accepted an unknown thread id")
	}

	// A plan state insert for an absent thread is refused by the FK, and a
	// bump for one is refused by bumpHistoryRevTx; either way nothing lands.
	if _, err := s.EnsureProposedPlanState("no-such-thread", "plan-9", 5000); err == nil {
		t.Fatal("EnsureProposedPlanState accepted an unknown thread id")
	}
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM proposed_plans WHERE thread_id = 'no-such-thread'`,
	).Scan(&count); err != nil {
		t.Fatalf("count orphan plan rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d proposed_plans rows written for an unknown thread", count)
	}
}

// TestHistoryContractForkCloneBumpsTargetOnly pins the fork row of §3.2:
// cloning history writes rows on the TARGET thread, so only its stamps
// move. A source thread that appeared to change would make every open
// pane holding it re-fetch for nothing.
func TestHistoryContractForkCloneBumpsTargetOnly(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	if _, err := s.AppendItem(contractItem("t", "i1", 0)); err != nil {
		t.Fatalf("seed source item: %v", err)
	}
	mustCreateThread(t, s, "fork")

	source := historyStampOf(t, s, "t")
	target := historyStampOf(t, s, "fork")

	if _, err := s.CloneThreadItems("t", "fork", nil); err != nil {
		t.Fatalf("clone thread items: %v", err)
	}

	if got := historyStampOf(t, s, "t"); got != source {
		t.Fatalf("source stamps moved on a fork: %+v -> %+v", source, got)
	}
	got := historyStampOf(t, s, "fork")
	if got.Rev <= target.Rev {
		t.Fatalf("fork target rev did not advance: %d -> %d", target.Rev, got.Rev)
	}
	if got.Epoch != target.Epoch {
		t.Fatalf("fork target epoch moved (%d -> %d); a clone only inserts", target.Epoch, got.Epoch)
	}
}

// TestHistoryTriggersFireOnRawSQL pins the structural guarantee
// independently of every store function: the contract is enforced by
// SQLite triggers, so a writer that bypasses this package entirely still
// advances it. This is the test that would fail if a future migration
// rebuilt `items` and forgot to recreate the triggers.
func TestHistoryTriggersFireOnRawSQL(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	assertDelta := func(name string, before HistoryStamp, wantRev, wantEpoch int64) HistoryStamp {
		t.Helper()
		after := historyStampOf(t, s, "t")
		if after.Rev-before.Rev != wantRev || after.Epoch-before.Epoch != wantEpoch {
			t.Fatalf("%s: (rev, epoch) delta = (%d, %d), want (%d, %d)",
				name, after.Rev-before.Rev, after.Epoch-before.Epoch, wantRev, wantEpoch)
		}
		return after
	}

	stamp := historyStampOf(t, s, "t")

	exec(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at)
	      VALUES ('raw', 't', 0, 0, 'assistant_text', 'assistant', 'completed', 'raw', 1, 1)`)
	stamp = assertDelta("raw INSERT", stamp, 1, 0)

	exec(`UPDATE items SET summary = 'raw edited' WHERE id = 'raw'`)
	stamp = assertDelta("raw content UPDATE", stamp, 1, 0)

	exec(`UPDATE items SET item_index = 7 WHERE id = 'raw'`)
	stamp = assertDelta("raw item_index UPDATE", stamp, 1, 1)

	exec(`UPDATE items SET turn_index = 1 WHERE id = 'raw'`)
	stamp = assertDelta("raw turn_index UPDATE", stamp, 1, 1)

	// A no-op UPDATE still writes the row, so it still counts: the
	// contract over-reports rather than risk under-reporting.
	exec(`UPDATE items SET item_index = 7 WHERE id = 'raw'`)
	stamp = assertDelta("raw idempotent UPDATE", stamp, 1, 0)

	exec(`DELETE FROM items WHERE id = 'raw'`)
	assertDelta("raw DELETE", stamp, 1, 1)

	// No store code moves a row between threads, but the trigger must not
	// depend on that staying true: a cross-thread UPDATE is a delete from
	// one ordering and an insert into another, so BOTH threads' stamps
	// advance and both take the epoch bump.
	seedContractThread(t, s, "t2")
	exec(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at)
	      VALUES ('mover', 't', 0, 0, 'assistant_text', 'assistant', 'completed', 'mover', 1, 1)`)
	stamp = historyStampOf(t, s, "t")
	stamp2 := historyStampOf(t, s, "t2")
	exec(`UPDATE items SET thread_id = 't2' WHERE id = 'mover'`)
	after := historyStampOf(t, s, "t")
	after2 := historyStampOf(t, s, "t2")
	if after.Rev-stamp.Rev != 1 || after.Epoch-stamp.Epoch != 1 {
		t.Fatalf("cross-thread move, source: (rev, epoch) delta = (%d, %d), want (1, 1)",
			after.Rev-stamp.Rev, after.Epoch-stamp.Epoch)
	}
	if after2.Rev-stamp2.Rev != 1 || after2.Epoch-stamp2.Epoch != 1 {
		t.Fatalf("cross-thread move, target: (rev, epoch) delta = (%d, %d), want (1, 1)",
			after2.Rev-stamp2.Rev, after2.Epoch-stamp2.Epoch)
	}
}

// TestBumpHistoryRevRefusesUnknownThread — the payload mutators' threadID
// is the enforcement, so a caller naming a thread that isn't there must
// fail loudly instead of writing content no stamp will ever describe.
func TestBumpHistoryRevRefusesUnknownThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if err := seedPayloadRow(s, Payload{ID: "p1", Kind: "command_output", Meta: "{}", Data: []byte("x"), CreatedAt: 1}); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	err := s.UpdatePayloadMeta("no-such-thread", "p1", `{"lines":1}`)
	if err == nil {
		t.Fatal("UpdatePayloadMeta accepted an unknown thread id")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdatePayloadMeta(unknown thread) err = %v, want wrapped sql.ErrNoRows", err)
	}
	// The refusal is a rollback, not a partial write.
	meta, err := s.GetPayloadMeta("p1")
	if err != nil {
		t.Fatalf("get payload meta: %v", err)
	}
	if meta.Meta != "{}" {
		t.Fatalf("payload meta = %q, want the write rolled back", meta.Meta)
	}

	if err := s.UpdatePayloadMeta("", "p1", `{"lines":1}`); err == nil {
		t.Fatal("UpdatePayloadMeta accepted an empty thread id")
	}
}
