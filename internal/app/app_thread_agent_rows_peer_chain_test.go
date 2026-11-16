package app

// Agents rows across a chain of paired computers over the real wire: each
// computer is paired with the next, and each thread on the chain spawned
// the next one's, so a computer answering agent_rows for its thread asks
// the next computer for its own spawn's rows.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// agentChain is computers paired one after the next: apps[0]'s caller
// spawned threads[0] on apps[1], threads[0] spawned threads[1] on apps[2],
// and so on.
type agentChain struct {
	apps  []*App
	wires []*pairedBackend
	peers []*countedAgentsPeer
	// ids are what each computer calls itself; computers[i] is what apps[i]
	// calls apps[i+1]; projects[i] is a project on apps[i].
	ids       []string
	computers []string
	projects  []string
	caller    store.Thread
	// spawns[i] is the token of apps[i]'s spawn on apps[i+1], and
	// threads[i] the thread it made there.
	spawns  []string
	threads []string
}

// countedAgentsPeer is a computer that counts the agent_rows calls it is
// sent, and those of them that ask for the thread's state alone.
type countedAgentsPeer struct {
	*App
	calls     atomic.Int32
	stateOnly atomic.Int32
}

func (p *countedAgentsPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool == threadAgentPeerRows {
		p.calls.Add(1)
		var args map[string]any
		if err := json.Unmarshal(call.Args, &args); err == nil && args["state_only"] == true {
			p.stateOnly.Add(1)
		}
	}
	return p.App.ThreadToolQuery(ctx, call)
}

// serveAppAs serves app's wire through receiver, which embeds app and
// overrides some of its methods.
func serveAppAs(t *testing.T, app *App, receiver any) *pairedBackend {
	t.Helper()
	return servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(receiver, transport.RegisterOptions{
			Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
		}); err != nil {
			t.Fatal(err)
		}
		cfg.Dispatcher = dispatcher
	})
}

// newAgentChain boots n computers, each after the first serving its wire
// and counting the agent_rows calls it is sent, and pairs each with the
// next. Each calls itself by an id of its own.
func newAgentChain(t *testing.T, n int) *agentChain {
	t.Helper()
	remotetest.Require(t)
	c := &agentChain{}
	for i := range n {
		app, _ := setupE2EApp(t)
		app.appCtx, app.appCancel = context.WithCancel(context.Background())
		t.Cleanup(app.appCancel)
		app.configDir = t.TempDir()
		var wire *pairedBackend
		var peer *countedAgentsPeer
		project := ""
		if i > 0 {
			if err := app.initIdentity(fmt.Sprintf("chain-computer-%d", i)); err != nil {
				t.Fatal(err)
			}
			peer = &countedAgentsPeer{App: app}
			wire = serveAppAs(t, app, peer)
			registered, err := app.ensureProjectForWorkspace(t.TempDir())
			if err != nil {
				t.Fatalf("register a project on computer %d: %v", i, err)
			}
			project = registered.ID
		}
		id := uuid.NewString()
		publishReachIdentity(t, app, id)
		app.installThreadRequestObserver()
		c.apps, c.wires, c.peers = append(c.apps, app), append(c.wires, wire), append(c.peers, peer)
		c.ids, c.projects = append(c.ids, id), append(c.projects, project)
	}
	for i := range n - 1 {
		manager, err := attachedbackends.New(t.TempDir(), fmt.Sprintf("computer %d", i), "test")
		if err != nil {
			t.Fatalf("attachedbackends.New: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		invite, _ := c.wires[i+1].mintLink(t, "full")
		peer, err := manager.Add(ctx, invite.URL)
		if err != nil {
			cancel()
			t.Fatalf("pair computer %d with the next: %v", i, err)
		}
		if err := c.apps[i+1].ConfirmDevicePairing(invite.LinkID); err != nil {
			cancel()
			t.Fatalf("confirm the pairing of computer %d: %v", i, err)
		}
		err = manager.Await(ctx, peer.ID)
		cancel()
		if err != nil {
			t.Fatalf("await the pairing of computer %d: %v", i, err)
		}
		c.apps[i].backends = manager
		manager.SetChanged(c.apps[i].backendSetChanged)
		c.computers = append(c.computers, peer.ID)
	}
	caller, err := createTestThread(t, c.apps[0], string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the calling thread: %v", err)
	}
	c.caller = caller
	return c
}

// spawnAlong has the thread at link i of the chain, the caller for 0,
// spawn a thread on the next computer.
func (c *agentChain) spawnAlong(t *testing.T, i int, prompt string) {
	t.Helper()
	caller := c.caller.ID
	if i > 0 {
		caller = c.threads[i-1]
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	ack, err := threadToolsApp{app: c.apps[i]}.Spawn(ctx,
		threadtools.Caller{ThreadID: caller, ComputerID: c.ids[i], ComputerName: fmt.Sprintf("computer %d", i)},
		threadtools.SpawnCall{Prompt: prompt, ComputerID: c.computers[i], ProjectID: c.projects[i+1]})
	if err != nil {
		t.Fatalf("spawn from computer %d: %v", i, err)
	}
	receipt, found, err := c.apps[i+1].store.GetThreadRequestReceipt(ack.Token)
	if err != nil || !found || receipt.TargetThreadID == "" {
		t.Fatalf("the receipt on computer %d = %+v, %v, %v; want it naming its thread", i+1, receipt, found, err)
	}
	c.spawns, c.threads = append(c.spawns, ack.Token), append(c.threads, receipt.TargetThreadID)
}

// settle waits for the thread spawn i made to answer, then polls its
// computer until it collects the answer.
func (c *agentChain) settle(t *testing.T, i int) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		receipt, found, err := c.apps[i+1].store.GetThreadRequestReceipt(c.spawns[i])
		return err == nil && found && receipt.SettledAt != 0
	})
	waitUntil(t, 10*time.Second, func() bool {
		c.apps[i].pollRemoteThreadRequests(time.Now().Add(time.Hour))
		c.apps[i].threadRequestsWG.Wait()
		row, found, err := c.apps[i].store.GetThreadRequest(c.spawns[i])
		return err == nil && found && threadRequestSettled(row)
	})
}

// atRest waits until the threads of links from to to, end excluded, are
// at rest, as the wakes the answers sent them end, and every computer's
// background asking is done.
func (c *agentChain) atRest(t *testing.T, from, to int) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		for i := from; i < to; i++ {
			rest, err := c.apps[i+1].workerUnrest(c.threads[i])
			if err != nil {
				t.Fatal(err)
			}
			if rest.unrest != "" {
				return false
			}
		}
		return true
	})
	for _, app := range c.apps {
		app.threadRequestsWG.Wait()
	}
}

// entryAt is when computer i last recorded an answer for spawn, the zero
// time when it holds none.
func (c *agentChain) entryAt(i int, spawn string) (time.Time, error) {
	cache := &c.apps[i].threadAgentPeers
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[spawn]
	if !ok {
		return time.Time{}, nil
	}
	return entry.at, entry.err
}
