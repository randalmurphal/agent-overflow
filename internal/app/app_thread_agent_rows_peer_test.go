package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	listing := pair.listRows(t)
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
}

// Stop on the spawn's row runs the cascade there: the child's own spawn is
// cancelled as a user Stop, not only the request and the turn.
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
	if grand.State != store.ThreadRequestCancelled || !strings.Contains(string(grand.Answer), "The user stopped it") || !grand.Notify {
		t.Fatalf("the child's spawn = %s %q notify %v, want cancelled by the user with its wake armed", grand.State, grand.Answer, grand.Notify)
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
	if grand, _, _ := pair.dest.store.GetThreadRequest(pair.grand); threadRequestSettled(grand) {
		t.Fatalf("a refused stop stopped the child's spawn: %s", grand.State)
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
// the row and in the Stop's report.
func TestThreadAgentRowsSayWhenAPairedComputerIsOlder(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, serveOlderAgentsPeer)

	listing := pair.listRows(t)
	parent, ok := agentRowByToken(listing.Rows, pair.spawn.Token)
	if !ok || parent.NestedUnavailable != errThreadAgentPeerOlder.Error() {
		t.Fatalf("the spawn row = %+v, want it to say the computer is older", parent)
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
	if row := pair.request(t, pair.spawn.Token); row.State != store.ThreadRequestCancelled {
		t.Fatalf("the spawn is %s, want cancelled", row.State)
	}
}

// Nothing here learns of a change on the paired computer by itself, so each
// poll of an open remote spawn has its caller's rows read again.
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
