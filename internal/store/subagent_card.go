package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Subagent cards kept in memory between flushes.
//
// A writer that writes rows under a parent opens a card for that parent
// (OpenSubagentCard) and passes it with every row it writes there
// (Item.SubagentCard, ItemPartialUpdate.SubagentCard). The card holds one
// accumulator per stamp the rows reach: every local anchor up the
// parent's chain whose card counts them, with a resumed root's last round
// and whole-transcript counters, and the parent's tray. After a write
// commits, the store feeds the accumulators from the row it wrote, in Go,
// by the rules below; a write costs no statement for its cards. A flush
// writes each changed accumulator back with one keyed statement,
// optimistic on the stamp's generation, and recomputes a stamp whose
// generation moved. Every card of a thread shares one set of
// accumulators, so two agents under one root add to the same count.
//
// The rules, per stamp a counted row (a visible row with a parent)
// reaches:
//   - a root without resume rounds takes the row into its card: count,
//     newest position, and the preview when the row is previewable and
//     newer than the preview it has (betterSubagentPreview);
//   - a resumed root takes the row into its transcript count and newest
//     position, and its last round's carrier takes it into its card as
//     above; a row stored before that round's prompt makes the root
//     recompute at the next flush;
//   - a resume prompt directly under a root opens its next round
//     (notePrompt): the root keeps its count and takes the prompt into
//     its transcript, and the carrier the prompt names opens its card
//     with the prompt; a prompt stored before a row the root took, one
//     naming the root or a carrier the thread holds, and one under a
//     stamp that is not live recompute at the next flush. A prompt's
//     carrier counts only when the named row is anchorable and its
//     transcript root is the prompt's parent; otherwise the prompt cuts
//     the round with no card, and a prompt naming its root leaves its
//     family readTime (subagentResumeRounds, computeSubagentFamilies);
//   - the parent takes a toolable row into its tray when it is newer than
//     the tray row;
//   - a changed summary of the preview row or the tray row replaces it
//     while the row still qualifies, and otherwise makes that stamp
//     recompute; a changed summary of another row takes the preview or
//     the tray when it now qualifies and is newer.
//
// A carrier's own children count toward no card of the carrier, only
// toward its tray. A stamp the recompute left readTime is walked by every
// read and takes no value. A row that reaches a stamp and leaves its
// values, readTime or not, makes the next flush check that no recompute
// changed the stamp since: a writer that moves rows into the thread
// rewrites stamps outside the cards (recomputeLocalizedCardsTx). The item
// triggers serve every card a row changes anew: the anchors on its chain,
// and the carriers their prompts name, whose transcript root is on that
// chain.
//
// A write the rules do not follow recomputes the stamps it changed in its
// own transaction (recomputeSubagentChainsTx): a row inserted after rows
// already written under it, a carrier stored after the prompt that names
// it, a change of a row's parent, position, visibility, kind, tool or
// resume prompt identity, a row that starts or stops anchoring or changes
// its transcript root, a delete, and every bulk writer. Such a recompute
// retires the accumulators it covered and makes every card of the thread
// read its chain again at its next write.
//
// A card is exact at each flush; between flushes a read serves the stamp
// of the last one. A crash loses the rows written since the last flush
// from the stamps; the boot pass (RecoverSubagentCards, which
// RecoverCrashedTurns runs first) recomputes the stamps of every agent
// that was running. What the cards hold must stay within its reach
// (cardWrite.settle): a write with a card no running agent covers (live),
// and a write that stops an agent while a card it kept live holds rows no
// other live card reaches, flush the thread's cards in their own
// transaction. Whether a card is live is read when it resolves its chain;
// a write that can end or start an agent (a completion sibling, a change
// to a tool call row, a teardown) makes the cards it concerns resolve
// again (subagentCards.relive). A write that can stop an agent holds the
// lock of the thread's cards (writeItems, bulkWriteItems).

// ErrSubagentAnchor reports a write whose subagent card the store cannot
// accept: a visible row with a parent written without a card outside a
// bulk writer, a card opened for another parent or thread, or a closed
// card.
var ErrSubagentAnchor = errors.New("store: invalid subagent card")

// subagentRow is a written row as the card rules read it.
type subagentRow struct {
	id, parentID, kind, toolName, summary string
	turn, index                           int
	// prompt reports a resume prompt (aggPromptSQL) and carrier the
	// carrier it names, or "".
	prompt  bool
	carrier string
	// root is an anchorable row's transcript root (transcriptRootFromMeta),
	// or "" when it names none or itself.
	root string
	// completionOf is the launch a completion sibling settles, or "".
	completionOf string
	// status and background are the row's columns, and inactive reports
	// a meta whose live_background_active is false or 0: what decides
	// whether the row is a running agent (running).
	status               string
	background, inactive bool
}

func subagentRowOf(item Item) subagentRow {
	row := subagentRow{
		id: item.ID, parentID: item.ParentID, kind: item.Kind, toolName: item.ToolName,
		summary: item.Summary, turn: item.TurnIndex, index: item.ItemIndex, completionOf: item.CompletionOf,
		status: item.Status, background: item.IsBackground,
	}
	row.setMeta(item.Meta)
	return row
}

// setMeta derives the row's prompt identity, transcript root and
// background liveness from its meta.
func (r *subagentRow) setMeta(meta string) {
	r.prompt, r.carrier = subagentPromptFromMeta(r.kind, r.parentID, meta)
	r.root = ""
	if r.anchorable() {
		if root := transcriptRootFromMeta(meta); root != r.id {
			r.root = root
		}
	}
	r.inactive = liveBackgroundInactive(meta)
}

// liveBackgroundInactive is COALESCE(json_extract(meta,
// '$.live_background_active'), 1) = 0 on a valid meta: a teardown or a
// completion sibling settled the background launch.
func liveBackgroundInactive(meta string) bool {
	if !strings.Contains(meta, metaKeyLiveBackgroundActive) {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return false
	}
	switch v := decoded[metaKeyLiveBackgroundActive].(type) {
	case bool:
		return !v
	case float64:
		return v == 0
	}
	return false
}

// subagentPromptFromMeta is aggPromptSQL and aggPromptCarrierSQL in Go:
// a user_text row with a parent whose subagent_resume_prompt is true and
// whose resume_carrier_id is absent, null or a string.
func subagentPromptFromMeta(kind, parentID, meta string) (bool, string) {
	if kind != "user_text" || parentID == "" || !strings.Contains(meta, metaKeySubagentResumePrompt) {
		return false, ""
	}
	var decoded map[string]json.RawMessage
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return false, ""
	}
	if string(decoded[metaKeySubagentResumePrompt]) != "true" {
		return false, ""
	}
	raw, named := decoded[metaKeyResumeCarrierID]
	if !named || string(raw) == "null" {
		return true, ""
	}
	var carrier string
	if json.Unmarshal(raw, &carrier) != nil {
		return false, ""
	}
	return true, strings.Trim(carrier, subagentPreviewBlank)
}

// subagentRowColumns projects a stored row as subagentRowOf reads an
// Item, for scanSubagentRow. The meta is parsed only for a row that may
// be a resume prompt, a user_text row with a parent, or a carrier, an
// anchorable row whose meta names a transcript root, and for a meta that
// names live_background_active. `a` is the row reference with its
// trailing dot.
func subagentRowColumns(a string) string {
	return a + "id, " + a + "parent_id, " + a + "kind, " + a + "tool_name, " + a + "summary, " +
		a + "turn_index, " + a + "item_index, COALESCE(" + aggPromptSQL(a) + ", 0), " +
		"CASE WHEN " + aggPromptSQL(a) + " THEN " + aggPromptCarrierSQL(a) + " ELSE '' END, " +
		"CASE WHEN " + aggAnchorableSQL(a) + " AND instr(" + a + "meta, '" + metaKeyTranscriptRootID + "') THEN COALESCE(" +
		aggTranscriptRootSQL(a) + ", '') ELSE '' END, " + a + "completion_of, " + a + "status, " + a + "is_background, " +
		"CASE WHEN instr(" + a + "meta, '" + metaKeyLiveBackgroundActive + "') AND json_valid(" + a + "meta) THEN COALESCE(json_extract(" +
		a + "meta, '$." + metaKeyLiveBackgroundActive + "'), 1) = 0 ELSE 0 END"
}

// scanSubagentRow scans subagentRowColumns, then dest.
func scanSubagentRow(sc interface{ Scan(...any) error }, dest ...any) (subagentRow, error) {
	var r subagentRow
	err := sc.Scan(append([]any{&r.id, &r.parentID, &r.kind, &r.toolName, &r.summary,
		&r.turn, &r.index, &r.prompt, &r.carrier, &r.root, &r.completionOf, &r.status, &r.background, &r.inactive}, dest...)...)
	if r.root == r.id {
		r.root = ""
	}
	return r, err
}

// subagentRowSQL reads one local row for scanSubagentRow.
var subagentRowSQL = `SELECT ` + subagentRowColumns("") + ` FROM items WHERE thread_id = ? AND id = ?`

// readMutableSubagentRowTx is requireMutableItemTx that returns the row as
// the card rules read it.
func readMutableSubagentRowTx(tx *sql.Tx, threadID, itemID, label string) (subagentRow, error) {
	if err := handOffIDsTx(tx, threadID, []string{itemID}); err != nil {
		return subagentRow{}, fmt.Errorf("%s hand off item %s/%s: %w", label, threadID, itemID, err)
	}
	row, err := scanSubagentRow(tx.QueryRow(subagentRowSQL, threadID, itemID))
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return subagentRow{}, fmt.Errorf("%s inspect local item %s/%s: %w", label, threadID, itemID, err)
		}
		return row, nil
	}
	if err := ownShownItemTx(tx, threadID, itemID, label); err != nil {
		return subagentRow{}, err
	}
	if row, err = scanSubagentRow(tx.QueryRow(subagentRowSQL, threadID, itemID)); err != nil {
		return subagentRow{}, fmt.Errorf("%s read copied item %s/%s: %w", label, threadID, itemID, err)
	}
	return row, nil
}

// visible is visibleItemsFilterFor.
func (r subagentRow) visible() bool {
	return !(r.kind == "notification" && r.toolName == "plan_update")
}

// counts reports a row some card may count: a visible row with a parent.
func (r subagentRow) counts() bool { return r.parentID != "" && r.visible() }

func (r subagentRow) anchorable() bool { return SubagentAnchorable(r.kind, r.toolName) }

// running is liveSubagentAgentSQL: a running tool call the boot pass
// recovers the cards of, in the foreground or a live background launch.
func (r subagentRow) running() bool {
	return r.anchorable() && r.status == "running" && (!r.background || !r.inactive)
}

func (r subagentRow) position() TimelineCursor {
	return TimelineCursor{TurnIndex: r.turn, ItemIndex: r.index}
}

// previewable is previewableSubagentRow.
func (r subagentRow) previewable() bool { return previewableSubagentRow(r.kind, r.summary) }

// toolable is aggToolableSQL.
func (r subagentRow) toolable() bool {
	return r.anchorable() && strings.Trim(r.summary, subagentPreviewBlank) != ""
}

// newerThan is "the row is after the stored position, or none is stored".
func (r subagentRow) newerThan(turn, item sql.NullInt64) bool {
	if !turn.Valid {
		return true
	}
	return int64(r.turn) > turn.Int64 || (int64(r.turn) == turn.Int64 && item.Valid && int64(r.index) > item.Int64)
}

// beats is the preview and tray rule against a stored row: newer, or at
// the same position with a smaller id.
func (r subagentRow) beats(turn, item sql.NullInt64, id sql.NullString) bool {
	if r.newerThan(turn, item) {
		return true
	}
	return turn.Int64 == int64(r.turn) && item.Valid && item.Int64 == int64(r.index) && id.Valid && r.id < id.String
}

func (r subagentRow) pick(v *subagentStampValues) {
	v.Summary = sql.NullString{String: r.summary, Valid: true}
	v.PickTurn, v.PickItem, v.PickID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}

func (r subagentRow) tool(v *subagentStampValues) {
	v.ToolSummary = sql.NullString{String: strings.Trim(r.summary, subagentPreviewBlank), Valid: true}
	v.ToolTurn, v.ToolItem, v.ToolID = validInt(r.turn), validInt(r.index), sql.NullString{String: r.id, Valid: true}
}

// cardState is what a note may do to a stamp.
type cardState uint8

const (
	// cardLive is a clean stamp: notes keep its values exact.
	cardLive cardState = iota
	// cardInert is a readTime stamp: reads walk it, and a note only
	// marks it reached.
	cardInert
	// cardRecompute waits for the next flush to recompute it.
	cardRecompute
)

// cardStamp is one stamp's accumulator, shared by every card of the
// thread that reaches it.
type cardStamp struct {
	id    string
	state cardState
	// exists reports a stamp row; gen is its generation.
	exists bool
	gen    int64
	// stored is the row as last read or written, values the accumulator.
	stored, values subagentStampValues
	// carrier reports a carrier: its own children count toward no card of
	// it, only toward its tray.
	carrier bool
	// reached marks a stamp a note reached since the last flush.
	reached bool
	// openedBy is the root whose resume prompt opened this carrier's card
	// in memory (notePrompt), until its first stamp is written.
	openedBy string
	// rounds reports a live root with resume rounds. A row at or after lo
	// lands in its last round, whose carrier stamp is round (nil when the
	// round's prompt names none). roundID names that carrier until
	// linkRounds finds its accumulator.
	rounds  bool
	lo      TimelineCursor
	round   *cardStamp
	roundID string
}

func (st *cardStamp) pending() bool {
	return st.state == cardRecompute || st.reached || (st.state == cardLive && st.values != st.stored)
}

// takes reports whether a note that reaches the stamp changes its values.
func (st *cardStamp) takes() bool {
	switch st.state {
	case cardInert:
		st.reached = true
		return false
	case cardRecompute:
		return false
	}
	st.reached = true
	return true
}

// roundFor is the stamp whose card a row at pos counts toward under the
// root st: st itself, its last round's carrier, or nil. A row before the
// last round's prompt makes the root recompute.
func (st *cardStamp) roundFor(pos TimelineCursor) *cardStamp {
	if !st.rounds {
		return st
	}
	if cursorBefore(pos, st.lo) {
		st.state = cardRecompute
		return nil
	}
	return st.round
}

// addRow is a round's card taking one more row.
func (st *cardStamp) addRow(r subagentRow) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	v.Count = validInt(int(v.Count.Int64) + 1)
	if r.previewable() && r.beats(v.PickTurn, v.PickItem, v.PickID) {
		r.pick(v)
	}
	if r.newerThan(v.NewestTurn, v.NewestItem) {
		v.NewestTurn, v.NewestItem = validInt(r.turn), validInt(r.index)
	}
}

// changedRow is a round's card seeing a counted row's summary change.
func (st *cardStamp) changedRow(r subagentRow) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	switch {
	case v.PickID.Valid && v.PickID.String == r.id:
		if !r.previewable() {
			st.state = cardRecompute
			return
		}
		r.pick(v)
	case r.previewable() && r.beats(v.PickTurn, v.PickItem, v.PickID):
		r.pick(v)
	}
}

// trayRow is the parent's tray seeing a direct child written; changed
// reports a summary change of a row already stored.
func (st *cardStamp) trayRow(r subagentRow, changed bool) {
	if st == nil || !st.takes() {
		return
	}
	v := &st.values
	switch {
	case changed && v.ToolID.Valid && v.ToolID.String == r.id:
		if !r.toolable() {
			st.state = cardRecompute
			return
		}
		r.tool(v)
	case r.toolable() && r.beats(v.ToolTurn, v.ToolItem, v.ToolID):
		r.tool(v)
	}
}

// subagentCards is the store's registry of card accumulators, one entry
// per thread with open cards or unflushed values.
type subagentCards struct {
	mu      sync.Mutex
	threads map[string]*cardThread
}

type cardThread struct {
	id string
	// Guarded by subagentCards.mu: refs counts open cards and operations
	// in flight; stale holds the stamps a recompute rewrote since the
	// last drain, invalid reports that one ran and all that it covered
	// the whole thread.
	refs    int
	stale   map[string]struct{}
	invalid bool
	all     bool
	// relive names the anchors whose agents may have started or stopped
	// since the last drain; reliveAll covers every card of the thread.
	relive    map[string]struct{}
	reliveAll bool

	// mu serializes the thread's item writes that hold it, and the card
	// resolves and flushes. It is taken before the writer connection,
	// never while holding it.
	mu      sync.Mutex
	stamps  map[string]*cardStamp
	handles map[*SubagentCard]struct{}
	// seeds are anchors a note left for the next flush to recompute that
	// no accumulator holds: the carrier a new resume prompt names.
	seeds map[string]struct{}
}

func (c *subagentCards) acquire(threadID string, create bool) *cardThread {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		if !create {
			return nil
		}
		if c.threads == nil {
			c.threads = make(map[string]*cardThread)
		}
		// The maps wait for the first card (OpenSubagentCard): an item
		// write without one holds the entry only for its lock.
		t = &cardThread{id: threadID}
		c.threads[threadID] = t
	}
	t.refs++
	return t
}

// release drops one reference; the caller holds t.mu. The last reference
// to a thread that holds nothing drops its entry.
func (c *subagentCards) release(t *cardThread) {
	idle := len(t.handles) == 0 && len(t.seeds) == 0
	for _, st := range t.stamps {
		if !idle {
			break
		}
		idle = !st.pending()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.refs--
	if t.refs == 0 && idle && c.threads[t.id] == t {
		delete(c.threads, t.id)
	}
}

// invalidate records, inside the transaction of a recompute, that the
// stamps ids were rewritten. The next card operation on the thread drops
// their accumulators and makes every card read its chain again. It takes
// only the registry lock, so a writer holding the connection can call it.
func (c *subagentCards) invalidate(threadID string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return
	}
	t.invalid = true
	if t.stale == nil {
		t.stale = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		t.stale[id] = struct{}{}
	}
}

// invalidateThread is invalidate for every stamp of the thread: a writer
// rebuilt it (restampSubagentAggregatesTx).
func (c *subagentCards) invalidateThread(threadID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.threads[threadID]; t != nil {
		t.invalid, t.all = true, true
	}
}

// holds reports whether the registry has an entry for the thread: open
// cards, an operation in flight, or accumulators not flushed.
func (c *subagentCards) holds(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[threadID] != nil
}

// relive records that the agents of anchors ids may have started or
// stopped running, or with ids nil that any agent of the thread may
// have: the next card operation makes the cards whose liveness they
// decide read their chain again (SubagentCard.live). It takes only the
// registry lock, so a writer holding the connection can call it; a
// transaction that rolls back after it costs a resolve, never a stale
// card.
func (c *subagentCards) relive(threadID string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.threads[threadID]
	if t == nil {
		return
	}
	if ids == nil {
		t.reliveAll = true
		return
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if t.relive == nil {
			t.relive = make(map[string]struct{})
		}
		t.relive[id] = struct{}{}
	}
}

// resetAll is invalidateThread for every thread: the rows under the
// accumulators were replaced (RestoreFrom).
func (c *subagentCards) resetAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.threads {
		t.invalid, t.all = true, true
	}
}

// drain applies the invalidations recorded since the last operation. The
// caller holds t.mu and has begun its transaction, so every recompute it
// drains has committed or rolled back, but it cannot tell which: a
// retired accumulator holding values not flushed yet recomputes at its
// next flush (retire).
func (c *subagentCards) drain(t *cardThread) {
	c.mu.Lock()
	relive, reliveAll := t.relive, t.reliveAll
	t.relive, t.reliveAll = nil, false
	if !t.invalid {
		c.mu.Unlock()
		t.reresolve(relive, reliveAll)
		return
	}
	stale, all := t.stale, t.all
	t.stale, t.invalid, t.all = nil, false, false
	c.mu.Unlock()
	t.reresolve(relive, reliveAll)
	if all {
		stale = make(map[string]struct{}, len(t.stamps))
		for id := range t.stamps {
			stale[id] = struct{}{}
		}
	}
	t.retire(slices.Collect(maps.Keys(stale)), false)
}

// retire drops the accumulators of stamps a recompute rewrote and makes
// every card read its chain again. committed reports that the caller saw
// the recompute commit. Otherwise an accumulator holding values not
// flushed yet is kept to recompute at the next flush: if the recompute
// rolled back, its rows are in no stamp. The caller holds t.mu.
func (t *cardThread) retire(ids []string, committed bool) {
	for _, id := range ids {
		st := t.stamps[id]
		if st == nil {
			continue
		}
		if !committed && st.pending() {
			st.state, st.reached = cardRecompute, false
			continue
		}
		delete(t.stamps, id)
	}
	for _, st := range t.stamps {
		if st.round != nil && t.stamps[st.round.id] != st.round {
			st.round, st.state = nil, cardRecompute
		}
	}
	t.unresolve()
}

// unresolve makes every card of the thread read its chain at its next
// write. The caller holds t.mu.
func (t *cardThread) unresolve() {
	for h := range t.handles {
		h.resolved = false
	}
}

// reresolve makes the cards whose liveness an anchor in ids decides, or
// every card with all, read their chain at their next write. The caller
// holds t.mu.
func (t *cardThread) reresolve(ids map[string]struct{}, all bool) {
	if all {
		t.unresolve()
		return
	}
	for h := range t.handles {
		if _, named := ids[h.liveAnchor]; named && h.liveAnchor != "" {
			h.resolved = false
		}
	}
}

// snapshot records what applying a resolve, a write's notes and a flush
// can change, for a transaction that applies them before it commits
// (cardWrite.settle): the returned func puts them back. c is the write's
// card, or nil. The caller holds t.mu.
func (t *cardThread) snapshot(c *SubagentCard) func() {
	stamps, seeds := maps.Clone(t.stamps), maps.Clone(t.seeds)
	saved := make(map[*cardStamp]cardStamp, len(t.stamps))
	for _, st := range t.stamps {
		saved[st] = *st
	}
	card := func() {}
	if c != nil {
		levels, tray, orphan, resolved := slices.Clone(c.levels), c.tray, c.orphan, c.resolved
		live, liveAnchor := c.live, c.liveAnchor
		card = func() {
			c.levels, c.tray, c.orphan, c.resolved = levels, tray, orphan, resolved
			c.live, c.liveAnchor = live, liveAnchor
		}
	}
	return func() {
		for st, was := range saved {
			*st = was
		}
		t.stamps, t.seeds = stamps, seeds
		card()
	}
}

// uncovered reports an accumulator or seed holding changes no boot pass
// would recover once the agents stops names have stopped: one no live
// card reaches, the cards those agents kept live not counted. all
// reports that any agent of the thread may have stopped. The caller
// holds t.mu.
func (t *cardThread) uncovered(stops []string, all bool) bool {
	if !t.pending() {
		return false
	}
	if all || len(t.seeds) > 0 {
		return true
	}
	for _, st := range t.stamps {
		if st.pending() && !t.covered(st, stops) {
			return true
		}
	}
	return false
}

// covered reports whether a live card whose agent is not in stops reaches
// st: the boot pass recovers the stamps a live card reaches
// (SubagentCard.live). The caller holds t.mu.
func (t *cardThread) covered(st *cardStamp, stops []string) bool {
	for h := range t.handles {
		if !h.resolved || !h.live || h.orphan || slices.Contains(stops, h.liveAnchor) {
			continue
		}
		if h.tray == st {
			return true
		}
		for _, level := range h.levels {
			if level == st || level.round == st {
				return true
			}
		}
	}
	return false
}

// pending reports accumulators or seeds a flush would write. The caller
// holds t.mu.
func (t *cardThread) pending() bool {
	if len(t.seeds) > 0 {
		return true
	}
	for _, st := range t.stamps {
		if st.pending() {
			return true
		}
	}
	return false
}

// SubagentCard is a writer's handle on the cards the rows under one
// parent count toward. It is safe for concurrent use and valid until
// Close. The rows it counts reach the stored stamps at each flush (Flush,
// FlushSubagentCards) and at Close.
type SubagentCard struct {
	s        *Store
	t        *cardThread
	threadID string
	parentID string

	// Guarded by t.mu.
	closed   bool
	resolved bool
	// orphan reports a parent not stored yet: its rows count toward
	// nothing, and the card reads the chain again at every write until
	// the parent arrives.
	orphan bool
	// levels are the local anchors up the parent's chain whose card
	// counts the rows, parent first; tray is the parent's stamp when the
	// parent is a local anchorable row.
	levels []*cardStamp
	tray   *cardStamp
	// live reports that the boot pass would recover the stamps the rows
	// reach: the nearest anchor on the parent's chain, liveAnchor, is a
	// running agent or the transcript root of one (subagentCardLiveSQL).
	// A card with no anchor reaches no stamp and is live.
	live       bool
	liveAnchor string
}

// ThreadID is the thread the card was opened in.
func (c *SubagentCard) ThreadID() string { return c.threadID }

// ParentID is the parent of the rows the card counts.
func (c *SubagentCard) ParentID() string { return c.parentID }

// OpenSubagentCard opens the card for rows written under parentID. It
// reads the parent's chain once, makes an imported anchor on it local,
// and seeds an accumulator per stamp the rows reach from the stored stamp
// rows, recomputing one that is dirty or predates the stamps. A parent
// not stored yet gives a card that counts nothing until it arrives.
func (s *Store) OpenSubagentCard(threadID, parentID string) (*SubagentCard, error) {
	if threadID == "" || parentID == "" {
		return nil, fmt.Errorf("%w: a card needs a thread and a parent, got %q/%q", ErrSubagentAnchor, threadID, parentID)
	}
	t := s.cards.acquire(threadID, true)
	c := &SubagentCard{s: s, t: t, threadID: threadID, parentID: parentID}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handles == nil {
		t.stamps, t.handles, t.seeds = make(map[string]*cardStamp), make(map[*SubagentCard]struct{}), make(map[string]struct{})
	}
	t.handles[c] = struct{}{}
	if err := s.cardTxLocked(t, "open subagent card", func(tx *sql.Tx) (func(), error) {
		apply, _, _, err := s.resolveCardTx(tx, t, c)
		return apply, err
	}); err != nil {
		delete(t.handles, c)
		s.cards.release(t)
		return nil, err
	}
	return c, nil
}

// WithSubagentCard opens the card for parentID, runs fn with it, and
// closes it, which flushes what fn wrote. For a one-off writer.
func (s *Store) WithSubagentCard(threadID, parentID string, fn func(*SubagentCard) error) error {
	card, err := s.OpenSubagentCard(threadID, parentID)
	if err != nil {
		return err
	}
	return errors.Join(fn(card), card.Close())
}

// Flush writes the thread's changed accumulators to the stamps. It is
// FlushSubagentCards for the card's thread.
func (c *SubagentCard) Flush() ([]string, error) {
	return c.s.FlushSubagentCards(c.threadID)
}

// Close flushes the thread's changed accumulators and releases the card.
// A failed flush keeps them for the next FlushSubagentCards and is
// returned; the card is closed either way.
func (c *SubagentCard) Close() error {
	t := c.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	delete(t.handles, c)
	err := c.s.flushLocked(t, nil)
	t.collect()
	c.s.cards.release(t)
	return err
}

// FlushSubagentCards writes the thread's changed accumulators to their
// stamps, recomputing any stamp that moved since it was read, and returns
// the anchors whose stamp changed. A thread with nothing pending costs
// no statement.
func (s *Store) FlushSubagentCards(threadID string) ([]string, error) {
	t := s.cards.acquire(threadID, false)
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var changed []string
	err := s.flushLocked(t, &changed)
	t.collect()
	s.cards.release(t)
	return changed, err
}

// FlushAllSubagentCards flushes every thread's accumulators: the store is
// about to close.
func (s *Store) FlushAllSubagentCards() error {
	s.cards.mu.Lock()
	threadIDs := make([]string, 0, len(s.cards.threads))
	for id := range s.cards.threads {
		threadIDs = append(threadIDs, id)
	}
	s.cards.mu.Unlock()
	slices.Sort(threadIDs)
	var errs []error
	for _, id := range threadIDs {
		if _, err := s.FlushSubagentCards(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SubagentCardPending reports whether the anchor's stamp holds changes
// no flush has written. It reads memory only.
func (s *Store) SubagentCardPending(threadID, anchorID string) bool {
	t := s.cards.acquire(threadID, false)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stamps[anchorID]
	pending := st != nil && st.pending()
	s.cards.release(t)
	return pending
}

// collect drops the accumulators no card reaches that hold nothing to
// flush. The caller holds t.mu.
func (t *cardThread) collect() {
	reached := make(map[*cardStamp]struct{})
	for h := range t.handles {
		for _, st := range h.levels {
			reached[st] = struct{}{}
			if st.round != nil {
				reached[st.round] = struct{}{}
			}
		}
		if h.tray != nil {
			reached[h.tray] = struct{}{}
		}
	}
	for id, st := range t.stamps {
		if _, ok := reached[st]; !ok && !st.pending() {
			delete(t.stamps, id)
		}
	}
}

// cardTxLocked runs one card transaction: drain, fn, commit, and fn's
// apply once committed. The caller holds t.mu.
func (s *Store) cardTxLocked(t *cardThread, label string, fn func(tx *sql.Tx) (func(), error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin %s in %s: %w", label, t.id, err)
	}
	defer tx.Rollback()
	s.cards.drain(t)
	apply, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit %s in %s: %w", label, t.id, err)
	}
	if apply != nil {
		apply()
	}
	return nil
}

// subagentChainRow is one row on a parent chain, read over every arm.
// imported reports a row outside the local overlay: imported history, or
// a row a pointer fork reads from an ancestor.
type subagentChainRow struct {
	id, parentID, kind, toolName string
	imported, visible            bool
}

// subagentChainTx reads the logical parent chain from fromID upward, by
// primary key, fromID first. With visibleOnly the walk stops above the
// first hidden row, which a descendant walk does not pass. A missing row
// ends it.
func subagentChainTx(q sqlQueryer, threadID, fromID string, visibleOnly bool) ([]subagentChainRow, error) {
	var chain []subagentChainRow
	seen := make(map[string]bool)
	for id, depth := fromID, 0; id != "" && depth < 64 && !seen[id]; depth++ {
		seen[id] = true
		row := subagentChainRow{id: id}
		query, args, err := timelineArms(q, threadID, timelineSelection{
			Columns: func(_, rev string) string {
				return "items.parent_id, items.kind, items.tool_name, " + rev + " < 0, " + visibleItemsFilterFor("items.")
			},
			KeyFirst: true,
			Where:    "items.id = ?", WhereArgs: []any{id},
		})
		if err != nil {
			return nil, err
		}
		err = q.QueryRow(query, args...).Scan(&row.parentID, &row.kind, &row.toolName, &row.imported, &row.visible)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("store: walk subagent chain %s/%s: %w", threadID, id, err)
		}
		chain = append(chain, row)
		if visibleOnly && !row.visible {
			break
		}
		id = row.parentID
	}
	return chain, nil
}

// resolveCardTx reads the card's chain, seeds the accumulators it
// reaches that the thread does not hold yet, and reads whether the card
// is live. It returns the change to the registry, applied once the
// transaction commits, so a rollback leaves the registry as it was, and
// the card's liveness and the anchor that decides it.
func (s *Store) resolveCardTx(tx *sql.Tx, t *cardThread, c *SubagentCard) (func(), bool, string, error) {
	chain, err := subagentChainTx(tx, c.threadID, c.parentID, true)
	if err != nil {
		return nil, false, "", err
	}
	if len(chain) == 0 {
		return func() {
			c.levels, c.tray, c.orphan, c.resolved = nil, nil, true, true
			c.live, c.liveAnchor = true, ""
		}, true, "", nil
	}
	var anchors []string
	for _, row := range chain {
		if !SubagentAnchorable(row.kind, row.toolName) {
			continue
		}
		if row.imported {
			// An anchor outside the local overlay holds no stamp: an
			// imported one is localized, and one a pointer fork reads from
			// an ancestor is copied (shadowInheritedItemTx). Every anchor on
			// a resolved chain is then local, so no later copy can put a
			// stamp on the chain that no accumulator keeps.
			localized, err := localizeImportedItemTx(tx, c.threadID, row.id, "store: open subagent card")
			if err != nil {
				return nil, false, "", err
			}
			if !localized {
				if _, err := shadowInheritedItemTx(tx, c.threadID, row.id); err != nil {
					return nil, false, "", err
				}
			}
		}
		anchors = append(anchors, row.id)
	}

	fresh := make(map[string]*cardStamp)
	held := func(id string) *cardStamp {
		if st := fresh[id]; st != nil {
			return st
		}
		return t.stamps[id]
	}
	var missing []string
	for _, id := range anchors {
		if t.stamps[id] == nil {
			missing = append(missing, id)
		}
	}
	targets, err := subagentStampTargets(tx, c.threadID, missing)
	if err != nil {
		return nil, false, "", err
	}
	var recompute, liveRoots []string
	for _, id := range missing {
		target, ok := targets[id]
		if !ok {
			continue
		}
		st, err := seedCardStamp(tx, c.threadID, target)
		if err != nil {
			return nil, false, "", err
		}
		fresh[id] = st
		switch {
		case st.state == cardRecompute:
			recompute = append(recompute, id)
		case st.state == cardLive && !st.carrier:
			liveRoots = append(liveRoots, id)
		}
	}
	// A live root's rounds: its last round takes the rows, and a stamp
	// that disagrees with the prompts recomputes.
	if len(liveRoots) > 0 {
		rounds, err := subagentResumeRounds(tx, c.threadID, liveRoots)
		if err != nil {
			return nil, false, "", err
		}
		last := make(map[string]subagentRound, len(liveRoots))
		for _, round := range rounds {
			last[round.rootID] = round
		}
		for _, id := range liveRoots {
			st := fresh[id]
			round, resumed := last[id]
			consistent := resumed == st.stored.TranscriptCount.Valid
			if resumed && (round.imported || round.anchorID == id) {
				consistent = false
			}
			if resumed && consistent && round.anchorID != round.promptID {
				named := held(round.anchorID)
				if named == nil {
					targets, err := subagentStampTargets(tx, c.threadID, []string{round.anchorID})
					if err != nil {
						return nil, false, "", err
					}
					if target, ok := targets[round.anchorID]; ok && target.root == id {
						if named, err = seedCardStamp(tx, c.threadID, target); err != nil {
							return nil, false, "", err
						}
						fresh[round.anchorID] = named
					}
				}
				consistent = named != nil && named.carrier && named.state != cardRecompute
			}
			if !consistent {
				st.state = cardRecompute
				recompute = append(recompute, id)
				continue
			}
			if resumed {
				st.rounds = true
				st.lo = TimelineCursor{TurnIndex: round.turnIndex, ItemIndex: round.itemIndex}
				if round.anchorID != round.promptID {
					st.roundID = round.anchorID
				}
			}
		}
	}
	if len(recompute) > 0 {
		members, err := recomputeSubagentFamiliesTx(tx, c.threadID, recompute, nil)
		if err != nil {
			return nil, false, "", err
		}
		for _, id := range recompute {
			delete(fresh, id)
		}
		for _, m := range members {
			fresh[m.id] = cardStampOf(m)
		}
	}
	live, liveAnchor := true, ""
	if len(anchors) > 0 {
		liveAnchor = anchors[0]
		if err := tx.QueryRow(subagentCardLiveSQL, c.threadID, liveAnchor).Scan(&live); err != nil {
			return nil, false, "", fmt.Errorf("store: probe the agents under %s/%s: %w", c.threadID, liveAnchor, err)
		}
	}
	return func() {
		for _, st := range fresh {
			t.put(st)
		}
		t.linkRounds()
		c.levels, c.tray = nil, nil
		for i, id := range anchors {
			st := t.stamps[id]
			if st == nil {
				continue
			}
			if i == 0 && id == c.parentID {
				c.tray = st
			}
			if !st.carrier {
				c.levels = append(c.levels, st)
			}
		}
		c.orphan, c.resolved = false, true
		c.live, c.liveAnchor = live, liveAnchor
	}, live, liveAnchor, nil
}

// seedCardStamp turns a stored stamp into an accumulator: a clean stamp
// is live, a readTime one inert, and a dirty one, an unstamped carrier
// and an unstamped anchor with children recompute. An unstamped anchor
// without children is live from zero, as a new anchor is.
func seedCardStamp(q sqlQueryer, threadID string, target subagentStampTarget) (*cardStamp, error) {
	st := &cardStamp{id: target.id, carrier: target.root != ""}
	switch {
	case target.stamped && target.stored.State == aggStateClean:
		st.state, st.exists, st.gen = cardLive, true, target.gen
		st.stored, st.values = target.stored, target.stored
	case target.stamped && target.stored.State == aggStateReadTime:
		st.state, st.exists, st.gen = cardInert, true, target.gen
		st.stored, st.values = target.stored, target.stored
	case target.stamped || st.carrier:
		st.state = cardRecompute
	default:
		var hasChild bool
		if err := q.QueryRow(`SELECT `+aggHasChildSQL("?1", "?2", ""), threadID, target.id).Scan(&hasChild); err != nil {
			return nil, fmt.Errorf("store: probe subagent children %s/%s: %w", threadID, target.id, err)
		}
		if hasChild {
			st.state = cardRecompute
			break
		}
		st.state = cardLive
		st.stored = subagentStampValues{State: aggStateClean}
		st.values = st.stored
	}
	return st, nil
}

// cardStampOf is a recomputed stamp as an accumulator.
func cardStampOf(m subagentFamilyMember) *cardStamp {
	st := &cardStamp{id: m.id, carrier: m.root != "", exists: true, gen: m.gen,
		stored: m.values, values: m.values, rounds: m.rounds, lo: m.lo, roundID: m.roundID}
	if m.values.State != aggStateClean {
		st.state = cardInert
	}
	return st
}

// put stores st as the thread's accumulator for its id, in place when one
// is held, so every card that reaches it sees the new values.
func (t *cardThread) put(st *cardStamp) {
	if held := t.stamps[st.id]; held != nil {
		*held = *st
		return
	}
	t.stamps[st.id] = st
}

// linkRounds points each resumed root at its last round's carrier
// accumulator. A recompute gives every stored anchorable carrier one, so
// a root left without is one whose last round names a carrier not stored
// as an anchorable row: its rows count toward its transcript alone, as
// the recompute counts them, until the carrier's insert recomputes the
// family (cardWrite.inserted).
func (t *cardThread) linkRounds() {
	for _, st := range t.stamps {
		if st.roundID == "" {
			continue
		}
		st.round = t.stamps[st.roundID]
		st.roundID = ""
	}
}

// Flush.

var (
	// flushSubagentStampSQL writes an accumulator back to its stamp row
	// while the row is at the generation and state it was read at.
	flushSubagentStampSQL = `UPDATE subagent_aggregates SET ` +
		strings.Join(subagentAggregateValueColumns, " = ?, ") + ` = ?
 WHERE thread_id = ? AND item_id = ? AND gen = ? AND state = ` + aggCleanLiteral
	// flushSubagentStampInsertSQL writes the first stamp of an anchor
	// that had none, while it has none and the row still anchors.
	flushSubagentStampInsertSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
		strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ` + aggCleanLiteral + `, ?3` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items a WHERE a.thread_id = ?1 AND a.id = ?2 AND ` + aggAnchorableSQL("a.") + `)
ON CONFLICT (thread_id, item_id) DO NOTHING`
	// flushSubagentCarrierInsertSQL writes the first stamp of a carrier a
	// resume prompt opened in memory (notePrompt), while the row is still
	// the carrier of the root the last parameter names, with no stamp and
	// no child of its own.
	flushSubagentCarrierInsertSQL = `INSERT INTO subagent_aggregates (thread_id, item_id, state, gen, ` +
		strings.Join(subagentAggregateValueColumns, ", ") + `)
SELECT ?1, ?2, ` + aggCleanLiteral + `, ?3` + strings.Repeat(", ?", len(subagentAggregateValueColumns)) + `
 WHERE EXISTS (SELECT 1 FROM items a WHERE a.thread_id = ?1 AND a.id = ?2 AND ` + aggAnchorableSQL("a.") + `
                 AND ` + aggTranscriptRootSQL("a.") + ` = ` + carrierRootParam + `
                 AND NOT ` + aggHasLocalChildSQL("a.thread_id", "a.id", "") + `)
ON CONFLICT (thread_id, item_id) DO NOTHING`
)

// carrierRootParam is flushSubagentCarrierInsertSQL's root parameter,
// after the thread, the id, the generation and the values.
var carrierRootParam = fmt.Sprintf("?%d", 4+len(subagentAggregateValueColumns))

// flushLocked writes the thread's pending accumulators. The caller holds
// t.mu. A thread with nothing pending runs no transaction. changed, when
// not nil, receives the anchors whose stamp changed.
func (s *Store) flushLocked(t *cardThread, changed *[]string) error {
	if !t.pending() {
		return nil
	}
	return s.cardTxLocked(t, "flush subagent cards", func(tx *sql.Tx) (func(), error) {
		return s.flushCardsTx(tx, t, changed, subagentBumpOnce(tx, t.id))
	})
}

// flushCardsTx writes the thread's pending accumulators in tx. bump
// advances the thread stamp before the first stamp write; a flush inside
// an item write, whose own rows already did, passes one that does
// nothing.
func (s *Store) flushCardsTx(tx *sql.Tx, t *cardThread, changedOut *[]string, bump func() error) (func(), error) {
	ids := make([]string, 0, len(t.stamps))
	for id, st := range t.stamps {
		if st.pending() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	seeds := make([]string, 0, len(t.seeds))
	for id := range t.seeds {
		seeds = append(seeds, id)
	}
	type landed struct {
		st  *cardStamp
		gen int64
	}
	var wrote []landed
	// checked are the stamps a note reached and left as they were, whose
	// generation the flush checked: their reach is reset once it commits.
	var checked []*cardStamp
	var changed []string
	for _, id := range ids {
		st := t.stamps[id]
		unchanged := st.state == cardInert || st.values == st.stored
		switch {
		case st.state == cardRecompute:
			seeds = append(seeds, id)
		case unchanged && !st.exists && st.state == cardLive:
			checked = append(checked, st)
		case unchanged:
			want := int64(aggStateClean)
			if st.state == cardInert {
				want = aggStateReadTime
			}
			var gen, state int64
			err := tx.QueryRow(`SELECT gen, state FROM subagent_aggregates WHERE thread_id = ? AND item_id = ?`, t.id, id).Scan(&gen, &state)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				seeds = append(seeds, id)
			case err != nil:
				return nil, fmt.Errorf("store: check subagent stamp %s/%s: %w", t.id, id, err)
			case gen != st.gen || state != want:
				seeds = append(seeds, id)
			default:
				checked = append(checked, st)
			}
		default:
			if err := bump(); err != nil {
				return nil, err
			}
			var result sql.Result
			var err error
			gen := st.gen
			switch {
			case st.exists:
				args := append(st.values.args(), t.id, id, st.gen)
				result, err = tx.Exec(flushSubagentStampSQL, args...)
			case st.openedBy != "":
				gen = newSubagentGen()
				args := append(append([]any{t.id, id, gen}, st.values.args()...), st.openedBy)
				result, err = tx.Exec(flushSubagentCarrierInsertSQL, args...)
			default:
				gen = newSubagentGen()
				args := append([]any{t.id, id, gen}, st.values.args()...)
				result, err = tx.Exec(flushSubagentStampInsertSQL, args...)
			}
			if err != nil {
				return nil, fmt.Errorf("store: flush subagent stamp %s/%s: %w", t.id, id, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("store: count flushed subagent stamp %s/%s: %w", t.id, id, err)
			}
			if n == 0 {
				// The stamp moved, or the carrier a prompt opened is not
				// that root's childless carrier: the recompute decides,
				// the root's family with it.
				seeds = append(seeds, id)
				if st.openedBy != "" {
					seeds = append(seeds, st.openedBy)
				}
				continue
			}
			wrote = append(wrote, landed{st, gen})
			changed = append(changed, id)
		}
	}
	var members []subagentFamilyMember
	if len(seeds) > 0 {
		var err error
		if members, err = recomputeSubagentFamiliesTx(tx, t.id, seeds, bump); err != nil {
			return nil, err
		}
		for _, m := range members {
			if m.written {
				changed = append(changed, m.id)
			}
		}
	}
	if changedOut != nil {
		slices.Sort(changed)
		*changedOut = slices.Compact(changed)
	}
	return func() {
		for _, w := range wrote {
			w.st.stored, w.st.exists, w.st.gen, w.st.openedBy, w.st.reached = w.st.values, true, w.gen, "", false
		}
		for _, st := range checked {
			st.reached = false
		}
		fresh := make(map[string]*cardStamp, len(members))
		for _, m := range members {
			fresh[m.id] = cardStampOf(m)
		}
		var vanished []string
		for _, id := range seeds {
			if st := fresh[id]; st != nil {
				t.put(st)
			} else if t.stamps[id] != nil {
				vanished = append(vanished, id)
			}
		}
		for id, st := range fresh {
			if t.stamps[id] != nil {
				t.put(st)
			}
		}
		if len(vanished) > 0 {
			// A stamp the recompute found no anchor for: the row is gone,
			// stopped anchoring, or is a carrier a prompt named before it
			// was stored. Retired before the rounds link, so a root whose
			// last round names it links to no round card.
			t.retire(vanished, true)
		}
		for _, st := range t.stamps {
			if st.roundID != "" && t.stamps[st.roundID] == nil && fresh[st.roundID] != nil {
				t.put(fresh[st.roundID])
			}
		}
		t.linkRounds()
		clear(t.seeds)
	}, nil
}

// subagentBumpOnce advances the thread stamp before the first stamp write
// of a transaction: the stamp triggers stamp each anchor they write with
// the thread's history_rev, which must be one no reader has seen.
func subagentBumpOnce(tx *sql.Tx, threadID string) func() error {
	bumped := false
	return func() error {
		if bumped {
			return nil
		}
		if _, err := tx.Exec(bumpSubagentAggregateRevSQL, threadID); err != nil {
			return fmt.Errorf("store: bump history for subagent stamps in %s: %w", threadID, err)
		}
		bumped = true
		return nil
	}
}

// subagentCardLiveSQL reports whether the boot pass would recover the
// stamps a card's rows reach (RecoverSubagentCards): the card's nearest
// anchor ?2 is a running agent (liveSubagentAgentSQL), or a running
// agent's transcript root, a carrier it resumes (idx_items_transcript_root).
// The root matches as stored, where the boot pass trims it: under a root
// a carrier names with padding, which no provider writes, a card is not
// live and its writes flush in their own transaction.
var subagentCardLiveSQL = `SELECT EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND id = ?2 AND ` + liveSubagentAgentSQL("") + `)
 OR EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND ` + transcriptRootExpr + ` = ?2 AND ` + liveSubagentAgentSQL("") + `)`

// subagentPromptNamesSQL reports whether a local resume prompt directly
// under the root ?2 names the carrier ?3 (idx_items_subagent_resume_prompt).
var subagentPromptNamesSQL = `SELECT EXISTS (SELECT 1 FROM items p
 WHERE p.thread_id = ?1 AND p.parent_id = ?2 AND ` + aggPromptSQL("p.") + ` AND ` + aggPromptCarrierSQL("p.") + ` = ?3)`

// Writes.

// cardWrite is one write transaction's effect on the subagent cards: the
// rows its card counts, and what the rules do not follow, which the
// transaction recomputes before it commits (finish).
type cardWrite struct {
	s        *Store
	tx       *sql.Tx
	threadID string
	card     *SubagentCard
	// bulk permits counted rows without a card: the writer recomputes
	// every chain it touched instead. bump makes the recompute advance
	// the thread stamp, for a bulk writer none of whose writes did.
	bulk, bump bool

	inserts, changes []subagentRow
	chains, seeds    []string
	unanchor         []string
	// carriers are inserted carriers, whose root may hold a prompt that
	// named them before they arrived.
	carriers []subagentRow
	// relive names the anchors whose agents the write may have started
	// or stopped (subagentCards.relive); reliveAll is every agent of the
	// thread, for a completion sibling written or deleted, whose trigger
	// settles or revives its launch.
	relive    []string
	reliveAll bool
	// stops name the agents the write stopped and the transcript roots
	// they kept live; stopAll is any agent of the thread, for a
	// completion sibling written, whose trigger settles its launch.
	stops   []string
	stopAll bool
	// stale lists the stamps finish recomputed, once it has.
	stale   []string
	touched bool

	// t is the thread's cards, when the writer holds their lock
	// (writeItems, bulkWriteItems): finish then settles what the write
	// leaves in memory. A write without it may stop an agent only in a
	// thread whose cards hold nothing, or in a boot sweep (sweep), which
	// runs before any card is opened.
	t     *cardThread
	sweep bool
	// live and liveAnchor are the card's liveness as the transaction read
	// it; resolved applies the chain it read.
	live       bool
	liveAnchor string
	resolved   func()
	// undo puts back what settle applied before the commit.
	undo func()
}

// check refuses a row the card does not cover: one under another parent,
// or a counted row written without a card outside a bulk writer.
func (w *cardWrite) check(row subagentRow) error {
	if w.card != nil && w.card.parentID != row.parentID {
		return fmt.Errorf("%w: %s/%s is under %q, its card under %q", ErrSubagentAnchor, w.threadID, row.id, row.parentID, w.card.parentID)
	}
	if w.card == nil && !w.bulk && row.counts() {
		return fmt.Errorf("%w: %s/%s under %s was written without its card", ErrSubagentAnchor, w.threadID, row.id, row.parentID)
	}
	return nil
}

// inserted records an inserted row. hasChild reports rows already stored
// under it, which it adopts: its card and its chain's recompute.
func (w *cardWrite) inserted(row subagentRow, hasChild bool) {
	if row.completionOf != "" {
		w.reliveAll, w.stopAll = true, true
	}
	if row.anchorable() {
		w.relive = append(w.relive, row.root)
	}
	if row.anchorable() && row.root != "" && !hasChild {
		w.carriers = append(w.carriers, row)
	}
	if hasChild {
		if row.anchorable() {
			w.seeds = append(w.seeds, row.id)
		}
		if row.counts() {
			w.chains = append(w.chains, row.parentID)
		}
	}
	if !row.counts() {
		return
	}
	if w.card == nil {
		w.chains = append(w.chains, row.parentID)
		if row.prompt {
			w.seeds = append(w.seeds, row.carrier)
		}
		return
	}
	w.inserts = append(w.inserts, row)
}

// updated records an update from old to row: a move between cards or
// rounds recomputes both chains, a row that starts or stops anchoring or
// changes its transcript root recomputes its family, and a summary change
// of a counted preview-kind row is a note for its card.
func (w *cardWrite) updated(old, row subagentRow) error {
	if w.card != nil && w.card.parentID != row.parentID {
		return fmt.Errorf("%w: %s/%s is under %q, its card under %q", ErrSubagentAnchor, w.threadID, row.id, row.parentID, w.card.parentID)
	}
	if old.anchorable() || row.anchorable() {
		// A tool call row's status, background flag or meta decides
		// whether its agent runs, for it and for its transcript root.
		w.relive = append(w.relive, row.id, old.root, row.root)
	}
	if old.running() && (!row.running() || old.root != row.root) {
		w.stops = append(w.stops, old.id, old.root)
	}
	if old.completionOf != row.completionOf {
		w.reliveAll = true
	}
	structural := (old.counts() || row.counts()) && (old.parentID != row.parentID ||
		old.turn != row.turn || old.index != row.index || old.visible() != row.visible() ||
		old.kind != row.kind || old.toolName != row.toolName ||
		old.prompt != row.prompt || old.carrier != row.carrier)
	if structural {
		w.chains = append(w.chains, old.parentID, row.parentID)
		w.seeds = append(w.seeds, old.carrier, row.carrier)
	}
	if old.anchorable() != row.anchorable() || old.root != row.root {
		w.seeds = append(w.seeds, row.id, old.root, row.root)
		if old.anchorable() && !row.anchorable() {
			w.unanchor = append(w.unanchor, row.id)
		}
	}
	if structural || !row.counts() || old.summary == row.summary || !previewKind(row.kind) {
		return nil
	}
	switch {
	case w.card != nil:
		w.changes = append(w.changes, row)
	case w.bulk:
		w.chains = append(w.chains, row.parentID)
	default:
		return fmt.Errorf("%w: %s/%s under %s changed its summary without its card", ErrSubagentAnchor, w.threadID, row.id, row.parentID)
	}
	return nil
}

// deleted records a deleted row: its chain loses it and its subtree, a
// prompt's carrier loses its round, and its own stamp goes with it.
func (w *cardWrite) deleted(old subagentRow) {
	if old.completionOf != "" {
		w.reliveAll = true
	}
	if old.anchorable() {
		w.relive = append(w.relive, old.id, old.root)
	}
	if old.running() {
		w.stops = append(w.stops, old.id, old.root)
	}
	if old.counts() {
		w.chains = append(w.chains, old.parentID)
	}
	if old.prompt {
		w.seeds = append(w.seeds, old.carrier)
	}
	if old.anchorable() {
		w.seeds = append(w.seeds, old.id, old.root)
	}
}

// subtreesChanged records anchors whose subtrees the write changed without
// writing a row of theirs: a pointer fork's copied anchors when the fork
// stops showing rows it inherits (forkViewChangedTx). finish recomputes
// them with their families.
func (w *cardWrite) subtreesChanged(ids []string) {
	w.seeds = append(w.seeds, ids...)
}

// finish recomputes what the write changed that the rules do not follow,
// then settles what it leaves in memory (settle). A write that recomputes
// anything recomputes its noted rows' chains with it, and its card takes
// no note. A writer without the cards' lock has every accumulator of a
// stamp it recomputed retired at the next card operation.
func (w *cardWrite) finish() error {
	switch {
	case w.reliveAll:
		w.s.cards.relive(w.threadID, nil)
	case len(w.relive) > 0:
		w.s.cards.relive(w.threadID, w.relive)
	}
	w.relive, w.reliveAll = nil, false
	if err := w.recompute(); err != nil {
		return err
	}
	return w.settle()
}

// recompute is finish's recompute of what the rules do not follow.
func (w *cardWrite) recompute() error {
	// A carrier stored after the prompt that names it: the root's last
	// round has had no card (linkRounds), so the family is recomputed.
	for _, row := range w.carriers {
		var named bool
		if err := w.tx.QueryRow(subagentPromptNamesSQL, w.threadID, row.root, row.id).Scan(&named); err != nil {
			return fmt.Errorf("store: probe the prompts naming %s/%s: %w", w.threadID, row.id, err)
		}
		if named {
			w.seeds = append(w.seeds, row.id)
		}
	}
	w.carriers = nil
	if len(w.chains) == 0 && len(w.seeds) == 0 && len(w.unanchor) == 0 {
		return nil
	}
	w.touched = true
	for _, row := range append(w.inserts, w.changes...) {
		w.chains = append(w.chains, row.parentID)
		if row.prompt {
			w.seeds = append(w.seeds, row.carrier)
		}
	}
	w.inserts, w.changes = nil, nil
	// A writer whose own writes advanced the thread stamp needs no bump.
	bump := func() error { return nil }
	if w.bump {
		bump = subagentBumpOnce(w.tx, w.threadID)
	}
	stale, err := w.s.recomputeSubagentChainsTx(w.tx, w.threadID, w.chains, w.seeds, w.unanchor, bump)
	if err != nil {
		return err
	}
	w.chains, w.seeds, w.unanchor = nil, nil, nil
	w.stale = append(w.stale, stale...)
	if w.t == nil {
		w.s.cards.invalidate(w.threadID, stale)
	}
	return nil
}

// settle keeps what the write leaves in memory recoverable by the boot
// pass. The notes of a card that is not live, or whose agent the write
// stopped, and the accumulators the stopped agents kept recoverable that
// no other live card reaches (uncovered), are written to their stamps in
// this transaction: settle applies the notes before the commit, and undo
// puts them back if it does not commit. The rows advanced the thread
// stamp in it, so the flush adds no bump.
func (w *cardWrite) settle() error {
	stops := slices.DeleteFunc(w.stops, func(id string) bool { return id == "" })
	all := w.stopAll
	w.stops, w.stopAll = nil, false
	t := w.t
	if t == nil {
		// Without the lock, only a thread whose cards hold nothing can
		// lose no accumulator: a card changes them in a transaction of
		// its own, and holds the thread's entry until it has applied them.
		if (len(stops) > 0 || all) && !w.sweep && w.s.cards.holds(w.threadID) {
			return fmt.Errorf("store: a write that stops an agent of %s holds no lock on its subagent cards", w.threadID)
		}
		return nil
	}
	noted := len(w.inserts)+len(w.changes) > 0
	// The notes of a card that stays live reach stamps its agent keeps
	// recoverable; they are applied once the write commits.
	kept := !noted || (w.live && !all && !slices.Contains(stops, w.liveAnchor))
	if kept && (len(stops) == 0 && !all || !t.uncovered(stops, all)) {
		return nil
	}
	if w.undo == nil {
		w.undo = t.snapshot(w.card)
	}
	w.apply()
	flushed, err := w.s.flushCardsTx(w.tx, t, nil, func() error { return nil })
	if err != nil {
		return err
	}
	flushed()
	return nil
}

// apply feeds the cards what the write left them, once: the chain the
// card's transaction read, then its notes, or after a recompute the
// retire of the accumulators it rewrote. It runs when the write commits,
// or before from settle, whose undo puts it back.
func (w *cardWrite) apply() {
	if w.resolved != nil {
		w.resolved()
		w.resolved = nil
	}
	if w.touched {
		w.touched = false
		w.t.retire(w.stale, true)
		w.stale = nil
		return
	}
	for _, row := range w.inserts {
		w.card.noteInsert(row)
	}
	for _, row := range w.changes {
		w.card.noteChange(row)
	}
	w.inserts, w.changes = nil, nil
}

// writeItems runs one item write transaction under the lock of the
// thread's cards. card is the card the write carries, or nil. fn records
// what it wrote through w; the transaction recomputes what the rules do
// not follow and settles what no boot pass would recover before it
// commits, and the cards take the rest once it has.
func (s *Store) writeItems(threadID string, card *SubagentCard, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	return s.itemWriteTx(threadID, card, false, label, fn)
}

// bulkWriteItems is writeItems for a bulk writer of one thread: counted
// rows need no card, and fn recomputes every chain it touched.
func (s *Store) bulkWriteItems(threadID, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	return s.itemWriteTx(threadID, nil, true, label, fn)
}

func (s *Store) itemWriteTx(threadID string, card *SubagentCard, bulk bool, label string, fn func(tx *sql.Tx, w *cardWrite) error) error {
	t := s.cards.acquire(threadID, true)
	t.mu.Lock()
	defer t.mu.Unlock()
	defer s.cards.release(t)
	if card != nil {
		if card.s != s || card.threadID != threadID {
			return fmt.Errorf("%w: a card of %s/%s written in %s", ErrSubagentAnchor, card.threadID, card.parentID, threadID)
		}
		// An open card keeps its thread's entry: another entry means the
		// card was closed.
		if card.t != t || card.closed {
			return fmt.Errorf("%w: the card of %s/%s is closed", ErrSubagentAnchor, card.threadID, card.parentID)
		}
	}
	w := &cardWrite{s: s, threadID: threadID, card: card, bulk: bulk, t: t, live: true}
	err := s.cardTxLocked(t, label, func(tx *sql.Tx) (func(), error) {
		w.tx = tx
		if card != nil {
			w.live, w.liveAnchor = card.live, card.liveAnchor
			if !card.resolved || card.orphan {
				var err error
				if w.resolved, w.live, w.liveAnchor, err = s.resolveCardTx(tx, t, card); err != nil {
					return nil, err
				}
			}
		}
		if err := fn(tx, w); err != nil {
			return nil, err
		}
		if err := w.finish(); err != nil {
			return nil, err
		}
		return w.apply, nil
	})
	if err != nil && w.undo != nil {
		w.undo()
	}
	return err
}

// bulkItemWrites is the cardWrite of a bulk writer's transaction that
// does not hold the lock of the thread's cards: counted rows need no
// card, and the writer calls finish before it commits. It stops an agent
// only in a thread whose cards hold nothing (settle). bump is for a
// writer whose own writes do not advance the thread stamp.
func (s *Store) bulkItemWrites(tx *sql.Tx, threadID string, bump bool) *cardWrite {
	return &cardWrite{s: s, tx: tx, threadID: threadID, bulk: true, bump: bump}
}

// sweepItemWrites is bulkItemWrites for a boot sweep of every thread,
// which stops agents: it runs before any card is opened, once
// FlushAllSubagentCards has written any the store holds.
func (s *Store) sweepItemWrites(tx *sql.Tx, threadID string) *cardWrite {
	return &cardWrite{s: s, tx: tx, threadID: threadID, bulk: true, sweep: true}
}

// noteInsert feeds a committed counted row to the card's accumulators.
func (c *SubagentCard) noteInsert(r subagentRow) {
	if c.orphan {
		return
	}
	pos := r.position()
	for i, st := range c.levels {
		if r.prompt && i == 0 && st.id == r.parentID {
			c.notePrompt(st, r)
			continue
		}
		if !st.rounds {
			st.addRow(r)
			continue
		}
		if st.state != cardLive {
			continue
		}
		round := st.roundFor(pos)
		if st.state != cardLive {
			continue
		}
		v := &st.values
		v.TranscriptCount = validInt(int(v.TranscriptCount.Int64) + 1)
		if r.newerThan(v.TranscriptNewestTurn, v.TranscriptNewestItem) {
			v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(r.turn), validInt(r.index)
		}
		round.addRow(r)
	}
	if r.prompt && c.tray != nil && c.tray.carrier {
		// A round resumed from a carrier: a shape the recompute decides.
		c.tray.state = cardRecompute
		if r.carrier != "" {
			c.t.seeds[r.carrier] = struct{}{}
		}
	}
	c.tray.trayRow(r, false)
}

// notePrompt feeds a resume prompt written directly under the root st:
// it opens the root's next round. The root's card keeps its count, its
// transcript takes the prompt, and the carrier the prompt names opens its
// card with the prompt as its one row, a first stamp the flush writes
// only while the row is still that root's childless, unstamped carrier
// (flushSubagentCarrierInsertSQL). A prompt stored before a row the root
// already took, one naming the root itself, one naming a carrier the
// thread already holds, and one under a stamp that is not live recompute
// at the next flush.
func (c *SubagentCard) notePrompt(st *cardStamp, r subagentRow) {
	// A round can change which agent keeps the root's cards live.
	c.t.unresolve()
	v := &st.values
	if st.state != cardLive || r.carrier == st.id || c.t.stamps[r.carrier] != nil ||
		!r.newerThan(v.NewestTurn, v.NewestItem) || !r.newerThan(v.TranscriptNewestTurn, v.TranscriptNewestItem) {
		st.state, st.reached = cardRecompute, false
		if r.carrier != "" {
			c.t.seeds[r.carrier] = struct{}{}
		}
		return
	}
	base := v.Count.Int64
	if v.TranscriptCount.Valid {
		base = v.TranscriptCount.Int64
	}
	v.Count = validInt(int(v.Count.Int64))
	v.TranscriptCount = validInt(int(base) + 1)
	v.TranscriptNewestTurn, v.TranscriptNewestItem = validInt(r.turn), validInt(r.index)
	st.rounds, st.lo, st.round = true, r.position(), nil
	if r.carrier == "" {
		return
	}
	opened := subagentStampValues{State: aggStateClean}
	round := &cardStamp{id: r.carrier, state: cardLive, carrier: true, openedBy: st.id, stored: opened}
	round.values = opened
	round.values.Count = validInt(1)
	round.values.NewestTurn, round.values.NewestItem = validInt(r.turn), validInt(r.index)
	c.t.stamps[r.carrier] = round
	st.round = round
}

// noteChange feeds a committed summary change of a counted preview-kind
// row to the card's accumulators.
func (c *SubagentCard) noteChange(r subagentRow) {
	if c.orphan {
		return
	}
	for _, st := range c.levels {
		target := st
		if st.rounds {
			if st.state != cardLive {
				continue
			}
			target = st.roundFor(r.position())
		}
		target.changedRow(r)
	}
	c.tray.trayRow(r, true)
}

// Generations.

// subagentGenSeq issues stamp generations. It starts at a random point so
// that a generation read before a restart is not issued again after it.
var subagentGenSeq = func() *atomic.Int64 {
	var seq atomic.Int64
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err == nil {
		seq.Store(int64(binary.LittleEndian.Uint64(seed[:]) >> 2))
	}
	return &seq
}()

// newSubagentGen is a generation no stamp row has held.
func newSubagentGen() int64 { return subagentGenSeq.Add(1) }

// Served keys.

// storedServedKeysSQL finds rows a read serves the subagent stamps to,
// anchors and borrowing completions, whose stored meta holds a served key
// (subagentServedKeys), in either arm. A Codex spawn's completion stores
// its card as a snapshot and is not one of them.
var storedServedKeysSQL = func() string {
	holds := func(a string) string {
		terms := make([]string, 0, len(subagentServedKeys))
		for _, served := range subagentServedKeys {
			terms = append(terms, "json_type("+a+"meta, '$."+served.key+"') IS NOT NULL")
		}
		return "(" + aggAnchorableSQL(a) + " OR " + aggBorrowsCardSQL(a) + ") AND json_valid(" + a + "meta) AND (" +
			strings.Join(terms, " OR ") + ")"
	}
	return `SELECT items.thread_id || '/' || items.id FROM items WHERE ` + holds("items.") + `
UNION ALL
SELECT refs.thread_id || '/' || imported.id FROM import_history_items imported
  JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
 WHERE ` + holds("imported.") + `
LIMIT ?`
}()

// GetThreadItemForWrite is GetThreadItem for a caller that writes the
// row back: its meta is the stored meta, without the keys a read serves
// from the subagent stamps.
func (s *Store) GetThreadItemForWrite(threadID, id string) (Item, bool, error) {
	type result struct {
		item  Item
		found bool
	}
	got, err := readSnapshot(s.reader(), "item for write", func(q sqlQueryer) (result, error) {
		item, found, err := s.getThreadItem(q, threadID, id)
		if err != nil || !found || item.Rev < 0 {
			// An imported row serves its stored meta.
			return result{item, found}, err
		}
		if err := q.QueryRow(`SELECT meta FROM items WHERE thread_id = ? AND id = ?`, threadID, id).Scan(&item.Meta); err != nil {
			return result{}, fmt.Errorf("store: read stored meta of %s/%s: %w", threadID, id, err)
		}
		return result{item, true}, nil
	})
	return got.item, got.found, err
}

// RowsStoringServedSubagentKeys lists up to limit rows, as thread/id,
// whose stored meta holds a key a read serves from the subagent stamps.
// There must be none: a writer that stores a meta it read back would
// freeze a card in the row. storetest asserts it after every test.
func (s *Store) RowsStoringServedSubagentKeys(limit int) ([]string, error) {
	rows, err := s.reader().Query(storedServedKeysSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("store: find stored served subagent keys: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan stored served subagent key row: %w", err), rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate stored served subagent key rows: %w", err)
	}
	return ids, nil
}
