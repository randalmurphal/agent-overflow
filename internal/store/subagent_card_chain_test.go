package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// cardChainFixture is a thread with background agents running side by
// side, each with its session card holding a row no flush has written.
type cardChainFixture struct {
	s       *Store
	thread  string
	session *cardSessionForTest
	agents  []string
}

func newCardChainFixture(t *testing.T, agents ...string) *cardChainFixture {
	t.Helper()
	s := newTestStore(t)
	f := &cardChainFixture{s: s, thread: "t-chain", agents: agents}
	if err := s.CreateThread(makeThread(f.thread, "claude")); err != nil {
		t.Fatal(err)
	}
	f.session = newCardSessionForTest(t, s, f.thread)
	for i, id := range agents {
		launch := stampFixtureRow{id: id, kind: "tool_call", tool: "Agent", summary: "Agent: " + id,
			status: "running", background: true, turn: 1, index: i}.item(f.thread)
		if err := s.InsertItem(launch); err != nil {
			t.Fatalf("launch %s: %v", id, err)
		}
	}
	f.session.flush()
	for i, id := range agents {
		row := stampFixtureRow{id: id + "-r", kind: "assistant_text", summary: "working " + id,
			parent: id, turn: 2, index: i}.item(f.thread)
		row.SubagentCard = f.session.card(id)
		if err := s.InsertItem(row); err != nil {
			t.Fatalf("write under %s: %v", id, err)
		}
	}
	if got := pendingCardsForTest(s, f.thread); !reflect.DeepEqual(got, agents) {
		t.Fatalf("the agents' writes left %v pending, want %v", got, agents)
	}
	return f
}

// resolved reports, after the drain the next card operation runs, which
// agents' cards still hold the chain they read.
func (f *cardChainFixture) resolved() []string {
	t := f.s.cards.acquire(f.thread, false)
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	defer f.s.cards.release(t)
	f.s.cards.drain(t)
	var ids []string
	for _, id := range f.agents {
		if f.session.cards[id].resolved {
			ids = append(ids, id)
		}
	}
	return ids
}

func without(ids []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == drop })
}

// TestSubagentCardCompletionTouchesOnlyItsChain pins that an agent's
// completion sibling concerns its own chain alone: the launch it settles
// and that launch's cards resolve again and flush in the completing
// write, and every other agent's card stays resolved with its rows
// pending for its own flush, its stamp row untouched. A crash right
// after it still loses nothing.
func TestSubagentCardCompletionTouchesOnlyItsChain(t *testing.T) {
	agents := []string{"A", "B", "X"}
	f := newCardChainFixture(t, agents...)
	if got := f.resolved(); !reflect.DeepEqual(got, agents) {
		t.Fatalf("resolved cards before the completion: %v, want %v", got, agents)
	}
	before := subagentStampRowsForTest(t, f.s, f.thread)

	done := stampFixtureRow{id: "X-done", kind: "tool_completion", tool: "Agent", summary: "done",
		background: true, completionOf: "X", turn: 3}.item(f.thread)
	if _, err := f.s.AppendCompletionItem(Item{ID: "X", ThreadID: f.thread}, done, nil); err != nil {
		t.Fatal(err)
	}

	if got, want := pendingCardsForTest(f.s, f.thread), without(agents, "X"); !reflect.DeepEqual(got, want) {
		t.Errorf("the completion of X left %v pending, want %v: it flushed another agent's card", got, want)
	}
	after := subagentStampRowsForTest(t, f.s, f.thread)
	for _, id := range without(agents, "X") {
		if after[id] != before[id] {
			t.Errorf("the completion of X wrote the stamp of %s: %+v, was %+v", id, after[id], before[id])
		}
	}
	if after["X"] == before["X"] {
		t.Error("the completion of X left its own card unwritten")
	}
	if got, want := f.resolved(), without(agents, "X"); !reflect.DeepEqual(got, want) {
		t.Errorf("resolved cards after the completion of X: %v, want %v", got, want)
	}

	crashSubagentCardsForTest(f.s)
	if _, err := f.s.RecoverSubagentCards(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSubagentStampParity(t, f.s, f.thread, "a crash after the completion", true)
	assertStampsAreTheRecompute(t, f.s, f.thread, "a crash after the completion")
}

// TestSubagentCardChainFlushWritesOnlyTheChain pins FlushSubagentChain:
// the accumulators on the named parent's chain are written, and the
// other agents' stay pending with their stamp rows untouched.
func TestSubagentCardChainFlushWritesOnlyTheChain(t *testing.T) {
	agents := []string{"A", "B", "X"}
	f := newCardChainFixture(t, agents...)
	before := subagentStampRowsForTest(t, f.s, f.thread)

	changed, err := f.s.FlushSubagentChain(f.thread, "X")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []string{"X"}) {
		t.Errorf("the chain flush of X changed %v, want [X]", changed)
	}
	if got, want := pendingCardsForTest(f.s, f.thread), without(agents, "X"); !reflect.DeepEqual(got, want) {
		t.Errorf("the chain flush of X left %v pending, want %v", got, want)
	}
	after := subagentStampRowsForTest(t, f.s, f.thread)
	for _, id := range without(agents, "X") {
		if after[id] != before[id] {
			t.Errorf("the chain flush of X wrote the stamp of %s", id)
		}
	}
	f.session.flush()
	assertSubagentStampParity(t, f.s, f.thread, "the thread flush after the chain flush", true)
	assertStampsAreTheRecompute(t, f.s, f.thread, "the thread flush after the chain flush")
}

// TestSubagentCardCloseWritesOnlyItsReach pins Close: the closing card's
// accumulators are written, and the other open cards keep theirs pending
// for their own flush.
func TestSubagentCardCloseWritesOnlyItsReach(t *testing.T) {
	agents := []string{"A", "B", "X"}
	f := newCardChainFixture(t, agents...)
	before := subagentStampRowsForTest(t, f.s, f.thread)

	if err := f.session.cards["X"].Close(); err != nil {
		t.Fatal(err)
	}
	delete(f.session.cards, "X")
	if got, want := pendingCardsForTest(f.s, f.thread), without(agents, "X"); !reflect.DeepEqual(got, want) {
		t.Errorf("closing the card of X left %v pending, want %v", got, want)
	}
	after := subagentStampRowsForTest(t, f.s, f.thread)
	for _, id := range without(agents, "X") {
		if after[id] != before[id] {
			t.Errorf("closing the card of X wrote the stamp of %s", id)
		}
	}
	if after["X"] == before["X"] {
		t.Error("closing the card of X left its rows unwritten")
	}
	f.session.flush()
	assertSubagentStampParity(t, f.s, f.thread, "the thread flush after the close", true)
	assertStampsAreTheRecompute(t, f.s, f.thread, "the thread flush after the close")
}

// TestSubagentCardStopWritesWhatNoCardReaches pins the other half of the
// stop rule: an accumulator no open card reaches, here left by a close
// whose flush failed, is written by the next write that stops an agent,
// as no card's own flush writes it and the stop may have been what kept
// it recoverable.
func TestSubagentCardStopWritesWhatNoCardReaches(t *testing.T) {
	agents := []string{"A", "X"}
	f := newCardChainFixture(t, agents...)
	mustExec(t, f.s.db, `CREATE TRIGGER fail_flush BEFORE INSERT ON subagent_aggregates BEGIN SELECT RAISE(ABORT, 'injected flush failure'); END`)
	err := f.session.cards["X"].Close()
	mustExec(t, f.s.db, `DROP TRIGGER fail_flush`)
	if err == nil || !strings.Contains(err.Error(), "injected flush failure") {
		t.Fatalf("close with a failing flush: %v, want the injected failure", err)
	}
	delete(f.session.cards, "X")
	if got := pendingCardsForTest(f.s, f.thread); !reflect.DeepEqual(got, agents) {
		t.Fatalf("the failed close left %v pending, want %v", got, agents)
	}

	done := stampFixtureRow{id: "X-done", kind: "tool_completion", tool: "Agent", summary: "done",
		background: true, completionOf: "X", turn: 3}.item(f.thread)
	if _, err := f.s.AppendCompletionItem(Item{ID: "X", ThreadID: f.thread}, done, nil); err != nil {
		t.Fatal(err)
	}
	if got := pendingCardsForTest(f.s, f.thread); !reflect.DeepEqual(got, []string{"A"}) {
		t.Errorf("the completion of X left %v pending, want [A]: what no card reached waits for a flush", got)
	}
	crashSubagentCardsForTest(f.s)
	if _, err := f.s.RecoverSubagentCards(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSubagentStampParity(t, f.s, f.thread, "a crash after the completion", true)
	assertStampsAreTheRecompute(t, f.s, f.thread, "a crash after the completion")
}

// TestSubagentCardChainFlushCostsTheSameAtAnyWidth pins the cost of a
// completion against the number of agents beside it: the statements the
// completing write and the chain flush before it run do not grow with
// the agents that keep running.
func TestSubagentCardChainFlushCostsTheSameAtAnyWidth(t *testing.T) {
	cost := func(n int) []string {
		agents := make([]string, n)
		for i := range agents {
			agents[i] = fmt.Sprintf("A%03d", i)
		}
		f := newCardChainFixture(t, agents...)
		last := agents[n-1]
		before := subagentStampRowsForTest(t, f.s, f.thread)
		rec := recordStatements(t, f.s)
		ran := rec.capture(func() {
			if _, err := f.s.FlushSubagentChain(f.thread, last); err != nil {
				t.Fatal(err)
			}
			done := stampFixtureRow{id: last + "-done", kind: "tool_completion", tool: "Agent", summary: "done",
				background: true, completionOf: last, turn: 3}.item(f.thread)
			if _, err := f.s.AppendCompletionItem(Item{ID: last, ThreadID: f.thread}, done, nil); err != nil {
				t.Fatal(err)
			}
		})
		after := subagentStampRowsForTest(t, f.s, f.thread)
		for _, id := range agents[:n-1] {
			if after[id] != before[id] {
				t.Fatalf("n=%d: the completion of %s wrote the stamp of %s", n, last, id)
			}
		}
		queries := make([]string, len(ran))
		for i, statement := range ran {
			queries[i] = statement.query
		}
		return queries
	}
	small, wide := cost(3), cost(60)
	if len(wide) != len(small) {
		t.Errorf("a completion beside 2 agents ran %d statements, beside 59 %d: its cost grows with the agents\nbeside 2:\n%s\nbeside 59:\n%s",
			len(small), len(wide), strings.Join(small, "\n"), strings.Join(wide, "\n"))
	}
}
