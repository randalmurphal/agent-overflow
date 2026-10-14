package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// stalledAgentsPeer is a destination whose agent_rows answer comes after
// delay, or not at all when delay is zero. calls counts the agent_rows
// calls it was sent.
type stalledAgentsPeer struct {
	*App
	calls    atomic.Int32
	answered atomic.Int32
	delay    time.Duration
}

// serveStalledAgentsPeer serves app as a stalledAgentsPeer that never
// answers, which it returns through peer.
func serveStalledAgentsPeer(peer **stalledAgentsPeer) func(*testing.T, *App) *pairedBackend {
	return serveSlowAgentsPeer(peer, 0)
}

// serveSlowAgentsPeer serves app as a stalledAgentsPeer answering after
// delay.
func serveSlowAgentsPeer(peer **stalledAgentsPeer, delay time.Duration) func(*testing.T, *App) *pairedBackend {
	return func(t *testing.T, app *App) *pairedBackend {
		stalled := &stalledAgentsPeer{App: app, delay: delay}
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
		wait := p.delay
		if wait == 0 {
			wait = 20 * time.Second
		}
		select {
		case <-time.After(wait):
			defer p.answered.Add(1)
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
	// Serial: it counts the calls sent within a fixed 200 ms window.
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveStalledAgentsPeer(&peer))
	a := pair.source
	stalled := make([]store.ThreadRequest, 9)
	for i := range stalled {
		stalled[i] = pair.spawn
		stalled[i].Token = fmt.Sprintf("stalled-%d", i)
		a.refreshPeerAgentRows(stalled[i], 0)
	}
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() >= threadAgentPeerListsPerComputer })
	time.Sleep(200 * time.Millisecond)
	if calls := peer.calls.Load(); calls != threadAgentPeerListsPerComputer {
		t.Fatalf("one computer was sent %d calls at once; want %d", calls, threadAgentPeerListsPerComputer)
	}
	other := pair.spawn
	other.Token, other.TargetComputerID = "other-computer", uuid.NewString()
	// Only the queue's bounds are under test: no second computer is paired.
	a.refreshPairedPeerAgentRows(other, 0)
	a.threadAgentPeers.mu.Lock()
	waiting := a.threadAgentPeers.queued[other.Token]
	a.threadAgentPeers.mu.Unlock()
	if waiting {
		t.Fatal("another computer's spawn waits behind a stalled computer")
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

// However many spawns are refreshed together, the calls in flight, those
// to one computer, the spawns waiting and the answers kept stay bounded; a
// spawn dropped from the full queue has its caller told to read again, and
// the read that follows gets its answer.
func TestThreadAgentPeerRefreshesStayBounded(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(new(*stalledAgentsPeer), 30*time.Millisecond))
	a := pair.source
	frames := recordAgentRowFrames(a)
	check := func(when string) {
		c := &a.threadAgentPeers
		c.mu.Lock()
		defer c.mu.Unlock()
		perComputer := 0
		for _, n := range c.perComputer {
			perComputer = max(perComputer, n)
		}
		if c.inFlight > threadAgentPeerListsInFlight || perComputer > threadAgentPeerListsPerComputer ||
			len(c.waiting) > threadAgentPeerCacheLimit || len(c.entries) > threadAgentPeerCacheLimit {
			t.Fatalf("%s: %d in flight, %d to one computer, %d waiting, %d answers",
				when, c.inFlight, perComputer, len(c.waiting), len(c.entries))
		}
	}
	spawns := make([]store.ThreadRequest, threadAgentPeerCacheLimit+45)
	for i := range spawns {
		spawns[i] = pair.spawn
		if i > 0 {
			spawns[i].Token, spawns[i].CallerThreadID = fmt.Sprintf("many-%03d", i), fmt.Sprintf("caller-%03d", i)
		}
		a.refreshPeerAgentRows(spawns[i], 0)
		check(fmt.Sprintf("after %d spawns", i+1))
	}
	// The first spawns run at once; the oldest still waiting is dropped.
	dropped := spawns[threadAgentPeerListsPerComputer+1]
	waitForPeerAnswer(t, a, spawns[0], 10*time.Second)
	waitUntil(t, 60*time.Second, func() bool {
		check("draining")
		a.threadAgentPeers.mu.Lock()
		defer a.threadAgentPeers.mu.Unlock()
		return len(a.threadAgentPeers.waiting) == 0 && a.threadAgentPeers.inFlight == 0
	})
	a.threadRequestsWG.Wait()
	check("drained")
	waitUntil(t, 5*time.Second, func() bool { return frames.count(dropped.CallerThreadID) > 0 })
	a.refreshPeerAgentRows(dropped, threadAgentPeerRefreshEvery)
	waitForPeerAnswer(t, a, dropped, 10*time.Second)
}

// agentRowFrames counts the thread:agent_rows_changed frames each thread
// is sent.
type agentRowFrames struct {
	mu  sync.Mutex
	got map[string]int
}

func recordAgentRowFrames(app *App) *agentRowFrames {
	frames := &agentRowFrames{got: map[string]int{}}
	app.testEmitHook = func(name string, data any) {
		if change, ok := data.(ThreadAgentRowChange); ok && name == string(eventchan.ThreadAgentRowsChanged) && change.ThreadID != "" {
			frames.mu.Lock()
			frames.got[change.ThreadID]++
			frames.mu.Unlock()
		}
	}
	return frames
}

func (f *agentRowFrames) count(threadID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[threadID]
}

// settleWithOpenWorkThere leaves the pair's spawn settled an hour ago with
// open work recorded there and its answer cached, every background call
// done, and returns a recorder of the frames sent from then on.
func settleWithOpenWorkThere(t *testing.T, pair *agentPeerPair) *agentRowFrames {
	t.Helper()
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
	// The emit drain joins the same group: no frame from before is left.
	a.threadRequestsWG.Wait()
	if !pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("the spawn does not record open work there")
	}
	if _, cached := a.cachedPeerAgentRows(pair.spawn); !cached {
		t.Fatal("the spawn's answer is not cached")
	}
	return recordAgentRowFrames(a)
}

// requireForgotten checks a computer's open work is gone from the spawn:
// the row, the sweep, the listing and the cache, with the caller told.
func requireForgotten(t *testing.T, pair *agentPeerPair, frames *agentRowFrames) {
	t.Helper()
	a := pair.source
	if pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("a forgotten computer's spawn still records open work there")
	}
	if schedule, err := a.store.NextThreadRequestWork(); err != nil || schedule.NestedOpen {
		t.Fatalf("schedule = %+v, %v; the sweep still asks a forgotten computer", schedule, err)
	}
	if _, cached := a.cachedPeerAgentRows(pair.spawn); cached {
		t.Fatal("a forgotten computer's answer is still cached")
	}
	a.threadAgentPeers.mu.Lock()
	_, backingOff := a.threadAgentPeers.backoff[pair.computer]
	a.threadAgentPeers.mu.Unlock()
	if backingOff {
		t.Fatal("a forgotten computer's backoff is kept")
	}
	waitUntil(t, 5*time.Second, func() bool { return frames.count(pair.caller.ID) > 0 })
	if _, listed := agentRowByToken(pair.listRows(t).Rows, pair.spawn.Token); listed {
		t.Fatal("a forgotten computer's settled spawn is still listed")
	}
}

// Forgetting a computer forgets its spawns' open work: a settled spawn
// whose computer last listed open work leaves the listing and the sweep,
// and its caller's pane is told to read again.
func TestThreadAgentRowsForgetAForgottenComputersOpenWork(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	frames := settleWithOpenWorkThere(t, pair)
	pair.source.threadAgentPeers.mu.Lock()
	pair.source.threadAgentPeers.backoff[pair.computer] = threadAgentPeerBackoff{until: time.Now().Add(time.Minute)}
	pair.source.threadAgentPeers.mu.Unlock()
	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	requireForgotten(t, pair, frames)
}

// A computer gone from the paired set without a removal this computer
// heard is forgotten by the next sweep.
func TestThreadAgentSweepForgetsAComputerNoLongerPaired(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	frames := settleWithOpenWorkThere(t, pair)
	pair.manager.SetChanged(func(attachedbackends.SetChange) {})
	if err := pair.manager.Remove(pair.computer); err != nil {
		t.Fatal(err)
	}
	pair.manager.SetChanged(pair.source.backendSetChanged)
	pair.source.refreshNestedOpenRemoteSpawns()
	pair.source.threadRequestsWG.Wait()
	requireForgotten(t, pair, frames)
}

// A membership change names no machine: every computer the cache knows
// that is no longer paired is forgotten.
func TestThreadAgentMembershipChangeForgetsComputersNoLongerPaired(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	frames := settleWithOpenWorkThere(t, pair)
	pair.manager.SetChanged(func(attachedbackends.SetChange) {})
	if err := pair.manager.Remove(pair.computer); err != nil {
		t.Fatal(err)
	}
	pair.manager.SetChanged(pair.source.backendSetChanged)
	pair.source.backendSetChanged(attachedbackends.SetChange{Action: attachedbackends.SetMembership})
	requireForgotten(t, pair, frames)
}

// A refresh asked for while the spawn's own call is running reads again
// once that call returns: a change may have landed after it read the rows.
func TestThreadAgentPeerRefreshAskedDuringACallReadsAgain(t *testing.T) {
	t.Parallel()
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, 400*time.Millisecond))
	a := pair.source
	a.threadRequestsWG.Wait()
	before := peer.calls.Load()
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+1 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+2 })
	a.threadRequestsWG.Wait()
	if got := peer.calls.Load() - before; got != 2 {
		t.Fatalf("one refresh and two asked during its call made %d calls; want 2", got)
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

// A computer forgotten while a spawn's call to it runs, with a read again
// asked for, is not asked again and keeps nothing: the call's answer is
// not recorded, and no mark is left.
func TestThreadAgentRowsForgetDuringACallKeepsNothing(t *testing.T) {
	t.Parallel()
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, time.Second))
	a := pair.source
	frames := settleWithOpenWorkThere(t, pair)
	before := peer.calls.Load()
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+1 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	if err := a.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	a.threadRequestsWG.Wait()
	if got := peer.calls.Load() - before; got != 1 {
		t.Fatalf("calls after the forget = %d, want the one already running", got)
	}
	requireForgotten(t, pair, frames)
	c := &a.threadAgentPeers
	c.mu.Lock()
	state := [4]int{len(c.waiting), len(c.queued), len(c.running), len(c.again)}
	c.mu.Unlock()
	if state != [4]int{} {
		t.Fatalf("waiting/queued/running/again = %v after the forget, want none", state)
	}
}

// An answer that lands after its computer was forgotten is not recorded:
// it neither marks open work there again nor is cached.
func TestThreadAgentRowsAnswerAfterTheForgetIsNotRecorded(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	a := pair.source
	frames := settleWithOpenWorkThere(t, pair)
	answer, cached := a.cachedPeerAgentRows(pair.spawn)
	if !cached || answer.err != nil {
		t.Fatalf("cached answer = %+v (%v), want the open rows", answer, cached)
	}
	if err := a.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	a.notePeerAgentRows(pair.spawn, answer.listing, nil)
	a.threadRequestsWG.Wait()
	requireForgotten(t, pair, frames)
}

// A refresh asked for during the read again marks that one too: three
// calls in all, and no more.
func TestThreadAgentPeerRefreshDuringTheReReadReadsOnceMore(t *testing.T) {
	t.Parallel()
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, 400*time.Millisecond))
	a := pair.source
	a.threadRequestsWG.Wait()
	before := peer.calls.Load()
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+1 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+2 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+3 })
	a.threadRequestsWG.Wait()
	if got := peer.calls.Load() - before; got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

// requireNothingQueued checks the cache asks nothing more: no spawn
// waiting, queued, running or marked, and no call in flight.
func requireNothingQueued(t *testing.T, a *App) {
	t.Helper()
	c := &a.threadAgentPeers
	c.mu.Lock()
	state := [5]int{len(c.waiting), len(c.queued), len(c.running), len(c.again), c.inFlight}
	c.mu.Unlock()
	if state != [5]int{} {
		t.Fatalf("waiting/queued/running/again/inFlight = %v, want none", state)
	}
}

// Shutdown during a call with a read again marked ends the call, asks
// nothing more and leaves no goroutine.
func TestThreadAgentPeerShutdownDuringACallLeavesNothing(t *testing.T) {
	t.Parallel()
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, 10*time.Second))
	a := pair.source
	a.threadRequestsWG.Wait()
	before := peer.calls.Load()
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+1 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	a.threadAgentPeers.mu.Lock()
	_, marked := a.threadAgentPeers.again[pair.spawn.Token]
	a.threadAgentPeers.mu.Unlock()
	if !marked {
		t.Fatal("the refresh during the call marked no read again")
	}
	a.appCancel()
	joined := make(chan struct{})
	go func() { a.threadRequestsWG.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("a call outlived shutdown")
	}
	requireNothingQueued(t, a)
	if got := peer.calls.Load() - before; got != 1 {
		t.Fatalf("calls = %d, want the one shutdown ended", got)
	}
}

// A call with a read again marked that answers once shutdown has begun is
// recorded but not asked again.
func TestThreadAgentPeerAnswerDuringShutdownIsNotAskedAgain(t *testing.T) {
	// Serial: the answer must reach this computer within a fixed window
	// before shutdown begins.
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, 300*time.Millisecond))
	a := pair.source
	a.threadRequestsWG.Wait()
	a.threadAgentPeers.mu.Lock()
	delete(a.threadAgentPeers.entries, pair.spawn.Token)
	a.threadAgentPeers.mu.Unlock()
	before, answered := peer.calls.Load(), peer.answered.Load()
	// Held, the call's answer waits to be recorded until shutdown began.
	a.threadAgentPeers.recording.Lock()
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.calls.Load() == before+1 })
	a.refreshPeerAgentRows(pair.spawn, 0)
	waitUntil(t, 5*time.Second, func() bool { return peer.answered.Load() == answered+1 })
	time.Sleep(200 * time.Millisecond)
	a.appCancel()
	a.threadAgentPeers.recording.Unlock()
	a.threadRequestsWG.Wait()
	if answer, cached := a.cachedPeerAgentRows(pair.spawn); !cached || answer.err != nil {
		t.Fatalf("cached answer = %+v (%v); the call did not answer before shutdown", answer, cached)
	}
	requireNothingQueued(t, a)
	if got := peer.calls.Load() - before; got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

// A forget that lands while an answer is being recorded waits for it, so it
// clears what that answer wrote.
func TestThreadAgentRowsForgetWaitsForAnAnswerBeingRecorded(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	a := pair.source
	settleWithOpenWorkThere(t, pair)
	a.threadAgentPeers.recording.Lock()
	forgot := make(chan struct{})
	go func() { a.forgetPeerAgentRows(pair.computer); close(forgot) }()
	select {
	case <-forgot:
		a.threadAgentPeers.recording.Unlock()
		t.Fatal("the forget ran while an answer was being recorded")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := a.store.SetThreadRequestNestedOpen(pair.spawn.Token, true); err != nil {
		t.Fatal(err)
	}
	a.threadAgentPeers.recording.Unlock()
	<-forgot
	if pair.request(t, pair.spawn.Token).NestedOpen {
		t.Fatal("an answer recorded during the forget left open work there")
	}
}

// A spawn on a computer no longer paired is asked nothing: each read of its
// row says so, queues no call and keeps no answer.
func TestThreadAgentRowsAskNothingOfAComputerNoLongerPaired(t *testing.T) {
	t.Parallel()
	var peer *stalledAgentsPeer
	pair := newAgentPeerPair(t, serveSlowAgentsPeer(&peer, 10*time.Millisecond))
	a := pair.source
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		_, ok := agentRowByToken(rows.Rows, pair.grand)
		return ok
	})
	if err := a.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	a.threadRequestsWG.Wait()
	calls := peer.calls.Load()
	for read := range 3 {
		listing := pair.listRows(t)
		row, ok := agentRowByToken(listing.Rows, pair.spawn.Token)
		if !ok {
			t.Fatalf("read %d: the spawn is not listed: %+v", read, listing.Rows)
		}
		if want := "That computer is no longer paired"; row.NestedUnavailable != want {
			t.Fatalf("read %d: the row says %q, want %q", read, row.NestedUnavailable, want)
		}
		if _, nested := agentRowByToken(listing.Rows, pair.grand); nested {
			t.Fatalf("read %d: the forgotten computer's rows are still nested", read)
		}
		requireNothingQueued(t, a)
		if _, cached := a.cachedPeerAgentRows(pair.spawn); cached {
			t.Fatalf("read %d: an answer is cached for a computer no longer paired", read)
		}
	}
	// A poll's refresh asks nothing either. Held, a call it started could
	// not finish before the check.
	a.threadAgentPeers.recording.Lock()
	a.refreshPeerAgentRows(pair.spawn, 0)
	a.threadAgentPeers.mu.Lock()
	started := len(a.threadAgentPeers.running) + len(a.threadAgentPeers.waiting)
	a.threadAgentPeers.mu.Unlock()
	a.threadAgentPeers.recording.Unlock()
	if started != 0 {
		t.Fatalf("a poll's refresh queued %d calls to a computer no longer paired", started)
	}
	a.threadRequestsWG.Wait()
	if got := peer.calls.Load(); got != calls {
		t.Fatalf("the computer no longer paired was asked %d times", got-calls)
	}
}
