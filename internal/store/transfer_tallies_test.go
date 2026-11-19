package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A transfer's history is materialized, not done: a copy is a new thread
// that has done nothing yet, whatever its archive holds, and counts from
// its first own call. A move is the same thread going on elsewhere: its
// archive carries its tallies, which replace what the destination held, so
// a thread that moves away, works and comes back keeps the work done on
// both computers. An archive from a sender that carries no tallies
// installs none.
func TestATransferCountsOnlyWhatTheThreadDid(t *testing.T) {
	ctx := context.Background()
	a := newTestStore(t)
	mustCreateThread(t, a, "moving")
	recordWork(t, a, "moving", 0, 20, 500, 60_000)
	if got := tallyOf(t, a, "moving"); got != (ThreadTally{ToolCalls: 20, Tokens: 500, RuntimeMs: 60_000}) {
		t.Fatalf("source tallies %+v", got)
	}
	export := func(s *Store, move bool) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := s.ExportThreadHistoryWith(ctx, "moving", &out, ThreadHistoryExport{Tallies: move}); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	commit := func(s *Store, request ThreadTransfer, history []byte) {
		t.Helper()
		if _, err := s.CreateThreadTransfer(request); err != nil {
			t.Fatal(err)
		}
		advanceTransfer(t, s, request.ID, "prepared")
		if _, err := s.CommitIncomingThreadTransfer(ctx, request.ID, strings.Repeat("b", 64), transferTestSecret(), makeThread(request.ThreadID, "claude"), bytes.NewReader(history)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a copy starts at zero", func(t *testing.T) {
		for name, history := range map[string][]byte{"copy archive": export(a, false), "archive with tallies": export(a, true)} {
			b := newTestStore(t)
			commit(b, transferRequest("copied", "copy", "incoming"), history)
			if items, err := b.ListItems("copied"); err != nil || len(items) != 20 {
				t.Fatalf("%s: copied %d rows, %v; want the 20 calls", name, len(items), err)
			}
			if got := tallyOf(t, b, "copied"); got != (ThreadTally{}) {
				t.Fatalf("%s: copy's tallies %+v; want none", name, got)
			}
			recordWork(t, b, "copied", 1, 1, 0, 0)
			if got := tallyOf(t, b, "copied"); got.ToolCalls != 1 {
				t.Fatalf("%s: copy's tallies after its own call %+v; want 1", name, got)
			}
		}
		b := newTestStore(t)
		if err := b.ImportThreadHistory(ctx, makeThread("validated", "claude"), bytes.NewReader(export(a, true))); err != nil {
			t.Fatal(err)
		}
		if got := tallyOf(t, b, "validated"); got != (ThreadTally{}) {
			t.Fatalf("an import's tallies %+v; want none", got)
		}
	})

	t.Run("a move keeps them, there and back", func(t *testing.T) {
		b := newTestStore(t)
		commit(b, transferRequest("moving", "move", "incoming"), export(a, true))
		if got := tallyOf(t, b, "moving"); got != (ThreadTally{ToolCalls: 20, Tokens: 500, RuntimeMs: 60_000}) {
			t.Fatalf("moved tallies %+v; want the source's", got)
		}
		recordWork(t, b, "moving", 1, 1, 30, 10_000)
		back := export(b, true)
		outgoing := transferRequest("moving", "move", "outgoing")
		if _, err := a.CreateThreadTransfer(outgoing); err != nil {
			t.Fatal(err)
		}
		advanceTransfer(t, a, outgoing.ID, "prepared", "committed", "complete")
		returning := transferRequest("moving", "move", "incoming")
		returning.OwnershipEpoch = 2
		commit(a, returning, back)
		if got := tallyOf(t, a, "moving"); got != (ThreadTally{ToolCalls: 21, Tokens: 530, RuntimeMs: 70_000}) {
			t.Fatalf("returned tallies %+v; want the work done on both, once", got)
		}
	})

	t.Run("an archive without tallies installs none", func(t *testing.T) {
		b := newTestStore(t)
		commit(b, transferRequest("moving", "move", "incoming"), export(a, false))
		if got := tallyOf(t, b, "moving"); got != (ThreadTally{}) {
			t.Fatalf("tallies from an archive without them %+v; want none", got)
		}
		c := newTestStore(t)
		mustCreateThread(t, c, "moving")
		recordWork(t, c, "moving", 0, 2, 0, 1_000)
		outgoing := transferRequest("moving", "move", "outgoing")
		if _, err := c.CreateThreadTransfer(outgoing); err != nil {
			t.Fatal(err)
		}
		advanceTransfer(t, c, outgoing.ID, "prepared", "committed", "complete")
		returning := transferRequest("moving", "move", "incoming")
		returning.OwnershipEpoch = 2
		commit(c, returning, export(a, false))
		if got := tallyOf(t, c, "moving"); got != (ThreadTally{ToolCalls: 2, RuntimeMs: 1_000}) {
			t.Fatalf("returned tallies with none carried %+v; want what this computer held", got)
		}
	})
}

// recordWork records, as the thread's own writers do, calls top-level tool calls
// in turn index, tokens of its own spend and a turn that ran ms.
func recordWork(t *testing.T, s *Store, threadID string, index, calls int, tokens, ms int64) {
	t.Helper()
	for i := range calls {
		if err := s.InsertItem(Item{ID: fmt.Sprintf("call-%d-%d", index, i), ThreadID: threadID, TurnIndex: index, ItemIndex: i,
			Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "call", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if tokens > 0 {
		if err := s.AppendUsageAndReconcile("scope-"+threadID, nil, OwnSpend{ThreadID: threadID, Tokens: tokens}); err != nil {
			t.Fatal(err)
		}
	}
	if ms > 0 {
		turnID := fmt.Sprintf("%s:%d", threadID, index)
		if err := s.InsertTurn(Turn{TurnID: turnID, ThreadID: threadID, TurnIndex: index, StartedAt: 1_000}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateTurnCompleted(turnID, 1_000+ms, "end_turn", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func tallyOf(t *testing.T, s *Store, threadID string) ThreadTally {
	t.Helper()
	got, err := s.ThreadTallies([]string{threadID}, time.UnixMilli(0))
	if err != nil {
		t.Fatal(err)
	}
	return got[threadID]
}
