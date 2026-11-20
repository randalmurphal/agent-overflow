package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/agentmeta"
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
// The store reads one request per run and one row per worker the thread
// spawned, before any limit (store.ListThreadAgentTree): the row takes the
// spawn's place, with the worker's own rows nested under it.

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
// start while it is open, live as it is for a spawn that tracks no run
// while it is open, as rows did before runs were kept, and none once such a
// spawn settled: it recorded no figures.
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
	case request.RunOpen():
		row.ToolCalls, row.Tokens, row.RuntimeMs = live.toolCalls, live.tokens, live.runtimeMs
	default:
		row.ToolCalls, row.Tokens, row.RuntimeMs = 0, 0, 0
		return
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
// thread on its paired computer when that computer's answer says they
// ended there, with what the thread had done over its life when they did
// (ThreadAgentRows.RunEnd), whoever runs it since; or, from a computer
// that does not say, when its answer says the thread is at rest, with what
// it reported the thread did over its life, unless it says a run is still
// open there (RunOpen): a nudge's message on its way, not yet started. A
// computer that predates the open state says nothing of rest, and its runs
// stay open: its rows read as they did before runs were kept. The answer's
// nudge run is recorded, or ended, after (recordRemoteNudge).
func (a *App) endRemoteRunsAtRest(spawn store.ThreadRequest, listing ThreadAgentRows) error {
	var figures store.RunFigures
	switch {
	case listing.RunEnd != nil:
		figures = runFiguresOf(*listing.RunEnd)
	case listing.Open != nil && !*listing.Open && !listing.RunOpen:
		figures = store.RunFigures{ToolCalls: listing.ToolCalls, Tokens: listing.Tokens, TurnMs: listing.RuntimeMs}
	default:
		return a.recordRemoteNudge(spawn, listing)
	}
	if err := a.store.EndRemoteThreadRequestRuns(spawn.TargetComputerID, spawn.TargetThreadID, figures); err != nil {
		return err
	}
	return a.recordRemoteNudge(spawn, listing)
}

// recordRemoteNudge records the nudge run a paired computer's answer
// reports on its worker as the spawn's caller's nudge row here, the row a
// nudge of a worker on this computer has, or ends it once the answer says
// it ended (store.RecordRemoteNudgeRun). Nothing is delivered for it and
// nobody is woken: the row is settled when written and owes nothing.
func (a *App) recordRemoteNudge(spawn store.ThreadRequest, listing ThreadAgentRows) error {
	nudge := listing.Nudge
	if nudge == nil {
		return nil
	}
	run := store.NudgeRun{Token: nudge.Token, StartedAt: nudge.StartedAt, Base: runFiguresOf(nudge.Base)}
	if nudge.End != nil {
		end := runFiguresOf(*nudge.End)
		run.End = &end
	}
	_, err := a.store.RecordRemoteNudgeRun(spawn.Token, run)
	return err
}

// runFiguresOf is figures an agent_rows answer carried.
func runFiguresOf(end ThreadAgentRunEnd) store.RunFigures {
	return store.RunFigures{ToolCalls: end.ToolCalls, Tokens: end.Tokens, TurnMs: end.RuntimeMs}
}

// remoteRunBase is where the run of row, a request to a paired computer
// about to be written, starts: nothing for a spawn, whose thread is new;
// for a send to a thread its caller spawned there, the figures that
// computer last reported for it (at rest, they have not moved since), none
// when it reported nothing, or the open run's base, which the store
// prefers (store.InsertThreadRequest). Nil, no run, for any other request.
// A failed read of the spawn is the error: the request is not written
// without the run it would start.
func (a *App) remoteRunBase(row store.ThreadRequest) (*store.RunFigures, error) {
	switch {
	case row.Kind == store.ThreadRequestSpawn:
		return &store.RunFigures{}, nil
	case row.Kind != store.ThreadRequestSend || row.TargetThreadID == "":
		return nil, nil
	}
	spawn, found, err := a.store.RemoteSpawnLinkTo(row.TargetComputerID, row.TargetThreadID)
	if err != nil {
		return nil, fmt.Errorf("read the spawn of %s on %s: %w", row.TargetThreadID, row.TargetComputerID, err)
	}
	if !found || spawn.CallerThreadID != row.CallerThreadID {
		return nil, nil
	}
	answer, ok := a.cachedPeerAgentRows(spawn)
	if !ok || answer.err != nil {
		// Nothing reported: the run counts the worker's life, as rows did
		// before runs were kept.
		return &store.RunFigures{}, nil
	}
	return &store.RunFigures{ToolCalls: answer.listing.ToolCalls, Tokens: answer.listing.Tokens, TurnMs: answer.listing.RuntimeMs}, nil
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
		if err := a.endRunsAtRest(thread); err != nil {
			log.Printf("thread tools: %v", err)
		}
	}
}

// workStarts counts, by thread, the messages between their run boundary
// (beginWork) and their placement in the thread: no rest ends a run there
// meanwhile (endRunsAtRest), so the run a nudge starts is not ended before
// its message reaches the thread.
type workStarts struct {
	mu      sync.Mutex
	threads map[string]int
}

// hold counts one message on threadID until the returned release.
func (w *workStarts) hold(threadID string) func() {
	w.mu.Lock()
	if w.threads == nil {
		w.threads = map[string]int{}
	}
	w.threads[threadID]++
	w.mu.Unlock()
	return sync.OnceFunc(func() {
		w.mu.Lock()
		if w.threads[threadID]--; w.threads[threadID] <= 0 {
			delete(w.threads, threadID)
		}
		w.mu.Unlock()
	})
}

// holds reports whether a message on threadID is between its boundary and
// its placement.
func (w *workStarts) holds(threadID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threads[threadID] > 0
}

// workBy is whose message opts sends, for its run boundary: the thread an
// agent request comes from, here or on a paired computer, or the person
// (empty) for a message the person sends. nudges is false for a message the
// app writes, a wake or an answer, which starts no run of anyone's.
func workBy(opts sendMessageOptions) (by string, nudges bool) {
	switch opts.Origin {
	case "":
		return "", true
	case agentmeta.OriginAgentThread:
		if from := opts.OriginThread; from != nil && from.Kind != agentmeta.OriginKindWake {
			return from.ThreadID, true
		}
	}
	return "", false
}

// beginWork is the run boundary of a message that is about to start work on
// threadID: the runs its rest already ended are recorded ended first
// (endRunsBeforeWork), and until done no rest ends a run on the thread, so
// the nudge run its message may start (beginNudge) counts the work it
// starts. An error is returned before anything is recorded, and the caller
// fails with it.
func (a *App) beginWork(ctx context.Context, threadID string) (done func(), err error) {
	if err := a.endRunsBeforeWork(ctx, threadID); err != nil {
		return nil, err
	}
	release := a.workStarts.hold(threadID)
	return func() {
		release()
		// A rest pass that found the thread held here skipped it, and the
		// thread may have rested meanwhile: a quick turn can end before its
		// message's send returns, and nothing else may come to read it.
		a.noteRestWaitsMayChange()
	}, nil
}

// pendingNudge is where the run of a message that may be a nudge starts, read
// when the message is admitted (beginNudge).
type pendingNudge struct {
	threadID, by string
	start        store.NudgeStart
	reaches      bool
}

// beginNudge reads where the run of a message sent with opts to threadID
// starts, under the send's admission and its thread's action lock, inside
// its beginWork. nudges is false for a message the app writes, a wake or an
// answer, which starts no run of anyone's (workBy).
func (a *App) beginNudge(threadID string, opts sendMessageOptions) (pendingNudge, error) {
	by, nudges := workBy(opts)
	if !nudges {
		return pendingNudge{}, nil
	}
	start, reaches, err := a.store.ReadNudgeStart(threadID)
	if err != nil {
		return pendingNudge{}, err
	}
	return pendingNudge{threadID: threadID, by: by, start: start, reaches: reaches}, nil
}

// accepted records the nudge run of a message accepted as new work, by the
// worker's placement and runs now: on an owned worker whose spawner has no
// run open on it, a message of the person's or another thread's is a nudge,
// whose work is a run of the spawner's of its own (store.StartNudgeRun). A
// message that is not accepted, or a repeated frame of one already
// accepted, never comes here. The message is the caller's either way, so a
// failed write is logged: the worker's work then counts in no run.
func (n pendingNudge) accepted(a *App) {
	if !n.reaches {
		return
	}
	// The store's change hook asks the rest pass to read the run it records.
	if _, err := a.store.StartNudgeRun(newThreadRequestToken(), n.threadID, n.by, n.start); err != nil {
		log.Printf("thread tools: record the nudge run of an accepted message: %v", err)
	}
}

// endRunsBeforeWork ends the runs on threadID that its rest has already
// ended and the rest pass has not yet recorded, before a message starts work
// there: that work is a run of its own, the person's, another thread's or a
// later request's, and never extends a run that ended. While the boot has
// not recovered what threads wait on, no rest is read, so it waits for that
// first (awaitRestRecovered). Its caller fails with the error before it
// dispatches anything, since the work would otherwise count in a run that
// ended.
func (a *App) endRunsBeforeWork(ctx context.Context, threadID string) error {
	open, err := a.store.ThreadRunsEnding(threadID)
	if err != nil {
		return err
	}
	if !open {
		return nil
	}
	if err := a.awaitRestRecovered(ctx); err != nil {
		return err
	}
	return a.endRunsAtRest(threadID)
}

// restRecoveryWait bounds how long a message waits for the boot's recovery
// of rest (awaitRestRecovered): several of the sweep's retries of a failed
// recovery read (restRecoveryRetry).
const restRecoveryWait = 30 * time.Second

// errRestRecovering fails a message whose run boundary waited out
// restRecoveryWait: the boot has not yet read what its threads wait on, so
// whether the thread's run ended cannot be decided.
var errRestRecovering = errors.New("the app is still recovering what its agents were doing before it restarted; send again in a moment")

// awaitRestRecovered returns once the boot's recovery of rest has lifted
// its bar (recoverRestAtBoot), at once when none stands. It fails with
// errRestRecovering after restRecoveryWait (a.restRecoveryWait in tests),
// and with ctx's error when ctx ends first.
func (a *App) awaitRestRecovered(ctx context.Context) error {
	a.threadRequests.mu.Lock()
	lifted := a.threadRequests.rest.lifted
	a.threadRequests.mu.Unlock()
	if lifted == nil {
		return nil
	}
	wait := restRecoveryWait
	if a.restRecoveryWaitOverride > 0 {
		wait = a.restRecoveryWaitOverride
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-lifted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errRestRecovering
	}
}

// endRunsAtRest ends the runs of the settled requests to threadID if it is
// at rest now, by the rule for decisions that cannot be taken back: no read
// while the boot has not recovered every source of rest or a stop brings
// the thread to rest, as heldThreadAtRest. Its read leaves the held
// requests' retries (noteRestRead) to their own attempts. A read or write
// that fails returns its error and ends nothing.
func (a *App) endRunsAtRest(threadID string) error {
	a.threadRequests.mu.Lock()
	barred := a.threadRequests.rest.barred
	a.threadRequests.mu.Unlock()
	if barred || a.resting.holds(threadID) || a.workStarts.holds(threadID) {
		return nil
	}
	if _, held := a.runEndsHeldForTest.Load(threadID); held {
		return nil
	}
	read, err := a.readRest([]string{threadID})
	defer read.done()
	if err != nil {
		return fmt.Errorf("read whether %s is at rest to end its runs: %w", threadID, err)
	}
	if !read.rests(threadID) {
		return nil
	}
	if _, err := a.store.EndThreadRequestRunsAtRest(threadID, read.guard(threadID)); err != nil {
		return fmt.Errorf("end the runs on %s: %w", threadID, err)
	}
	return nil
}
