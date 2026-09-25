package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestSubagentCardSequencesMatchTheRecompute is the differential test of
// the card rules against the recompute. Each sequence, from a fixed seed,
// writes 40 random operations into one thread as a live session writes
// them: each row with its parent's card, the cards kept per parent between
// writes. The operations: launches, nested and adopting ones, and their
// children of every kind; carriers of roots and of carriers, stored before
// and after the prompts that name them; resume and wake prompts under a
// root or a carrier a round was resumed from, after and before rows it
// took, naming its carrier, another root's, one another prompt names, the
// root itself, a row that is no carrier, or a carrier not stored yet;
// completion siblings; summary changes of preview rows, tray rows
// and other rows; a thread's own copy of a tool call or an anchor it
// reads from its imported history or an ancestor, which changes no card
// it serves; status changes and interrupts; moves; position changes;
// kind, tool and visibility changes; meta changes that settle a
// background launch, make or re-root a carrier or change a prompt;
// streaming appends; deletes of prompts, carriers and anchors; the bulk
// writers (revert cut, force-close, background teardown, history block,
// fold, Codex runtime retirement); pointer forks of the
// thread or of a fork, cut at a turn, before a row or not at all, which
// settle the rows they copy, and any operation above but a position change
// written in a fork after its cut, which copies the inherited rows it
// changes; the first card on a fork's copy of an anchor whose children it
// inherits, from a database written before the stamps; and any of them
// rolled back by an injected failure, which must leave the rows and the
// stamps as they were. A sequence may start from an imported chunk whose
// anchors its writes make local.
//
// After a random subset of operations a random boundary follows: the
// refresh timer, a card's flush, an agent's settle, the session's end, a
// store reopen, from a database written before the stamps in some, with
// a batch of v121's backfill after it in some, or a crash that loses
// every accumulator before the boot pass. Every boundary must end
// settled, in the thread and in each fork: each row is served what the
// read-time aggregator computes, each stamp is what a recompute derives,
// and nothing waits for a flush. At the end the backfill stamps what is
// left, and every stamp row must equal what restampSubagentAggregatesTx
// rebuilds on a copy of the database.
//
// The shapes stay within what providers write: no row is its own
// ancestor through parents and transcript roots together. A failing
// sequence logs its seed and its operations.
func TestSubagentCardSequencesMatchTheRecompute(t *testing.T) {
	sequences := 200
	if testing.Short() {
		sequences = 20
	}
	const ops = 40
	for i := range sequences {
		seed := int64(7_001 + i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			newCardSequence(t, seed).run(ops)
		})
	}
}

// cardSequence is one sequence's thread, store and session.
type cardSequence struct {
	t    *testing.T
	seed int64
	rng  *rand.Rand
	path string
	s    *Store
	// thread is the thread the operations write; forkWrite points it at a
	// fork for one operation. main is the sequence's thread, forks its
	// pointer forks.
	thread string
	main   string
	forks  []string
	codex  bool
	// cards is the session: one open card per thread and parent, keyed
	// thread/parent, as triage keeps them
	// (internal/triage/subagent_cards.go).
	cards  map[string]*SubagentCard
	kinds  []cardSequenceOpKind
	script []string
	serial int
	// turn is the open turn, turnID its row, turnSeq numbers turn rows.
	turn    int
	turnID  string
	turnSeq int
	// boundaryNext runs a boundary after the next operation: one follows
	// a rolled-back write, so what it left is checked before other writes
	// can recompute it.
	boundaryNext bool
}

type cardSequenceOp struct {
	desc string
	run  func() error
}

type cardSequenceOpKind struct {
	name   string
	weight int
	// wrap reports an operation that is one transaction, which an
	// injected failure rolls back whole.
	wrap  bool
	build func(*cardSequence, []seqRow) (cardSequenceOp, bool)
}

func cardSequenceOps() []cardSequenceOpKind {
	return []cardSequenceOpKind{
		{"launch", 8, true, (*cardSequence).opLaunch},
		{"child", 16, true, (*cardSequence).opChild},
		{"carrier", 5, true, (*cardSequence).opCarrier},
		{"prompt", 6, true, (*cardSequence).opPrompt},
		{"completion", 3, true, (*cardSequence).opCompletion},
		{"summary", 10, true, (*cardSequence).opSummary},
		{"localize", 4, true, (*cardSequence).opLocalize},
		{"status", 7, true, (*cardSequence).opStatus},
		{"interrupt", 2, true, (*cardSequence).opInterrupt},
		{"move", 4, true, (*cardSequence).opMove},
		{"position", 3, true, (*cardSequence).opPosition},
		{"kind", 3, true, (*cardSequence).opKind},
		{"meta", 4, true, (*cardSequence).opMeta},
		{"append", 2, true, (*cardSequence).opAppend},
		{"delete", 5, true, (*cardSequence).opDelete},
		{"turn", 3, false, (*cardSequence).opTurn},
		{"cut", 1, false, (*cardSequence).opCut},
		{"force-close", 1, true, (*cardSequence).opForceClose},
		{"teardown", 1, true, (*cardSequence).opTeardown},
		{"fork", 1, true, (*cardSequence).opFork},
		{"fork write", 4, true, (*cardSequence).opForkWrite},
		{"fork copy child", 1, false, (*cardSequence).opForkCopyChild},
		{"history", 2, true, (*cardSequence).opHistory},
		{"fold", 1, true, (*cardSequence).opFold},
		{"codex retire", 1, true, (*cardSequence).opCodexRetire},
		{"fail", 4, false, (*cardSequence).opFail},
	}
}

func newCardSequence(t *testing.T, seed int64) *cardSequence {
	q := &cardSequence{t: t, seed: seed, rng: rand.New(rand.NewSource(seed)), path: newTestStorePath(t),
		thread: "t-seq", main: "t-seq", cards: make(map[string]*SubagentCard), kinds: cardSequenceOps()}
	var err error
	if q.s, err = New(q.path); err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() {
		if q.s == nil {
			return
		}
		if err := q.s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return q
}

func (q *cardSequence) run(ops int) {
	defer func() {
		if q.t.Failed() {
			q.t.Logf("seed %d; replay with -run 'TestSubagentCardSequencesMatchTheRecompute/seed=%d$'; operations:\n%s",
				q.seed, q.seed, strings.Join(q.script, "\n"))
		}
	}()
	q.start()
	for range ops {
		op := q.pick(q.rows(), false)
		q.exec(op)
		if q.boundaryNext || q.rng.Intn(100) < 35 {
			q.boundaryNext = false
			q.boundary()
		}
	}
	q.log("-- end: the session ends")
	q.closeAll()
	q.settled("end")
	if q.listed() {
		q.log("-- end: the backfill stamps what is left")
		q.backfill(0)
		q.settled("the backfill")
	}
	for _, thread := range q.threads() {
		q.assertRebuild(thread)
	}
	if rows, err := q.s.RowsStoringServedSubagentKeys(10); err != nil || len(rows) > 0 {
		q.t.Fatalf("rows storing served keys: %v %v", rows, err)
	}
}

func (q *cardSequence) log(line string) {
	q.script = append(q.script, fmt.Sprintf("%3d %s", len(q.script), line))
}

func (q *cardSequence) exec(op cardSequenceOp) {
	q.t.Helper()
	q.log(op.desc)
	if err := op.run(); err != nil {
		q.t.Fatalf("%s: %v", op.desc, err)
	}
}

func (q *cardSequence) id(prefix string) string {
	q.serial++
	return fmt.Sprintf("%s%d", prefix, q.serial)
}

func (q *cardSequence) now() int64 { return int64(100_000 + len(q.script)) }

func (q *cardSequence) chance(percent int) bool { return q.rng.Intn(100) < percent }

// pick builds a random operation, one that runs in one transaction when
// wrapped.
func (q *cardSequence) pick(rows []seqRow, wrapped bool) cardSequenceOp {
	usable := slices.DeleteFunc(slices.Clone(q.kinds), func(k cardSequenceOpKind) bool {
		return (wrapped && !k.wrap) || (q.inFork() && strings.HasPrefix(k.name, "fork"))
	})
	total := 0
	for _, k := range usable {
		total += k.weight
	}
	for range 100 {
		x := q.rng.Intn(total)
		for _, k := range usable {
			if x >= k.weight {
				x -= k.weight
				continue
			}
			if op, ok := k.build(q, rows); ok {
				return op
			}
			break
		}
	}
	op, _ := q.opLaunch(rows)
	return op
}

// start creates the thread, from an imported chunk in some sequences,
// and opens its first local turn.
func (q *cardSequence) start() {
	q.codex = q.chance(25)
	switch {
	case q.codex:
		q.log("start: a Codex thread")
		if err := q.s.CreateThread(makeThread(q.thread, "codex")); err != nil {
			q.t.Fatal(err)
		}
	case q.chance(50):
		newImportTargetThread(q.t, q.s, q.thread)
		q.importChunk()
	default:
		q.log("start: a Claude thread")
		if err := q.s.CreateThread(makeThread(q.thread, "claude")); err != nil {
			q.t.Fatal(err)
		}
	}
	q.openTurn(1)
}

// importChunk imports turn 0: a completed launch with children, and in
// some sequences a nested launch and a resumed round.
func (q *cardSequence) importChunk() {
	row := func(id, kind, tool, summary, parent, meta string, index int) Item {
		role := "assistant"
		if kind == "user_text" {
			role = "user"
		}
		if meta == "" {
			meta = "{}"
		}
		return Item{ID: id, ThreadID: q.thread, TurnIndex: 0, ItemIndex: index, Kind: kind, Role: role, ToolName: tool,
			Status: "completed", Summary: summary, ParentID: parent, Meta: meta, CreatedAt: 1_000, UpdatedAt: 1_000}
	}
	items := []Item{
		row("imp-u", "user_text", "", "imported ask", "", "", 10),
		row("imp-L", "tool_call", "Agent", "Agent: imported", "", "", 20),
		row("imp-L-1", "assistant_text", "", "imported text", "imp-L", "", 30),
		row("imp-L-2", "tool_call", "Bash", "Bash: imported", "imp-L", "", 40),
	}
	if q.chance(50) {
		items = append(items, row("imp-plan", "notification", "plan_update", "plan", "imp-L", "", 50))
	}
	if q.chance(50) {
		items = append(items,
			row("imp-N", "tool_call", "Agent", "Agent: imported nested", "imp-L", "", 60),
			row("imp-N-1", "tool_call", "Grep", "Grep: imported", "imp-N", "", 70))
	}
	if q.chance(50) {
		items = append(items,
			row("imp-C", "tool_call", "SendMessage", "Agent: continue", "", carrierMeta("imp-L"), 80),
			row("imp-P", "user_text", "", "imported resume", "imp-L", resumePromptMeta("imp-C"), 90),
			row("imp-L-3", "assistant_text", "", "imported round", "imp-L", "", 100))
	}
	ids := make([]string, len(items))
	batch := ImportBatch{Turns: []Turn{{TurnID: q.thread + ":imported", ThreadID: q.thread, TurnIndex: 0, StartedAt: 1_000}}}
	for i, item := range items {
		ids[i] = item.ID
		batch.Rows = append(batch.Rows, ImportRow{Item: item})
	}
	q.log("start: a Claude thread importing " + strings.Join(ids, ", "))
	if err := q.s.ApplyImportBatch(q.thread, batch); err != nil {
		q.t.Fatalf("import: %v", err)
	}
}

func (q *cardSequence) openTurn(turn int) {
	q.t.Helper()
	q.turnSeq++
	q.turn, q.turnID = turn, fmt.Sprintf("%s:%d:%d", q.thread, turn, q.turnSeq)
	if err := q.s.InsertTurn(Turn{TurnID: q.turnID, ThreadID: q.thread, TurnIndex: turn, StartedAt: q.now()}); err != nil {
		q.t.Fatalf("open turn %d: %v", turn, err)
	}
}

// Rows.

type seqRow struct {
	Item
	r subagentRow
}

// rows reads the thread's timeline, both arms, as the operations choose
// from it.
func (q *cardSequence) rows() []seqRow {
	q.t.Helper()
	items, err := q.s.ListItems(q.thread)
	if err != nil {
		q.t.Fatalf("list rows: %v", err)
	}
	out := make([]seqRow, 0, len(items))
	for _, item := range items {
		out = append(out, seqRow{Item: item, r: subagentRowOf(item)})
	}
	slices.SortFunc(out, func(a, b seqRow) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func where(rows []seqRow, keep func(seqRow) bool) []seqRow {
	var out []seqRow
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func choose[T any](rng *rand.Rand, xs []T) (T, bool) {
	var zero T
	if len(xs) == 0 {
		return zero, false
	}
	return xs[rng.Intn(len(xs))], true
}

func isAnchor(r seqRow) bool  { return r.r.anchorable() }
func isCarrier(r seqRow) bool { return r.r.anchorable() && r.r.root != "" }
func isRoot(r seqRow) bool    { return r.r.anchorable() && r.r.root == "" && r.CompletionOf == "" }
func isLocal(r seqRow) bool   { return r.Rev >= 0 }
func hasParent(r seqRow) bool { return r.ParentID != "" }

func childOf(parent string) func(seqRow) bool {
	return func(r seqRow) bool { return r.ParentID == parent }
}

// orphanParents are the parents rows name that are not stored.
func orphanParents(rows []seqRow) []string {
	stored := make(map[string]bool, len(rows))
	for _, r := range rows {
		stored[r.ID] = true
	}
	var out []string
	for _, r := range rows {
		if r.ParentID != "" && !stored[r.ParentID] && !slices.Contains(out, r.ParentID) {
			out = append(out, r.ParentID)
		}
	}
	slices.Sort(out)
	return out
}

// orphanParent is a parent not stored: one rows already name, or a new one.
func (q *cardSequence) orphanParent(rows []seqRow) string {
	if id, ok := choose(q.rng, orphanParents(rows)); ok && q.chance(50) {
		return id
	}
	return q.id("o")
}

// reaches reports whether from depends on target through parents and
// transcript roots: a row that names target as its parent or root would
// close a cycle.
func reaches(rows []seqRow, from, target string) bool {
	up := make(map[string][]string, len(rows))
	for _, r := range rows {
		up[r.ID] = []string{r.ParentID, transcriptRootFromMeta(r.Meta)}
	}
	seen := map[string]bool{}
	for next := []string{from}; len(next) > 0; {
		id := next[len(next)-1]
		next = next[:len(next)-1]
		if id == "" || seen[id] {
			continue
		}
		if id == target {
			return true
		}
		seen[id] = true
		next = append(next, up[id]...)
	}
	return false
}

// appendAt is a free slot ten past the last row of turn.
func appendAt(rows []seqRow, turn int) (int, int) {
	last := 0
	for _, r := range rows {
		if r.TurnIndex == turn && r.ItemIndex > last {
			last = r.ItemIndex
		}
	}
	return turn, (last/10 + 1) * 10
}

// before is a free slot just before r in its turn.
func before(rows []seqRow, r seqRow) (int, int, bool) {
	prev := -1
	for _, o := range rows {
		if o.TurnIndex == r.TurnIndex && o.ItemIndex < r.ItemIndex && o.ItemIndex > prev {
			prev = o.ItemIndex
		}
	}
	if r.ItemIndex-prev < 2 {
		return 0, 0, false
	}
	return r.TurnIndex, prev + (r.ItemIndex-prev)/2, true
}

// place is where a new row under parent goes: mostly the end of the open
// turn, sometimes just before a row, one of parent's when it has any. A
// fork's rows go after its cut, at the end of its open turn (writeTurn).
func (q *cardSequence) place(rows []seqRow, parent string) (turn, index int, explicit bool) {
	if q.chance(20) && !q.inFork() {
		candidates := where(rows, childOf(parent))
		if parent == "" || len(candidates) == 0 {
			candidates = rows
		}
		if r, ok := choose(q.rng, candidates); ok {
			if turn, index, ok := before(rows, r); ok {
				return turn, index, true
			}
		}
	}
	turn, index = appendAt(rows, q.writeTurn())
	return turn, index, false
}

// writeTurn is the turn a new row goes to: the open turn, or in a fork
// whose cut the open turn precedes, after a revert of the thread it was
// cut from, the turn after the cut. A fork of a thread with no rows has
// no lineage.
func (q *cardSequence) writeTurn() int {
	q.t.Helper()
	if !q.inFork() {
		return q.turn
	}
	var cut int
	err := q.s.reader().QueryRow(`SELECT cut_turn_index FROM thread_fork_lineage WHERE thread_id = ? AND depth = 1`,
		q.thread).Scan(&cut)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return q.turn
	case err != nil:
		q.t.Fatalf("read the cut of %s: %v", q.thread, err)
	}
	if q.turn < cut {
		return cut + 1
	}
	return q.turn
}

func describe(item Item) string {
	out := fmt.Sprintf("%s %s", item.ID, item.Kind)
	if item.ToolName != "" {
		out += "/" + item.ToolName
	}
	out += fmt.Sprintf(" %s %q parent=%q at %d.%d", item.Status, item.Summary, item.ParentID, item.TurnIndex, item.ItemIndex)
	if item.IsBackground {
		out += " background"
	}
	if item.CompletionOf != "" {
		out += " completes " + item.CompletionOf
	}
	if item.Meta != "" && item.Meta != "{}" {
		out += " meta=" + item.Meta
	}
	return out
}

// Cards.

// card is the session's open card for rows under parent, opened on first
// use, or nil for a top-level row.
func (q *cardSequence) card(parent string) *SubagentCard {
	q.t.Helper()
	if parent == "" {
		return nil
	}
	key := q.thread + "/" + parent
	if card := q.cards[key]; card != nil {
		return card
	}
	card, err := q.s.OpenSubagentCard(q.thread, parent)
	if err != nil {
		q.t.Fatalf("open the card of %s: %v", key, err)
	}
	q.cards[key] = card
	return card
}

// openCards are the keys of the open cards, thread/parent.
func (q *cardSequence) openCards() []string {
	return slices.Sorted(maps.Keys(q.cards))
}

func (q *cardSequence) closeAll() {
	q.t.Helper()
	for _, key := range q.openCards() {
		if err := q.cards[key].Close(); err != nil {
			q.t.Fatalf("close the card of %s: %v", key, err)
		}
	}
	clear(q.cards)
}

// threads are the sequence's thread and its forks.
func (q *cardSequence) threads() []string {
	return append([]string{q.main}, q.forks...)
}

// Operations.

// insert writes a new row with its parent's card through one of the
// insert paths; a row placed at an explicit slot takes InsertItem.
func (q *cardSequence) insert(what string, item Item, explicit bool) cardSequenceOp {
	item.ThreadID = q.thread
	item.CreatedAt, item.UpdatedAt = q.now(), q.now()
	if item.Role == "" {
		item.Role = "assistant"
	}
	if item.Meta == "" {
		item.Meta = "{}"
	}
	item.SubagentCard = q.card(item.ParentID)
	api := "InsertItem"
	if !explicit {
		switch q.rng.Intn(5) {
		case 0:
			api = "UpsertItem"
		case 1:
			api = "AppendItem"
		}
	}
	return cardSequenceOp{fmt.Sprintf("%s %s %s", api, what, describe(item)), func() error {
		switch api {
		case "UpsertItem":
			_, err := q.s.UpsertItem(item, nil)
			return err
		case "AppendItem":
			_, err := q.s.AppendItem(item)
			return err
		}
		return q.s.InsertItem(item)
	}}
}

// rewrite rewrites a stored row whole through UpsertItem, with its
// parent's card, as triage's persist does.
func (q *cardSequence) rewrite(id, what string, change func(*Item)) (cardSequenceOp, bool) {
	q.t.Helper()
	item, found, err := q.s.GetThreadItemForWrite(q.thread, id)
	if err != nil || !found {
		q.t.Fatalf("read %s: found=%v err=%v", id, found, err)
	}
	change(&item)
	item.UpdatedAt = q.now()
	item.SubagentCard = q.card(item.ParentID)
	return cardSequenceOp{fmt.Sprintf("UpsertItem %s: %s", what, describe(item)), func() error {
		_, err := q.s.UpsertItem(item, nil)
		return err
	}}, true
}

func (q *cardSequence) opLaunch(rows []seqRow) (cardSequenceOp, bool) {
	id, parent, what := q.id("L"), "", "launch"
	switch x := q.rng.Intn(20); {
	case x < 9:
	case x < 17:
		if a, ok := choose(q.rng, where(rows, isAnchor)); ok {
			parent, what = a.ID, "nested launch"
		}
	case x < 19:
		if o, ok := choose(q.rng, orphanParents(rows)); ok {
			id, what = o, "launch adopting its children"
		}
	default:
		parent, what = q.orphanParent(rows), "launch under a parent not stored"
	}
	item := Item{ID: id, Kind: "tool_call", ToolName: "Agent", Status: "running", Summary: "Agent: " + id,
		ParentID: parent, IsBackground: q.chance(30)}
	if q.chance(20) {
		item.Status = "completed"
	}
	var explicit bool
	item.TurnIndex, item.ItemIndex, explicit = q.place(rows, parent)
	return q.insert(what, item, explicit), true
}

func (q *cardSequence) opChild(rows []seqRow) (cardSequenceOp, bool) {
	var parent string
	switch x := q.rng.Intn(20); {
	case x < 15:
		anchors := where(rows, isAnchor)
		if running := where(anchors, func(r seqRow) bool { return r.r.running() }); len(running) > 0 && q.chance(65) {
			anchors = running
		}
		a, ok := choose(q.rng, anchors)
		if !ok {
			return cardSequenceOp{}, false
		}
		parent = a.ID
	case x < 18:
		r, ok := choose(q.rng, where(rows, func(r seqRow) bool { return !r.r.anchorable() }))
		if !ok {
			return cardSequenceOp{}, false
		}
		parent = r.ID
	default:
		parent = q.orphanParent(rows)
	}
	id := q.id("c")
	item := Item{ID: id, ParentID: parent, Status: "completed"}
	switch q.rng.Intn(10) {
	case 0, 1, 2:
		item.Kind, item.ToolName, item.Summary = "tool_call", "Bash", "Bash: "+id
		item.Status = []string{"running", "completed", "streaming"}[q.rng.Intn(3)]
		if q.chance(15) {
			item.Summary = " \t"
		}
	case 3, 4:
		item.Kind, item.Summary = "assistant_text", "text "+id
		item.Status = []string{"streaming", "completed"}[q.rng.Intn(2)]
	case 5:
		item.Kind, item.Summary = "error", "error "+id
	case 6:
		item.Kind, item.ToolName, item.Summary = "notification", "plan_update", "plan "+id
	case 7:
		item.Kind, item.ToolName, item.Summary = "tool_call", "collab_agent", "spawn "+id
	case 8:
		item.Kind, item.Role, item.Summary = "user_text", "user", "user "+id
	default:
		item.Kind, item.Summary = "terminal_interaction", "terminal "+id
	}
	var explicit bool
	item.TurnIndex, item.ItemIndex, explicit = q.place(rows, parent)
	return q.insert("child", item, explicit), true
}

func (q *cardSequence) opCarrier(rows []seqRow) (cardSequenceOp, bool) {
	roots := where(rows, isRoot)
	if carriers := where(rows, isCarrier); len(carriers) > 0 && q.chance(20) {
		roots = carriers
	}
	root, ok := choose(q.rng, roots)
	if !ok {
		return cardSequenceOp{}, false
	}
	id, what := "", "carrier before its prompt"
	if q.chance(60) {
		stored := make(map[string]bool, len(rows))
		for _, r := range rows {
			stored[r.ID] = true
		}
		var named []string
		for _, r := range rows {
			if r.r.prompt && r.ParentID == root.ID && r.r.carrier != "" && !stored[r.r.carrier] && !slices.Contains(named, r.r.carrier) {
				named = append(named, r.r.carrier)
			}
		}
		if c, ok := choose(q.rng, named); ok {
			id, what = c, "carrier after its prompt"
		}
	}
	if id == "" {
		id = q.id("C")
	}
	parent := ""
	if a, ok := choose(q.rng, where(rows, isAnchor)); ok && q.chance(20) && !reaches(rows, a.ID, id) {
		parent = a.ID
	}
	item := Item{ID: id, Kind: "tool_call", ToolName: "SendMessage", Status: "running", Summary: "Agent: continue " + id,
		ParentID: parent, Meta: carrierMeta(root.ID)}
	if q.chance(30) {
		item.Status = "completed"
	}
	var explicit bool
	item.TurnIndex, item.ItemIndex, explicit = q.place(rows, parent)
	return q.insert(what+" of "+root.ID, item, explicit), true
}

func (q *cardSequence) opPrompt(rows []seqRow) (cardSequenceOp, bool) {
	roots := where(rows, isRoot)
	carriers := where(rows, isCarrier)
	if resumed := where(where(rows, isAnchor), func(r seqRow) bool {
		return slices.ContainsFunc(carriers, func(c seqRow) bool { return c.r.root == r.ID })
	}); len(resumed) > 0 && q.chance(65) {
		roots = resumed
	}
	root, ok := choose(q.rng, roots)
	if !ok {
		return cardSequenceOp{}, false
	}
	named := make(map[string]bool)
	for _, r := range rows {
		if r.r.prompt && r.r.carrier != "" {
			named[r.r.carrier] = true
		}
	}
	carrier, how := "", ""
	switch x := q.rng.Intn(20); {
	case x < 9:
		if c, ok := choose(q.rng, where(carriers, func(c seqRow) bool { return c.r.root == root.ID && !named[c.ID] })); ok {
			carrier, how = c.ID, "its stored carrier"
		}
	case x < 11:
		how = "no carrier: a wake"
	case x < 13:
		if c, ok := choose(q.rng, where(carriers, func(c seqRow) bool { return c.r.root != root.ID })); ok {
			carrier, how = c.ID, "another root's carrier"
		}
	case x < 14:
		if c, ok := choose(q.rng, where(carriers, func(c seqRow) bool { return named[c.ID] })); ok {
			carrier, how = c.ID, "a carrier another prompt names"
		}
	case x < 15:
		carrier, how = root.ID, "the root itself"
	case x < 16:
		if r, ok := choose(q.rng, where(rows, func(r seqRow) bool { return !isCarrier(r) && r.ID != root.ID })); ok {
			carrier, how = r.ID, "a row that is no carrier"
		}
	}
	if how == "" {
		carrier, how = q.id("C"), "a carrier not stored yet"
	}
	item := Item{ID: q.id("P"), Kind: "user_text", Role: "user", Summary: "resume", ParentID: root.ID, Meta: resumePromptMeta(carrier)}
	item.TurnIndex, item.ItemIndex = appendAt(rows, q.writeTurn())
	explicit, at := false, "after"
	if kid, ok := choose(q.rng, where(rows, childOf(root.ID))); ok && q.chance(35) && !q.inFork() {
		if turn, index, ok := before(rows, kid); ok {
			item.TurnIndex, item.ItemIndex, explicit, at = turn, index, true, "before "+kid.ID+" in"
		}
	}
	return q.insert(fmt.Sprintf("prompt %s the rows of %s naming %s", at, root.ID, how), item, explicit), true
}

func (q *cardSequence) opCompletion(rows []seqRow) (cardSequenceOp, bool) {
	done := make(map[string]bool)
	for _, r := range rows {
		if r.CompletionOf != "" {
			done[r.CompletionOf] = true
		}
	}
	launch, ok := choose(q.rng, where(rows, func(r seqRow) bool {
		return r.r.anchorable() && r.IsBackground && r.CompletionOf == "" && !done[r.ID]
	}))
	if !ok {
		return cardSequenceOp{}, false
	}
	item := Item{ID: "done-" + launch.ID, Kind: "tool_completion", ToolName: launch.ToolName, Status: "completed",
		Summary: "done " + launch.ID, ParentID: launch.ParentID, CompletionOf: launch.ID, IsBackground: true}
	if q.chance(30) {
		item.ThreadID, item.TurnIndex, item.Role, item.Meta = q.thread, q.writeTurn(), "assistant", "{}"
		item.CreatedAt, item.UpdatedAt = q.now(), q.now()
		item.SubagentCard = q.card(item.ParentID)
		return cardSequenceOp{"AppendCompletionItem " + describe(item), func() error {
			_, err := q.s.AppendCompletionItem(Item{ID: launch.ID, ThreadID: q.thread}, item, nil)
			return err
		}}, true
	}
	item.TurnIndex, item.ItemIndex = appendAt(rows, q.writeTurn())
	return q.insert("completion sibling", item, false), true
}

// stampedRow is a row a stored stamp names in column: a preview or a tray
// row.
func (q *cardSequence) stampedRow(rows []seqRow, column string) (seqRow, bool) {
	q.t.Helper()
	ids, err := subagentAnchorIDs(q.s.reader(), `SELECT DISTINCT `+column+` FROM subagent_aggregates
	 WHERE thread_id = ? AND `+column+` IS NOT NULL ORDER BY 1`, q.thread)
	if err != nil {
		q.t.Fatalf("read stamped %s: %v", column, err)
	}
	return choose(q.rng, where(rows, func(r seqRow) bool { return slices.Contains(ids, r.ID) }))
}

func (q *cardSequence) opSummary(rows []seqRow) (cardSequenceOp, bool) {
	var target seqRow
	ok, how := false, "a row"
	switch x := q.rng.Intn(10); {
	case x < 4:
		target, ok = q.stampedRow(rows, "pick_id")
		how = "a preview row"
	case x < 7:
		target, ok = q.stampedRow(rows, "tool_id")
		how = "a tray row"
	}
	if !ok {
		if target, ok = choose(q.rng, where(rows, hasParent)); !ok {
			return cardSequenceOp{}, false
		}
		how = "a row"
	}
	summary := "summary " + q.id("s")
	if q.chance(25) {
		summary = "  "
	}
	card := q.card(target.ParentID)
	return cardSequenceOp{fmt.Sprintf("UpdateItemFields summary of %s (%s) to %q", target.ID, how, summary), func() error {
		_, err := q.s.UpdateItemFields(q.thread, target.ID, ItemPartialUpdate{Summary: &summary, SubagentCard: card})
		return err
	}}, true
}

// opLocalize gives the thread its own copy of a row it reads from its
// imported history or an ancestor, the row unchanged: a tool call under
// its parent's card, or an anchor, whose children stay where they are. A
// fork's copy of an anchor whose children it inherits is the shape a
// card opened on it after v121 seeds through the lineage's child probe
// (seedCardStamp). The cards flush when the operation is built, so each
// serves its settled values, and the copy must change none of them: the
// tray, the walk and the recompute read every arm.
func (q *cardSequence) opLocalize(rows []seqRow) (cardSequenceOp, bool) {
	target, ok := choose(q.rng, where(rows, func(r seqRow) bool {
		return !isLocal(r) && (hasParent(r) && r.r.toolable() || r.r.anchorable())
	}))
	if !ok {
		return cardSequenceOp{}, false
	}
	q.refresh("")
	card := q.card(target.ParentID)
	status := target.Status
	return cardSequenceOp{fmt.Sprintf("the cards flush, UpdateItemFields copies %s under %q unchanged", target.ID, target.ParentID), func() error {
		before := q.servedCards()
		if _, err := q.s.UpdateItemFields(q.thread, target.ID, ItemPartialUpdate{Status: &status, SubagentCard: card}); err != nil {
			return err
		}
		if after := q.servedCards(); after != before {
			return fmt.Errorf("the copy of %s changed the cards the thread serves from\n%s\nto\n%s", target.ID, before, after)
		}
		return nil
	}}, true
}

// servedCards is every card the thread serves, as text.
func (q *cardSequence) servedCards() string {
	q.t.Helper()
	return fmt.Sprint(subagentCardsForTest(q.t, q.s, q.s.reader(), q.thread))
}

func (q *cardSequence) opStatus(rows []seqRow) (cardSequenceOp, bool) {
	candidates := rows
	if q.chance(70) {
		candidates = where(rows, isAnchor)
	}
	target, ok := choose(q.rng, candidates)
	if !ok {
		return cardSequenceOp{}, false
	}
	status := []string{"completed", "errored"}[q.rng.Intn(2)]
	if target.Status != "running" && target.Status != "streaming" && q.chance(30) {
		status = "running"
	}
	var card *SubagentCard
	if q.chance(80) {
		card = q.card(target.ParentID)
	}
	return cardSequenceOp{fmt.Sprintf("UpdateItemFields status of %s from %s to %s (card: %v)", target.ID, target.Status, status, card != nil), func() error {
		_, err := q.s.UpdateItemFields(q.thread, target.ID, ItemPartialUpdate{Status: &status, SubagentCard: card})
		return err
	}}, true
}

func (q *cardSequence) opInterrupt(rows []seqRow) (cardSequenceOp, bool) {
	target, ok := choose(q.rng, where(rows, func(r seqRow) bool {
		return isLocal(r) && (r.Status == "running" || r.Status == "streaming")
	}))
	if !ok {
		return cardSequenceOp{}, false
	}
	item, _, err := q.s.GetThreadItemForWrite(q.thread, target.ID)
	if err != nil {
		q.t.Fatal(err)
	}
	card := q.card(target.ParentID)
	return cardSequenceOp{"ErrorActiveItemIfRevision " + target.ID, func() error {
		_, _, err := q.s.ErrorActiveItemIfRevision(q.thread, target.ID, item.Rev, "interrupted", q.now(), card)
		return err
	}}, true
}

func (q *cardSequence) opMove(rows []seqRow) (cardSequenceOp, bool) {
	candidates := rows
	if q.chance(70) {
		candidates = where(rows, hasParent)
	}
	target, ok := choose(q.rng, candidates)
	if !ok {
		return cardSequenceOp{}, false
	}
	parent := ""
	switch x := q.rng.Intn(10); {
	case x < 6:
		a, ok := choose(q.rng, where(rows, func(a seqRow) bool { return isAnchor(a) && !reaches(rows, a.ID, target.ID) }))
		if !ok {
			return cardSequenceOp{}, false
		}
		parent = a.ID
	case x < 8:
	default:
		if parent = q.orphanParent(rows); reaches(rows, parent, target.ID) {
			return cardSequenceOp{}, false
		}
	}
	if parent == target.ParentID {
		return cardSequenceOp{}, false
	}
	return q.rewrite(target.ID, fmt.Sprintf("move %s from %q to %q", target.ID, target.ParentID, parent), func(item *Item) { item.ParentID = parent })
}

func (q *cardSequence) opPosition(rows []seqRow) (cardSequenceOp, bool) {
	if q.inFork() {
		return cardSequenceOp{}, false
	}
	candidates := rows
	if q.chance(70) {
		candidates = where(rows, hasParent)
	}
	target, ok := choose(q.rng, candidates)
	if !ok {
		return cardSequenceOp{}, false
	}
	if q.chance(50) {
		return cardSequenceOp{"BumpItemToTurnEnd " + target.ID, func() error {
			_, err := q.s.BumpItemToTurnEnd(q.thread, target.ID, nil, q.now())
			return err
		}}, true
	}
	var turns []int
	for turn := 0; turn <= q.turn; turn++ {
		if turn != target.TurnIndex && !slices.ContainsFunc(rows, func(r seqRow) bool {
			return r.TurnIndex == turn && r.ItemIndex == target.ItemIndex
		}) {
			turns = append(turns, turn)
		}
	}
	turn, ok := choose(q.rng, turns)
	if !ok {
		return cardSequenceOp{}, false
	}
	return q.rewrite(target.ID, fmt.Sprintf("move %s from turn %d to %d", target.ID, target.TurnIndex, turn), func(item *Item) { item.TurnIndex = turn })
}

func (q *cardSequence) opKind(rows []seqRow) (cardSequenceOp, bool) {
	target, ok := choose(q.rng, where(rows, func(r seqRow) bool { return r.CompletionOf == "" }))
	if !ok {
		return cardSequenceOp{}, false
	}
	shapes := [][2]string{{"tool_call", "Agent"}, {"tool_call", "Bash"}, {"tool_call", "collab_agent"},
		{"assistant_text", ""}, {"notification", "plan_update"}, {"error", ""}}
	shapes = slices.DeleteFunc(shapes, func(s [2]string) bool { return s[0] == target.Kind && s[1] == target.ToolName })
	shape, _ := choose(q.rng, shapes)
	return q.rewrite(target.ID, fmt.Sprintf("%s from %s/%s to %s/%s", target.ID, target.Kind, target.ToolName, shape[0], shape[1]),
		func(item *Item) { item.Kind, item.ToolName = shape[0], shape[1] })
}

func (q *cardSequence) opMeta(rows []seqRow) (cardSequenceOp, bool) {
	target, ok := choose(q.rng, where(rows, func(r seqRow) bool {
		return r.r.anchorable() || (r.Kind == "user_text" && r.ParentID != "")
	}))
	if !ok {
		return cardSequenceOp{}, false
	}
	var meta, how string
	if target.r.anchorable() {
		switch q.rng.Intn(4) {
		case 0:
			meta, how = `{"live_background_active":false}`, "settles the launch"
		case 1:
			meta, how = `{"live_background_active":true}`, "keeps the launch live"
		case 2:
			root, ok := choose(q.rng, where(rows, func(r seqRow) bool {
				return isAnchor(r) && r.ID != target.ID && !reaches(rows, r.ID, target.ID)
			}))
			if !ok {
				return cardSequenceOp{}, false
			}
			meta, how = carrierMeta(root.ID), "resumes "+root.ID
		default:
			meta, how = "{}", "plain"
		}
	} else {
		switch q.rng.Intn(3) {
		case 0:
			carrier := q.id("C")
			if c, ok := choose(q.rng, where(rows, isCarrier)); ok && q.chance(70) {
				carrier = c.ID
			}
			meta, how = resumePromptMeta(carrier), "a prompt naming "+carrier
		case 1:
			meta, how = resumePromptMeta(""), "a wake prompt"
		default:
			meta, how = "{}", "not a prompt"
		}
	}
	desc := fmt.Sprintf("meta of %s: %s", target.ID, how)
	if q.rng.Intn(2) == 0 {
		return cardSequenceOp{"UpdateItemMetaMerge " + desc, func() error {
			_, _, err := q.s.UpdateItemMetaMerge(q.thread, target.ID, func(string) (string, error) { return meta, nil }, q.now())
			return err
		}}, true
	}
	return cardSequenceOp{"UpdateItemMeta " + desc, func() error { return q.s.UpdateItemMeta(q.thread, target.ID, meta) }}, true
}

// opAppend streams text onto a row; the store refuses a row a card
// previews, which needs its card.
func (q *cardSequence) opAppend(rows []seqRow) (cardSequenceOp, bool) {
	target, ok := choose(q.rng, where(rows, func(r seqRow) bool { return isLocal(r) && r.Status == "streaming" }))
	if !ok {
		return cardSequenceOp{}, false
	}
	refused := target.r.counts() && previewKind(target.Kind)
	return cardSequenceOp{fmt.Sprintf("AppendItemSummary %s (refused: %v)", target.ID, refused), func() error {
		_, err := q.s.AppendItemSummary(q.thread, target.ID, " more", q.now())
		switch {
		case refused && errors.Is(err, ErrSubagentAnchor):
			return nil
		case refused && err == nil:
			return errors.New("an append to a previewed row was accepted")
		}
		return err
	}}, true
}

func (q *cardSequence) opDelete(rows []seqRow) (cardSequenceOp, bool) {
	var candidates []seqRow
	switch q.rng.Intn(4) {
	case 0:
		candidates = where(rows, func(r seqRow) bool { return r.r.prompt })
	case 1:
		candidates = where(rows, isCarrier)
	case 2:
		candidates = where(rows, isAnchor)
	}
	if len(candidates) == 0 {
		candidates = rows
	}
	target, ok := choose(q.rng, candidates)
	if !ok {
		return cardSequenceOp{}, false
	}
	return cardSequenceOp{fmt.Sprintf("DeleteThreadItem %s (%s/%s under %q)", target.ID, target.Kind, target.ToolName, target.ParentID), func() error {
		return q.s.DeleteThreadItem(q.thread, target.ID)
	}}, true
}

func (q *cardSequence) opTurn([]seqRow) (cardSequenceOp, bool) {
	return cardSequenceOp{fmt.Sprintf("turn %d completes, turn %d opens", q.turn, q.turn+1), func() error {
		if err := q.s.UpdateTurnCompleted(q.turnID, q.now(), "end_turn", "", "", ""); err != nil {
			return err
		}
		q.openTurn(q.turn + 1)
		return nil
	}}, true
}

func (q *cardSequence) opCut([]seqRow) (cardSequenceOp, bool) {
	if q.turn < 3 {
		return cardSequenceOp{}, false
	}
	from := q.turn - q.rng.Intn(2)
	return cardSequenceOp{fmt.Sprintf("DeleteConversationFromTurn %d", from), func() error {
		if _, _, err := q.s.DeleteConversationFromTurn(q.thread, from); err != nil {
			return err
		}
		q.openTurn(from)
		return nil
	}}, true
}

func (q *cardSequence) opForceClose(rows []seqRow) (cardSequenceOp, bool) {
	turn := q.turn
	if r, ok := choose(q.rng, where(rows, func(r seqRow) bool { return r.r.running() })); ok {
		turn = r.TurnIndex
	}
	return cardSequenceOp{fmt.Sprintf("ForceCloseRunningToolCallsInTurn %d", turn), func() error {
		_, err := q.s.ForceCloseRunningToolCallsInTurn(q.thread, turn, func(string) string { return "stopped" }, q.now())
		return err
	}}, true
}

func (q *cardSequence) opTeardown([]seqRow) (cardSequenceOp, bool) {
	return cardSequenceOp{"MarkLiveBackgroundToolCallsInactive", func() error {
		_, err := q.s.MarkLiveBackgroundToolCallsInactive(q.thread, q.now())
		return err
	}}, true
}

// opFork creates a pointer fork of the thread, or of a fork, cut after a
// turn, before a row or not at all.
func (q *cardSequence) opFork(rows []seqRow) (cardSequenceOp, bool) {
	if len(q.forks) >= 3 {
		return cardSequenceOp{}, false
	}
	source := q.main
	if f, ok := choose(q.rng, q.forks); ok && q.chance(30) {
		source = f
	}
	sourceRows := rows
	if source != q.thread {
		q.in(source, func() { sourceRows = q.rows() })
	}
	var cut ForkCut
	how := "whole"
	switch q.rng.Intn(3) {
	case 0:
		turn := q.rng.Intn(q.turn + 1)
		cut.ThroughTurn, how = &turn, fmt.Sprintf("through turn %d", turn)
	case 1:
		if r, ok := choose(q.rng, sourceRows); ok {
			cut.BeforeItemID, how = r.ID, "before "+r.ID
		}
	}
	fork := q.id(q.main + "-f")
	provider := "claude"
	if q.codex {
		provider = "codex"
	}
	return cardSequenceOp{fmt.Sprintf("CreatePointerFork %s of %s, %s", fork, source, how), func() error {
		if err := q.s.CreatePointerFork(makeThread(fork, provider), source, cut, func(string) string { return "stopped" }, q.now()); err != nil {
			return err
		}
		q.forks = append(q.forks, fork)
		return nil
	}}, true
}

// opForkWrite runs one operation in a fork, over the rows the fork reads,
// with the fork's cards: a write that changes an inherited row copies it
// into the fork first.
func (q *cardSequence) opForkWrite([]seqRow) (cardSequenceOp, bool) {
	fork, ok := choose(q.rng, q.forks)
	if !ok {
		return cardSequenceOp{}, false
	}
	var inner cardSequenceOp
	q.in(fork, func() { inner = q.pick(q.rows(), true) })
	return cardSequenceOp{"in " + fork + ": " + inner.desc, func() error {
		var err error
		q.in(fork, func() { err = inner.run() })
		return err
	}}, true
}

// opForkCopyChild is the first card on a pointer fork's copy of an anchor
// whose children the fork inherits, in a database written before v121:
// the fork copies the anchor unchanged, the store reopens from before the
// stamps, and a child is written under the copy with its card. The copy
// carries no stamp until the backfill reaches it, so the card seeds it
// through the lineage arms of the child probe (seedCardStamp).
func (q *cardSequence) opForkCopyChild([]seqRow) (cardSequenceOp, bool) {
	fork, ok := choose(q.rng, q.forks)
	if !ok {
		return cardSequenceOp{}, false
	}
	var anchor seqRow
	var child Item
	q.in(fork, func() {
		rows := q.rows()
		anchors := where(rows, func(r seqRow) bool {
			return r.r.anchorable() && r.r.root == "" &&
				slices.ContainsFunc(rows, func(c seqRow) bool { return !isLocal(c) && c.ParentID == r.ID })
		})
		if anchor, ok = choose(q.rng, anchors); !ok {
			return
		}
		id := q.id("c")
		child = Item{ID: id, ThreadID: fork, Kind: "tool_call", ToolName: "Bash", Status: "completed", Summary: "Bash: " + id,
			ParentID: anchor.ID, Role: "assistant", Meta: "{}", CreatedAt: q.now(), UpdatedAt: q.now()}
		child.TurnIndex, child.ItemIndex = appendAt(rows, q.writeTurn())
	})
	if !ok {
		return cardSequenceOp{}, false
	}
	desc := fmt.Sprintf("in %s: UpdateItemFields copies %s unchanged, the store reopens from before v121, InsertItem %s with the copy's card",
		fork, anchor.ID, describe(child))
	return cardSequenceOp{desc, func() error {
		var err error
		q.in(fork, func() {
			status := anchor.Status
			if _, err = q.s.UpdateItemFields(fork, anchor.ID, ItemPartialUpdate{Status: &status, SubagentCard: q.card(anchor.ParentID)}); err != nil {
				return
			}
			q.reopen()
			q.predateStamps()
			child.SubagentCard = q.card(anchor.ID)
			err = q.s.InsertItem(child)
		})
		return err
	}}, true
}

// inFork reports operations pointed at a fork.
func (q *cardSequence) inFork() bool { return q.thread != q.main }

// in runs fn with the operations pointed at thread.
func (q *cardSequence) in(thread string, fn func()) {
	was := q.thread
	q.thread = thread
	defer func() { q.thread = was }()
	fn()
}

// opHistory writes a block of settled rows under stored anchors in one
// transaction, with a completion sibling in some blocks.
func (q *cardSequence) opHistory(rows []seqRow) (cardSequenceOp, bool) {
	anchors := where(rows, isAnchor)
	if len(anchors) == 0 {
		return cardSequenceOp{}, false
	}
	turn, index := appendAt(rows, q.writeTurn())
	var batch ThreadHistoryBatch
	var ids []string
	for range 1 + q.rng.Intn(3) {
		parent, _ := choose(q.rng, anchors)
		id := q.id("h")
		item := Item{ID: id, ThreadID: q.thread, TurnIndex: turn, ItemIndex: index, Kind: "tool_call", Role: "assistant",
			ToolName: "Bash", Status: "completed", Summary: "Bash: " + id, ParentID: parent.ID, Meta: "{}", CreatedAt: q.now(), UpdatedAt: q.now()}
		if q.chance(40) {
			item.Kind, item.ToolName, item.Summary = "assistant_text", "", "text "+id
		}
		batch.Rows = append(batch.Rows, HistoryRow{Item: item})
		ids = append(ids, describe(item))
		index += 10
	}
	done := make(map[string]bool)
	for _, r := range rows {
		if r.CompletionOf != "" {
			done[r.CompletionOf] = true
		}
	}
	if launch, ok := choose(q.rng, where(anchors, func(r seqRow) bool { return r.IsBackground && r.CompletionOf == "" && !done[r.ID] })); ok && q.chance(40) {
		item := Item{ID: "done-" + launch.ID, ThreadID: q.thread, TurnIndex: turn, ItemIndex: index, Kind: "tool_completion", Role: "assistant",
			ToolName: launch.ToolName, Status: "completed", Summary: "done " + launch.ID, ParentID: launch.ParentID,
			CompletionOf: launch.ID, IsBackground: true, Meta: "{}", CreatedAt: q.now(), UpdatedAt: q.now()}
		batch.Rows = append(batch.Rows, HistoryRow{Item: item})
		ids = append(ids, describe(item))
	}
	return cardSequenceOp{"InsertThreadHistory " + strings.Join(ids, "; "), func() error {
		return q.s.InsertThreadHistory(q.thread, batch)
	}}, true
}

func (q *cardSequence) opFold(rows []seqRow) (cardSequenceOp, bool) {
	survivor, ok := choose(q.rng, where(rows, func(r seqRow) bool { return r.Kind == "user_text" }))
	if !ok {
		return cardSequenceOp{}, false
	}
	others := where(rows, func(r seqRow) bool { return r.ID != survivor.ID })
	var folded []string
	for range 1 + q.rng.Intn(2) {
		if r, ok := choose(q.rng, others); ok && !slices.Contains(folded, r.ID) {
			folded = append(folded, r.ID)
		}
	}
	if len(folded) == 0 {
		return cardSequenceOp{}, false
	}
	meta := "{}"
	if q.chance(70) {
		item, _, err := q.s.GetThreadItemForWrite(q.thread, survivor.ID)
		if err != nil {
			q.t.Fatal(err)
		}
		meta = item.Meta
	}
	return cardSequenceOp{fmt.Sprintf("FoldUserTextRows into %s folding %v meta=%s", survivor.ID, folded, meta), func() error {
		_, err := q.s.FoldUserTextRows(q.thread, survivor.ID, folded, "folded", meta, q.now())
		return err
	}}, true
}

func (q *cardSequence) opCodexRetire([]seqRow) (cardSequenceOp, bool) {
	if !q.codex {
		return cardSequenceOp{}, false
	}
	return cardSequenceOp{"RetireCodexBackgroundRuntime", func() error {
		_, err := q.s.RetireCodexBackgroundRuntime(q.thread, func(string) string { return "stopped" }, q.now())
		return err
	}}, true
}

// cardSequenceFailures are the injected failures: each set of triggers
// aborts the statements it names.
var cardSequenceFailures = []struct {
	name     string
	triggers []string
}{
	{"stamp writes", []string{"BEFORE INSERT ON subagent_aggregates", "BEFORE UPDATE ON subagent_aggregates"}},
	{"stamp deletes", []string{"BEFORE DELETE ON subagent_aggregates"}},
	{"item inserts", []string{"AFTER INSERT ON items"}},
	{"item updates", []string{"AFTER UPDATE ON items"}},
	{"item deletes", []string{"AFTER DELETE ON items"}},
}

// opFail runs one operation under an injected failure. A write the
// failure aborts must leave the rows and the stamps as they were, and
// what its cards applied must not outlive it, which the boundary that
// follows checks.
func (q *cardSequence) opFail(rows []seqRow) (cardSequenceOp, bool) {
	inner := q.pick(rows, true)
	failure := cardSequenceFailures[q.rng.Intn(len(cardSequenceFailures))]
	return cardSequenceOp{fmt.Sprintf("under failing %s: %s", failure.name, inner.desc), func() error {
		threads := q.threads()
		items := make(map[string][]Item, len(threads))
		stamps := make(map[string]map[string]string, len(threads))
		for _, thread := range threads {
			var err error
			if items[thread], err = q.s.ListItems(thread); err != nil {
				return err
			}
			stamps[thread] = q.stampSnapshot(thread)
		}
		for i, on := range failure.triggers {
			mustExec(q.t, q.s.db, fmt.Sprintf(`CREATE TRIGGER seq_fail_%d %s BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, i, on))
		}
		runErr := inner.run()
		for i := range failure.triggers {
			mustExec(q.t, q.s.db, fmt.Sprintf(`DROP TRIGGER seq_fail_%d`, i))
		}
		if runErr == nil {
			return nil
		}
		if !strings.Contains(runErr.Error(), "injected failure") {
			return runErr
		}
		q.log("    (rolled back)")
		for _, thread := range threads {
			after, err := q.s.ListItems(thread)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(items[thread], after) {
				return fmt.Errorf("the rolled-back write changed the rows of %s", thread)
			}
			if got := q.stampSnapshot(thread); !reflect.DeepEqual(got, stamps[thread]) {
				return fmt.Errorf("the rolled-back write changed the stamps of %s: %v, was %v", thread, got, stamps[thread])
			}
		}
		q.boundaryNext = q.chance(70)
		return nil
	}}, true
}

// stampSnapshot reads every stamp row of thread whole, generation
// included.
func (q *cardSequence) stampSnapshot(thread string) map[string]string {
	q.t.Helper()
	columns := append([]string{"gen", "state"}, subagentAggregateValueColumns...)
	for i, column := range columns {
		columns[i] = "quote(" + column + ")"
	}
	rows, err := q.s.reader().Query(`SELECT item_id, `+strings.Join(columns, " || '|' || ")+`
	  FROM subagent_aggregates WHERE thread_id = ?`, thread)
	if err != nil {
		q.t.Fatalf("read stamps: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, values string
		if err := rows.Scan(&id, &values); err != nil {
			q.t.Fatalf("scan stamp: %v", err)
		}
		out[id] = values
	}
	if err := rows.Err(); err != nil {
		q.t.Fatalf("iterate stamps: %v", err)
	}
	return out
}

// Boundaries.

// boundary runs a random point at which the cards reach their stamps, or
// a crash that loses them before the boot pass, and checks that it ends
// settled.
func (q *cardSequence) boundary() {
	q.t.Helper()
	summarise := func(string) string { return "stopped" }
	var name string
	switch q.rng.Intn(6) {
	case 0:
		name = "refresh timer"
		q.refresh("")
	case 1:
		key, ok := choose(q.rng, q.openCards())
		if !ok {
			name = "refresh timer"
			q.refresh("")
			break
		}
		name = "the card of " + key + " flushes, the other threads' timers fire"
		card := q.cards[key]
		if _, err := card.Flush(); err != nil {
			q.t.Fatalf("flush the card of %s: %v", key, err)
		}
		q.refresh(card.ThreadID())
	case 2:
		key, ok := choose(q.rng, q.openCards())
		name = "an agent settles"
		q.refresh("")
		if ok {
			name = key + " settles"
			card := q.cards[key]
			delete(q.cards, key)
			if err := card.Close(); err != nil {
				q.t.Fatalf("close the card of %s: %v", key, err)
			}
		}
	case 3:
		name = "the session ends"
		q.closeAll()
	case 4:
		name = "the store reopens"
		q.reopen()
		if q.chance(20) {
			name += " from before v121"
			q.predateStamps()
		}
		if q.chance(50) {
			name += " and boots"
			q.boot(summarise)
		}
		if q.chance(50) && q.listed() {
			limit := 1 + q.rng.Intn(4)
			name += fmt.Sprintf(", and the backfill stamps a batch of %d", limit)
			q.backfill(limit)
		}
	default:
		name = "a crash, then the boot pass"
		crashSubagentCardsForTest(q.s)
		clear(q.cards)
		if q.chance(50) {
			if _, err := q.s.RecoverSubagentCards(q.t.Context()); err != nil {
				q.t.Fatalf("boot pass: %v", err)
			}
		} else {
			name += " and the crash sweeps"
			q.boot(summarise)
		}
	}
	q.log("-- boundary: " + name)
	q.settled(name)
}

// predateStamps puts the database where v121 leaves one written before
// the stamps: no stamp, and every thread with a local tool call listed
// in subagent_aggregate_backfill. Reads walk a listed thread's unstamped
// anchors, and a card opened on one with children recomputes it
// (seedCardStamp), whose child probe reads a pointer fork's lineage for
// a copy whose children it inherits. It runs on a reopened store, before
// any card, as the migration does.
func (q *cardSequence) predateStamps() {
	q.t.Helper()
	for _, statement := range []string{
		`DELETE FROM subagent_aggregates`,
		`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id)
		 SELECT id FROM threads
		  WHERE EXISTS (SELECT 1 FROM items WHERE items.thread_id = threads.id AND items.kind = 'tool_call')`,
	} {
		if _, err := q.s.db.Exec(statement); err != nil {
			q.t.Fatalf("put the database before v121: %v", err)
		}
	}
}

// listed reports whether v121's backfill lists a thread.
func (q *cardSequence) listed() bool {
	q.t.Helper()
	var listed bool
	if err := q.s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM subagent_aggregate_backfill)`).Scan(&listed); err != nil {
		q.t.Fatalf("read the backfill list: %v", err)
	}
	return listed
}

// backfill is one batch of v121's deferred phase in each listed thread,
// of at most limit anchors. With limit 0 it runs the phase to its end.
func (q *cardSequence) backfill(limit int) {
	q.t.Helper()
	batch := limit
	if batch == 0 {
		batch = subagentBackfillBatch
	}
	for _, thread := range q.threads() {
		for batches := 0; ; batches++ {
			if batches == 1000 {
				q.t.Fatalf("the backfill of %s does not end", thread)
			}
			result, err := q.s.RecomputeSubagentAggregates(q.t.Context(), thread, batch)
			if err != nil {
				q.t.Fatalf("backfill %s: %v", thread, err)
			}
			if limit > 0 || !result.Remaining {
				break
			}
		}
	}
	if limit == 0 && q.listed() {
		q.t.Fatal("the backfill ended with a thread listed")
	}
}

// reopen closes the store, which drops every card, and opens it again.
func (q *cardSequence) reopen() {
	q.t.Helper()
	if err := q.s.Close(); err != nil {
		q.t.Fatalf("close: %v", err)
	}
	q.s = nil
	clear(q.cards)
	var err error
	if q.s, err = New(q.path); err != nil {
		q.t.Fatalf("reopen: %v", err)
	}
}

// refresh is each thread's refresh timer but skip's.
func (q *cardSequence) refresh(skip string) {
	q.t.Helper()
	for _, thread := range q.threads() {
		if thread == skip {
			continue
		}
		if _, err := q.s.FlushSubagentCards(thread); err != nil {
			q.t.Fatalf("flush %s: %v", thread, err)
		}
	}
}

// boot is the app's start: the boot pass, the crash sweeps, and a new
// turn after the one they settled.
func (q *cardSequence) boot(summarise func(string) string) {
	q.t.Helper()
	if _, err := q.s.RecoverCrashedTurns(summarise, q.now()); err != nil {
		q.t.Fatalf("recover crashed turns: %v", err)
	}
	if q.codex {
		if _, err := q.s.RecoverCodexBackgroundRuntime(summarise, q.now()); err != nil {
			q.t.Fatalf("recover the Codex runtime: %v", err)
		}
	}
	q.openTurn(q.turn + 1)
}

// settled asserts what every boundary ends on: each row served what the
// read-time aggregator computes, each stamp what a recompute derives,
// and nothing pending.
func (q *cardSequence) settled(stage string) {
	q.t.Helper()
	for _, thread := range q.threads() {
		at := stage + " in " + thread
		assertSubagentStampParity(q.t, q.s, thread, at, true)
		assertStampsAreTheRecompute(q.t, q.s, thread, at)
		if pending := pendingCardsForTest(q.s, thread); len(pending) > 0 {
			q.t.Errorf("%s: accumulators still pending: %v", at, pending)
		}
	}
	if q.t.Failed() {
		q.t.FailNow()
	}
}

// assertRebuild compares every stamp row with what
// restampSubagentAggregatesTx rebuilds from the rows on a copy of the
// database. The rebuild stamps the anchors a read decorates and their
// families; a stamp it has no row for, an anchor that lost its children,
// is compared with the recompute of that anchor on the copy. An anchor it
// stamps that holds no stamp is one every read walks: a carrier, which
// stays unstamped until a prompt opens its round, or a childless root a
// carrier names, whose rebuilt stamp must then be the empty card.
func (q *cardSequence) assertRebuild(thread string) {
	t := q.t
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), "rebuild.sqlite")
	if _, err := q.s.db.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatalf("copy the database: %v", err)
	}
	rebuiltStore, err := New(copyPath)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer func() {
		if err := rebuiltStore.Close(); err != nil {
			t.Errorf("close the copy: %v", err)
		}
	}()
	tx, err := rebuiltStore.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := rebuiltStore.restampSubagentAggregatesTx(tx, thread); err != nil {
		t.Fatalf("rebuild %s: %v", thread, err)
	}
	rebuilt := subagentStampRowsInForTest(t, tx, thread)
	stored := subagentStampRowsForTest(t, q.s, thread)
	var unrebuilt []string
	for id := range stored {
		if _, ok := rebuilt[id]; !ok {
			unrebuilt = append(unrebuilt, id)
		}
	}
	slices.Sort(unrebuilt)
	if len(unrebuilt) > 0 {
		members, err := computeSubagentFamilies(tx, thread, unrebuilt)
		if err != nil {
			t.Fatalf("recompute on the copy: %v", err)
		}
		for _, m := range members {
			if slices.Contains(unrebuilt, m.id) {
				rebuilt[m.id] = m.values
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(stored)) {
		want, ok := rebuilt[id]
		switch {
		case !ok:
			t.Errorf("end: %s/%s is stamped %+v, the rebuild derives no stamp", thread, id, stored[id])
		case want != stored[id]:
			t.Errorf("end: %s/%s is stored as %+v, the rebuild derives %+v", thread, id, stored[id], want)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(rebuilt)) {
		if _, ok := stored[id]; ok {
			continue
		}
		var carrier, hasChild bool
		if err := q.s.reader().QueryRow(`SELECT `+aggCarrierSQL("a.")+`, `+aggHasChildSQL("a.thread_id", "a.id", "")+`
		  FROM items a WHERE a.thread_id = ? AND a.id = ?`, thread, id).Scan(&carrier, &hasChild); err != nil {
			t.Fatalf("probe %s/%s: %v", thread, id, err)
		}
		if !carrier && (hasChild || rebuilt[id] != (subagentStampValues{State: aggStateClean})) {
			t.Errorf("end: %s/%s carries no stamp, the rebuild derives %+v", thread, id, rebuilt[id])
		}
	}
}
