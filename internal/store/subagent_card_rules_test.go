package store

import (
	"path/filepath"
	"testing"
)

// cardRulesFixture is one thread written as a live session writes it:
// each row with its parent's session card, kept between writes.
type cardRulesFixture struct {
	t       *testing.T
	s       *Store
	thread  string
	session *cardSessionForTest
}

func newCardRulesFixture(t *testing.T) *cardRulesFixture {
	t.Helper()
	s := newTestStore(t)
	const thread = "t-rules"
	mustCreateThread(t, s, thread)
	return &cardRulesFixture{t: t, s: s, thread: thread, session: newCardSessionForTest(t, s, thread)}
}

func (f *cardRulesFixture) write(rows ...stampFixtureRow) {
	f.t.Helper()
	for _, r := range rows {
		item := r.item(f.thread)
		item.SubagentCard = f.session.card(r.parent)
		if err := f.s.InsertItem(item); err != nil {
			f.t.Fatalf("insert %s: %v", r.id, err)
		}
	}
}

// settled asserts that every row serves what the read-time aggregator
// computes and every stamp is the recompute.
func (f *cardRulesFixture) settled(stage string) {
	f.t.Helper()
	assertSubagentStampParity(f.t, f.s, f.thread, stage, true)
	assertStampsAreTheRecompute(f.t, f.s, f.thread, stage)
}

// TestSubagentCardSecondPromptNamingItsCarrierRecomputes pins a prompt
// naming a carrier a round already opened in memory: one carrier named by
// two prompts is a shape the recompute decides (readTime), not a second
// round the card opens.
func TestSubagentCardSecondPromptNamingItsCarrierRecomputes(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 1},
		stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
		stampFixtureRow{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2},
	)
	f.session.flush()
	f.write(
		stampFixtureRow{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 1},
		stampFixtureRow{id: "P2", kind: "user_text", summary: "and again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 2},
	)
	f.session.flush()
	f.settled("after the second prompt")
}

// TestSubagentCardRestoreRetiresItsAccumulators pins RestoreFrom against
// an open card: the rows its accumulators took after the snapshot are gone,
// so the next flush recomputes them even where the snapshot holds the
// stamp at the generation the card read.
func TestSubagentCardRestoreRetiresItsAccumulators(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: l", status: "running", turn: 1},
		stampFixtureRow{id: "L-1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1},
	)
	f.session.flush()
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := f.s.SnapshotTo(snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	f.write(stampFixtureRow{id: "L-2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "L", turn: 1, index: 2})
	if _, err := f.s.RestoreFrom(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	f.session.flush()
	f.settled("after the restore")
	f.write(stampFixtureRow{id: "L-3", kind: "tool_call", tool: "Bash", summary: "Bash: three", parent: "L", turn: 1, index: 3})
	f.session.closeAll()
	f.settled("after a write past the restore")
}

// TestSubagentCardDeletedPromptLeavesNoRound pins the delete of a prompt
// whose round a card opened in memory for a carrier not stored yet: the
// carrier, stored later, is named by no prompt, so its card is the
// recompute's (readTime), not the round the deleted prompt opened.
func TestSubagentCardDeletedPromptLeavesNoRound(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 1},
		stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
	)
	f.session.flush()
	f.write(stampFixtureRow{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 1})
	if err := f.s.DeleteThreadItem(f.thread, "P"); err != nil {
		t.Fatal(err)
	}
	f.write(stampFixtureRow{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2, index: 2})
	f.session.flush()
	f.settled("after the carrier")
}

// TestSubagentCardCarrierAfterItsPromptIsStamped pins a carrier stored
// after the local prompt that names it, once a flush has found it missing:
// its insert recomputes the family, so the carrier is stamped, not left
// to the walk.
func TestSubagentCardCarrierAfterItsPromptIsStamped(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 1},
		stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
		stampFixtureRow{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 1},
		stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 2, index: 2},
	)
	f.session.flush()
	f.write(stampFixtureRow{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2, index: 3})
	if _, mode := subagentStampStateForTest(t, f.s, f.thread, "C"); mode != subagentStampClean {
		t.Errorf("the carrier stored after its prompt is in mode %v, want a clean stamp", mode)
	}
	f.write(stampFixtureRow{id: "R-a3", kind: "assistant_text", summary: "round two again", parent: "R", turn: 2, index: 4})
	f.session.flush()
	f.settled("after the carrier")
}

// TestSubagentCardOrphanCountsOnceItsParentArrives pins a card opened
// for a parent not stored yet: once the parent is stored, the rows the
// card writes count toward it.
func TestSubagentCardOrphanCountsOnceItsParentArrives(t *testing.T) {
	f := newCardRulesFixture(t)
	f.session.card("L")
	f.write(
		stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: l", status: "running", turn: 1},
		stampFixtureRow{id: "L-1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1},
	)
	f.session.flush()
	f.settled("after the parent arrived")
}

// TestSubagentCardCopiesTheInheritedAnchorsOnItsChain pins a card opened
// in a pointer fork under anchors the fork reads from its ancestor: the
// card copies every anchor on its chain, so a later copy of one, made by
// a write that resolves no card under it, cannot put a stamp on the
// chain that no accumulator keeps.
func TestSubagentCardCopiesTheInheritedAnchorsOnItsChain(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "Q", kind: "tool_call", tool: "Agent", summary: "Agent: q", turn: 1},
		stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: l", parent: "Q", turn: 1, index: 1},
		stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: n", parent: "L", turn: 1, index: 2},
		stampFixtureRow{id: "N-1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "N", turn: 1, index: 3},
	)
	f.session.closeAll()
	const fork = "t-rules-fork"
	if err := f.s.CreatePointerFork(makeThread(fork, "claude"), f.thread, ForkCut{}, func(string) string { return "stopped" }, 9_000); err != nil {
		t.Fatal(err)
	}
	g := &cardRulesFixture{t: t, s: f.s, thread: fork, session: newCardSessionForTest(t, f.s, fork)}
	g.write(stampFixtureRow{id: "N-2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "N", turn: 2, index: 1})
	g.session.flush()
	g.settled("after the first write in the fork")
	// The fork's copy of L, unchanged, under Q's card.
	status := "completed"
	if _, err := f.s.UpdateItemFields(fork, "L", ItemPartialUpdate{Status: &status, SubagentCard: g.session.card("Q")}); err != nil {
		t.Fatal(err)
	}
	g.write(stampFixtureRow{id: "N-3", kind: "tool_call", tool: "Bash", summary: "Bash: three", parent: "N", turn: 2, index: 2})
	g.session.flush()
	g.settled("after a write past the copy")
}

// TestSubagentCardRecomputedRootRelinksItsRound pins the flush that
// recomputes a resumed root whose last round's carrier the thread no
// longer holds: a placement that shifts the agent's rows recomputes the
// family outside the cards' lock, and the next card operation keeps the
// root's pending accumulator to recompute and drops the carrier's. The
// carrier's recomputed accumulator must join the thread again, so the
// root's open card counts the round's rows toward it.
func TestSubagentCardRecomputedRootRelinksItsRound(t *testing.T) {
	f := newCardRulesFixture(t)
	f.write(
		stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 1, index: 1},
		stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 2},
		stampFixtureRow{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 1, index: 4},
		stampFixtureRow{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 1, index: 5},
		stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 1, index: 6},
	)
	f.session.flush()
	// A row before the round's prompt leaves R pending, C as it was.
	f.write(stampFixtureRow{id: "R-a0", kind: "assistant_text", summary: "late round one", parent: "R", turn: 1, index: 3})
	user := stampFixtureRow{id: "U", kind: "user_text", summary: "hello", turn: 1}.item(f.thread)
	if _, err := f.s.PlaceUserItemsAfterBoundary(f.thread, 1, "R", []Item{user}, nil, 9_000); err != nil {
		t.Fatal(err)
	}
	f.session.flush()
	f.settled("after the placement")
	f.write(stampFixtureRow{id: "R-a3", kind: "assistant_text", summary: "round two again", parent: "R", turn: 1, index: 90})
	f.session.flush()
	f.settled("after a row in the round")
}

// TestSubagentCardResolveRecomputesAStampThePromptsContradict pins the
// check a card makes of a live root's clean stamp against the root's
// rounds: a stamp the rounds contradict, written here directly as no card
// rule writes it, is recomputed, not taken as the accumulator.
func TestSubagentCardResolveRecomputesAStampThePromptsContradict(t *testing.T) {
	t.Run("resumed root without a transcript count", func(t *testing.T) {
		f := newCardRulesFixture(t)
		f.write(
			stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 1},
			stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
			stampFixtureRow{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2},
			stampFixtureRow{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 1},
			stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 2, index: 2},
		)
		f.session.closeAll()
		mustExec(t, f.s.db, `UPDATE subagent_aggregates SET transcript_count = NULL, transcript_newest_turn = NULL, transcript_newest_item = NULL
			WHERE thread_id = ? AND item_id = 'R'`, f.thread)
		f.session = newCardSessionForTest(t, f.s, f.thread)
		f.write(stampFixtureRow{id: "R-a3", kind: "assistant_text", summary: "round two again", parent: "R", turn: 2, index: 3})
		f.session.flush()
		f.settled("after a row in the round")
	})
	t.Run("last round imported", func(t *testing.T) {
		s := newTestStore(t)
		const thread = "t-rules-imported"
		newImportTargetThread(t, s, thread)
		batch := ImportBatch{Turns: []Turn{{TurnID: thread + ":imported", ThreadID: thread, StartedAt: 1_000}}}
		for _, r := range []stampFixtureRow{
			{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", index: 20},
			{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", index: 30},
			{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), index: 80},
			{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), index: 90},
			{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", index: 100},
		} {
			item := r.item(thread)
			if item.Meta == "" {
				item.Meta = "{}"
			}
			batch.Rows = append(batch.Rows, ImportRow{Item: item})
		}
		if err := s.ApplyImportBatch(thread, batch); err != nil {
			t.Fatalf("import: %v", err)
		}
		// Localized R and C are readTime under the imported prompt; clean
		// stamps with values the rounds would otherwise accept replace them.
		for _, id := range []string{"R", "C"} {
			summary := "Agent: local " + id
			if _, err := s.UpdateItemFields(thread, id, ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}
		mustExec(t, s.db, `UPDATE subagent_aggregates SET state = ?, descendant_count = 1,
			transcript_count = CASE item_id WHEN 'R' THEN 1 END WHERE thread_id = ? AND item_id IN ('R', 'C')`, aggStateClean, thread)
		f := &cardRulesFixture{t: t, s: s, thread: thread, session: newCardSessionForTest(t, s, thread)}
		f.write(stampFixtureRow{id: "R-a3", kind: "assistant_text", summary: "local round", parent: "R", turn: 2, index: 1})
		f.session.flush()
		f.settled("after a local row in the round")
	})
}
