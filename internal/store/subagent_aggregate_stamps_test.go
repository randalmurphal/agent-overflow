package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// setSubagentStampStateForTest moves an anchor's stamp to state, keeping
// its values: the state a trigger or a recompute leaves, written directly.
func setSubagentStampStateForTest(t *testing.T, s *Store, threadID, id string, state int) {
	t.Helper()
	result, err := s.db.Exec(`UPDATE subagent_aggregates SET state = ? WHERE thread_id = ? AND item_id = ?`, state, threadID, id)
	if err != nil {
		t.Fatalf("set stamp state %s/%s: %v", threadID, id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("set stamp state %s/%s: %d rows (%v)", threadID, id, n, err)
	}
}

// stripSubagentStampsForTest leaves rows as a store before v121 held
// them: no stamp. No ids strips the whole thread.
func stripSubagentStampsForTest(t *testing.T, s *Store, threadID string, ids ...string) {
	t.Helper()
	where, args := "thread_id = ?", []any{threadID}
	if len(ids) > 0 {
		clause, idArgs := inClause("item_id", ids)
		where += " AND " + clause
		args = append(args, idArgs...)
	}
	if _, err := s.db.Exec(`DELETE FROM subagent_aggregates WHERE `+where, args...); err != nil {
		t.Fatalf("strip stamps: %v", err)
	}
}

// subagentStampCardKeys are the keys a stamp owns and a read serves.
var subagentStampCardKeys = []string{
	metaKeySubagentDescendantCount, metaKeySubagentLatestChildSummary,
	metaKeySubagentTranscriptDescendantCount, metaKeySubagentLatestToolSummary,
	metaKeySubagentLatestToolTurn, metaKeySubagentLatestToolItem,
}

type subagentCard map[string]any

func subagentCardOf(t *testing.T, meta string) subagentCard {
	t.Helper()
	card := subagentCard{}
	if strings.TrimSpace(meta) == "" {
		return card
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	for _, key := range subagentStampCardKeys {
		if value, ok := decoded[key]; ok {
			card[key] = value
		}
	}
	return card
}

func threadTimelineIDsForTest(t *testing.T, q sqlQueryer, threadID string) []string {
	t.Helper()
	ids, err := subagentAnchorIDs(q, `SELECT id FROM timeline_items WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("list timeline ids: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// subagentCardsForTest reads every row as the page (ListWireItems) and the
// tray (decorateLatestDirectSubagentTools) serve it.
func subagentCardsForTest(t *testing.T, s *Store, q sqlQueryer, threadID string) map[string]subagentCard {
	t.Helper()
	rows, err := s.listWireItemsTx(q, threadID, threadTimelineIDsForTest(t, q, threadID))
	if err != nil {
		t.Fatalf("read wire items: %v", err)
	}
	rows, err = s.decorateLatestDirectSubagentTools(q, threadID, rows)
	if err != nil {
		t.Fatalf("decorate tray: %v", err)
	}
	out := make(map[string]subagentCard, len(rows))
	for _, row := range rows {
		out[row.ID] = subagentCardOf(t, row.Meta)
	}
	return out
}

// servedRevsForTest reads every row's revision as the page serves it.
func servedRevsForTest(t *testing.T, s *Store, threadID string) map[string]int64 {
	t.Helper()
	rows, err := s.listWireItemsTx(s.reader(), threadID, threadTimelineIDsForTest(t, s.reader(), threadID))
	if err != nil {
		t.Fatalf("read wire items: %v", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Rev
	}
	return out
}

// walkedSubagentCardsForTest is the reference read: every stamp removed
// and the thread listed for the backfill, so every anchor goes through the
// read-time aggregator. The transaction is rolled back.
func walkedSubagentCardsForTest(t *testing.T, s *Store, threadID string) map[string]subagentCard {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin walked read: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, threadID); err != nil {
		t.Fatalf("strip stamps: %v", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, threadID); err != nil {
		t.Fatalf("list thread: %v", err)
	}
	return subagentCardsForTest(t, s, tx, threadID)
}

// assertSubagentStampParity compares every row's served card with the
// read-time aggregator's. settled adds the post-write invariants: nothing
// is left dirty, and outside the backfill every anchor a read decorates
// carries a stamp.
func assertSubagentStampParity(t *testing.T, s *Store, threadID, stage string, settled bool) {
	t.Helper()
	got := subagentCardsForTest(t, s, s.reader(), threadID)
	want := walkedSubagentCardsForTest(t, s, threadID)
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if !reflect.DeepEqual(got[id], want[id]) {
			t.Errorf("%s: %s serves %v, read-time aggregator says %v", stage, id, got[id], want[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: served %d rows, reference read %d", stage, len(got), len(want))
	}
	if !settled {
		return
	}
	dirty, err := subagentAnchorIDs(s.reader(), subagentDirtyAnchorsSQL, threadID, 100)
	if err != nil {
		t.Fatalf("%s: dirty probe: %v", stage, err)
	}
	if len(dirty) > 0 {
		t.Errorf("%s: anchors left dirty after the write: %v", stage, dirty)
	}
	listed, err := subagentBackfillListed(s.reader(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if !listed {
		// A carrier stays unstamped, and walked, until the prompt that
		// opens its round arrives.
		unstamped, err := subagentAnchorIDs(s.reader(), `SELECT l.id FROM (`+subagentLegacyAnchorsSQL+`) AS l
		  CROSS JOIN items a ON a.thread_id = ?1 AND a.id = l.id
		 WHERE NOT `+aggCarrierSQL("a."), threadID, 100)
		if err != nil {
			t.Fatalf("%s: legacy probe: %v", stage, err)
		}
		if len(unstamped) > 0 {
			t.Errorf("%s: anchors a read decorates carry no stamp: %v", stage, unstamped)
		}
	}
}

// subagentStampMode is how a read treats a local anchorable row.
type subagentStampMode int

const (
	// subagentUnstamped: no stamp row.
	subagentUnstamped subagentStampMode = iota
	// subagentStampClean: the stamp is the read.
	subagentStampClean
	// subagentStampWalk: dirty or readTime; the read-time aggregator
	// answers.
	subagentStampWalk
)

// subagentStampStateForTest reads a row's generation and mode; gen is -1
// for an unstamped row.
func subagentStampStateForTest(t *testing.T, s *Store, threadID, id string) (int64, subagentStampMode) {
	t.Helper()
	var gen, state int64
	err := s.db.QueryRow(`SELECT gen, state FROM subagent_aggregates WHERE thread_id = ? AND item_id = ?`,
		threadID, id).Scan(&gen, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, subagentUnstamped
	}
	if err != nil {
		t.Fatalf("read stamp %s/%s: %v", threadID, id, err)
	}
	if state == aggStateClean {
		return gen, subagentStampClean
	}
	return gen, subagentStampWalk
}

// subagentStampRowForTest reads a row's whole stamp and generation.
func subagentStampRowForTest(t *testing.T, s *Store, threadID, id string) (subagentStampValues, int64) {
	t.Helper()
	targets, err := subagentStampTargets(s.db, threadID, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	target, ok := targets[id]
	if !ok || !target.stamped {
		t.Fatalf("%s/%s carries no stamp", threadID, id)
	}
	gen, _ := subagentStampStateForTest(t, s, threadID, id)
	return target.stored, gen
}

type stampFixtureRow struct {
	id, kind, tool, summary, parent, meta, status string
	turn, index                                   int
	background                                    bool
	completionOf                                  string
}

func (r stampFixtureRow) item(threadID string) Item {
	status := r.status
	if status == "" {
		status = "completed"
	}
	role := "assistant"
	if r.kind == "user_text" {
		role = "user"
	}
	return Item{
		ID: r.id, ThreadID: threadID, TurnIndex: r.turn, ItemIndex: r.index,
		Kind: r.kind, Role: role, ToolName: r.tool, Status: status, Summary: r.summary,
		ParentID: r.parent, Meta: r.meta, IsBackground: r.background, CompletionOf: r.completionOf,
		CreatedAt: int64(1_000 + r.turn*1_000 + r.index), UpdatedAt: int64(1_000 + r.turn*1_000 + r.index),
	}
}

func resumePromptMeta(carrierID string) string {
	if carrierID == "" {
		return fmt.Sprintf(`{"wire_only":true,%q:true}`, metaKeySubagentResumePrompt)
	}
	return fmt.Sprintf(`{"wire_only":true,%q:true,%q:%q}`, metaKeySubagentResumePrompt, metaKeyResumeCarrierID, carrierID)
}

func carrierMeta(rootID string) string {
	return fmt.Sprintf(`{%q:%q}`, metaKeyTranscriptRootID, rootID)
}

// subagentStampRowsForTest reads a thread's stamp rows without their
// generation, which only says which path wrote a row.
func subagentStampRowsForTest(t *testing.T, s *Store, threadID string) map[string]subagentStampValues {
	t.Helper()
	rows, err := s.reader().Query(`SELECT item_id, state, `+strings.Join(subagentAggregateValueColumns, ", ")+`
	  FROM subagent_aggregates WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatalf("read stamp rows: %v", err)
	}
	defer rows.Close()
	out := make(map[string]subagentStampValues)
	for rows.Next() {
		var id string
		var values subagentStampValues
		if err := rows.Scan(append([]any{&id, &values.State}, values.scanTargets()...)...); err != nil {
			t.Fatalf("scan stamp row: %v", err)
		}
		out[id] = values
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stamp rows: %v", err)
	}
	return out
}

// assertStampsAreTheRecompute recomputes every stamped anchor of the
// thread and fails on each whose stored row differs from what the
// recompute derives, internal columns included. An unstamped row is left
// out: a carrier stays unstamped until its prompt arrives.
func assertStampsAreTheRecompute(t *testing.T, s *Store, threadID, stage string) {
	t.Helper()
	stored := subagentStampRowsForTest(t, s, threadID)
	ids := make([]string, 0, len(stored))
	for id := range stored {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	writes, err := computeSubagentStamps(s.reader(), threadID, ids)
	if err != nil {
		t.Fatalf("%s: recompute: %v", stage, err)
	}
	for _, write := range writes {
		if was, ok := stored[write.id]; ok && was != write.values {
			t.Errorf("%s: %s is stored as %+v, the recompute derives %+v", stage, write.id, was, write.values)
		}
	}
}

// withParentCardForTest runs write with item carrying the card of its
// parent, opened for this write and closed after it, which flushes it. A
// top-level row is written as it is.
func withParentCardForTest(s *Store, item Item, write func(Item) error) error {
	if item.ParentID == "" {
		return write(item)
	}
	return s.WithSubagentCard(item.ThreadID, item.ParentID, func(card *SubagentCard) error {
		item.SubagentCard = card
		return write(item)
	})
}

// insertWithCardForTest is InsertItem with the parent's card.
func insertWithCardForTest(t *testing.T, s *Store, item Item) {
	t.Helper()
	if err := withParentCardForTest(s, item, s.InsertItem); err != nil {
		t.Fatalf("insert %s/%s: %v", item.ThreadID, item.ID, err)
	}
}

// The item writers with the written row's parent card
// (withParentCardForTest), for fixtures that seed rows straight to the
// store.

func insertCarded(s *Store, item Item) error {
	return withParentCardForTest(s, item, s.InsertItem)
}

func appendCarded(s *Store, item Item) (index int, err error) {
	err = withParentCardForTest(s, item, func(item Item) error {
		index, err = s.AppendItem(item)
		return err
	})
	return index, err
}

func insertWithPayloadCarded(s *Store, item Item, payload Payload) error {
	return withParentCardForTest(s, item, func(item Item) error { return s.InsertItemWithPayload(item, payload) })
}

func appendWithPayloadCarded(s *Store, item Item, payload Payload) (index int, err error) {
	err = withParentCardForTest(s, item, func(item Item) error {
		index, err = s.AppendItemWithPayload(item, payload)
		return err
	})
	return index, err
}

func upsertCarded(s *Store, item Item, payload *Payload) (written Item, err error) {
	err = withParentCardForTest(s, item, func(item Item) error {
		written, err = s.UpsertItem(item, payload)
		return err
	})
	return written, err
}

func upsertAtTurnHeadCarded(s *Store, item Item) (written Item, err error) {
	err = withParentCardForTest(s, item, func(item Item) error {
		written, err = s.UpsertItemAtTurnHead(item)
		return err
	})
	return written, err
}

// cardSessionForTest is a live writer's cards: one open card per parent,
// kept across writes as triage keeps them (internal/triage/subagent_cards.go).
type cardSessionForTest struct {
	t      *testing.T
	s      *Store
	thread string
	cards  map[string]*SubagentCard
}

func newCardSessionForTest(t *testing.T, s *Store, thread string) *cardSessionForTest {
	return &cardSessionForTest{t: t, s: s, thread: thread, cards: make(map[string]*SubagentCard)}
}

// card is the open card for rows under parent, opened on first use, or
// nil for a top-level row.
func (c *cardSessionForTest) card(parent string) *SubagentCard {
	c.t.Helper()
	if parent == "" {
		return nil
	}
	if card := c.cards[parent]; card != nil {
		return card
	}
	card, err := c.s.OpenSubagentCard(c.thread, parent)
	if err != nil {
		c.t.Fatalf("open the card of %s/%s: %v", c.thread, parent, err)
	}
	c.cards[parent] = card
	return card
}

// flush is the refresh timer's flush of the thread.
func (c *cardSessionForTest) flush() {
	c.t.Helper()
	if _, err := c.s.FlushSubagentCards(c.thread); err != nil {
		c.t.Fatalf("flush %s: %v", c.thread, err)
	}
}

// settle is an agent's completion, park or kill: the thread is flushed
// and the agent's card closed (Router.settleSubagentCard).
func (c *cardSessionForTest) settle(parent string) {
	c.t.Helper()
	c.flush()
	if card := c.cards[parent]; card != nil {
		delete(c.cards, parent)
		if err := card.Close(); err != nil {
			c.t.Fatalf("close the card of %s: %v", parent, err)
		}
	}
}

// closeAll closes every card: the session ends.
func (c *cardSessionForTest) closeAll() {
	c.t.Helper()
	for _, parent := range slices.Sorted(maps.Keys(c.cards)) {
		if err := c.cards[parent].Close(); err != nil {
			c.t.Fatalf("close the card of %s: %v", parent, err)
		}
	}
	clear(c.cards)
}

// crashSubagentCardsForTest loses every card accumulator, as a process
// that stops without a flush does. Cards opened before are unusable.
func crashSubagentCardsForTest(s *Store) {
	s.cards.mu.Lock()
	s.cards.threads = nil
	s.cards.mu.Unlock()
}

// pendingCardsForTest lists the thread's accumulators holding changes no
// flush has written, and the anchors left for the next flush's recompute.
func pendingCardsForTest(s *Store, threadID string) []string {
	t := s.cards.acquire(threadID, false)
	if t == nil {
		return nil
	}
	t.mu.Lock()
	var ids []string
	for id, st := range t.stamps {
		if st.pending() {
			ids = append(ids, id)
		}
	}
	for id := range t.seeds {
		ids = append(ids, "seed "+id)
	}
	s.cards.release(t)
	t.mu.Unlock()
	slices.Sort(ids)
	return ids
}

// cardBoundaryForTest is a point at which the cards reach their stamps.
type cardBoundaryForTest struct {
	name  string
	flush func(parent string)
}

// TestSubagentAggregateStampsMatchTheReadTimeAggregator drives the shapes
// the stamps must keep through every kind of write, written as a live
// session writes them: each row with its parent's card, the cards kept
// open between writes. Each write is followed by one of the flush
// boundaries in turn: the refresh timer, a card's own flush, an agent's
// settle (completion, park, kill), the session's end, and a shutdown that
// reopens the store. After each, every row must be served what the
// read-time aggregator computes from the rows, the stamps must be exactly
// what a recompute of every stamped anchor derives, internal columns
// included, and no accumulator may hold anything unwritten. The shapes: a
// plain launch, a nested launch, a §E6 root with two carrier rounds and a
// wake prompt, a detached launch with its completion sibling, plan_update
// notifications, and an imported chunk with a local child under an
// imported launch. A write the card rules follow under a running agent
// must change no stamp before its flush, and the flush must keep the
// stamps it names at their generation, which only a recompute moves, so a
// rule that stopped working cannot hide behind the recompute. A write
// under a chain no running agent covers (a completed launch, an old
// round's carrier, an imported launch) must reach the stamps in its own
// transaction. A bulk write after unflushed live writes, and a crash that
// loses them before the boot pass, must end on the recompute too, and a
// row written under a completed agent must survive the crash with no
// boot pass.
func TestSubagentAggregateStampsMatchTheReadTimeAggregator(t *testing.T) {
	const thread = "t-parity"
	path := newTestStorePath(t)
	s, err := New(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	session := newCardSessionForTest(t, s, thread)
	newImportTargetThread(t, s, thread)
	imported := func(r stampFixtureRow) ImportRow { return ImportRow{Item: r.item(thread)} }
	if err := s.ApplyImportBatch(thread, ImportBatch{
		Turns: []Turn{{TurnID: thread + ":0", ThreadID: thread, TurnIndex: 0, StartedAt: 1_000}},
		Rows: []ImportRow{
			imported(stampFixtureRow{id: "imp-user", kind: "user_text", summary: "imported ask"}),
			imported(stampFixtureRow{id: "imp-launch", kind: "tool_call", tool: "Agent", summary: "Agent: imported", index: 1}),
			imported(stampFixtureRow{id: "imp-c1", kind: "assistant_text", summary: "imported child", parent: "imp-launch", index: 2}),
			imported(stampFixtureRow{id: "imp-c2", kind: "tool_call", tool: "Grep", summary: "grep x", parent: "imp-launch", index: 3}),
			imported(stampFixtureRow{id: "imp-plan", kind: "notification", tool: "plan_update", summary: "plan", parent: "imp-launch", index: 4}),
		},
	}); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	assertSubagentStampParity(t, s, thread, "imported chunk", true)

	boundaries := []cardBoundaryForTest{
		{"refresh timer", func(string) { session.flush() }},
		{"card flush", func(parent string) {
			card := session.cards[parent]
			if card == nil {
				session.flush()
				return
			}
			if _, err := card.Flush(); err != nil {
				t.Fatalf("flush the card of %s: %v", parent, err)
			}
		}},
		{"agent settles", func(parent string) { session.settle(parent) }},
		{"session ends", func(string) { session.closeAll() }},
		{"shutdown", func(string) {
			// Close flushes every card; the next process opens the file.
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if s, err = New(path); err != nil {
				t.Fatalf("reopen: %v", err)
			}
			session = newCardSessionForTest(t, s, thread)
		}},
	}
	steps := 0

	parentOf := func(id string) string {
		t.Helper()
		row, found, err := s.GetThreadItemForWrite(thread, id)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", id, found, err)
		}
		return row.ParentID
	}
	gens := func(ids ...string) map[string]int64 {
		out := make(map[string]int64, len(ids))
		for _, id := range ids {
			out[id], _ = subagentStampStateForTest(t, s, thread, id)
		}
		return out
	}
	// settled asserts the invariants every boundary ends on: the served
	// cards are the walk's, the stamps the recompute's, and nothing waits
	// for a flush.
	settled := func(stage string) {
		t.Helper()
		assertSubagentStampParity(t, s, thread, stage, true)
		assertStampsAreTheRecompute(t, s, thread, stage)
		if pending := pendingCardsForTest(s, thread); len(pending) > 0 {
			t.Errorf("%s: accumulators still pending after the flush: %v", stage, pending)
		}
	}
	// step runs one write and the next boundary. keep names the stamps the
	// card rules keep on their own: the write changes no stamp row before
	// the flush, and the flush leaves them clean at the generation they
	// had (a stamp that had none is written clean). A row whose served
	// card changed must be served at a new revision, or a client holding
	// it could prove a stale card fresh: the stamp's own row and every
	// completion sibling borrowing it.
	//
	// A write under a chain no running agent covers (flushed) reaches the
	// stamps in its own transaction: they are the recompute before any
	// flush.
	runStep := func(stage string, keep []string, flushed bool, write func() string) {
		t.Helper()
		boundary := boundaries[steps%len(boundaries)]
		steps++
		stage += " (" + boundary.name + ")"
		before := gens(keep...)
		stampsBefore := subagentStampRowsForTest(t, s, thread)
		cardsBefore := subagentCardsForTest(t, s, s.reader(), thread)
		revsBefore := servedRevsForTest(t, s, thread)
		parent := write()
		switch {
		case flushed:
			settled(stage + ", before any flush")
		case keep != nil:
			if got := subagentStampRowsForTest(t, s, thread); !reflect.DeepEqual(got, stampsBefore) {
				t.Errorf("%s: the write changed stamp rows before any flush", stage)
			}
		}
		boundary.flush(parent)
		settled(stage)
		for id, was := range before {
			gen, mode := subagentStampStateForTest(t, s, thread, id)
			if mode != subagentStampClean || (was >= 0 && gen != was) {
				t.Errorf("%s: %s is mode %d at gen %d, want clean at gen %d (kept by the card)", stage, id, mode, gen, was)
			}
		}
		cardsAfter := subagentCardsForTest(t, s, s.reader(), thread)
		revsAfter := servedRevsForTest(t, s, thread)
		for id, card := range cardsAfter {
			was, existed := cardsBefore[id]
			if existed && !mapsEqual(was, card) && revsAfter[id] == revsBefore[id] {
				t.Errorf("%s: %s's card changed %v -> %v at the same revision %d", stage, id, was, card, revsAfter[id])
			}
		}
	}
	step := func(stage string, keep []string, write func() string) {
		t.Helper()
		runStep(stage, keep, false, write)
	}
	flushedStep := func(stage string, keep []string, write func() string) {
		t.Helper()
		runStep(stage, keep, true, write)
	}
	add := func(r stampFixtureRow) func() string {
		return func() string {
			t.Helper()
			item := r.item(thread)
			item.SubagentCard = session.card(r.parent)
			if err := s.InsertItem(item); err != nil {
				t.Fatalf("insert %s: %v", r.id, err)
			}
			return r.parent
		}
	}
	update := func(id string, update ItemPartialUpdate) func() string {
		return func() string {
			t.Helper()
			parent := parentOf(id)
			update.SubagentCard = session.card(parent)
			if _, err := s.UpdateItemFields(thread, id, update); err != nil {
				t.Fatalf("update %s: %v", id, err)
			}
			return parent
		}
	}
	summary := func(id, text string) func() string { return update(id, ItemPartialUpdate{Summary: &text}) }
	status := func(id, value string) func() string { return update(id, ItemPartialUpdate{Status: &value}) }
	// An append carries no card: the streaming writers append only to
	// text and thinking rows, which no card previews.
	appendSummary := func(id, delta string) func() string {
		return func() string {
			t.Helper()
			if _, err := s.AppendItemSummary(thread, id, delta, 5_000); err != nil {
				t.Fatalf("append summary %s: %v", id, err)
			}
			return parentOf(id)
		}
	}
	remove := func(id string) func() string {
		return func() string {
			t.Helper()
			parent := parentOf(id)
			if err := s.DeleteThreadItem(thread, id); err != nil {
				t.Fatalf("delete %s: %v", id, err)
			}
			return parent
		}
	}
	// upsert rewrites a whole row through UpsertItem, the path triage's
	// persist takes for a row that exists, with the row's stored meta.
	upsert := func(id string, change func(*Item)) func() string {
		return func() string {
			t.Helper()
			row, found, err := s.GetThreadItemForWrite(thread, id)
			if err != nil || !found {
				t.Fatalf("read %s: found=%v err=%v", id, found, err)
			}
			change(&row)
			row.SubagentCard = session.card(row.ParentID)
			if _, err := s.UpsertItem(row, nil); err != nil {
				t.Fatalf("upsert %s: %v", id, err)
			}
			return row.ParentID
		}
	}

	// A plain launch.
	step("launch", nil, add(stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: plain", status: "running", turn: 1}))
	step("launch first child", []string{"L"}, add(stampFixtureRow{id: "L-a1", kind: "assistant_text", summary: "thinking", parent: "L", status: "streaming", turn: 1, index: 1}))
	step("launch tool child", []string{"L"}, add(stampFixtureRow{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: ls", parent: "L", turn: 1, index: 2}))
	step("launch blank tool", []string{"L"}, add(stampFixtureRow{id: "L-b2", kind: "tool_call", tool: "Bash", summary: " \t", parent: "L", turn: 1, index: 3}))
	step("launch plan_update", []string{"L"}, add(stampFixtureRow{id: "L-plan", kind: "notification", tool: "plan_update", summary: "plan", parent: "L", turn: 1, index: 4}))
	step("older summary grows", []string{"L"}, appendSummary("L-a1", " harder"))
	step("pick summary changes", []string{"L"}, summary("L-b1", "Bash: ls -la"))
	step("blank tool gains text", []string{"L"}, summary("L-b2", "Bash: pwd"))
	step("pick goes blank", nil, summary("L-b2", "  "))
	step("blank pick gains text again", []string{"L"}, summary("L-b2", "Bash: pwd -P"))
	step("child status flip", []string{"L"}, status("L-b1", "errored"))
	step("child rewritten whole", []string{"L"}, upsert("L-b1", func(row *Item) { row.Summary, row.Status = "Bash: ls -la /", "completed" }))
	step("launch status flip", []string{"L"}, status("L", "completed"))

	// A nested launch counts toward its parent's card; a grandchild's
	// card carries every level up its chain.
	flushedStep("nested launch", []string{"L"}, add(stampFixtureRow{id: "N", kind: "tool_call", tool: "Agent", summary: "Agent: nested", parent: "L", status: "running", turn: 1, index: 5}))
	step("nested first child", []string{"L", "N"}, add(stampFixtureRow{id: "N-a1", kind: "assistant_text", summary: "nested work", parent: "N", turn: 1, index: 6}))
	step("nested tool", []string{"L", "N"}, add(stampFixtureRow{id: "N-b1", kind: "tool_call", tool: "Read", summary: "Read: file", parent: "N", status: "streaming", turn: 1, index: 7}))
	step("nested tool summary", []string{"L", "N"}, summary("N-b1", "Read: file.go"))
	step("doubly nested launch", []string{"L", "N"}, add(stampFixtureRow{id: "NN", kind: "tool_call", tool: "Agent", summary: "Agent: deeper", parent: "N", status: "running", turn: 1, index: 8}))
	step("doubly nested tool", []string{"L", "N", "NN"}, add(stampFixtureRow{id: "NN-b1", kind: "tool_call", tool: "Grep", summary: "Grep: deep", parent: "NN", turn: 1, index: 9}))
	step("doubly nested pick changes", []string{"L", "N", "NN"}, summary("NN-b1", "Grep: deeper"))
	// A streaming append to a previewed row would change a card without
	// one: the store refuses it and writes nothing.
	step("nested tool append refused", []string{"L", "N"}, func() string {
		t.Helper()
		if _, err := s.AppendItemSummary(thread, "N-b1", " more", 5_000); !errors.Is(err, ErrSubagentAnchor) {
			t.Fatalf("append to a previewed row: %v, want ErrSubagentAnchor", err)
		}
		return "N"
	})

	// A §E6 root resumed twice, then woken without a carrier.
	step("root", nil, add(stampFixtureRow{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", status: "running", turn: 2}))
	step("root first child", []string{"R"}, add(stampFixtureRow{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 2, index: 1}))
	step("carrier one", []string{"R"}, add(stampFixtureRow{id: "C1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 3}))
	step("prompt one", []string{"R", "C1"}, add(stampFixtureRow{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C1"), turn: 3, index: 1}))
	step("round two child", []string{"R", "C1"}, add(stampFixtureRow{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 3, index: 2}))
	step("round two tool", []string{"R", "C1"}, add(stampFixtureRow{id: "R-b2", kind: "tool_call", tool: "Bash", summary: "Bash: make", parent: "R", status: "streaming", turn: 3, index: 3}))
	step("round two pick changes", []string{"R", "C1"}, summary("R-b2", "Bash: make test"))
	step("carrier two", []string{"R", "C1"}, add(stampFixtureRow{id: "C2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 4}))
	step("prompt two", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "P2", kind: "user_text", summary: "once more", parent: "R", meta: resumePromptMeta("C2"), turn: 4, index: 1}))
	step("round three child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a3", kind: "tool_call", tool: "Grep", summary: "Grep: x", parent: "R", turn: 4, index: 2}))
	step("round three pick changes", []string{"R", "C1", "C2"}, summary("R-a3", "Grep: y"))
	step("wake prompt", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "W", kind: "user_text", summary: "wake", parent: "R", meta: resumePromptMeta(""), turn: 5}))
	step("woken child", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "R-a4", kind: "assistant_text", summary: "woken", parent: "R", turn: 5, index: 1}))
	// Rows under a carrier itself count toward no card, only its tray.
	flushedStep("text under a carrier", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "C1-a", kind: "assistant_text", summary: "under the carrier", parent: "C1", turn: 5, index: 2}))
	flushedStep("tool under a carrier", []string{"R", "C1", "C2"}, add(stampFixtureRow{id: "C1-b", kind: "tool_call", tool: "Bash", summary: "Bash: under", parent: "C1", turn: 5, index: 3}))
	flushedStep("tool under a carrier changes", []string{"R", "C1", "C2"}, summary("C1-b", "Bash: under the carrier"))

	// A detached launch and its completion sibling, which carries the
	// launch's card.
	step("detached launch", nil, add(stampFixtureRow{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: detached", status: "running", background: true, turn: 6}))
	step("detached child", []string{"B"}, add(stampFixtureRow{id: "B-a1", kind: "assistant_text", summary: "detached work", parent: "B", turn: 6, index: 1}))
	step("detached tool", []string{"B"}, add(stampFixtureRow{id: "B-b1", kind: "tool_call", tool: "Bash", summary: "Bash: sleep", parent: "B", turn: 6, index: 2}))
	step("completion sibling", []string{"B"}, func() string {
		if _, err := s.AppendCompletionItem(Item{ID: "B", ThreadID: thread},
			stampFixtureRow{id: "B-done", kind: "tool_completion", tool: "Agent", summary: "done", turn: 7}.item(thread), nil); err != nil {
			t.Fatalf("append completion: %v", err)
		}
		return "B"
	})
	flushedStep("late detached child", []string{"B"}, add(stampFixtureRow{id: "B-a2", kind: "error", summary: "late", parent: "B", turn: 7, index: 5}))
	flushedStep("detached blank tool", []string{"B"}, add(stampFixtureRow{id: "B-b2", kind: "tool_call", tool: "Bash", parent: "B", turn: 7, index: 6}))
	// B-a2 is B's pick and not its tray: its going blank is the preview
	// rule's alone.
	flushedStep("error pick goes blank", nil, summary("B-a2", "  "))
	flushedStep("error pick gains text again", []string{"B"}, summary("B-a2", "late again"))
	// B-b1 is B's tray and not its pick.
	flushedStep("tray goes blank", nil, summary("B-b1", " "))
	flushedStep("tray gains text again", []string{"B"}, summary("B-b1", "Bash: sleep 1"))

	// A local child under the imported launch shadows the launch into
	// the overlay, where it is stamped like a local one.
	flushedStep("child under imported launch", nil, add(stampFixtureRow{id: "imp-local", kind: "tool_call", tool: "Bash", summary: "Bash: local", parent: "imp-launch", turn: 8}))
	if gen, mode := subagentStampStateForTest(t, s, thread, "imp-launch"); mode != subagentStampClean {
		t.Fatalf("shadowed imported launch is mode %d gen %d, want clean", mode, gen)
	}
	flushedStep("second child under shadowed launch", []string{"imp-launch"}, add(stampFixtureRow{id: "imp-local-2", kind: "assistant_text", summary: "local text", parent: "imp-launch", turn: 8, index: 1}))

	// Top-level rows no parent chain reaches: a launch inserted after its
	// children adopts them, and a launch whose meta later names a
	// transcript root becomes that root's carrier.
	step("orphan child", nil, add(stampFixtureRow{id: "O-a1", kind: "assistant_text", summary: "early", parent: "O", turn: 9, index: 1}))
	step("orphan tool", nil, add(stampFixtureRow{id: "O-b1", kind: "tool_call", tool: "Bash", summary: "Bash: early", parent: "O", turn: 9, index: 2}))
	step("launch adopts its children", nil, add(stampFixtureRow{id: "O", kind: "tool_call", tool: "Agent", summary: "Agent: late", status: "running", turn: 9}))
	step("orphan's card after its parent arrived", []string{"O"}, add(stampFixtureRow{id: "O-b3", kind: "tool_call", tool: "Bash", summary: "Bash: later", parent: "O", turn: 9, index: 3}))
	step("second root", nil, add(stampFixtureRow{id: "Q", kind: "tool_call", tool: "Agent", summary: "Agent: second root", turn: 10}))
	step("launch becomes a carrier", nil, update("O", ItemPartialUpdate{Meta: new(carrierMeta("Q"))}))
	step("top-level text", nil, add(stampFixtureRow{id: "T-top", kind: "assistant_text", summary: "top level", turn: 11}))

	// A root resumed before any child arrived: the prompt is its first
	// child, which opens its transcript and its carrier's round at once.
	step("childless root", nil, add(stampFixtureRow{id: "S", kind: "tool_call", tool: "Agent", summary: "Agent: quiet", status: "running", turn: 12}))
	step("childless root's carrier", nil, add(stampFixtureRow{id: "SC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("S"), turn: 13}))
	step("prompt is the first child", []string{"S", "SC"}, add(stampFixtureRow{id: "SP", kind: "user_text", summary: "go on", parent: "S", meta: resumePromptMeta("SC"), turn: 13, index: 1}))
	step("childless root's second round", []string{"S", "SC"}, add(stampFixtureRow{id: "S-b2", kind: "tool_call", tool: "Bash", summary: "Bash: go", parent: "S", turn: 13, index: 2}))

	// A prompt stored before a child already written cuts that child's
	// round: the rounds are recomputed.
	step("root with late rows", nil, add(stampFixtureRow{id: "U", kind: "tool_call", tool: "Agent", summary: "Agent: late rows", status: "running", turn: 14}))
	step("late root child", []string{"U"}, add(stampFixtureRow{id: "U-a1", kind: "assistant_text", summary: "first", parent: "U", turn: 14, index: 1}))
	step("late root newer child", []string{"U"}, add(stampFixtureRow{id: "U-b2", kind: "tool_call", tool: "Bash", summary: "Bash: newer", parent: "U", turn: 14, index: 5}))
	step("late root carrier", nil, add(stampFixtureRow{id: "UC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("U"), turn: 14, index: 6}))
	step("prompt before the newest child", nil, add(stampFixtureRow{id: "UP", kind: "user_text", summary: "resume", parent: "U", meta: resumePromptMeta("UC"), turn: 14, index: 3}))
	step("child after the out-of-order prompt", []string{"U", "UC"}, add(stampFixtureRow{id: "U-a3", kind: "assistant_text", summary: "after", parent: "U", turn: 14, index: 7}))

	// One carrier named by two prompts: the family is readTime.
	step("twice-resumed root", nil, add(stampFixtureRow{id: "V", kind: "tool_call", tool: "Agent", summary: "Agent: twice", status: "running", turn: 15}))
	step("twice-resumed child", []string{"V"}, add(stampFixtureRow{id: "V-a1", kind: "assistant_text", summary: "v", parent: "V", turn: 15, index: 1}))
	step("twice-named carrier", nil, add(stampFixtureRow{id: "VC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("V"), turn: 16}))
	step("first prompt naming it", []string{"V", "VC"}, add(stampFixtureRow{id: "VP1", kind: "user_text", summary: "again", parent: "V", meta: resumePromptMeta("VC"), turn: 16, index: 1}))
	step("round child", []string{"V", "VC"}, add(stampFixtureRow{id: "V-b2", kind: "tool_call", tool: "Bash", summary: "Bash: v", parent: "V", turn: 16, index: 2}))
	step("second prompt naming it", nil, add(stampFixtureRow{id: "VP2", kind: "user_text", summary: "again again", parent: "V", meta: resumePromptMeta("VC"), turn: 17, index: 1}))
	step("child of a readTime family", nil, add(stampFixtureRow{id: "V-a3", kind: "assistant_text", summary: "v3", parent: "V", turn: 17, index: 2}))

	// A prompt under one root naming another root's carrier: the carrier's
	// card is that root's round, so this root's rounds are recomputed.
	step("root naming a foreign carrier", nil, add(stampFixtureRow{id: "X", kind: "tool_call", tool: "Agent", summary: "Agent: x", status: "running", turn: 18}))
	step("foreign-carrier root child", []string{"X"}, add(stampFixtureRow{id: "X-a1", kind: "assistant_text", summary: "x", parent: "X", turn: 18, index: 1}))
	step("carrier's own root", nil, add(stampFixtureRow{id: "Y", kind: "tool_call", tool: "Agent", summary: "Agent: y", turn: 18, index: 2}))
	step("carrier of the other root", nil, add(stampFixtureRow{id: "XC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("Y"), turn: 19}))
	step("prompt naming the foreign carrier", nil, add(stampFixtureRow{id: "XP", kind: "user_text", summary: "resume x", parent: "X", meta: resumePromptMeta("XC"), turn: 19, index: 1}))
	step("child in the foreign carrier's round", nil, add(stampFixtureRow{id: "X-b2", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "X", turn: 19, index: 2}))
	step("foreign round pick changes", nil, summary("X-b2", "Bash: x2"))
	step("later child in the foreign round", nil, add(stampFixtureRow{id: "X-b4", kind: "tool_call", tool: "Bash", summary: "Bash: x4", parent: "X", turn: 19, index: 4}))
	step("own carrier of the foreign-carrier root", nil, add(stampFixtureRow{id: "XC2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("X"), turn: 19, index: 6}))
	step("prompt cuts the foreign carrier's round", nil, add(stampFixtureRow{id: "XP2", kind: "user_text", summary: "resume x again", parent: "X", meta: resumePromptMeta("XC2"), turn: 19, index: 3}))

	// A root woken without a carrier has a transcript and no carrier.
	step("wake-only root", nil, add(stampFixtureRow{id: "Z", kind: "tool_call", tool: "Agent", summary: "Agent: z", status: "running", turn: 21}))
	step("wake-only root child", []string{"Z"}, add(stampFixtureRow{id: "Z-a1", kind: "assistant_text", summary: "z", parent: "Z", turn: 21, index: 1}))
	step("wake prompt as the only prompt", []string{"Z"}, add(stampFixtureRow{id: "ZW", kind: "user_text", summary: "wake", parent: "Z", meta: resumePromptMeta(""), turn: 22, index: 1}))
	step("woken root's child", []string{"Z"}, add(stampFixtureRow{id: "Z-b2", kind: "tool_call", tool: "Bash", summary: "Bash: z", parent: "Z", turn: 22, index: 2}))

	// A prompt stored inside a later round moves that round's later rows
	// to the round it opens.
	step("root with two rounds", nil, add(stampFixtureRow{id: "K", kind: "tool_call", tool: "Agent", summary: "Agent: k", status: "running", turn: 24}))
	step("two-round root child", []string{"K"}, add(stampFixtureRow{id: "K-a1", kind: "assistant_text", summary: "k", parent: "K", turn: 24, index: 1}))
	step("first carrier of the two-round root", nil, add(stampFixtureRow{id: "KC1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("K"), turn: 25}))
	step("first prompt of the two-round root", []string{"K", "KC1"}, add(stampFixtureRow{id: "KP1", kind: "user_text", summary: "k again", parent: "K", meta: resumePromptMeta("KC1"), turn: 25, index: 1}))
	step("second round child", []string{"K", "KC1"}, add(stampFixtureRow{id: "K-b2", kind: "tool_call", tool: "Bash", summary: "Bash: k2", parent: "K", turn: 25, index: 3}))
	step("second round later child", []string{"K", "KC1"}, add(stampFixtureRow{id: "K-b3", kind: "tool_call", tool: "Bash", summary: "Bash: k3", parent: "K", turn: 25, index: 5}))
	step("second carrier of the two-round root", nil, add(stampFixtureRow{id: "KC2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("K"), turn: 25, index: 6}))
	step("prompt inside the second round", nil, add(stampFixtureRow{id: "KP2", kind: "user_text", summary: "k split", parent: "K", meta: resumePromptMeta("KC2"), turn: 25, index: 4}))

	// A prompt naming its own root: the family is readTime.
	step("self-resumed root", nil, add(stampFixtureRow{id: "H", kind: "tool_call", tool: "Agent", summary: "Agent: h", status: "running", turn: 26}))
	step("self-resumed root child", []string{"H"}, add(stampFixtureRow{id: "H-a1", kind: "assistant_text", summary: "h", parent: "H", turn: 26, index: 1}))
	step("prompt naming its own root", nil, add(stampFixtureRow{id: "HP", kind: "user_text", summary: "h again", parent: "H", meta: resumePromptMeta("H"), turn: 27, index: 1}))
	step("self-resumed round child", nil, add(stampFixtureRow{id: "H-b2", kind: "tool_call", tool: "Bash", summary: "Bash: h", parent: "H", turn: 27, index: 2}))

	// A prompt stored before the carrier it names: the carrier arrives
	// unstamped and is stamped by the recompute its round's first row
	// makes.
	step("root resumed before its carrier", nil, add(stampFixtureRow{id: "E", kind: "tool_call", tool: "Agent", summary: "Agent: e", status: "running", turn: 28}))
	step("early-prompt root child", []string{"E"}, add(stampFixtureRow{id: "E-a1", kind: "assistant_text", summary: "e", parent: "E", turn: 28, index: 1}))
	step("prompt before its carrier", nil, add(stampFixtureRow{id: "EP", kind: "user_text", summary: "e again", parent: "E", meta: resumePromptMeta("EC"), turn: 29, index: 1}))
	step("late carrier", nil, add(stampFixtureRow{id: "EC", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("E"), turn: 29, index: 2}))
	step("child in the late carrier's round", nil, add(stampFixtureRow{id: "E-b2", kind: "tool_call", tool: "Bash", summary: "Bash: e", parent: "E", turn: 29, index: 3}))
	step("next child in the late carrier's round", []string{"E", "EC"}, add(stampFixtureRow{id: "E-b3", kind: "tool_call", tool: "Bash", summary: "Bash: e3", parent: "E", turn: 29, index: 4}))

	// A launch written before the stamps existed carries none, and its
	// thread is listed for the backfill, whose reads walk it. A card
	// opened under it recomputes it.
	step("unstamped launch with children", nil, func() string {
		for _, r := range []stampFixtureRow{
			{id: "G", kind: "tool_call", tool: "Agent", summary: "Agent: legacy", status: "running", turn: 23},
			{id: "G-a1", kind: "assistant_text", summary: "g", parent: "G", turn: 23, index: 1},
			{id: "G-b2", kind: "tool_call", tool: "Bash", summary: "Bash: g", parent: "G", turn: 23, index: 2},
		} {
			add(r)()
		}
		session.settle("G")
		stripSubagentStampsForTest(t, s, thread, "G")
		mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)
		return ""
	})
	step("child under the unstamped launch", nil, add(stampFixtureRow{id: "G-b3", kind: "tool_call", tool: "Bash", summary: "Bash: g3", parent: "G", turn: 23, index: 3}))
	step("next child under the recomputed launch", []string{"G"}, add(stampFixtureRow{id: "G-a4", kind: "assistant_text", summary: "g4", parent: "G", turn: 23, index: 4}))
	// The backfill finds nothing left to stamp.
	mustExec(t, s.db, `DELETE FROM subagent_aggregate_backfill WHERE thread_id = ?`, thread)
	settled("backfill done")

	// A rewrite that moves its row to another launch recomputes both.
	step("child moves to another launch", nil, upsert("B-a1", func(row *Item) { row.ParentID = "L" }))
	step("child leaves for the top level", nil, upsert("B-b2", func(row *Item) { row.ParentID = "" }))
	step("top-level row joins a launch", nil, upsert("T-top", func(row *Item) { row.ParentID = "L" }))

	// A bulk write after live writes no flush has written: the revert cut
	// takes them and a flushed row under the completed L, recomputes the
	// anchors it leaves, and the accumulators it retired recompute at the
	// next flush. The rows under completed agents go first: each write
	// under one flushes every pending accumulator of the thread.
	step("bulk cut after live writes", nil, func() string {
		for _, r := range []stampFixtureRow{
			{id: "L-t30", kind: "tool_call", tool: "Bash", summary: "Bash: cut", parent: "L", turn: 30},
			{id: "B-t29", kind: "tool_call", tool: "Bash", summary: "Bash: kept", parent: "B", turn: 29, index: 9},
			{id: "NN-t30", kind: "tool_call", tool: "Bash", summary: "Bash: cut too", parent: "NN", turn: 30, index: 1},
			{id: "R-t30", kind: "assistant_text", summary: "cut round row", parent: "R", turn: 30, index: 2},
		} {
			add(r)()
		}
		if pending := pendingCardsForTest(s, thread); len(pending) == 0 {
			t.Fatal("the live writes left nothing for the flush")
		}
		if _, _, err := s.DeleteConversationFromTurn(thread, 30); err != nil {
			t.Fatalf("revert cut: %v", err)
		}
		return "B"
	})
	step("delete an older child", nil, remove("L-a1"))
	step("delete the pick", nil, remove("N-b1"))
	// B's newest row is B-t29; B-b1 is only its tray row and B-a2 only its
	// preview.
	step("delete the tray row", nil, remove("B-b1"))
	step("delete a preview that is not the newest", nil, remove("B-a2"))
	step("delete the transcript's newest", nil, remove("R-a4"))
	step("delete a round row", nil, remove("R-a2"))
	step("delete a prompt", nil, remove("P2"))
	step("delete the prompt naming the foreign carrier", nil, remove("XP"))
	step("delete a nested launch", nil, remove("N"))

	// A crash: live writes under running agents, nested and in a resumed
	// round, that no flush wrote, lost with the process. The boot pass
	// recomputes every agent that was running. B completed before the
	// crash, so it is not one of them: the row written under it after its
	// completion reached its stamp in its own transaction.
	step("running agents", nil, func() string {
		for _, r := range []stampFixtureRow{
			{id: "M", kind: "tool_call", tool: "Agent", summary: "Agent: running", status: "running", turn: 31},
			{id: "M-a1", kind: "assistant_text", summary: "m", parent: "M", turn: 31, index: 1},
			{id: "M2", kind: "tool_call", tool: "Agent", summary: "Agent: running nested", status: "running", parent: "M", turn: 31, index: 2},
			{id: "M2-b1", kind: "tool_call", tool: "Bash", summary: "Bash: m2", parent: "M2", turn: 31, index: 3},
			{id: "RC3", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), status: "running", turn: 32},
			{id: "P3", kind: "user_text", summary: "resume again", parent: "R", meta: resumePromptMeta("RC3"), turn: 32, index: 1},
			{id: "R-a5", kind: "assistant_text", summary: "round four", parent: "R", turn: 32, index: 2},
		} {
			add(r)()
		}
		return "M2"
	})
	session.flush()
	for _, r := range []stampFixtureRow{
		{id: "B-b9", kind: "tool_call", tool: "Bash", summary: "Bash: after the completion", parent: "B", turn: 32, index: 9},
		{id: "M2-b2", kind: "tool_call", tool: "Bash", summary: "Bash: lost", parent: "M2", turn: 31, index: 4},
		{id: "M-b3", kind: "tool_call", tool: "Read", summary: "Read: lost", parent: "M", turn: 31, index: 5},
		{id: "R-b6", kind: "tool_call", tool: "Grep", summary: "Grep: lost", parent: "R", turn: 32, index: 3},
	} {
		add(r)()
	}
	crashSubagentCardsForTest(s)
	session = newCardSessionForTest(t, s, thread)
	served := subagentCardsForTest(t, s, s.reader(), thread)
	walked := walkedSubagentCardsForTest(t, s, thread)
	for _, id := range []string{"M", "M2", "R", "RC3"} {
		if mapsEqual(served[id], walked[id]) {
			t.Errorf("crash: %s serves %v, which the lost writes should have left stale", id, served[id])
		}
	}
	for _, id := range []string{"B", "B-done"} {
		if !mapsEqual(served[id], walked[id]) {
			t.Errorf("crash: %s serves %v, the walk %v: a row no boot pass recovers was left for a flush", id, served[id], walked[id])
		}
	}
	if _, err := s.RecoverSubagentCards(t.Context()); err != nil {
		t.Fatalf("boot pass: %v", err)
	}
	settled("boot pass after a crash")

	// A clean shutdown flushed everything: the boot pass recomputes what
	// the stamps already hold.
	add(stampFixtureRow{id: "M-b4", kind: "tool_call", tool: "Bash", summary: "Bash: flushed at close", parent: "M", turn: 31, index: 6})()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = New(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	session = newCardSessionForTest(t, s, thread)
	settled("clean shutdown")
	if _, err := s.RecoverSubagentCards(t.Context()); err != nil {
		t.Fatalf("boot pass: %v", err)
	}
	settled("boot pass after a clean shutdown")

	// Every anchor the fixture names ends on a stamp a page serves as
	// stored: the parity above was not won by walking everything. C2
	// lost the prompt that named it, so it shows the whole transcript,
	// which only the walk computes.
	for _, id := range []string{"L", "R", "C1", "B", "M", "M2", "imp-launch"} {
		if _, mode := subagentStampStateForTest(t, s, thread, id); mode != subagentStampClean {
			t.Errorf("%s ends mode %d, want clean", id, mode)
		}
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "C2"); mode != subagentStampWalk {
		t.Errorf("unnamed carrier C2 ends mode %d, want readTime", mode)
	}
	if steps < 3*len(boundaries) {
		t.Fatalf("%d steps exercise each boundary less than three times", steps)
	}
}

// TestSubagentAggregateChainedCarrierJoinsItsRootsFamily pins the
// recompute of a carrier whose transcript root is itself a carrier: a
// round resumed from a resumed round. Seeded alone, as a backfill batch
// can seed it, it brings in the carrier it names and that carrier's own
// root, so the named carrier is stamped from its root's rounds instead of
// being written readTime as a root, which would put every read of it and
// of its completion sibling on a walk of the root's whole transcript.
func TestSubagentAggregateChainedCarrierJoinsItsRootsFamily(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-chained"
	mustCreateThread(t, s, thread)
	for _, r := range []stampFixtureRow{
		{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", turn: 1},
		{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
		{id: "C1", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 2},
		{id: "P1", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C1"), turn: 2, index: 1},
		{id: "R-a2", kind: "assistant_text", summary: "round two", parent: "R", turn: 2, index: 2},
		{id: "C2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("C1"), turn: 3},
		{id: "P2", kind: "user_text", summary: "once more", parent: "C1", meta: resumePromptMeta("C2"), turn: 3, index: 1},
		{id: "C1-a1", kind: "assistant_text", summary: "under the carrier", parent: "C1", turn: 3, index: 2},
	} {
		insertWithCardForTest(t, s, r.item(thread))
	}
	if _, err := s.AppendCompletionItem(Item{ID: "C1", ThreadID: thread},
		stampFixtureRow{id: "C1-done", kind: "tool_completion", tool: "SendMessage", summary: "done", turn: 4}.item(thread), nil); err != nil {
		t.Fatalf("append completion: %v", err)
	}
	stripSubagentStampsForTest(t, s, thread)
	mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread)

	writes, err := computeSubagentStamps(s.reader(), thread, []string{"C2"})
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]int64, len(writes))
	for _, write := range writes {
		states[write.id] = write.values.State
	}
	want := map[string]int64{"R": aggStateClean, "C1": aggStateClean, "C2": aggStateReadTime}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("a batch seeded with the chained carrier writes %v, want %v", states, want)
	}

	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("backfill does not finish")
		}
		result, err := s.RecomputeSubagentAggregates(t.Context(), thread, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Remaining {
			break
		}
	}
	assertSubagentStampParity(t, s, thread, "backfilled", true)
	for id, mode := range map[string]subagentStampMode{"R": subagentStampClean, "C1": subagentStampClean, "C2": subagentStampWalk} {
		if _, got := subagentStampStateForTest(t, s, thread, id); got != mode {
			t.Errorf("%s ends mode %d, want %d", id, got, mode)
		}
	}
	completion, found, err := s.GetThreadItem(thread, "C1-done")
	if err != nil || !found {
		t.Fatalf("read completion: found=%v err=%v", found, err)
	}
	if needs, err := s.ItemReadNeedsDecoration(completion); err != nil || !needs {
		t.Fatalf("completion needs decoration = %v (%v); completions always go through the page read", needs, err)
	}
	if card := subagentCardOf(t, completion.Meta); !mapsEqual(card, subagentCard{"subagentDescendantCount": float64(2)}) {
		t.Errorf("the completion's read-back is %v, want C1's round under R", card)
	}

	// The walk gives C1 its round under R in every window: alone, with
	// the round resumed from it, and with that round's carrier ahead of it.
	walkWindows := func(stage string) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, thread); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, thread); err != nil {
			t.Fatal(err)
		}
		full := subagentCardsForTest(t, s, tx, thread)
		for _, window := range [][]string{{"C1"}, {"C1-done"}, {"C1", "C2"}, {"C2", "C1-done"}, {"C2", "C1"}, {"R", "C2", "C1-done"}} {
			rows, err := s.listWireItemsTx(tx, thread, window)
			if err != nil {
				t.Fatal(err)
			}
			byID := make(map[string]Item, len(rows))
			for _, row := range rows {
				byID[row.ID] = row
			}
			ordered := make([]Item, 0, len(window))
			for _, id := range window {
				ordered = append(ordered, byID[id])
			}
			decorated, err := s.decorateSubagentAnchors(tx, thread, ordered)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range decorated {
				if row.ID == "C1" || row.ID == "C1-done" {
					if got := subagentCardOf(t, row.Meta); !mapsEqual(got, full[row.ID]) {
						t.Errorf("%s: window %v walks %s as %v, the whole thread as %v", stage, window, row.ID, got, full[row.ID])
					}
				}
			}
		}
	}
	walkWindows("backfilled")

	// Writes after the backfill: a row under C1 is in no card C1 shows,
	// and a row under R extends C1's round.
	for _, r := range []stampFixtureRow{
		{id: "C1-a2", kind: "assistant_text", summary: "under the carrier again", parent: "C1", turn: 5, index: 1},
		{id: "R-a3", kind: "assistant_text", summary: "round two goes on", parent: "R", turn: 5, index: 2},
	} {
		insertWithCardForTest(t, s, r.item(thread))
		assertSubagentStampParity(t, s, thread, "after "+r.id, true)
	}
	if _, mode := subagentStampStateForTest(t, s, thread, "C1"); mode != subagentStampClean {
		t.Errorf("C1 ends mode %d after the writes, want clean", mode)
	}
	walkWindows("after writes")
}

// TestSubagentServedKeysAreNeverStored pins that the card keys a read
// serves stay out of every stored meta. A read of an anchor, and of the
// completion sibling that borrows its card, serves them; a writer that
// writes such a row back reads it through GetThreadItemForWrite, which
// returns the stored meta, and the write returns the row as a read serves
// it, so an emitter pushes what a read returns.
// RowsStoringServedSubagentKeys, which the storetest cleanup runs after
// every test of the packages above the store, finds a meta that holds
// one, and not a Codex spawn completion's snapshot, which is its own.
func TestSubagentServedKeysAreNeverStored(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-served"
	mustCreateThread(t, s, thread)
	insertWithCardForTest(t, s, stampFixtureRow{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: a",
		meta: `{"input":{"prompt":"go"}}`, status: "running", turn: 1}.item(thread))
	insertWithCardForTest(t, s, stampFixtureRow{id: "A-b1", kind: "tool_call", tool: "Bash", summary: "Bash: ls",
		parent: "A", turn: 1, index: 1}.item(thread))
	if _, err := s.AppendCompletionItem(Item{ID: "A", ThreadID: thread},
		stampFixtureRow{id: "A-done", kind: "tool_completion", tool: "Agent", summary: "done", meta: `{"result":"ok"}`, turn: 2}.item(thread), nil); err != nil {
		t.Fatalf("append completion: %v", err)
	}
	for _, id := range []string{"A", "A-done"} {
		served, _, err := s.GetThreadItem(thread, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(subagentCardOf(t, served.Meta)) == 0 {
			t.Fatalf("a read of %s serves no card: %s", id, served.Meta)
		}
		stored, found, err := s.GetThreadItemForWrite(thread, id)
		if err != nil || !found {
			t.Fatalf("read %s for write: found=%v err=%v", id, found, err)
		}
		if card := subagentCardOf(t, stored.Meta); len(card) > 0 || stored.Meta != itemMetaForTest(t, s, thread, id) {
			t.Fatalf("%s for write reads %s, want the stored meta %s", id, stored.Meta, itemMetaForTest(t, s, thread, id))
		}
		// Writing back what a write reads stores no card key.
		stored.Status = "completed"
		if _, err := s.UpsertItem(stored, nil); err != nil {
			t.Fatalf("write back %s: %v", id, err)
		}
		updated, err := s.UpdateItemFields(thread, id, ItemPartialUpdate{Meta: &stored.Meta})
		if err != nil {
			t.Fatalf("write back %s's meta: %v", id, err)
		}
		if got, want := subagentCardOf(t, updated.Meta), subagentCardOf(t, served.Meta); !mapsEqual(got, want) {
			t.Errorf("UpdateItemFields returned %s with %v, a read serves %v", id, got, want)
		}
	}
	rows, err := s.RowsStoringServedSubagentKeys(10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("after writes of the stored meta, rows storing served keys = %v (%v), want none", rows, err)
	}

	// Writing back a served meta stores the card, and the check finds it.
	served, _, err := s.GetThreadItem(thread, "A")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateItemMeta(thread, "A", served.Meta); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.RowsStoringServedSubagentKeys(10); err != nil || !slices.Equal(rows, []string{thread + "/A"}) {
		t.Fatalf("after a served meta was written back, rows storing served keys = %v (%v), want %s/A", rows, err, thread)
	}
	if err := s.UpdateItemMeta(thread, "A", `{"input":{"prompt":"go"}}`); err != nil {
		t.Fatal(err)
	}

	// A Codex spawn's completion stores its card as a snapshot.
	insertWithCardForTest(t, s, stampFixtureRow{id: "S", kind: "tool_call", tool: "collab_agent", summary: "spawn", turn: 3}.item(thread))
	if _, err := s.AppendCompletionItem(Item{ID: "S", ThreadID: thread},
		stampFixtureRow{id: "S-done", kind: "tool_completion", tool: "collab_agent", summary: "done",
			meta: `{"subagentDescendantCount":4,"subagentLatestChildSummary":"codex work"}`, turn: 4}.item(thread), nil); err != nil {
		t.Fatalf("append spawn completion: %v", err)
	}
	if rows, err := s.RowsStoringServedSubagentKeys(10); err != nil || len(rows) != 0 {
		t.Fatalf("a spawn completion's snapshot counts as a stored served key: %v (%v)", rows, err)
	}
}

// TestSubagentCardIsRequired pins the handle requirement: outside a bulk
// writer, a visible row with a parent is written only with its parent's
// card, and a counted preview-kind row changes its summary only with it.
// A refused write stores nothing. Rows no card counts, a hidden row and a
// top-level one, need none, and a bulk writer recomputes instead.
func TestSubagentCardIsRequired(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-card-required"
	mustCreateThread(t, s, thread)
	mustCreateThread(t, s, "t-other")
	insertWithCardForTest(t, s, stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: l", status: "running", turn: 1}.item(thread))
	insertWithCardForTest(t, s, stampFixtureRow{id: "L2", kind: "tool_call", tool: "Agent", summary: "Agent: l2", status: "running", turn: 1, index: 1}.item(thread))
	insertWithCardForTest(t, s, stampFixtureRow{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 2}.item(thread))
	child := stampFixtureRow{id: "L-b2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "L", turn: 1, index: 3}.item(thread)
	stamps := subagentStampRowsForTest(t, s, thread)

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSubagentAnchor) {
			t.Errorf("%s: %v, want ErrSubagentAnchor", what, err)
		}
		if _, found, err := s.GetThreadItemForWrite(thread, child.ID); err != nil || found {
			t.Errorf("%s stored the row (found=%v err=%v)", what, found, err)
		}
	}
	refused("InsertItem without a card", s.InsertItem(child))
	_, err := s.AppendItem(child)
	refused("AppendItem without a card", err)
	refused("InsertItemWithPayload without a card", s.InsertItemWithPayload(child, Payload{ID: "p-b2", Kind: "text", Meta: "{}", Data: []byte("x"), CreatedAt: 1}))
	_, err = s.UpsertItem(child, nil)
	refused("UpsertItem without a card", err)

	card, err := s.OpenSubagentCard(thread, "L2")
	if err != nil {
		t.Fatal(err)
	}
	withCard := child
	withCard.SubagentCard = card
	refused("InsertItem with another parent's card", s.InsertItem(withCard))
	if err := card.Close(); err != nil {
		t.Fatal(err)
	}
	if card, err = s.OpenSubagentCard("t-other", "L"); err != nil {
		t.Fatal(err)
	}
	withCard.SubagentCard = card
	refused("InsertItem with another thread's card", s.InsertItem(withCard))
	if err := card.Close(); err != nil {
		t.Fatal(err)
	}
	if card, err = s.OpenSubagentCard(thread, "L"); err != nil {
		t.Fatal(err)
	}
	if err := card.Close(); err != nil {
		t.Fatal(err)
	}
	withCard.SubagentCard = card
	refused("InsertItem with a closed card", s.InsertItem(withCard))

	// A summary change of a counted preview-kind row needs the card; a
	// status change, which moves no card, does not.
	if _, err := s.UpdateItemFields(thread, "L-b1", ItemPartialUpdate{Summary: new("Bash: changed")}); !errors.Is(err, ErrSubagentAnchor) {
		t.Errorf("summary change without a card: %v, want ErrSubagentAnchor", err)
	}
	stored, _, err := s.GetThreadItemForWrite(thread, "L-b1")
	if err != nil || stored.Summary != "Bash: one" {
		t.Errorf("the refused summary change stored %q (%v)", stored.Summary, err)
	}
	stored.Summary = "Bash: rewritten"
	if _, err := s.UpsertItem(stored, nil); !errors.Is(err, ErrSubagentAnchor) {
		t.Errorf("whole-row summary change without a card: %v, want ErrSubagentAnchor", err)
	}
	if _, err := s.UpdateItemFields(thread, "L-b1", ItemPartialUpdate{Status: new("errored")}); err != nil {
		t.Errorf("status change without a card: %v", err)
	}
	if got := subagentStampRowsForTest(t, s, thread); !reflect.DeepEqual(got, stamps) {
		t.Errorf("the refused writes changed stamps: %v -> %v", stamps, got)
	}

	// Rows no card counts need none.
	if err := s.InsertItem(stampFixtureRow{id: "top", kind: "assistant_text", summary: "top", turn: 2}.item(thread)); err != nil {
		t.Errorf("top-level row: %v", err)
	}
	if err := s.InsertItem(stampFixtureRow{id: "L-plan", kind: "notification", tool: "plan_update", summary: "plan", parent: "L", turn: 1, index: 4}.item(thread)); err != nil {
		t.Errorf("hidden row under L: %v", err)
	}
	// A bulk writer writes counted rows without a card and recomputes the
	// chains it touched.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	w := s.bulkItemWrites(tx, thread, true)
	bulk := stampFixtureRow{id: "L-b3", kind: "tool_call", tool: "Bash", summary: "Bash: bulk", parent: "L", turn: 1, index: 3}.item(thread)
	applyItemDefaults(&bulk)
	if err := insertItemTx(tx, w, bulk, "test bulk"); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := w.finish(); err != nil {
		t.Fatalf("bulk finish: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The card writes what the refused ones could not.
	insertWithCardForTest(t, s, stampFixtureRow{id: "L-b5", kind: "tool_call", tool: "Bash", summary: "Bash: five", parent: "L", turn: 1, index: 5}.item(thread))
	assertSubagentStampParity(t, s, thread, "after the card writes", true)
	assertStampsAreTheRecompute(t, s, thread, "after the card writes")
}

// TestSubagentCardLivenessIsTheBootPass pins subagentCardLiveSQL to the
// boot pass: an anchor is live exactly when RecoverSubagentCards marks it
// from the running agents (liveSubagentAgentsSQL), as a running agent or
// the transcript root of one, across the columns that decide whether an
// agent runs, in a thread beside another with its own agents. A root a
// carrier names with padding is the one difference, on the side that
// flushes: the boot pass marks it, the probe does not.
func TestSubagentCardLivenessIsTheBootPass(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-card-live"
	mustCreateThread(t, s, thread)
	mustCreateThread(t, s, "t-other")
	meta := func(fields map[string]any) string {
		t.Helper()
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	rows := map[string][]stampFixtureRow{
		thread: {{id: "P", kind: "tool_call", tool: "Agent", summary: "Agent: parent", turn: 0}},
		// Another thread's running carrier names a root of this one.
		"t-other": {{id: "other", kind: "tool_call", tool: "Agent", summary: "Agent: other", status: "running", meta: carrierMeta("r1"), turn: 0}},
	}
	// Each combination is an agent (a<n>) and a carrier (c<n>) resuming a
	// completed root (r<n>) that runs only through it.
	n := 0
	for _, tool := range []string{"Agent", "collab_agent", "Bash"} {
		for _, status := range []string{"running", "completed", "streaming", "errored"} {
			for _, background := range []bool{false, true} {
				for _, active := range []any{nil, true, false} {
					for _, parent := range []string{"", "P"} {
						n++
						fields := map[string]any{}
						if active != nil {
							fields["live_background_active"] = active
						}
						agent := stampFixtureRow{id: fmt.Sprintf("a%d", n), kind: "tool_call", tool: tool, summary: "Agent: a",
							parent: parent, status: status, background: background, meta: meta(fields), turn: n}
						root := stampFixtureRow{id: fmt.Sprintf("r%d", n), kind: "tool_call", tool: "Agent", summary: "Agent: r", turn: n, index: 1}
						fields["transcript_root_id"] = root.id
						carrier := agent
						carrier.id, carrier.parent, carrier.index, carrier.meta = fmt.Sprintf("c%d", n), "", 2, meta(fields)
						rows[thread] = append(rows[thread], agent, root, carrier)
					}
				}
			}
		}
	}
	// A background launch its completion sibling settles (the v74
	// triggers), and a running carrier naming its root with padding.
	n++
	rows[thread] = append(rows[thread],
		stampFixtureRow{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: b", status: "running", background: true, turn: n},
		stampFixtureRow{id: "B-done", kind: "tool_completion", tool: "Agent", summary: "done", background: true, completionOf: "B", turn: n, index: 1},
		stampFixtureRow{id: "rp", kind: "tool_call", tool: "Agent", summary: "Agent: padded root", turn: n, index: 2},
		stampFixtureRow{id: "cp", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", status: "running", meta: carrierMeta(" rp "), turn: n, index: 3},
	)
	for _, threadID := range slices.Sorted(maps.Keys(rows)) {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		w := s.bulkItemWrites(tx, threadID, true)
		for _, r := range rows[threadID] {
			item := r.item(threadID)
			applyItemDefaults(&item)
			if err := insertItemTx(tx, w, item, "test liveness"); err != nil {
				t.Fatalf("insert %s/%s: %v", threadID, r.id, err)
			}
		}
		if err := w.finish(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	marked := make(map[string]bool)
	query, err := s.reader().Query(liveSubagentAgentsSQL)
	if err != nil {
		t.Fatal(err)
	}
	for query.Next() {
		var threadID, id, root string
		if err := query.Scan(&threadID, &id, &root); err != nil {
			t.Fatal(err)
		}
		if threadID != thread {
			continue
		}
		marked[id] = true
		if root != "" && root != id {
			marked[root] = true
		}
	}
	if err := errors.Join(query.Err(), query.Close()); err != nil {
		t.Fatal(err)
	}
	var live, idle, throughCarrier int
	for _, r := range rows[thread] {
		var got bool
		if err := s.reader().QueryRow(subagentCardLiveSQL, thread, r.id).Scan(&got); err != nil {
			t.Fatalf("probe %s: %v", r.id, err)
		}
		if r.id == "rp" {
			if got || !marked[r.id] {
				t.Errorf("the padded root probes live=%v, the boot pass marks it %v; want false and true", got, marked[r.id])
			}
			continue
		}
		if got != marked[r.id] {
			t.Errorf("%s (%s %s background=%v meta=%s) probes live=%v, the boot pass marks it %v",
				r.id, r.tool, r.status, r.background, r.meta, got, marked[r.id])
		}
		switch {
		case got && strings.HasPrefix(r.id, "r"):
			throughCarrier++
		case got:
			live++
		default:
			idle++
		}
	}
	// The grid reaches both answers and the carrier leg; B stopped when
	// its completion landed.
	if live == 0 || idle == 0 || throughCarrier == 0 {
		t.Errorf("live=%d idle=%d through a carrier=%d: the grid does not span the rule", live, idle, throughCarrier)
	}
	if marked["B"] {
		t.Error("the boot pass marks B, whose completion settled it")
	}
	if marked["r1"] != marked["c1"] {
		t.Error("another thread's carrier decided r1")
	}
}

// TestSubagentCardResolvesAgainWhenItsAgentStops pins the relive rule: a
// card kept open across writes reads its liveness again when its agent
// stops, whichever writer stops it, so a row written under the stopped
// agent reaches its stamps in its own transaction. A card whose agent
// still runs keeps accumulating after it.
func TestSubagentCardResolvesAgainWhenItsAgentStops(t *testing.T) {
	status := func(id, value string) func(*testing.T, *Store, string) {
		return func(t *testing.T, s *Store, thread string) {
			if _, err := s.UpdateItemFields(thread, id, ItemPartialUpdate{Status: &value}); err != nil {
				t.Fatalf("status of %s: %v", id, err)
			}
		}
	}
	launch := stampFixtureRow{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: l", status: "running", turn: 1}
	background := launch
	background.background = true
	for _, tc := range []struct {
		name string
		// rows are written in order, each under its parent's session card.
		rows   []stampFixtureRow
		parent string
		stop   func(t *testing.T, s *Store, thread string)
	}{
		{"foreground launch completes", []stampFixtureRow{launch}, "L", status("L", "completed")},
		{"foreground launch interrupted", []stampFixtureRow{launch}, "L", func(t *testing.T, s *Store, thread string) {
			row, _, err := s.GetThreadItemForWrite(thread, "L")
			if err != nil {
				t.Fatal(err)
			}
			if _, changed, err := s.ErrorActiveItemIfRevision(thread, "L", row.Rev, "interrupted", 9_000, nil); err != nil || !changed {
				t.Fatalf("interrupt L: changed=%v err=%v", changed, err)
			}
		}},
		{"foreground launch force-closed", []stampFixtureRow{launch}, "L", func(t *testing.T, s *Store, thread string) {
			if _, err := s.ForceCloseRunningToolCallsInTurn(thread, 1, func(string) string { return "closed" }, 9_000); err != nil {
				t.Fatal(err)
			}
		}},
		{"background launch gets its completion", []stampFixtureRow{background}, "L", func(t *testing.T, s *Store, thread string) {
			if _, err := s.AppendCompletionItem(Item{ID: "L", ThreadID: thread},
				stampFixtureRow{id: "L-done", kind: "tool_completion", tool: "Agent", summary: "done", turn: 3}.item(thread), nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"background session torn down", []stampFixtureRow{background}, "L", func(t *testing.T, s *Store, thread string) {
			if n, err := s.MarkLiveBackgroundToolCallsInactive(thread, 9_000); err != nil || n != 1 {
				t.Fatalf("mark inactive: %d, %v", n, err)
			}
		}},
		{"carrier of a completed root completes", []stampFixtureRow{
			{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: root", turn: 1},
			{id: "R-a1", kind: "assistant_text", summary: "round one", parent: "R", turn: 1, index: 1},
			{id: "C", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", status: "running", meta: carrierMeta("R"), turn: 2},
			{id: "P", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("C"), turn: 2, index: 1},
		}, "R", status("C", "completed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			const thread = "t-relive"
			mustCreateThread(t, s, thread)
			session := newCardSessionForTest(t, s, thread)
			write := func(r stampFixtureRow) {
				t.Helper()
				item := r.item(thread)
				item.SubagentCard = session.card(r.parent)
				if err := s.InsertItem(item); err != nil {
					t.Fatalf("insert %s: %v", r.id, err)
				}
			}
			// K runs throughout, in a turn no force-close reaches.
			write(stampFixtureRow{id: "K", kind: "tool_call", tool: "Agent", summary: "Agent: k", status: "running", turn: 5})
			for _, r := range tc.rows {
				write(r)
			}
			session.flush()
			stamps := subagentStampRowsForTest(t, s, thread)
			write(stampFixtureRow{id: "x-1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: tc.parent, turn: 4, index: 1})
			if pending := pendingCardsForTest(s, thread); !slices.Contains(pending, tc.parent) {
				t.Fatalf("the running agent's write left %v pending, want %s: the card is not live", pending, tc.parent)
			}
			if got := subagentStampRowsForTest(t, s, thread); !reflect.DeepEqual(got, stamps) {
				t.Fatal("the running agent's write changed stamp rows before any flush")
			}

			tc.stop(t, s, thread)
			write(stampFixtureRow{id: "x-2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: tc.parent, turn: 4, index: 2})
			if pending := pendingCardsForTest(s, thread); len(pending) > 0 {
				t.Errorf("the write under the stopped agent left %v pending: the card kept its liveness", pending)
			}
			assertSubagentStampParity(t, s, thread, "after the stop", true)
			assertStampsAreTheRecompute(t, s, thread, "after the stop")

			stamps = subagentStampRowsForTest(t, s, thread)
			write(stampFixtureRow{id: "K-1", kind: "tool_call", tool: "Bash", summary: "Bash: k", parent: "K", turn: 5, index: 1})
			if pending := pendingCardsForTest(s, thread); !reflect.DeepEqual(pending, []string{"K"}) {
				t.Errorf("the running K's write left %v pending, want [K]", pending)
			}
			if got := subagentStampRowsForTest(t, s, thread); !reflect.DeepEqual(got, stamps) {
				t.Error("the running K's write changed stamp rows before any flush")
			}
			session.flush()
			assertSubagentStampParity(t, s, thread, "after the flush", true)
			assertStampsAreTheRecompute(t, s, thread, "after the flush")
		})
	}
}

// TestSubagentCardInWriteFlushRollsBackWithItsWrite pins the undo of a
// flush inside an item write: when the flush fails, the write rolls back
// and the card and its accumulators are what they were before it, so the
// next write under the card flushes only rows that exist.
func TestSubagentCardInWriteFlushRollsBackWithItsWrite(t *testing.T) {
	s := newTestStore(t)
	const thread = "t-flush-undo"
	mustCreateThread(t, s, thread)
	session := newCardSessionForTest(t, s, thread)
	write := func(r stampFixtureRow) error {
		t.Helper()
		item := r.item(thread)
		item.SubagentCard = session.card(r.parent)
		return s.InsertItem(item)
	}
	for _, r := range []stampFixtureRow{
		{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: done", turn: 1},
		{id: "L-b1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "L", turn: 1, index: 1},
	} {
		if err := write(r); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	assertStampsAreTheRecompute(t, s, thread, "the completed agent's first row")

	mustExec(t, s.db, `CREATE TRIGGER fail_flush BEFORE UPDATE ON subagent_aggregates BEGIN SELECT RAISE(ABORT, 'injected flush failure'); END`)
	err := write(stampFixtureRow{id: "L-b2", kind: "tool_call", tool: "Bash", summary: "Bash: lost", parent: "L", turn: 1, index: 2})
	if err == nil || !strings.Contains(err.Error(), "injected flush failure") {
		t.Fatalf("write with a failing flush: %v, want the injected failure", err)
	}
	if _, found, err := s.GetThreadItemForWrite(thread, "L-b2"); err != nil || found {
		t.Fatalf("the failed write stored its row (found=%v err=%v)", found, err)
	}
	if pending := pendingCardsForTest(s, thread); len(pending) > 0 {
		t.Errorf("the failed write left %v pending", pending)
	}
	mustExec(t, s.db, `DROP TRIGGER fail_flush`)

	if err := write(stampFixtureRow{id: "L-b3", kind: "tool_call", tool: "Bash", summary: "Bash: three", parent: "L", turn: 1, index: 3}); err != nil {
		t.Fatalf("insert L-b3: %v", err)
	}
	if pending := pendingCardsForTest(s, thread); len(pending) > 0 {
		t.Errorf("the write after the failure left %v pending", pending)
	}
	assertSubagentStampParity(t, s, thread, "after the failed flush", true)
	assertStampsAreTheRecompute(t, s, thread, "after the failed flush")
}
