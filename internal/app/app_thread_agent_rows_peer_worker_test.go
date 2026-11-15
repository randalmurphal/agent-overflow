package app

// A spawn on a paired computer is open while the thread it made there is
// not at rest, wherever its row falls in a listing and whatever the last
// call to that computer came to (docs/specs/standing-agents.md, "Owned
// workers").

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// failingAgentsPeer is a destination whose agent_rows calls fail while
// fail is set, as a computer that drops off the network does.
type failingAgentsPeer struct {
	*App
	fail atomic.Bool
}

func (p *failingAgentsPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == threadAgentPeerRows && p.fail.Load() {
		return ThreadPeerReply{}, errors.New("connection reset")
	}
	return p.App.ThreadToolQuery(ctx, call)
}

func serveFailingAgentsPeer(peer **failingAgentsPeer) func(*testing.T, *App) *pairedBackend {
	return func(t *testing.T, app *App) *pairedBackend {
		failing := &failingAgentsPeer{App: app}
		*peer = failing
		return servePairedApp(t, app, func(cfg *transport.Config) {
			dispatcher := transport.NewDispatcher()
			if _, err := dispatcher.Register(failing, transport.RegisterOptions{
				Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
			}); err != nil {
				t.Fatal(err)
			}
			cfg.Dispatcher = dispatcher
		})
	}
}

// askAgainUntilRow asks the destination about the pair's spawn as the
// sweep does, with its cached answer aged out, until done holds of the
// spawn's row in its caller's listing past the settled window.
func (p *agentPeerPair) askAgainUntilRow(t *testing.T, done func(ThreadAgentRow, bool) bool) ThreadAgentRow {
	t.Helper()
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		cache := &p.source.threadAgentPeers
		cache.mu.Lock()
		if entry, ok := cache.entries[p.spawn.Token]; ok {
			entry.at = time.Time{}
		}
		delete(cache.backoff, p.spawn.TargetComputerID)
		cache.mu.Unlock()
		p.source.refreshNestedOpenRemoteSpawns()
		p.source.threadRequestsWG.Wait()
		view := newThreadAgentView(p.source)
		view.since = time.Now().Add(time.Second).UnixMilli()
		listing, _, err := p.source.threadAgentTreeDepth(t.Context(), view, p.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
		if err != nil {
			t.Fatal(err)
		}
		var listed bool
		row, listed = agentRowByToken(listing.Rows, p.spawn.Token)
		return done(row, listed)
	})
	return row
}

// expectRemoteRowOpen checks that the remote spawn's row is open with the
// worker state want, counted, and out of Past.
func (p *agentPeerPair) expectRemoteRowOpen(t *testing.T, row ThreadAgentRow, want string) {
	t.Helper()
	if !row.open() || row.Worker != want {
		t.Fatalf("row %+v; want it open, %s", row, want)
	}
	if got, err := p.source.GetThreadWorkerSummary(t.Context(), p.caller.ID); err != nil || got.Open != 1 {
		t.Fatalf("summary %+v, %v; want the remote worker counted", got, err)
	}
	if past, err := p.source.ListThreadPastAgentRows(p.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 0 {
		t.Fatalf("past = %+v, %v; want the remote worker kept out", past, err)
	}
}

// A failed read of a paired computer is no news of rest: a remote worker
// last reported running stays open, running, counted and out of Past, with
// the row saying that computer did not answer, past the settled window.
// With nothing remembered, as after a restart, its recorded open work
// keeps it open, waiting. Once that computer answers again the row
// follows it, and settles into Past when the thread there rests.
func TestAFailedReadOfAPairedComputerKeepsItsWorkerOpen(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var peer *failingAgentsPeer
	pair := newReachPairServedBy(t, serveFailingAgentsPeer(&peer))
	agentPair, child := remoteRunningWorker(t, pair)
	row := agentPair.askAgainUntilRow(t, func(row ThreadAgentRow, listed bool) bool { return listed && row.Worker == workerRunning })
	agentPair.expectRemoteRowOpen(t, row, workerRunning)

	peer.fail.Store(true)
	row = agentPair.askAgainUntilRow(t, func(row ThreadAgentRow, listed bool) bool { return listed && row.NestedUnavailable != "" })
	agentPair.expectRemoteRowOpen(t, row, workerRunning)
	if want := row.Computer + " did not answer; its agents are not shown"; row.NestedUnavailable != want {
		t.Fatalf("the row says %q, want %q", row.NestedUnavailable, want)
	}
	if !pair.request(t, agentPair.spawn.Token).NestedOpen {
		t.Fatal("a failed read cleared the recorded open work")
	}

	// Nothing remembered of that computer's answers, as after a restart.
	cache := &pair.source.threadAgentPeers
	cache.mu.Lock()
	clear(cache.entries)
	cache.mu.Unlock()
	view := newThreadAgentView(pair.source)
	view.since = time.Now().Add(time.Second).UnixMilli()
	listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, pair.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = agentRowByToken(listing.Rows, agentPair.spawn.Token)
	agentPair.expectRemoteRowOpen(t, row, workerWaiting)
	row = agentPair.askAgainUntilRow(t, func(row ThreadAgentRow, listed bool) bool { return listed && row.NestedUnavailable != "" })
	agentPair.expectRemoteRowOpen(t, row, workerWaiting)

	peer.fail.Store(false)
	row = agentPair.askAgainUntilRow(t, func(row ThreadAgentRow, listed bool) bool {
		return listed && row.NestedUnavailable == "" && row.Worker == workerRunning
	})
	agentPair.expectRemoteRowOpen(t, row, workerRunning)
	if err := pair.dest.StopSession(child); err != nil {
		t.Fatalf("StopSession there: %v", err)
	}
	if err := pair.source.StopSession(pair.caller.ID); err != nil {
		t.Fatalf("StopSession here: %v", err)
	}
	agentPair.askAgainUntilRow(t, func(_ ThreadAgentRow, listed bool) bool { return !listed })
	if past, err := pair.source.ListThreadPastAgentRows(pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("past = %+v, %v; want the rested worker's spawn", past, err)
	}
}

// A computer that refuses the thread as not this spawn's has told the
// truth about it: the last answer it gave is not kept as its worker state.
func TestARefusalForgetsTheLastWorkerState(t *testing.T) {
	t.Parallel()
	c := &threadAgentPeerCache{}
	spawn := store.ThreadRequest{Token: "gone", TargetComputerID: "elsewhere"}
	c.noteAnswerLocked(spawn, ThreadAgentRows{Worker: workerRunning}, nil)
	refused := errorsx.Public(threadtools.CodeNotYours, "That thread is not this spawn's.", nil)
	c.noteAnswerLocked(spawn, ThreadAgentRows{}, refused)
	if entry := c.entries[spawn.Token]; entry.listing.Worker != "" || entry.err == nil {
		t.Fatalf("entry %+v; want the refusal kept and no worker state", entry)
	}
}

// remoteRunningWorker spawns a thread on the pair's destination that
// replies and keeps its turn open, and waits until the source records it
// open there.
func remoteRunningWorker(t *testing.T, pair *reachPair) (*agentPeerPair, string) {
	t.Helper()
	holding := [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":"watching the deploy"}]}}`,
	}}
	installMockClaudeTurns(t, pair.dest, holding)
	// The caller here takes the reply as a wake.
	installMockClaudeTurns(t, pair.source, holding)
	ack := pair.spawnThere(t, "rerun the deploy, tell me when it starts, then watch it", map[string]any{"notify": true})
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := pair.dest.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
	destID, _ := pair.dest.backendIdentity()
	if _, err := (threadToolsApp{app: pair.dest}).Reply(t.Context(),
		threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"},
		threadtools.ReplyCall{Token: ack.Token, Text: "The deploy started; I am watching it."}); err != nil {
		t.Fatalf("Reply there: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	pair.poll(t)
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool {
		return row.State == store.ThreadRequestReplied && row.NestedOpen
	})
	agentPair.spawn = pair.request(t, ack.Token)
	return agentPair, child
}

// A remote worker at the last level a listing shows still has its own
// state there: three local spawns above the thread that made it, settled
// long ago, list it at depth 3, running, open and counted, with every link
// above it open, waiting, and out of Past. Once it rests, all of them
// settle.
func TestARemoteWorkerAtTheListingsLastLevelKeepsItsState(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	agentPair, child := remoteRunningWorker(t, pair)
	root, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	caller := root.ID
	links := []string{"root-a", "a-b", "b-caller"}
	for i, token := range links {
		target := pair.caller.ID
		if i < len(links)-1 {
			thread, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
			if err != nil {
				t.Fatal(err)
			}
			target = thread.ID
		}
		seedSettledAgentRow(t, pair.source, store.ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: store.ThreadRequestSpawn, TargetThreadID: target,
		}, time.Now().Add(-time.Hour).UnixMilli(), "done")
		caller = target
	}
	rootRows := func() []ThreadAgentRow {
		view := newThreadAgentView(pair.source)
		view.since = time.Now().Add(time.Second).UnixMilli()
		listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, root.ID, threadAgentRowDepth, threadAgentPeersCached)
		if err != nil {
			t.Fatal(err)
		}
		return listing.Rows
	}
	var rows []ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		rows = rootRows()
		return len(rows) == 4 && rows[3].Worker == workerRunning
	})
	remote := rows[3]
	if remote.Token != agentPair.spawn.Token || remote.Depth != 3 || !remote.open() || remote.ComputerID != pair.computer {
		t.Fatalf("rows %+v; want the remote worker at depth 3, open", rows)
	}
	for _, row := range rows[:3] {
		if !row.open() || row.Worker == "" {
			t.Fatalf("rows %+v; want every link above the remote worker open", rows)
		}
	}
	if got, err := pair.source.GetThreadWorkerSummary(t.Context(), root.ID); err != nil || got.Open != 4 {
		t.Fatalf("root's summary %+v, %v; want the three links and the remote worker counted", got, err)
	}
	for _, thread := range []string{root.ID, pair.caller.ID} {
		if past, err := pair.source.ListThreadPastAgentRows(thread, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 0 {
			t.Fatalf("past of %s = %+v, %v; want the open work kept out", thread, past, err)
		}
	}

	// Both threads rest: the one there, and the caller here.
	if err := pair.dest.StopSession(child); err != nil {
		t.Fatalf("StopSession there: %v", err)
	}
	if err := pair.source.StopSession(pair.caller.ID); err != nil {
		t.Fatalf("StopSession here: %v", err)
	}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return !row.NestedOpen })
	waitUntil(t, 10*time.Second, func() bool {
		pair.source.threadRequestsWG.Wait()
		return len(rootRows()) == 0
	})
	if past, err := pair.source.ListThreadPastAgentRows(root.ID, store.PastAgentRowsCursor{}, 8); err != nil || past.Total != 1 {
		t.Fatalf("root's past = %+v, %v; want its link back", past, err)
	}
}

// A remote spawn at the listing's last level nests none of its computer's
// rows and says that it has them, as a local one at the bound does.
func TestARemoteSpawnAtTheBoundSaysItsAgentsNestDeeper(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	spawn := store.ThreadRequest{Token: "edge", Kind: store.ThreadRequestSpawn, TargetComputerID: "elsewhere", TargetThreadID: "there"}
	for _, listed := range [][]ThreadAgentRow{nil, {{Token: "below", Kind: store.ThreadRequestSpawn, State: threadtools.RequestRunning}}} {
		parent := ThreadAgentRow{Token: "edge", Depth: threadAgentRowDepth - 1}
		rows, entries := f.app.nestPeerAgentRows(&parent, threadAgentRemoteSlot{depth: threadAgentRowDepth, spawn: spawn},
			threadAgentPeerAnswer{listing: ThreadAgentRows{Rows: listed, Worker: workerRunning}}, threadAgentRowDepth)
		if len(rows) != 0 || len(entries) != 0 || parent.Worker != workerRunning || parent.NestedHidden != (len(listed) > 0) {
			t.Fatalf("with %d rows there: nested %v, parent %+v; want none nested, its state kept, and its rows said to nest deeper", len(listed), rows, parent)
		}
	}
}
