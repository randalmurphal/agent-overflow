package store

import (
	"strings"
	"testing"
)

// itemRevisionOf reads one row's stamp straight from SQL, for the same
// reason historyStampOf does: these tests check the column, so they must
// not read it through an accessor that could be projecting it.
func itemRevisionOf(t *testing.T, s *Store, threadID, itemID string) int64 {
	t.Helper()
	var rev int64
	if err := s.db.QueryRow(
		`SELECT rev FROM items WHERE thread_id = ? AND id = ?`, threadID, itemID,
	).Scan(&rev); err != nil {
		t.Fatalf("read rev for %s/%s: %v", threadID, itemID, err)
	}
	return rev
}

// TestItemRevisionTriggerArithmetic pins the exact numbers the triggers
// produce, not just "it moved". The stamp a client compares is the thread
// counter, and the stamp it compares per row is this column; if one insert
// ever costs two bumps (a trigger firing another trigger's stamping
// UPDATE) the two are still self-consistent and nothing else would notice.
func TestItemRevisionTriggerArithmetic(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")

	base := historyStampOf(t, s, "t").Rev

	if err := s.InsertItem(contractItem("t", "row", 0)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := historyStampOf(t, s, "t").Rev; got != base+1 {
		t.Fatalf("history_rev after insert = %d, want %d", got, base+1)
	}
	if got := itemRevisionOf(t, s, "t", "row"); got != base+1 {
		t.Fatalf("rev after insert = %d, want %d", got, base+1)
	}

	if err := s.UpdateItemMeta("t", "row", `{"a":1}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := historyStampOf(t, s, "t").Rev; got != base+2 {
		t.Fatalf("history_rev after update = %d, want %d", got, base+2)
	}
	if got := itemRevisionOf(t, s, "t", "row"); got != base+2 {
		t.Fatalf("rev after update = %d, want %d", got, base+2)
	}

	// A child insert stamps the PARENT too: a page renders the anchor
	// from its descendants (decorateSubagentAnchors), so the anchor's read
	// result changed even though nothing wrote to its row.
	child := contractItem("t", "child", 1)
	child.ParentID = "row"
	if err := s.InsertItem(child); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	if got := historyStampOf(t, s, "t").Rev; got != base+3 {
		t.Fatalf("history_rev after child insert = %d, want %d", got, base+3)
	}
	if got := itemRevisionOf(t, s, "t", "child"); got != base+3 {
		t.Fatalf("child rev = %d, want %d", got, base+3)
	}
	if got := itemRevisionOf(t, s, "t", "row"); got != base+3 {
		t.Fatalf("parent rev after child insert = %d, want %d", got, base+3)
	}

	// A child update stamps the parent as well.
	if err := s.UpdateItemMeta("t", "child", `{"b":2}`); err != nil {
		t.Fatalf("update child: %v", err)
	}
	if got := itemRevisionOf(t, s, "t", "row"); got != base+4 {
		t.Fatalf("parent rev after child update = %d, want %d", got, base+4)
	}

	// And a child delete: the anchor's descendant summary shrinks.
	if err := s.DeleteThreadItem("t", "child"); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	stamp := historyStampOf(t, s, "t")
	if stamp.Rev != base+5 {
		t.Fatalf("history_rev after delete = %d, want %d", stamp.Rev, base+5)
	}
	if got := itemRevisionOf(t, s, "t", "row"); got != base+5 {
		t.Fatalf("parent rev after child delete = %d, want %d", got, base+5)
	}
}

// TestItemRevisionAdvancesForEveryItemWriter is the coverage the window
// digest rests on: `fresh` over a held window is only sound if there is no
// exported way to change a row without moving its revision. Each case
// drives one writer and asserts the touched row's stamp advanced, plus —
// for the writers that return the row — that the returned copy carries the
// new value rather than the one it read on the way in.
func TestItemRevisionAdvancesForEveryItemWriter(t *testing.T) {
	runningToolCall := func(id string, index int) Item {
		item := contractItem("t", id, index)
		item.Kind = "tool_call"
		item.Status = "running"
		item.ToolName = "Bash"
		return item
	}
	cases := []struct {
		name string
		// seed replaces the default single-row seed when a writer needs a
		// particular row shape.
		seed func(t *testing.T, s *Store)
		// run performs the write and returns the row the writer handed
		// back, or nil for the writers that return nothing.
		run    func(t *testing.T, s *Store) *Item
		itemID string
	}{
		{
			name:   "UpsertItem update",
			itemID: "row",
			run: func(t *testing.T, s *Store) *Item {
				item := contractItem("t", "row", 0)
				item.Summary = "changed"
				got, err := s.UpsertItem(item, nil)
				if err != nil {
					t.Fatalf("upsert item: %v", err)
				}
				return &got
			},
		},
		{
			name:   "UpsertItemWithInputPayload",
			itemID: "row",
			run: func(t *testing.T, s *Store) *Item {
				item := contractItem("t", "row", 0)
				item.Summary = "with input"
				got, err := s.UpsertItemWithInputPayload(item, nil, &Payload{
					ID: "in", Kind: "tool_input", Meta: "{}", Data: []byte("x"), CreatedAt: 2000,
				})
				if err != nil {
					t.Fatalf("upsert with input payload: %v", err)
				}
				return &got
			},
		},
		{
			name:   "UpsertItemAtTurnHead",
			itemID: "head",
			run: func(t *testing.T, s *Store) *Item {
				got, err := s.UpsertItemAtTurnHead(contractItem("t", "head", 0))
				if err != nil {
					t.Fatalf("upsert at head: %v", err)
				}
				return &got
			},
		},
		{
			name:   "AppendItemSummary",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				item := contractItem("t", "row", 0)
				item.Status = "streaming"
				if err := s.InsertItem(item); err != nil {
					t.Fatalf("seed streaming row: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				got, err := s.AppendItemSummary("t", "row", " more", 3000)
				if err != nil {
					t.Fatalf("append summary: %v", err)
				}
				return &got
			},
		},
		{
			name:   "AppendItemSummaryTail",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				item := contractItem("t", "row", 0)
				item.Status = "streaming"
				if err := s.InsertItem(item); err != nil {
					t.Fatalf("seed streaming row: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				got, err := s.AppendItemSummaryTail("t", "row", " more", 8, 3000)
				if err != nil {
					t.Fatalf("append summary tail: %v", err)
				}
				return &got
			},
		},
		{
			name:   "UpdateItemMeta",
			itemID: "row",
			run: func(t *testing.T, s *Store) *Item {
				if err := s.UpdateItemMeta("t", "row", `{"k":1}`); err != nil {
					t.Fatalf("update item meta: %v", err)
				}
				return nil
			},
		},
		{
			name:   "UpdateItemMetaMerge",
			itemID: "row",
			run: func(t *testing.T, s *Store) *Item {
				got, changed, err := s.UpdateItemMetaMerge("t", "row", func(string) (string, error) {
					return `{"merged":true}`, nil
				}, 3000)
				if err != nil || !changed {
					t.Fatalf("merge item meta: changed=%v err=%v", changed, err)
				}
				return &got
			},
		},
		{
			// UpdateItemFields returns the revision rather than the row,
			// because the wire patch it feeds carries fields, not a row.
			// Same rule: the value it hands back is the value the write
			// left in SQLite.
			name:   "UpdateItemFields",
			itemID: "row",
			run: func(t *testing.T, s *Store) *Item {
				summary := "field write"
				rev, err := s.UpdateItemFields("t", "row", ItemPartialUpdate{Summary: &summary})
				if err != nil {
					t.Fatalf("update item fields: %v", err)
				}
				return &Item{ID: "row", ThreadID: "t", Rev: rev}
			},
		},
		{
			name:   "BumpItemToTurnEnd",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				if err := s.InsertItem(contractItem("t", "row", 0)); err != nil {
					t.Fatalf("seed row: %v", err)
				}
				if err := s.InsertItem(contractItem("t", "after", 1)); err != nil {
					t.Fatalf("seed trailing row: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				got, err := s.BumpItemToTurnEnd("t", "row", nil, 3000)
				if err != nil {
					t.Fatalf("bump to turn end: %v", err)
				}
				return &got
			},
		},
		{
			name:   "AppendCompletionItem",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				launch := runningToolCall("row", 0)
				launch.IsBackground = true
				if err := s.InsertItem(launch); err != nil {
					t.Fatalf("seed launch: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				launch := runningToolCall("row", 0)
				launch.IsBackground = true
				completion := contractItem("t", "complete:row", 1)
				completion.Kind = "tool_completion"
				completion.CompletionOf = "row"
				if _, err := s.AppendCompletionItem(launch, completion, nil); err != nil {
					t.Fatalf("append completion: %v", err)
				}
				return nil
			},
		},
		{
			name:   "ForceCloseRunningToolCallsInTurn",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				if err := s.InsertItem(runningToolCall("row", 0)); err != nil {
					t.Fatalf("seed running tool call: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				flipped, err := s.ForceCloseRunningToolCallsInTurn("t", 0, func(string) string { return "closed" }, 3000)
				if err != nil {
					t.Fatalf("force close: %v", err)
				}
				if len(flipped) != 1 {
					t.Fatalf("force close flipped %d rows, want 1", len(flipped))
				}
				return &flipped[0]
			},
		},
		{
			name:   "MarkLiveBackgroundToolCallsInactive",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				launch := runningToolCall("row", 0)
				launch.IsBackground = true
				if err := s.InsertItem(launch); err != nil {
					t.Fatalf("seed background launch: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				n, err := s.MarkLiveBackgroundToolCallsInactive("t", 3000)
				if err != nil {
					t.Fatalf("mark inactive: %v", err)
				}
				if n != 1 {
					t.Fatalf("marked %d rows inactive, want 1", n)
				}
				return nil
			},
		},
		{
			name:   "UpdateItemMetaAtBoundary",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				item := contractItem("t", "row", 0)
				item.Kind = "user_text"
				item.Role = "user"
				if err := s.InsertItem(item); err != nil {
					t.Fatalf("seed user row: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				boundary, err := s.CaptureUserPlacementBoundary("t", 0, []string{"row"})
				if err != nil {
					t.Fatalf("capture boundary: %v", err)
				}
				got, err := s.UpdateItemMetaAtBoundary("t", "row", boundary, func(meta string, _ int) (string, error) {
					return `{"placed":true}`, nil
				}, 3000)
				if err != nil {
					t.Fatalf("update meta at boundary: %v", err)
				}
				return &got
			},
		},
		{
			name:   "RetireCodexBackgroundRuntime",
			itemID: "row",
			seed: func(t *testing.T, s *Store) {
				if err := s.CreateThread(makeThread("codex", "codex")); err != nil {
					t.Fatalf("create codex thread: %v", err)
				}
				launch := runningToolCall("row", 0)
				launch.ThreadID = "codex"
				launch.IsBackground = true
				if err := s.InsertItem(launch); err != nil {
					t.Fatalf("seed codex launch: %v", err)
				}
			},
			run: func(t *testing.T, s *Store) *Item {
				retired, err := s.RetireCodexBackgroundRuntime("codex", func(string) string { return "lost" }, 3000)
				if err != nil {
					t.Fatalf("retire codex runtime: %v", err)
				}
				if len(retired) != 1 {
					t.Fatalf("retired %d rows, want 1", len(retired))
				}
				return &retired[0]
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedContractThread(t, s, "t")
			if tc.seed != nil {
				tc.seed(t, s)
			} else if err := s.InsertItem(contractItem("t", "row", 0)); err != nil {
				t.Fatalf("seed row: %v", err)
			}

			threadID := "t"
			if tc.name == "RetireCodexBackgroundRuntime" {
				threadID = "codex"
			}
			before := int64(-1)
			if tc.itemID != "head" {
				before = itemRevisionOf(t, s, threadID, tc.itemID)
			}

			returned := tc.run(t, s)

			after := itemRevisionOf(t, s, threadID, tc.itemID)
			if after <= before {
				t.Fatalf("rev after write = %d, want > %d", after, before)
			}
			if returned != nil && returned.Rev != after {
				t.Fatalf("returned rev = %d, stored rev = %d", returned.Rev, after)
			}
		})
	}
}

// TestPayloadWritersStampOwningItemRows covers the half of the contract
// the item triggers cannot see: payload content rides an item row on the
// wire, so a payload write has to move that row's revision, not just the
// thread counter. Spans are the documented exception.
func TestPayloadWritersStampOwningItemRows(t *testing.T) {
	seed := func(t *testing.T, s *Store) {
		t.Helper()
		seedContractThread(t, s, "t")
		item := contractItem("t", "row", 0)
		item.PayloadID = "pay"
		if err := s.InsertItemWithPayload(item, Payload{
			ID: "pay", Kind: "text", Meta: "{}", Data: []byte("base"), CreatedAt: 1000,
		}); err != nil {
			t.Fatalf("seed item with payload: %v", err)
		}
		// A second row referencing the same blob as its tool INPUT: both
		// projections are on the wire, so both rows must be stamped.
		input := contractItem("t", "input-row", 1)
		input.InputPayloadID = "pay"
		if err := s.InsertItem(input); err != nil {
			t.Fatalf("seed input row: %v", err)
		}
	}

	t.Run("AppendPayloadData", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s)
		before, beforeInput := itemRevisionOf(t, s, "t", "row"), itemRevisionOf(t, s, "t", "input-row")
		if err := s.AppendPayloadData("t", "pay", []byte(" delta"), "{}", 3000); err != nil {
			t.Fatalf("append payload data: %v", err)
		}
		if got := itemRevisionOf(t, s, "t", "row"); got <= before {
			t.Fatalf("owner rev = %d, want > %d", got, before)
		}
		if got := itemRevisionOf(t, s, "t", "input-row"); got <= beforeInput {
			t.Fatalf("input owner rev = %d, want > %d", got, beforeInput)
		}
	})

	t.Run("ReplacePayloadData", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s)
		before := itemRevisionOf(t, s, "t", "row")
		if err := s.ReplacePayloadData("t", "pay", []byte("final"), "{}", 3000); err != nil {
			t.Fatalf("replace payload data: %v", err)
		}
		if got := itemRevisionOf(t, s, "t", "row"); got <= before {
			t.Fatalf("owner rev = %d, want > %d", got, before)
		}
	})

	t.Run("UpdatePayloadMeta", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s)
		before := itemRevisionOf(t, s, "t", "row")
		if err := s.UpdatePayloadMeta("t", "pay", `{"command":"ls"}`); err != nil {
			t.Fatalf("update payload meta: %v", err)
		}
		if got := itemRevisionOf(t, s, "t", "row"); got <= before {
			t.Fatalf("owner rev = %d, want > %d", got, before)
		}
	})

	t.Run("UpdatePayloadSpans leaves the row stamp alone", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s)
		beforeRow := itemRevisionOf(t, s, "t", "row")
		beforeThread := historyStampOf(t, s, "t").Rev
		if err := s.UpdatePayloadSpans("t", "pay", `[{"v":1}]`, `[{"v":1}]`); err != nil {
			t.Fatalf("update payload spans: %v", err)
		}
		if got := itemRevisionOf(t, s, "t", "row"); got != beforeRow {
			t.Fatalf("span backfill moved the row rev: %d -> %d", beforeRow, got)
		}
		if got := historyStampOf(t, s, "t").Rev; got <= beforeThread {
			t.Fatalf("span backfill left history_rev at %d, want > %d", got, beforeThread)
		}
	})
}

// TestPayloadTouchProbesPayloadIndexes is the cost half of the payload
// stamp. The touch runs inside every payload write, including the
// streaming appends, so its plan has to be two index probes and never a
// walk of the thread: an OR over the two partial payload indexes puts
// SQLite on idx_items_thread, which reads every row of the thread per
// write and is invisible until a thread is long.
func TestPayloadTouchProbesPayloadIndexes(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")
	item := contractItem("t", "row", 0)
	item.PayloadID = "pay"
	if err := s.InsertItemWithPayload(item, Payload{
		ID: "pay", Kind: "text", Meta: "{}", Data: []byte("base"), CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("seed item with payload: %v", err)
	}

	plan := explainPlan(t, s, touchPayloadOwnerRowsSQL, "t", "pay", "t", "pay", "t")
	text := planText(plan)
	for _, r := range plan {
		if r.detail == "SCAN items" {
			t.Errorf("the payload touch scans the thread's items\n%s", text)
			break
		}
	}
	for _, index := range []string{"idx_items_payload_id", "idx_items_input_payload_id"} {
		if !strings.Contains(text, index) {
			t.Errorf("the payload touch does not probe %s\n%s", index, text)
		}
	}
}

// TestProposedPlanWritersStampPlanItemRow covers the other decoration
// source: proposed-plan state and comments are projected onto the plan
// row's meta at read time (decorateProposedPlanItems), so their writers
// must stamp that row.
func TestProposedPlanWritersStampPlanItemRow(t *testing.T) {
	newPlanStore := func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		seedContractThread(t, s, "t")
		plan := contractItem("t", "plan", 0)
		plan.Kind = "tool_call"
		plan.ToolName = "ExitPlanMode"
		if err := s.InsertItem(plan); err != nil {
			t.Fatalf("seed plan row: %v", err)
		}
		return s
	}

	t.Run("EnsureProposedPlanState", func(t *testing.T) {
		s := newPlanStore(t)
		before := itemRevisionOf(t, s, "t", "plan")
		if _, err := s.EnsureProposedPlanState("t", "plan", 2000); err != nil {
			t.Fatalf("ensure plan state: %v", err)
		}
		if got := itemRevisionOf(t, s, "t", "plan"); got <= before {
			t.Fatalf("plan rev = %d, want > %d", got, before)
		}
	})

	t.Run("comment lifecycle", func(t *testing.T) {
		s := newPlanStore(t)
		if _, err := s.EnsureProposedPlanState("t", "plan", 2000); err != nil {
			t.Fatalf("ensure plan state: %v", err)
		}
		steps := []struct {
			name string
			run  func(t *testing.T)
		}{
			{"create", func(t *testing.T) {
				if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
					ID: "c1", ThreadID: "t", PlanItemID: "plan",
					StartLine: 1, EndLine: 1, SelectedText: "line", Body: "note",
					CreatedAt: 2000, UpdatedAt: 2000,
				}); err != nil {
					t.Fatalf("create comment: %v", err)
				}
			}},
			{"update", func(t *testing.T) {
				if _, err := s.UpdateProposedPlanComment("t", "c1", ProposedPlanCommentUpdate{
					Body: "edited",
				}, 2500); err != nil {
					t.Fatalf("update comment: %v", err)
				}
			}},
			{"mark sent", func(t *testing.T) {
				if err := s.MarkProposedPlanCommentsSent("t", "plan", []string{"c1"}, 2600, "t:0"); err != nil {
					t.Fatalf("mark sent: %v", err)
				}
			}},
			{"resolve", func(t *testing.T) {
				if err := s.DeleteOrResolveProposedPlanComment("t", "c1", 2700); err != nil {
					t.Fatalf("resolve comment: %v", err)
				}
			}},
			{"mark implemented", func(t *testing.T) {
				if err := s.MarkProposedPlanImplemented("t", "plan", "t", "impl", 2800); err != nil {
					t.Fatalf("mark implemented: %v", err)
				}
			}},
		}
		for _, step := range steps {
			before := itemRevisionOf(t, s, "t", "plan")
			step.run(t)
			if got := itemRevisionOf(t, s, "t", "plan"); got <= before {
				t.Fatalf("%s left the plan rev at %d, want > %d", step.name, got, before)
			}
		}
	})
}

// TestItemRevisionUnderBulkLoad pins the one place the stamp and the
// thread counter deliberately come apart.
//
// ApplyImportBatch writes shared import history, not `items`, and settles
// the thread counter above everything the batch contributed; the imported
// rows read as -1 because a shared chunk has nowhere thread-scoped to
// stamp. Materializing that history into the thread's own overlay is a
// representation change with no logical effect, so the thread stamps stay
// exactly where they were while the copied rows gain a stamp.
func TestItemRevisionUnderBulkLoad(t *testing.T) {
	s := newTestStore(t)
	seedContractThread(t, s, "t")

	imported := contractItem("t", "imported", 0)
	imported.TurnIndex = 1
	if err := s.ApplyImportBatch("t", ImportBatch{
		Turns: []Turn{{TurnID: "t:1", ThreadID: "t", TurnIndex: 1, StartedAt: 1000}},
		Rows:  []ImportRow{{Item: imported}},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}

	afterImport := historyStampOf(t, s, "t")
	rows, err := windowDigestRowsTx(s.reader(), "t",
		TimelineCursor{TurnIndex: 0, ItemIndex: 0},
		TimelineCursor{TurnIndex: 1, ItemIndex: 0},
		10,
	)
	if err != nil {
		t.Fatalf("read window rows: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "imported" || rows[0].Rev != -1 {
		t.Fatalf("imported window rows = %+v, want one row stamped -1", rows)
	}
	if afterImport.Rev <= 0 {
		t.Fatalf("history_rev after import = %d, want the batch's row count added", afterImport.Rev)
	}

	// Materialize: the imported row moves into `items` under the bulk-load
	// flag. The logical timeline is unchanged, so the thread stamps must
	// not move — and the row must come out stamped, because from here on
	// it is an ordinary local row a digest can describe.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := materializeSharedHistoryTx(tx, "t", "test materialize"); err != nil {
		tx.Rollback()
		t.Fatalf("materialize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := historyStampOf(t, s, "t"); got != afterImport {
		t.Fatalf("materialization moved the stamps: %+v -> %+v", afterImport, got)
	}
	rev := itemRevisionOf(t, s, "t", "imported")
	if rev != afterImport.Rev {
		t.Fatalf("materialized row rev = %d, want the frozen history_rev %d", rev, afterImport.Rev)
	}

	// From here the row behaves like any other: the next write to it
	// advances both counters together.
	if err := s.UpdateItemMeta("t", "imported", `{"local":true}`); err != nil {
		t.Fatalf("update materialized row: %v", err)
	}
	if got := itemRevisionOf(t, s, "t", "imported"); got != afterImport.Rev+1 {
		t.Fatalf("rev after local update = %d, want %d", got, afterImport.Rev+1)
	}
}

// TestRecursiveTriggersStayOff is the pin the trigger bodies depend on.
// With recursion ON, the stamping UPDATE inside each trigger would
// re-enter the trigger that issued it.
func TestRecursiveTriggersStayOff(t *testing.T) {
	s := newTestStore(t)
	var on int
	if err := s.db.QueryRow(`PRAGMA recursive_triggers`).Scan(&on); err != nil {
		t.Fatalf("read recursive_triggers: %v", err)
	}
	if on != 0 {
		t.Fatalf("recursive_triggers = %d on the writer connection, want 0", on)
	}
}
