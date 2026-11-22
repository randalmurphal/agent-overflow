package app

// A row is a run (docs/specs/standing-agents.md, "Agents segment"): the
// store reads one request per run, and one row per worker the caller
// spawned, before the listing's limit, so a run of many requests takes one
// place in the listing, its counts, Hidden and Past.

import (
	"fmt"
	"testing"

	"agent-overflow/internal/store"
)

// A run with more sends than the listing's limit is one row, and the next
// worker's row is still listed: the limit counts rows, not requests.
func TestARunOfMoreSendsThanTheLimitIsOneRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	busy, other := f.plainThread(t), f.plainThread(t)
	f.acceptAgentRequest(t, "busy-spawn", f.caller.ID, store.ThreadRequestSpawn, busy.ID)
	for i := range threadAgentRowLimit + 2 {
		f.acceptAgentRequest(t, fmt.Sprintf("busy-send-%03d", i), f.caller.ID, store.ThreadRequestSend, busy.ID)
	}
	if row := f.request(t, "busy-send-100"); !row.RunOpen() || row.RunBase == nil || row.RunToken != "busy-spawn" {
		t.Fatalf("a send during the spawn's run %+v; want it in the spawn's run", row)
	}
	f.acceptAgentRequest(t, "other-spawn", f.caller.ID, store.ThreadRequestSpawn, other.ID)

	listing := f.agentRows(t, f.caller.ID)
	if got := agentRowTokens(listing.Rows); len(got) != 2 || got[0] != "busy-spawn" || got[1] != "other-spawn" || listing.Hidden {
		t.Fatalf("rows %v, hidden %v; want one row per worker, nothing hidden", got, listing.Hidden)
	}
	if got := f.workerSummary(t, f.caller.ID); got.Open != 2 {
		t.Fatalf("summary %+v; want both workers open", got)
	}
	reads, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, []string{"busy-spawn", "busy-send-007", "other-spawn"})
	if err != nil {
		t.Fatal(err)
	}
	if !reads[0].Listed || reads[1].Listed || !reads[2].Listed {
		t.Fatalf("named reads %+v; want the run's row and the other worker's, not a send the run's row stands for", reads)
	}

	// Settled inside the window, the run is still one row.
	f.settleAgentRequest(t, "busy-spawn")
	for i := range threadAgentRowLimit + 2 {
		f.settleAgentRequest(t, fmt.Sprintf("busy-send-%03d", i))
	}
	f.settleAgentRequest(t, "other-spawn")
	listing = f.agentRows(t, f.caller.ID)
	if got := agentRowTokens(listing.Rows); len(got) != 2 || got[1] != "other-spawn" || listing.Hidden {
		t.Fatalf("settled rows %v, hidden %v; want one row per worker, nothing hidden", got, listing.Hidden)
	}
}

// Past lists a run once, by its newest settled request with the run's
// figures, counts runs, and pages them without a repeat or a gap.
func TestPastListsEachRunOnce(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, "paged-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	workerTurn(t, f.app, worker.ID, "t0", 2, 60)
	f.settleAgentRequest(t, "paged-spawn")
	f.awaitRunEnded(t, "paged-spawn")
	const runs = 10
	runOf := map[string]int{"paged-spawn": 0}
	for i := 1; i <= runs; i++ {
		first, second := fmt.Sprintf("paged-%02d-a", i), fmt.Sprintf("paged-%02d-b", i)
		runOf[first], runOf[second] = i, i
		f.acceptAgentRequest(t, first, f.caller.ID, store.ThreadRequestSend, worker.ID)
		f.acceptAgentRequest(t, second, f.caller.ID, store.ThreadRequestSend, worker.ID)
		workerTurn(t, f.app, worker.ID, fmt.Sprintf("t%d", i), 1, 0)
		f.settleAgentRequest(t, first)
		f.settleAgentRequest(t, second)
		f.awaitRunEnded(t, first)
		f.awaitRunEnded(t, second)
	}

	seen := map[int]string{}
	cursor, pages := store.PastAgentRowsCursor{}, 0
	for _, limit := range []int{8, 20} {
		page, err := f.app.ListThreadPastAgentRows(t.Context(), f.caller.ID, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Total != runs+1 {
			t.Fatalf("page %d total %d; want %d runs", pages, page.Total, runs+1)
		}
		for _, row := range page.Rows {
			run, ok := runOf[row.Token]
			if !ok {
				t.Fatalf("Past row %s is no request of the runs", row.Token)
			}
			if earlier, twice := seen[run]; twice {
				t.Fatalf("run %d listed as %s and %s; want it once", run, earlier, row.Token)
			}
			seen[run] = row.Token
			if want := int64(1); run > 0 && row.ToolCalls != want {
				t.Fatalf("Past row %s %+v; want its run's one call", row.Token, row)
			}
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if len(seen) != runs+1 || pages != 2 {
		t.Fatalf("Past listed runs %v in %d pages; want each of %d runs once, in two pages", seen, pages, runs+1)
	}
}
