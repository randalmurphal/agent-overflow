package app

import (
	"log"

	"agent-overflow/internal/store"
)

// An Agents row is a run (docs/specs/standing-agents.md, "Agents segment"):
// one stretch of a worker's work, from when it left rest for a request of
// the listed thread's until it is at rest again. The run's start and its
// totals are the request's (store.ThreadRequest.RunBase, RunTotal,
// request_runs.go); the row counts the tool calls, tokens and turn time of
// that run only, live while it is open and the kept totals once it ended.
// A spawn whose run ended stays closed whatever its worker does later: a
// person typing in it, another thread's send or its own wake is not a run
// the spawner started. A send to the worker the caller spawned starts the
// next run, or joins the one open, and is the row for it.
//
// The listing shows one row per worker, however many of the thread's
// requests went to it (mergeAgentRuns): the oldest that is open, the run's
// first request, else the newest, in the place of the first, under which
// the worker's own rows nest.

// threadAgentFigures is what a worker did over its life as one read saw
// it: the open turn's time so far included, which turn is open named by its
// start.
type threadAgentFigures struct {
	toolCalls, tokens, runtimeMs int64
	turnOpen                     bool
	turnStartedAt                int64
}

// runFigures sets what request's run did from live, the worker's figures
// over its life: the kept totals once the run ended, live less the run's
// start while it is open, and live as it is for a request that tracks no
// run, as rows did before runs were kept.
func (row *ThreadAgentRow) runFigures(request store.ThreadRequest, live threadAgentFigures) {
	row.TurnOpen, row.TurnStartedAt = false, 0
	switch {
	case request.RunTotal != nil:
		total := request.RunTotal
		row.ToolCalls, row.Tokens, row.RuntimeMs = total.ToolCalls, total.Tokens, total.TurnMs
		return
	case request.RunBase != nil:
		base := request.RunBase
		row.ToolCalls = max(0, live.toolCalls-base.ToolCalls)
		row.Tokens = max(0, live.tokens-base.Tokens)
		row.RuntimeMs = max(0, live.runtimeMs-base.TurnMs)
	default:
		row.ToolCalls, row.Tokens, row.RuntimeMs = live.toolCalls, live.tokens, live.runtimeMs
	}
	if live.turnOpen {
		row.TurnOpen, row.TurnStartedAt = true, live.turnStartedAt
	}
}

// peerFigures is what a paired computer's agent_rows answer reported its
// thread did over its life.
func peerFigures(listing ThreadAgentRows) threadAgentFigures {
	return threadAgentFigures{
		toolCalls: listing.ToolCalls, tokens: listing.Tokens, runtimeMs: listing.RuntimeMs,
		turnOpen: listing.TurnOpen, turnStartedAt: listing.TurnStartedAt,
	}
}

// remoteRunAnswer is the last agent_rows answer for the thread a request
// went to on a paired computer, read through the spawn that links it here.
func (a *App) remoteRunAnswer(row store.ThreadRequest) (threadAgentPeerAnswer, bool) {
	spawn, found, err := a.store.RemoteSpawnLinkTo(row.TargetComputerID, row.TargetThreadID)
	if err != nil {
		log.Printf("agent rows: read the spawn of %s on %s: %v", row.TargetThreadID, row.TargetComputerID, err)
		return threadAgentPeerAnswer{}, false
	}
	if !found {
		return threadAgentPeerAnswer{}, false
	}
	answer, ok := a.cachedPeerAgentRows(spawn)
	if !ok && spawn.NestedOpen {
		// No answer since a restart: the recorded open state stands.
		open := true
		return threadAgentPeerAnswer{listing: ThreadAgentRows{Open: &open}}, true
	}
	return answer, ok
}

// remoteRunWorker is why a paired computer's worker, which an open run of
// row is on, is not at rest, as that computer last answered.
func (a *App) remoteRunWorker(row store.ThreadRequest) workerRest {
	answer, ok := a.remoteRunAnswer(row)
	if !ok {
		return workerRest{}
	}
	switch {
	case answer.listing.Worker != "":
		return workerRest{unrest: answer.listing.Worker, detail: answer.listing.Activity}
	case peerListingOpen(answer.listing):
		return workerRest{unrest: workerWaiting, detail: answer.listing.Activity}
	}
	return workerRest{}
}

// endRemoteRunsAtRest ends the runs of the settled requests to spawn's
// thread on its paired computer when that computer's answer says the
// thread is at rest, with what it reported the thread did over its life.
// A computer that predates the open state says nothing of rest, and its
// runs stay open: its rows read as they did before runs were kept.
func (a *App) endRemoteRunsAtRest(spawn store.ThreadRequest, listing ThreadAgentRows) error {
	if listing.Open == nil || *listing.Open {
		return nil
	}
	return a.store.EndRemoteThreadRequestRuns(spawn.TargetComputerID, spawn.TargetThreadID, store.RunFigures{
		ToolCalls: listing.ToolCalls, Tokens: listing.Tokens, TurnMs: listing.RuntimeMs,
	})
}

// remoteRunBase is where the run of row, a request to a paired computer
// about to be written, starts: nothing for a spawn, whose thread is new;
// for a send to a thread its caller spawned there, the figures that
// computer last reported for it (at rest, they have not moved since), none
// when it reported nothing, or the open run's base, which the store
// prefers (store.InsertThreadRequest). Nil, no run, for any other request.
func (a *App) remoteRunBase(row store.ThreadRequest) *store.RunFigures {
	switch {
	case row.Kind == store.ThreadRequestSpawn:
		return &store.RunFigures{}
	case row.Kind != store.ThreadRequestSend || row.TargetThreadID == "":
		return nil
	}
	spawn, found, err := a.store.RemoteSpawnLinkTo(row.TargetComputerID, row.TargetThreadID)
	if err != nil {
		log.Printf("thread tools: read the spawn of %s on %s: %v", row.TargetThreadID, row.TargetComputerID, err)
		return nil
	}
	if !found || spawn.CallerThreadID != row.CallerThreadID {
		return nil
	}
	answer, ok := a.cachedPeerAgentRows(spawn)
	if !ok || answer.err != nil {
		// Nothing reported: the run counts the worker's life, as rows did
		// before runs were kept.
		return &store.RunFigures{}
	}
	return &store.RunFigures{ToolCalls: answer.listing.ToolCalls, Tokens: answer.listing.Tokens, TurnMs: answer.listing.RuntimeMs}
}

// threadAgentRunKey names the worker a row's request went to, for
// mergeAgentRuns: the caller, the ledger that holds the request, and the
// thread on its computer. Empty for a row that is a worker of its own, an
// ask's copy, or a request not yet given a thread.
func threadAgentRunKey(row ThreadAgentRow) string {
	if row.ThreadID == "" || (row.Kind != "spawn" && row.Kind != "send") {
		return ""
	}
	return row.OwnerComputerID + "\x00" + row.CallerThreadID + "\x00" + row.ComputerID + "\x00" + row.ThreadID
}

// mergeAgentRuns keeps one row per worker of a listing in tree order, with
// what stops it: of the rows of one caller to one thread, the oldest open,
// which is the run's first request, else the newest, placed where the
// first of them is (a spawn, whose worker's own rows follow it). That row
// keeps what the first says of the rows under it, and a spawn's placement,
// which the thread holds whichever request the row shows.
func mergeAgentRuns(rows []ThreadAgentRow, entries []threadAgentEntry) ([]ThreadAgentRow, []threadAgentEntry) {
	type run struct{ first, shown int }
	runs := map[string]*run{}
	merged := false
	for index, row := range rows {
		key := threadAgentRunKey(row)
		if key == "" {
			continue
		}
		r := runs[key]
		if r == nil {
			runs[key] = &run{first: index, shown: index}
			continue
		}
		merged = true
		// Siblings are oldest first: a later open row is younger than the
		// open one kept, and a later closed one newer than the closed one.
		if shown := rows[r.shown]; !shown.open() {
			r.shown = index
		}
	}
	if !merged {
		return rows, entries
	}
	outRows := make([]ThreadAgentRow, 0, len(rows))
	outEntries := make([]threadAgentEntry, 0, len(entries))
	for index, row := range rows {
		key := threadAgentRunKey(row)
		r := runs[key]
		if key == "" || r == nil {
			outRows, outEntries = append(outRows, row), append(outEntries, entries[index])
			continue
		}
		if index != r.first {
			continue
		}
		first, shown := rows[r.first], rows[r.shown]
		shown.Depth = first.Depth
		shown.NestedHidden = shown.NestedHidden || first.NestedHidden
		if shown.NestedUnavailable == "" {
			shown.NestedUnavailable = first.NestedUnavailable
		}
		if shown.Kind != "spawn" && first.Kind == "spawn" {
			shown.Placement, shown.Archived = first.Placement, first.Archived
		}
		outRows, outEntries = append(outRows, shown), append(outEntries, entries[r.shown])
	}
	return outRows, outEntries
}

// noteRunsMayBeOpen records that a request changed, which may have started
// a run or settled one whose worker is already at rest: the rest pass reads
// the open runs again (endRestedRuns).
func (a *App) noteRunsMayBeOpen() {
	r := &a.threadRequests
	r.mu.Lock()
	r.rest.runsOpen = true
	r.rest.runsSeq++
	r.mu.Unlock()
}

// endRestedRuns ends the run of every settled request whose worker on this
// computer is at rest now: a decision that cannot be taken back, so it
// reads the worker's rest (heldThreadAtRest) and its write carries that
// rest as its guard. A worker not at rest keeps its run open, and the next
// change that can bring it to rest asks for another pass. A read or write
// that fails is logged and read again by the next pass.
func (a *App) endRestedRuns() {
	r := &a.threadRequests
	r.mu.Lock()
	open, seq := r.rest.runsOpen, r.rest.runsSeq
	r.mu.Unlock()
	if !open {
		return
	}
	threads, any, err := a.store.OpenRunThreads()
	if err != nil {
		log.Printf("thread tools: read the open runs: %v", err)
		return
	}
	if !any {
		r.mu.Lock()
		if r.rest.runsSeq == seq {
			r.rest.runsOpen = false
		}
		r.mu.Unlock()
		return
	}
	for _, thread := range threads {
		a.endRunsAtRest(thread)
	}
}

// endRunsAtRest ends the runs of the settled requests to threadID if it is
// at rest now, by the rule for decisions that cannot be taken back: no read
// while the boot has not recovered every source of rest or a stop brings
// the thread to rest, as heldThreadAtRest. Its read leaves the held
// requests' retries (noteRestRead) to their own attempts; one that fails is
// logged, and the next pass reads again.
func (a *App) endRunsAtRest(threadID string) {
	a.threadRequests.mu.Lock()
	barred := a.threadRequests.rest.barred
	a.threadRequests.mu.Unlock()
	if barred || a.resting.holds(threadID) {
		return
	}
	read, err := a.readRest([]string{threadID})
	if err != nil {
		log.Printf("thread tools: read whether %s is at rest to end its runs: %v", threadID, err)
	}
	defer read.done()
	if !read.rests(threadID) {
		return
	}
	if _, err := a.store.EndThreadRequestRunsAtRest(threadID, read.guard(threadID)); err != nil {
		log.Printf("thread tools: end the runs on %s: %v", threadID, err)
	}
}
