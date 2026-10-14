package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// agentPeerPair is a source thread whose spawn on a paired computer has a
// spawn of its own there: the lineage the forwarded Agents calls carry.
type agentPeerPair struct {
	*reachPair
	// spawn is the source's request; child the thread it made there.
	spawn store.ThreadRequest
	child string
	// grand is the child's own spawn request on that computer.
	grand string
}

func newAgentPeerPair(t *testing.T, serve func(*testing.T, *App) *pairedBackend) *agentPeerPair {
	t.Helper()
	remotetest.Require(t)
	pair := newReachPairServedBy(t, serve)
	// Every turn there holds open and answers an interrupt, so the spawns
	// stay open until a Stop ends them.
	// The caller here takes the user-cancelled wake a Stop sends it.
	holding := [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}}
	installMockClaudeTurns(t, pair.dest, holding)
	installMockClaudeTurns(t, pair.source, holding)
	ack := pair.spawnThere(t, "work over there", map[string]any{"notify": true})
	child := pair.receipt(t, ack.Token).TargetThreadID
	if child == "" {
		t.Fatal("the spawn names no thread on the destination")
	}
	destID, _ := pair.dest.backendIdentity()
	grand, err := threadToolsApp{app: pair.dest}.Spawn(t.Context(),
		threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"},
		threadtools.SpawnCall{Prompt: "a part of it", Title: "Grandchild", Notify: true})
	if err != nil {
		t.Fatalf("the child's spawn: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := pair.dest.store.GetThreadRequestReceipt(grand.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
	return &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token), child: child, grand: grand.Token}
}

func (p *agentPeerPair) listRows(t *testing.T) ThreadAgentRows {
	t.Helper()
	listing, err := p.source.ListThreadAgentRows(t.Context(), p.caller.ID)
	if err != nil {
		t.Fatalf("ListThreadAgentRows: %v", err)
	}
	return listing
}

// listRowsUntil reads the listing until done holds of it: a spawn's rows on
// the paired computer arrive with a read after the background call returns.
func (p *agentPeerPair) listRowsUntil(t *testing.T, done func(ThreadAgentRows) bool) ThreadAgentRows {
	t.Helper()
	var listing ThreadAgentRows
	waitUntil(t, 10*time.Second, func() bool {
		listing = p.listRows(t)
		return done(listing)
	})
	return listing
}

func agentRowByToken(rows []ThreadAgentRow, token string) (ThreadAgentRow, bool) {
	for _, row := range rows {
		if row.Token == token {
			return row, true
		}
	}
	return ThreadAgentRow{}, false
}

// A spawn on a paired computer nests that thread's own agents under its
// row, named by that computer, so the pane shows them as it would local
// ones.
func TestThreadAgentRowsNestAPairedComputersAgents(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })

	// The first read returns this computer's rows at once; the nested rows
	// arrive with a later read.
	first := pair.listRows(t)
	if _, ok := agentRowByToken(first.Rows, pair.spawn.Token); !ok {
		t.Fatalf("the first read does not list the spawn: %+v", first.Rows)
	}
	listing := pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		_, ok := agentRowByToken(rows.Rows, pair.grand)
		return ok
	})
	parent, ok := agentRowByToken(listing.Rows, pair.spawn.Token)
	if !ok || parent.Depth != 0 || parent.ComputerID != pair.computer || parent.NestedUnavailable != "" {
		t.Fatalf("the spawn row = %+v (listed %v)", parent, ok)
	}
	nested, ok := agentRowByToken(listing.Rows, pair.grand)
	if !ok {
		t.Fatalf("the child's own spawn is not nested: %+v", listing.Rows)
	}
	if nested.Depth != 1 || nested.ComputerID != pair.computer || nested.Computer != parent.Computer ||
		nested.OwnerComputerID != pair.computer || nested.CallerThreadID != pair.child {
		t.Fatalf("the nested row = %+v, want depth 1 on %s (%s) made by %s", nested, pair.computer, parent.Computer, pair.child)
	}
	child, err := pair.dest.store.GetThread(pair.child)
	if err != nil || child.Provider == "" || parent.Provider != child.Provider {
		t.Fatalf("the spawn row's provider = %q, want the child's %q (%v)", parent.Provider, child.Provider, err)
	}
}

// Stop on the spawn's row runs the cascade there: the child's own spawn is
// cancelled as a user Stop, not only the request and the turn, with its
// wake disarmed, since the child was stopped with it; the pressed row's own
// wake stays armed.
func TestThreadAgentStopCascadesOnAPairedComputer(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })

	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if stop.Outcome != threadAgentStopped || !stop.Interrupted || stop.Nested < 1 || len(stop.Failures) != 0 {
		t.Fatalf("stop = %+v, want stopped and interrupted with the nested spawn and no failures", stop)
	}
	var grand store.ThreadRequest
	waitUntil(t, 10*time.Second, func() bool {
		grand, _, _ = pair.dest.store.GetThreadRequest(pair.grand)
		return threadRequestSettled(grand)
	})
	if grand.State != store.ThreadRequestCancelled || !strings.Contains(string(grand.Answer), "The user stopped it") || grand.Notify {
		t.Fatalf("the child's spawn = %s %q notify %v, want cancelled by the user with its wake disarmed", grand.State, grand.Answer, grand.Notify)
	}
	if spawn, _, err := pair.source.store.GetThreadRequest(pair.spawn.Token); err != nil || !spawn.Notify {
		t.Fatalf("the stopped spawn's wake = %v, %v; a person's Stop keeps it armed", spawn.Notify, err)
	}
}

// Stop on a nested row stops that row alone, through the computer that
// holds it.
func TestThreadAgentStopOfANestedRemoteRowStopsItThere(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		_, ok := agentRowByToken(rows.Rows, pair.grand)
		return ok
	})

	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.grand)
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if stop.Token != pair.grand || stop.Outcome != threadAgentStopped {
		t.Fatalf("stop = %+v, want the nested row stopped", stop)
	}
	waitUntil(t, 10*time.Second, func() bool {
		grand, _, _ := pair.dest.store.GetThreadRequest(pair.grand)
		return grand.State == store.ThreadRequestCancelled
	})
	if row := pair.request(t, pair.spawn.Token); threadRequestSettled(row) {
		t.Fatalf("stopping the nested row settled its parent: %s", row.State)
	}
}

// Both calls answer only for a thread the asking device's thread spawned
// there or reached through that computer's own lineage.
func TestThreadAgentPeerCallsRefuseOtherThreads(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	unrelated := reachThread(t, pair.dest, uuid.NewString(), "Someone else's")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	call := func(manager *attachedbackends.Manager, computer, method, tool, threadID string) error {
		var reply ThreadPeerReply
		return manager.CallThreadPeer(ctx, computer, method, &reply, ThreadPeerCall{
			Tool: tool, Token: newThreadRequestToken(),
			Source: threadtools.Caller{ThreadID: pair.caller.ID, ComputerID: pair.sourceID},
			Args:   json.RawMessage(fmt.Sprintf(`{"thread_id":%q}`, threadID)),
		})
	}
	for _, method := range []struct{ name, tool string }{
		{"ThreadToolQuery", threadAgentPeerRows}, {"ThreadToolCall", threadAgentPeerStop},
	} {
		if code := peerCode(t, call(pair.manager, pair.computer, method.name, method.tool, unrelated.ID)); code != threadtools.CodeNotYours {
			t.Fatalf("%s for an unrelated thread = %q, want %q", method.tool, code, threadtools.CodeNotYours)
		}
		second := pair.pairWith(t, "second device "+method.tool)
		if code := peerCode(t, call(second, pair.attachedID(t, second), method.name, method.tool, pair.child)); code != threadtools.CodeNotYours {
			t.Fatalf("%s from another device = %q, want %q", method.tool, code, threadtools.CodeNotYours)
		}
	}
	// A sibling thread on the asking device does not reach the caller's
	// child, and the caller does not reach a grandchild an unrelated thread
	// spawned there.
	sibling := reachThread(t, pair.source, uuid.NewString(), "Sibling on source")
	destID, _ := pair.dest.backendIdentity()
	other := reachThread(t, pair.dest, uuid.NewString(), "Unrelated parent")
	foreign, err := threadToolsApp{app: pair.dest}.Spawn(t.Context(),
		threadtools.Caller{ThreadID: other.ID, ComputerID: destID, ComputerName: "destination"},
		threadtools.SpawnCall{Prompt: "elsewhere", Title: "Other grandchild", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	foreignThread := pair.receipt(t, foreign.Token).TargetThreadID
	callFrom := func(source, method, tool, threadID string) error {
		var reply ThreadPeerReply
		return pair.manager.CallThreadPeer(ctx, pair.computer, method, &reply, ThreadPeerCall{
			Tool: tool, Token: newThreadRequestToken(),
			Source: threadtools.Caller{ThreadID: source, ComputerID: pair.sourceID},
			Args:   json.RawMessage(fmt.Sprintf(`{"thread_id":%q}`, threadID)),
		})
	}
	for _, method := range []struct{ name, tool string }{
		{"ThreadToolQuery", threadAgentPeerRows}, {"ThreadToolCall", threadAgentPeerStop},
	} {
		if code := peerCode(t, callFrom(sibling.ID, method.name, method.tool, pair.child)); code != threadtools.CodeNotYours {
			t.Fatalf("%s from a sibling on the child = %q, want %q", method.tool, code, threadtools.CodeNotYours)
		}
		if code := peerCode(t, callFrom(pair.caller.ID, method.name, method.tool, foreignThread)); code != threadtools.CodeNotYours {
			t.Fatalf("%s on an unrelated parent's grandchild = %q, want %q", method.tool, code, threadtools.CodeNotYours)
		}
	}
	// The caller's own grandchild, through its child there, is reachable.
	if err := callFrom(pair.caller.ID, "ThreadToolQuery", threadAgentPeerRows, pair.receipt(t, pair.grand).TargetThreadID); err != nil {
		t.Fatalf("the caller's own grandchild was refused: %v", err)
	}
	if grand, _, _ := pair.dest.store.GetThreadRequest(pair.grand); threadRequestSettled(grand) {
		t.Fatalf("a refused stop stopped the child's spawn: %s", grand.State)
	}
}

// stalledAgentsPeer is a destination whose agent_rows answer does not come.
// calls counts the agent_rows calls it was sent.
type stalledAgentsPeer struct {
	*App
	calls atomic.Int32
}

// serveStalledAgentsPeer serves app as a stalledAgentsPeer, which it
// returns through peer.
func serveStalledAgentsPeer(peer **stalledAgentsPeer) func(*testing.T, *App) *pairedBackend {
	return func(t *testing.T, app *App) *pairedBackend {
		stalled := &stalledAgentsPeer{App: app}
		if peer != nil {
			*peer = stalled
		}
		return servePairedApp(t, app, func(cfg *transport.Config) {
			dispatcher := transport.NewDispatcher()
			if _, err := dispatcher.Register(stalled, transport.RegisterOptions{
				Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
			}); err != nil {
				t.Fatal(err)
			}
			cfg.Dispatcher = dispatcher
		})
	}
}

func (p *stalledAgentsPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == threadAgentPeerRows {
		p.calls.Add(1)
		select {
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
		}
	}
	return p.App.ThreadToolQuery(ctx, call)
}

// A computer that does not answer holds up neither the pane's read nor a
// Stop: this computer's rows come back at once, the row says that computer
// did not answer once its call gives up, and a Stop works from the ledger.
func TestThreadAgentRowsDoNotWaitOnAComputerThatDoesNotAnswer(t *testing.T) {
	// Serial: it times a read to 100 ms, which parallel tests would load.
	pair := newAgentPeerPair(t, serveStalledAgentsPeer(nil))
	start := time.Now()
	listing := pair.listRows(t)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("ListThreadAgentRows took %v with one computer not answering", took)
	}
	if _, ok := agentRowByToken(listing.Rows, pair.spawn.Token); !ok {
		t.Fatalf("the spawn is not listed: %+v", listing.Rows)
	}
	listing = pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		parent, _ := agentRowByToken(rows.Rows, pair.spawn.Token)
		return parent.NestedUnavailable != ""
	})
	parent, _ := agentRowByToken(listing.Rows, pair.spawn.Token)
	if want := parent.Computer + " did not answer; its agents are not shown"; parent.NestedUnavailable != want {
		t.Fatalf("the spawn row says %q, want %q", parent.NestedUnavailable, want)
	}
	start = time.Now()
	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	if took := time.Since(start); took >= threadAgentPeerListTimeout {
		t.Fatalf("the Stop took %v: it waited on a listing", took)
	}
}

// olderAgentsPeer is a destination that predates the forwarded Agents
// calls: it answers them as its switch answers any unknown tool.
type olderAgentsPeer struct{ *App }

func (p *olderAgentsPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == threadAgentPeerRows {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest, call.Tool+" is not a thread tools read call.", nil)
	}
	return p.App.ThreadToolQuery(ctx, call)
}

func (p *olderAgentsPeer) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == threadAgentPeerStop {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest, call.Tool+" is not a thread tools write call.", nil)
	}
	// It predates the user Stop too, and words every cancel as the sender's.
	call.UserStop = false
	return p.App.ThreadToolCall(ctx, call)
}

func serveOlderAgentsPeer(t *testing.T, app *App) *pairedBackend {
	t.Helper()
	return servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(&olderAgentsPeer{App: app}, transport.RegisterOptions{
			Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
		}); err != nil {
			t.Fatalf("register the older destination: %v", err)
		}
		cfg.Dispatcher = dispatcher
	})
}

// An older computer degrades to the request and the turn, and says so on
// the row and in the Stop's report; the caller's wake still says the user
// stopped it, whatever that computer wrote.
func TestThreadAgentRowsSayWhenAPairedComputerIsOlder(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, serveOlderAgentsPeer)

	listing := pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		parent, _ := agentRowByToken(rows.Rows, pair.spawn.Token)
		return parent.NestedUnavailable != ""
	})
	parent, _ := agentRowByToken(listing.Rows, pair.spawn.Token)
	if want := parent.Computer + " runs an older version: its agents are not shown or stopped from here"; parent.NestedUnavailable != want {
		t.Fatalf("the spawn row says %q, want %q", parent.NestedUnavailable, want)
	}
	if _, nested := agentRowByToken(listing.Rows, pair.grand); nested {
		t.Fatal("an older computer's nested row is listed")
	}

	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, pair.spawn.Token)
	if err != nil {
		t.Fatalf("StopThreadAgentRow: %v", err)
	}
	if stop.Outcome != threadAgentStopped || len(stop.Failures) != 1 || !strings.Contains(stop.Failures[0], errThreadAgentPeerOlder.Error()) {
		t.Fatalf("stop = %+v, want the request stopped and the older computer named", stop)
	}
	if row := pair.request(t, pair.spawn.Token); row.State != store.ThreadRequestCancelled ||
		!strings.Contains(string(row.Answer), threadRequestUserStop) || !row.Notify {
		t.Fatalf("the spawn is %s %q notify %v, want cancelled by the user with its wake armed", row.State, row.Answer, row.Notify)
	}
}

// Nothing here learns of a change on the paired computer by itself, so each
// poll of an open remote spawn asks for its rows again, and an answer that
// changed what the pane shows emits a frame for the caller.
func TestThreadAgentRowsReadAgainOnEachPollOfARemoteSpawn(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	frames := make(chan ThreadAgentRowChange, 16)
	pair.source.testEmitHook = func(name string, data any) {
		if change, ok := data.(ThreadAgentRowChange); ok && name == string(eventchan.ThreadAgentRowsChanged) {
			frames <- change
		}
	}
	pair.poll(t)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case change := <-frames:
			if change.ThreadID == pair.caller.ID && change.ID == "" {
				return
			}
		case <-deadline:
			t.Fatal("a poll of the remote spawn did not have its caller's rows read again")
		}
	}
}

// A remote spawn that settled while work under it there is still open
// stays listed past the settled window, and the sweep asks that computer
// again until it lists none.
func TestThreadAgentRowsKeepASettledRemoteSpawnWhileItsWorkRuns(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		_, ok := agentRowByToken(rows.Rows, pair.grand)
		return ok
	})
	if !pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("an answer listing open work did not record it on the spawn")
	}
	long := time.Now().Add(-time.Hour).UnixMilli()
	settle := func(app *App, token string) {
		if settled, err := app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: long,
		}); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", token, settled, err)
		}
	}
	settle(pair.source, pair.spawn.Token)
	if _, ok := agentRowByToken(pair.listRows(t).Rows, pair.spawn.Token); !ok {
		t.Fatal("a settled remote spawn with open work under it left the listing")
	}

	settle(pair.dest, pair.grand)
	waitUntil(t, 10*time.Second, func() bool {
		// The answer is as old as the sweep's pace allows.
		cache := &pair.source.threadAgentPeers
		cache.mu.Lock()
		cache.entries[pair.spawn.Token].at = time.Time{}
		cache.mu.Unlock()
		pair.source.refreshNestedOpenRemoteSpawns()
		pair.source.threadRequestsWG.Wait()
		return !pair.request(t, pair.spawn.Token).NestedOpen
	})
	if _, ok := agentRowByToken(pair.listRows(t).Rows, pair.spawn.Token); ok {
		t.Fatal("the spawn is still listed once its computer lists no open work")
	}
}

// The sweep asks about the settled remote spawns with open work a page at
// a time, starting each sweep after the last one it asked about, so the
// spawns past the first page are asked too.
func TestThreadAgentSweepPagesThroughRemoteSpawnsWithOpenWork(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	f := pair.source
	tokens := make([]string, threadRequestPollBatch+1)
	for index := range tokens {
		tokens[index] = fmt.Sprintf("remote-%02d", index)
		if err := f.store.InsertThreadRequest(store.ThreadRequest{
			Token: tokens[index], CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSpawn,
			TargetComputerID: pair.computer, TargetThreadID: "there", State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
		if settled, err := f.store.SettleThreadRequest(tokens[index], store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: int64(1000 + index),
		}); err != nil || !settled {
			t.Fatalf("settle: %v %v", settled, err)
		}
		if _, err := f.store.SetThreadRequestNestedOpen(tokens[index], true); err != nil {
			t.Fatal(err)
		}
	}
	asked := func(token string) bool {
		cache := &f.threadAgentPeers
		cache.mu.Lock()
		defer cache.mu.Unlock()
		_, ok := cache.entries[token]
		return ok
	}
	f.refreshNestedOpenRemoteSpawns()
	f.threadRequestsWG.Wait()
	last := tokens[len(tokens)-1]
	if !asked(tokens[0]) || asked(last) {
		t.Fatalf("the first sweep asked the first page: first %v, last %v", asked(tokens[0]), asked(last))
	}
	f.refreshNestedOpenRemoteSpawns()
	f.threadRequestsWG.Wait()
	if !asked(last) {
		t.Fatal("the second sweep did not reach the spawn past the first page")
	}
}

// waitForPeerAnswer waits until the spawn's answer is cached.
func waitForPeerAnswer(t *testing.T, app *App, spawn store.ThreadRequest, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		if _, ok := app.cachedPeerAgentRows(spawn); ok {
			return time.Since(start)
		}
		if time.Since(start) > within {
			t.Fatalf("no answer for %s after %v", spawn.Token, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A computer that does not answer holds two of the calls, not all of them:
// another computer's spawn is asked at once beside nine stalled ones. Once
// the stalled computer's calls time out, it is not asked again until its
// backoff passes.
func TestThreadAgentPeerRefreshesDoNotQueueBehindAStalledComputer(t *testing.T) {
	// Serial: it times a read to 100 ms, which parallel tests would load.
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveStalledAgentsPeer(&peer))
	a := pair.source
	stalled := make([]store.ThreadRequest, 9)
	for i := range stalled {
		stalled[i] = pair.spawn
		stalled[i].Token = fmt.Sprintf("stalled-%d", i)
		a.refreshPeerAgentRows(stalled[i], 0)
	}
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == threadAgentPeerListsPerComputer })
	other := pair.spawn
	other.Token, other.TargetComputerID = "other-computer", uuid.NewString()
	a.refreshPeerAgentRows(other, 0)
	if took := waitForPeerAnswer(t, a, other, 5*time.Second); took > 100*time.Millisecond {
		t.Fatalf("another computer's answer took %v behind a stalled computer", took)
	}

	// The two calls time out; the computer backs off, so the seven still
	// waiting are dropped and no refresh asks it again.
	waitForPeerAnswer(t, a, stalled[0], 2*threadAgentPeerListTimeout)
	a.threadRequestsWG.Wait()
	asked := peer.calls.Load()
	for _, spawn := range stalled {
		a.refreshPeerAgentRows(spawn, 0)
	}
	a.threadRequestsWG.Wait()
	if got := peer.calls.Load(); got != asked {
		t.Fatalf("a computer backing off was asked %d more times", got-asked)
	}
	a.threadAgentPeers.mu.Lock()
	backoff := a.threadAgentPeers.backoff[pair.computer]
	a.threadAgentPeers.mu.Unlock()
	if until := time.Until(backoff.until); until <= 0 || until > threadAgentPeerBackoffFirst {
		t.Fatalf("backoff ends in %v; want within %v", until, threadAgentPeerBackoffFirst)
	}
}

// A timeout lengthens a computer's backoff, doubling to its cap, and an
// answer clears it.
func TestThreadAgentPeerBackoffDoublesAndAnAnswerClearsIt(t *testing.T) {
	t.Parallel()
	var cache threadAgentPeerCache
	spawn := store.ThreadRequest{Token: "spawn", TargetComputerID: "computer"}
	timedOut := fmt.Errorf("%w: no answer", errThreadAgentPeerTimedOut)
	var waits []time.Duration
	for range 6 {
		before := time.Now()
		cache.noteAnswerLocked(spawn, ThreadAgentRows{}, timedOut)
		backoff := cache.backoff["computer"]
		waits = append(waits, backoff.until.Sub(before).Round(time.Second))
		// A second call timing out within the backoff is the same outage.
		cache.noteAnswerLocked(spawn, ThreadAgentRows{}, timedOut)
		if cache.backoff["computer"] != backoff {
			t.Fatal("a timeout inside the backoff lengthened it")
		}
		// The backoff passes; the next timeout is a new one.
		backoff.until = before
		cache.backoff["computer"] = backoff
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	if !slices.Equal(waits, want) {
		t.Fatalf("backoffs %v; want %v", waits, want)
	}
	cache.noteAnswerLocked(spawn, ThreadAgentRows{}, timedOut)
	cache.noteAnswerLocked(spawn, ThreadAgentRows{}, errors.New("refused"))
	if !cache.backingOffLocked("computer") {
		t.Fatal("a refusal cleared the backoff")
	}
	cache.noteAnswerLocked(spawn, ThreadAgentRows{}, nil)
	if cache.backingOffLocked("computer") {
		t.Fatal("an answer left the computer backing off")
	}
}

// However many spawns are refreshed together, the calls in flight, the
// spawns waiting and the answers kept stay bounded, and a spawn already
// being asked about still gets its answer.
func TestThreadAgentPeerRefreshesStayBounded(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, serveStalledAgentsPeer(nil))
	a := pair.source
	live := pair.spawn
	a.refreshPeerAgentRows(live, 0)
	for i := range 300 {
		spawn := pair.spawn
		spawn.Token, spawn.TargetComputerID = fmt.Sprintf("gone-%d", i), uuid.NewString()
		a.refreshPeerAgentRows(spawn, 0)
		a.threadAgentPeers.mu.Lock()
		inFlight, waiting := a.threadAgentPeers.inFlight, len(a.threadAgentPeers.waiting)
		a.threadAgentPeers.mu.Unlock()
		if inFlight > threadAgentPeerListsInFlight || waiting > threadAgentPeerCacheLimit {
			t.Fatalf("after %d spawns: %d calls in flight, %d waiting", i+1, inFlight, waiting)
		}
	}
	waitForPeerAnswer(t, a, live, 2*threadAgentPeerListTimeout)
	a.threadRequestsWG.Wait()
	a.threadAgentPeers.mu.Lock()
	entries := len(a.threadAgentPeers.entries)
	a.threadAgentPeers.mu.Unlock()
	if entries > threadAgentPeerCacheLimit {
		t.Fatalf("the cache holds %d answers; bound %d", entries, threadAgentPeerCacheLimit)
	}
}

// Forgetting a computer forgets its spawns' open work: a settled spawn
// whose computer last listed open work leaves the listing and the sweep.
func TestThreadAgentRowsForgetAForgottenComputersOpenWork(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	a := pair.source
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		_, ok := agentRowByToken(rows.Rows, pair.grand)
		return ok
	})
	if settled, err := a.store.SettleThreadRequest(pair.spawn.Token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal,
		SettledAt: time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	if !pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("the spawn does not record open work there")
	}
	if err := a.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	if pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("a forgotten computer's spawn still records open work there")
	}
	if schedule, err := a.store.NextThreadRequestWork(); err != nil || schedule.NestedOpen {
		t.Fatalf("schedule = %+v, %v; the sweep still asks a forgotten computer", schedule, err)
	}
	if _, listed := agentRowByToken(pair.listRows(t).Rows, pair.spawn.Token); listed {
		t.Fatal("a forgotten computer's settled spawn is still listed")
	}
	if _, cached := a.cachedPeerAgentRows(pair.spawn); cached {
		t.Fatal("a forgotten computer's answer is still cached")
	}
}

// A send to a thread on another computer shows that thread's provider, from
// the acknowledgement: no agent_rows call is made for a send.
func TestThreadAgentRowsShowARemoteSendsProvider(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	target := reachThread(t, pair.dest, uuid.NewString(), "Remote peer")
	ack, err := pair.adapter().Send(t.Context(), pair.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, ComputerID: pair.computer, Message: "check the changelog", Notify: true,
	})
	if err != nil {
		t.Fatalf("remote Send: %v", err)
	}
	listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := agentRowByToken(listing.Rows, ack.Token)
	if !ok || row.Kind != store.ThreadRequestSend || row.Provider != target.Provider {
		t.Fatalf("send row = %+v (listed %v); want the target's provider %q", row, ok, target.Provider)
	}
}
