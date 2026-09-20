package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// Agent thread tools across two computers, over the real wire: a real
// transport listener terminating real TLS, a real pairing carried by
// `internal/deviceclient`, and both ledgers backed by their own store.
//
// Nothing here is faked except the provider, which is a mock script. The
// destination runs its own turns, writes its own receipts and serves the
// five peer methods; the source keeps the request rows and collects them
// through the poller it would run on its own clock.

// reachPair is one source computer paired with one destination.
type reachPair struct {
	source   *App
	dest     *App
	destWire *pairedBackend
	manager  *attachedbackends.Manager
	// computer is the destination's id as the source's pairings name it.
	computer string
	// project is a project registered on the DESTINATION. A spawn there
	// names one, because projects are per computer.
	project string
	caller  store.Thread
	// sourceID is what the source computer calls itself.
	sourceID string
	// dbPath and settingsDir are the source's durable state, so a test can
	// boot a second App over what the first one wrote.
	dbPath      string
	settingsDir string
}

func newReachPair(t *testing.T) *reachPair {
	t.Helper()
	return newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		return servePairedApp(t, app)
	})
}

// newReachPairServedBy is newReachPair with the destination's listener
// built by the caller, which is how a test gives that computer a fault:
// a reply that never comes back, or a call that never returns.
func newReachPairServedBy(t *testing.T, serve func(*testing.T, *App) *pairedBackend) *reachPair {
	t.Helper()

	dest, _ := setupE2EApp(t)
	dest.configDir = t.TempDir()
	dest.initIdentity("thread-tools-destination")
	if dest.identityState() == nil {
		t.Fatal("the destination has no session core, so nothing can pair with it")
	}
	dest.installThreadRequestObserver()
	wire := serve(t, dest)
	project, err := dest.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("register a project on the destination: %v", err)
	}

	dbPath, settingsDir := storetest.ClonePath(t), t.TempDir()
	source, _ := setupE2EAppOn(t, dbPath, settingsDir)
	source.configDir = t.TempDir()
	source.installThreadRequestObserver()
	sourceID := uuid.NewString()
	publishReachIdentity(t, source, sourceID)

	pair := &reachPair{
		source: source, dest: dest, destWire: wire, sourceID: sourceID,
		project: project.ID, dbPath: dbPath, settingsDir: settingsDir,
	}
	pair.manager = pair.pairWith(t, "source")
	source.backends = pair.manager
	pair.computer = pair.attachedID(t, pair.manager)

	caller, err := createTestThread(t, source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the calling thread: %v", err)
	}
	pair.caller = caller
	return pair
}

// publishReachIdentity publishes the store identity Start would, under an
// id of this computer's own.
//
// The id is passed in because every fixture store is a byte copy of one
// migrated template and so carries the template's backend id. Two clones
// that both called themselves by it would be one computer as far as
// PairedComputers is concerned, which drops the pairing out of its own
// list. The destination keeps the template's id, which is what its
// listener publishes and what the pairing records.
func publishReachIdentity(t *testing.T, app *App, backendID string) {
	t.Helper()
	identity, err := app.store.Identity()
	if err != nil {
		t.Fatalf("store identity: %v", err)
	}
	if backendID != "" {
		identity.BackendID = backendID
	}
	app.storeIdentity.Store(&identity)
}

// pairWith pairs one more device with the destination and returns its
// manager. Every call is a separate device of its own, which is what an
// ownership test needs.
func (p *reachPair) pairWith(t *testing.T, label string) *attachedbackends.Manager {
	t.Helper()
	manager, err := attachedbackends.New(t.TempDir(), label, "test")
	if err != nil {
		t.Fatalf("attachedbackends.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invite, _ := p.destWire.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatalf("add the destination: %v", err)
	}
	if err := p.dest.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("confirm the pairing: %v", err)
	}
	if err := manager.Await(ctx, peer.ID); err != nil {
		t.Fatalf("await the pairing: %v", err)
	}
	return manager
}

func (p *reachPair) attachedID(t *testing.T, manager *attachedbackends.Manager) string {
	t.Helper()
	rows, err := manager.List()
	if err != nil {
		t.Fatalf("list pairings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the manager holds %d pairings, want the destination alone", len(rows))
	}
	return rows[0].ID
}

func (p *reachPair) callerIdentity() threadtools.Caller {
	id, _ := p.source.backendIdentity()
	return threadtools.Caller{ThreadID: p.caller.ID, Title: p.caller.Title, ComputerID: id, ComputerName: "source"}
}

func (p *reachPair) adapter() threadToolsApp { return threadToolsApp{app: p.source} }

// call runs one tool on the source exactly as a live session would, and
// decodes the result the model would read.
func (p *reachPair) call(t *testing.T, tool, args string) map[string]any {
	t.Helper()
	result, err := p.callRaw(t, tool, args)
	if err != nil {
		t.Fatalf("%s(%s): %v", tool, args, err)
	}
	return result
}

func (p *reachPair) callRaw(t *testing.T, tool, args string) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	answer, err := p.source.threadToolsServer().Call(ctx, p.callerIdentity(), tool, json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode %s result: %v", tool, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %s result: %v", tool, err)
	}
	return decoded, nil
}

// poll runs one whole pass of the request poller and joins it, in place of
// the ticker that drives it in a running app.
func (p *reachPair) poll(t *testing.T) {
	t.Helper()
	p.pollOn(t, p.source)
}

func (p *reachPair) pollOn(t *testing.T, app *App) {
	t.Helper()
	// Every open row is due: the cadence is tested by threadPollDelay, and
	// a test that waited out five seconds per pass would prove nothing
	// more.
	app.pollRemoteThreadRequests(time.Now().Add(time.Hour))
	app.threadRequestsWG.Wait()
}

func (p *reachPair) request(t *testing.T, token string) store.ThreadRequest {
	t.Helper()
	row, found, err := p.source.store.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("GetThreadRequest(%s): found=%v err=%v", token, found, err)
	}
	return row
}

func (p *reachPair) receipt(t *testing.T, token string) store.ThreadRequestReceipt {
	t.Helper()
	row, found, err := p.dest.store.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("GetThreadRequestReceipt(%s) on the destination: found=%v err=%v", token, found, err)
	}
	return row
}

// spawnThere starts one thread on the destination and returns its receipt.
func (p *reachPair) spawnThere(t *testing.T, prompt string, extra map[string]any) threadtools.RequestAck {
	t.Helper()
	call := threadtools.SpawnCall{Prompt: prompt, ComputerID: p.computer, ProjectID: p.project}
	if extra != nil {
		encoded, err := json.Marshal(extra)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &call); err != nil {
			t.Fatal(err)
		}
		call.ComputerID, call.ProjectID = p.computer, p.project
		call.Prompt = prompt
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ack, err := p.adapter().Spawn(ctx, p.callerIdentity(), call)
	if err != nil {
		t.Fatalf("remote spawn: %v", err)
	}
	return ack
}

// peerCode is the code the destination wrote on a refusal, as the RPC
// client carries it. A caller that goes through threadOperationError reads
// the same code off public prose instead.
func peerCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal from the destination, got none")
	}
	var remote *rpcclient.Error
	if !errors.As(err, &remote) {
		t.Fatalf("the destination's refusal is not an RPC error: %v", err)
	}
	return remote.Code
}

// TestThreadToolsReachFollowsTheCallersSwitchAndServesWithTheDestinationsOff
// pins the two halves of the availability rule: the switch governs what a
// computer's OWN agents may start, and a paired computer's request is
// answered either way.
func TestThreadToolsReachFollowsTheCallersSwitchAndServesWithTheDestinationsOff(t *testing.T) {
	pair := newReachPair(t)
	// A turn with no result line leaves the destination's thread running,
	// which is the state the foreign receipt has to keep the tools open in.
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	if _, err := pair.dest.settings.Update(map[string]any{"threadToolsEnabled": false}); err != nil {
		t.Fatalf("turn the destination's switch off: %v", err)
	}
	if pair.dest.threadToolsSwitchOn() {
		t.Fatal("the destination's own switch is still on")
	}

	ack := pair.spawnThere(t, "work on the other computer", nil)
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" {
		t.Fatalf("the destination accepted %s without a thread: %+v", ack.Token, receipt)
	}
	if !pair.dest.threadToolsEnabledFor(receipt.TargetThreadID) {
		t.Error("a thread answering a paired computer's request lost the tools to this computer's switch")
	}
	if pair.dest.threadToolsEnabledFor(pair.caller.ID) {
		t.Error("the switched-off destination serves tools to a thread with no foreign receipt")
	}
	if len(pair.dest.threadMCPTools(threadMCPAccess{ThreadID: receipt.TargetThreadID})) == 0 {
		t.Error("the thread serving the request lists no tools")
	}

	// The caller's switch is the one that governs the call. Turning it off
	// takes the tools away from the source's own session.
	if tools := pair.source.threadMCPTools(threadMCPAccess{ThreadID: pair.caller.ID}); len(tools) == 0 {
		t.Fatal("the source lists no tools while its switch is on")
	}
	if _, err := pair.source.settings.Update(map[string]any{"threadToolsEnabled": false}); err != nil {
		t.Fatalf("turn the source's switch off: %v", err)
	}
	if tools := pair.source.threadMCPTools(threadMCPAccess{ThreadID: pair.caller.ID}); tools != nil {
		t.Errorf("the source still offers %d tools with its switch off", len(tools))
	}
}

// TestThreadToolsReachShapeFollowsThePairingSet proves the schemas and the
// guide change when a computer is paired or forgotten, with no restart.
func TestThreadToolsReachShapeFollowsThePairingSet(t *testing.T) {
	pair := newReachPair(t)
	shape := pair.source.threadToolsShape(pair.caller.ID)
	if len(shape.Computers) != 1 || shape.Computers[0].ID != pair.computer {
		t.Fatalf("shape names %+v, want the paired destination alone", shape.Computers)
	}
	if shape.Defaults.Provider != string(provider.Claude) {
		t.Errorf("shape defaults = %+v, want the calling thread's provider", shape.Defaults)
	}
	tools := pair.source.threadMCPTools(threadMCPAccess{ThreadID: pair.caller.ID})
	if !threadToolsOfferComputer(tools) {
		t.Error("a paired build offers no computer_id parameter")
	}

	if err := pair.source.RemoveBackend(pair.computer, false); err != nil {
		t.Fatalf("forget the destination: %v", err)
	}
	if computers := pair.source.threadToolsShape(pair.caller.ID).Computers; len(computers) != 0 {
		t.Fatalf("the forgotten computer is still in the shape: %+v", computers)
	}
	if threadToolsOfferComputer(pair.source.threadMCPTools(threadMCPAccess{ThreadID: pair.caller.ID})) {
		t.Error("a single-computer build still offers computer_id")
	}
}

func threadToolsOfferComputer(tools []map[string]any) bool {
	for _, tool := range tools {
		schema, _ := tool["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if _, present := properties["computer_id"]; present {
			return true
		}
	}
	return false
}

// TestThreadToolsReceiptBelongsToTheDeviceThatMadeIt proves a second
// device of the same computer cannot redeem another device's token.
func TestThreadToolsReceiptBelongsToTheDeviceThatMadeIt(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	ack := pair.spawnThere(t, "work owned by the first device", nil)

	second := pair.pairWith(t, "second device")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	computer := pair.attachedID(t, second)

	var reply ThreadPeerReply
	err := second.CallThreadPeer(ctx, computer, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:  "thread_send",
		Token: ack.Token,
		// A well-formed source that is simply not the one that minted the
		// token: ownership is decided by the authenticated device, not by
		// what the sender claims about itself.
		Source: threadtools.Caller{ThreadID: pair.caller.ID, ComputerID: uuid.NewString()},
		Args:   json.RawMessage(`{"thread_id":"` + pair.receipt(t, ack.Token).TargetThreadID + `","message":"steal it"}`),
	})
	if code := peerCode(t, err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("a second device's reuse of the token = %q, want %q", code, threadtools.CodeRequestNotYours)
	}

	var status ThreadPeerPollReply
	if err := second.CallThreadPeer(ctx, computer, "ThreadToolRequestStatus", &status,
		ThreadPeerPoll{Tokens: []string{ack.Token}}); err != nil {
		t.Fatalf("second device's status poll: %v", err)
	}
	if len(status.Requests) != 1 || status.Requests[0].Known {
		t.Fatalf("a second device reads another device's request: %+v", status.Requests)
	}
	// The owning device still reads its own.
	if own, err := pair.source.callThreadRequestStatus(pair.computer, ThreadPeerPoll{Tokens: []string{ack.Token}}); err != nil {
		t.Fatalf("owning device's status poll: %v", err)
	} else if len(own.Requests) != 1 || !own.Requests[0].Known {
		t.Fatalf("the owning device lost its own request: %+v", own.Requests)
	}
}

// TestThreadToolsResolutionFansOutAcrossComputers proves a prefix is
// resolved on every computer at once: a match only the destination holds
// is found, and two matches on two computers are an ambiguity rather than
// a silent local win.
func TestThreadToolsResolutionFansOutAcrossComputers(t *testing.T) {
	pair := newReachPair(t)
	remote := reachThread(t, pair.dest, "aaaaaaa1-0000-4000-8000-000000000001", "Remote only")

	found := pair.call(t, "thread_show", `{"thread_id":"`+remote.ID+`","turns":1}`)
	if found["computer_id"] != pair.computer {
		t.Fatalf("thread_show stamped %v, want the destination %s", found["computer_id"], pair.computer)
	}

	// A prefix that matches on both computers is ambiguous, and the
	// refusal names both.
	reachThread(t, pair.source, "aaaaaaa1-0000-4000-8000-000000000002", "Local twin")
	_, err := pair.callRaw(t, "thread_show", `{"thread_id":"aaaaaaa1-0000"}`)
	if code := publicCode(t, err); code != threadtools.CodeAmbiguous {
		t.Fatalf("an ambiguity across computers = %q, want %q", code, threadtools.CodeAmbiguous)
	}
	_, message, _ := errorsx.PublicDetails(err)
	if !strings.Contains(message, remote.ID) || !strings.Contains(message, "aaaaaaa1-0000-4000-8000-000000000002") {
		t.Fatalf("the ambiguity does not name both candidates: %s", message)
	}
}

// TestThreadToolsResolutionReportsASilentComputer proves a destination
// that cannot answer makes a miss incomplete rather than a confident not
// found, and marks a single match elsewhere partial.
func TestThreadToolsResolutionReportsASilentComputer(t *testing.T) {
	pair := newReachPair(t)
	local := reachThread(t, pair.source, "bbbbbbb1-0000-4000-8000-000000000001", "Local only")
	shutdownReachDestination(t, pair)

	_, err := pair.callRaw(t, "thread_show", `{"thread_id":"cccccccc-0000"}`)
	if code := publicCode(t, err); code != threadtools.CodeResolutionIncomplete {
		t.Fatalf("a miss with a silent computer = %q, want %q", code, threadtools.CodeResolutionIncomplete)
	}

	partial := pair.call(t, "thread_show", `{"thread_id":"`+local.ID[:12]+`","turns":1}`)
	note, _ := partial["note"].(string)
	if !strings.Contains(note, "did not answer in time") {
		t.Fatalf("a match found while a computer was silent carries no partial note: %q", note)
	}
}

// reachThread inserts one ordinary thread straight into a computer's store
// so a test can choose its id.
func reachThread(t *testing.T, app *App, id, title string) store.Thread {
	t.Helper()
	project, err := app.ensureProjectForWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("register a project: %v", err)
	}
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID: id, ProjectID: project.ID, Title: title,
		Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, CreatedAt: now, UpdatedAt: now,
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
	return thread
}

// peerFaults is a destination whose thread peer methods can be made to
// behave like a computer whose answer was lost on the way back, or one
// that is asleep. It embeds the App and is registered under the same
// labels, so the source reaches it through the ordinary method ids.
type peerFaults struct {
	*App
	// dropReplies is how many answered ThreadToolCall replies are thrown
	// away before one is allowed back to the source. The destination has
	// done the work in each case; only the reply is lost.
	dropReplies atomic.Int32
	// calls counts every ThreadToolCall this destination answered.
	calls atomic.Int32
	// stall holds every ThreadToolRequestStatus until it is closed, which
	// is what a computer that accepts a connection and never answers
	// costs the poller.
	stall chan struct{}
}

func (p *peerFaults) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	reply, err := p.App.ThreadToolCall(ctx, call)
	if err != nil {
		return reply, err
	}
	if p.calls.Add(1) <= p.dropReplies.Load() {
		return ThreadPeerReply{}, errors.New("the answer was lost on the way back")
	}
	return reply, nil
}

func (p *peerFaults) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	if p.stall != nil {
		select {
		case <-p.stall:
		case <-ctx.Done():
			return ThreadPeerPollReply{}, ctx.Err()
		}
	}
	return p.App.ThreadToolRequestStatus(ctx, poll)
}

// serveFaultyApp serves one destination through the fault wrapper.
func serveFaultyApp(t *testing.T, app *App) (*pairedBackend, *peerFaults) {
	t.Helper()
	faults := &peerFaults{App: app}
	wire := servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(faults, transport.RegisterOptions{
			Package:   "main",
			TypeName:  "App",
			AllowList: transport.NewMethodAllowList(),
		}); err != nil {
			t.Fatalf("register the faulty destination: %v", err)
		}
		cfg.Dispatcher = dispatcher
	})
	return wire, faults
}

// attachDestination pairs one more computer with the source and returns the
// id the source's pairings give it.
func attachDestination(t *testing.T, pair *reachPair, wire *pairedBackend, dest *App) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	invite, _ := wire.mintLink(t, "full")
	peer, err := pair.manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatalf("add the second destination: %v", err)
	}
	if err := dest.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatalf("confirm the second pairing: %v", err)
	}
	if err := pair.manager.Await(ctx, peer.ID); err != nil {
		t.Fatalf("await the second pairing: %v", err)
	}
	return peer.ID
}

// shutdownReachDestination stops the destination's listener, which is what
// a computer that is asleep or offline looks like from the source.// shutdownReachDestination stops the destination's listener, which is what
// a computer that is asleep or offline looks like from the source.
func shutdownReachDestination(t *testing.T, pair *reachPair) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pair.destWire.srv.Shutdown(ctx); err != nil {
		t.Fatalf("stop the destination: %v", err)
	}
}

// TestThreadToolsRemoteRequestsRunThereAndAreCollectedHere is the whole
// round trip for the three calls that start work: the destination runs a
// real turn against its own mock provider, keeps the receipt, and the
// source's poller collects the answer and acknowledges it.
func TestThreadToolsRemoteRequestsRunThereAndAreCollectedHere(t *testing.T) {
	pair := newReachPair(t)
	// One reply per session: each of the three requests below runs in a
	// thread that has not run before, which is one mock process each.
	const answer = "the other computer answered"
	installMockClaudeReplies(t, pair.dest, answer)

	spawn := pair.spawnThere(t, "start work over there", nil)
	if spawn.ComputerID != pair.computer || (spawn.State != store.ThreadRequestAccepted && spawn.State != store.ThreadRequestRunning) {
		t.Fatalf("spawn ack = %+v, want an acceptance on %s", spawn, pair.computer)
	}
	row := pair.request(t, spawn.Token)
	if row.TargetComputerID != pair.computer {
		t.Fatalf("the source row names computer %q, want %q", row.TargetComputerID, pair.computer)
	}
	target := pair.collect(t, spawn.Token, answer)
	if target == "" {
		t.Fatal("the collected spawn names no thread on the destination")
	}
	if items, err := pair.dest.store.ListItems(target); err != nil || len(items) == 0 {
		t.Fatalf("the spawned thread holds %d items: %v", len(items), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	existing := reachThread(t, pair.dest, uuid.NewString(), "Already there")
	send, err := pair.adapter().Send(ctx, pair.callerIdentity(), threadtools.SendCall{
		ThreadID: existing.ID, ComputerID: pair.computer, Message: "carry on with this one",
	})
	if err != nil {
		t.Fatalf("remote send: %v", err)
	}
	if got := pair.collect(t, send.Token, answer); got != existing.ID {
		t.Fatalf("the send ran in thread %q, want the thread it named %q", got, existing.ID)
	}

	ask, err := pair.adapter().Ask(ctx, pair.callerIdentity(), threadtools.AskCall{
		ThreadID: target, ComputerID: pair.computer, Question: "what did you do?",
	})
	if err != nil {
		t.Fatalf("remote ask: %v", err)
	}
	if scratch := pair.collect(t, ask.Token, answer); scratch == target {
		t.Fatal("an ask ran in the target thread instead of its own scratch fork")
	}
	// The question never landed in the thread it was asked about.
	items, err := pair.dest.store.ListItems(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "what did you do?") {
			t.Fatalf("the ask's question reached the target thread: %s", item.ID)
		}
	}
}

// Deleting the caller stops the ask it left running on another computer.
// That cancel is a forwarded call like any other, so it has to name both
// the thread and the computer it came from: without the computer the
// destination refuses it and the hidden fork keeps running.
func TestThreadToolsDeletingTheCallerCancelsItsAskOnTheOtherComputer(t *testing.T) {
	pair := newReachPair(t)
	// A spawn first, because an ask forks the thread it names and a thread
	// with no messages cannot be forked.
	const done = "the spawn finished"
	installMockClaudeReplies(t, pair.dest, done)
	spawn := pair.spawnThere(t, "start work over there", nil)
	target := pair.collect(t, spawn.Token, done)

	// From here the destination's turns have no result line, so the ask
	// never answers and is still open when the caller goes away.
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ask, err := pair.adapter().Ask(ctx, pair.callerIdentity(), threadtools.AskCall{
		ThreadID: target, ComputerID: pair.computer, Question: "what did you do?",
	})
	if err != nil {
		t.Fatalf("remote ask: %v", err)
	}
	scratch := ""
	waitUntilE2E(t, 30*time.Second, "the destination forks the ask", func() bool {
		receipt, found, err := pair.dest.store.GetThreadRequestReceipt(ask.Token)
		if err != nil || !found || receipt.TargetThreadID == "" {
			return false
		}
		scratch = receipt.TargetThreadID
		return true
	})
	if scratch == target {
		t.Fatal("an ask ran in the target thread instead of its own scratch fork")
	}

	if err := pair.source.DeleteThread(pair.caller.ID); err != nil {
		t.Fatalf("delete the caller: %v", err)
	}
	waitUntilE2E(t, 30*time.Second, "the destination drops the fork", func() bool {
		_, err := pair.dest.store.GetThread(scratch)
		return err != nil
	})
	receipt, found, err := pair.dest.store.GetThreadRequestReceipt(ask.Token)
	if err != nil || !found {
		t.Fatalf("read receipt %s: %v", ask.Token, err)
	}
	if receipt.State != store.ThreadReceiptCancelled {
		t.Errorf("the destination left %s %q, want %q", ask.Token, receipt.State, store.ThreadReceiptCancelled)
	}
	// The thread the question was about was never the request's to delete.
	if _, err := pair.dest.store.GetThread(target); err != nil {
		t.Errorf("the ask's target thread went with the fork: %v", err)
	}
}

// collect waits for the destination to settle one request, runs the
// poller, and asserts the answer landed on the source row with the
// destination told it was stored. It returns the thread that ran.
func (p *reachPair) collect(t *testing.T, token, answer string) string {
	t.Helper()
	waitUntilE2E(t, 30*time.Second, "the destination settles "+token, func() bool {
		receipt, found, err := p.dest.store.GetThreadRequestReceipt(token)
		return err == nil && found && receipt.SettledAt != 0
	})
	receipt := p.receipt(t, token)
	if receipt.State != store.ThreadReceiptFinished {
		t.Fatalf("the destination settled %s as %q: %s", token, receipt.State, receipt.Answer)
	}
	p.poll(t)
	row := p.request(t, token)
	if row.State != store.ThreadRequestFinished || string(row.Answer) != answer {
		t.Fatalf("collected %s as %q %q, want finished %q", token, row.State, row.Answer, answer)
	}
	// An ask's fork is deleted once it has answered, which unbinds its
	// receipt, so the destination only still names a thread that outlives
	// the request.
	if settled := p.receipt(t, token); settled.TargetThreadID != "" && row.TargetThreadID != settled.TargetThreadID {
		t.Fatalf("the source row names thread %q, the destination ran %q", row.TargetThreadID, settled.TargetThreadID)
	}
	if collected := p.receipt(t, token); collected.CollectedRevision < receipt.Revision || collected.CollectedAt == 0 {
		t.Fatalf("the destination was not told the answer was stored: %+v", collected)
	}
	return row.TargetThreadID
}

// A wake for work that ran on another computer names that thread the way a
// local one does, in its body and in the attribution a client renders as
// the origin chip. The caller has no row for that thread, so the only name
// it has is what the destination reported while the request was open.
func TestThreadToolsRemoteWakeNamesTheThreadAndComputerThatAnswered(t *testing.T) {
	pair := newReachPair(t)
	const answer = "the kernel build finished clean"
	installMockClaudeReplies(t, pair.dest, answer)
	// The wake starts the caller's own session to read it, so this
	// computer needs a mock of its own.
	installMockClaudeReplies(t, pair.source, "read the wake")

	spawn := pair.spawnThere(t, "build the kernel", map[string]any{"title": "Kernel build", "notify": true})
	target := pair.collect(t, spawn.Token, answer)

	var wake store.FlushQueueItem
	waitUntilE2E(t, 30*time.Second, "the caller's wake is queued", func() bool {
		for _, queued := range durableQueueRows(t, pair.source, pair.caller.ID) {
			if queued.SendID == threadWakeSendID(spawn.Token) {
				wake = queued
				return true
			}
		}
		return false
	})
	name, id := threadToolsApp{app: pair.source}.threadToolsBackendName(pair.computer)
	if name == "" || id == "" {
		t.Fatalf("the source has no name for computer %q", pair.computer)
	}
	if !strings.Contains(wake.Message, "Kernel build") || !strings.Contains(wake.Message, name) {
		t.Errorf("wake body = %q, want the thread %q on %q", wake.Message, "Kernel build", name)
	}
	var payload flushqueue.Payload
	if err := json.Unmarshal(wake.Payload, &payload); err != nil {
		t.Fatalf("decode wake payload: %v", err)
	}
	if payload.OriginThread == nil {
		t.Fatalf("the wake carries no attribution: %+v", payload)
	}
	got := *payload.OriginThread
	if got.Title != "Kernel build" || got.ThreadID != target || got.ComputerID != id || got.ComputerName != name {
		t.Errorf("wake origin = %+v, want %q (%s) on %q (%s)", got, "Kernel build", target, name, id)
	}
}

// TestThreadToolsRemoteRequestRetriesOneTokenWithoutRunningTwice proves the
// token is the idempotency key: a call repeated after a lost reply reads
// the existing acceptance back and starts no second piece of work.
func TestThreadToolsRemoteRequestRetriesOneTokenWithoutRunningTwice(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "exactly once", nil)
	first := pair.receipt(t, spawn.Token)

	// The reply is what was lost, not the call: the source sends the same
	// token again the way its retry does.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args, err := json.Marshal(threadtools.SpawnCall{Prompt: "exactly once", ProjectID: pair.project})
	if err != nil {
		t.Fatal(err)
	}
	repeat, _, err := pair.source.callThreadPeerRequest(ctx, threadtools.Computer{ID: pair.computer}, ThreadPeerCall{
		Tool: "thread_spawn", Args: args, Token: spawn.Token, Source: pair.callerIdentity(),
		Inherit: threadtools.SpawnDefaults{
			Provider: pair.caller.Provider, Model: pair.caller.Model,
			Effort: pair.caller.ReasoningEffort, Mode: pair.caller.Mode,
			RuntimeMode: pair.caller.RuntimeMode,
		},
	})
	if err != nil {
		t.Fatalf("retry with the same token: %v", err)
	}
	if !repeat.Known || repeat.TargetThreadID != first.TargetThreadID {
		t.Fatalf("the retry answered %+v, want the thread the first call accepted %q", repeat, first.TargetThreadID)
	}
	items, err := pair.dest.store.ListItems(first.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	prompts := 0
	for _, item := range items {
		if item.Kind == "user_text" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("the retry queued the prompt %d times", prompts)
	}
	receipts, err := pair.dest.store.ListThreadRequestReceiptsForThread(first.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("one token left %d receipts on the destination", len(receipts))
	}
}

// TestThreadToolsUnconfirmedRequestSettlesFromTheDestinationsAnswer proves
// a row the destination does not hold is settled from what it reports
// rather than polled forever, and that the two reasons read differently.
func TestThreadToolsUnconfirmedRequestSettlesFromTheDestinationsAnswer(t *testing.T) {
	pair := newReachPair(t)
	token := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSend,
		TargetComputerID: pair.computer, State: store.ThreadRequestUnconfirmed,
	}); err != nil {
		t.Fatalf("insert the unconfirmed row: %v", err)
	}
	pair.poll(t)
	row := pair.request(t, token)
	if row.State != store.ThreadRequestRefused || !strings.Contains(string(row.Answer), "never accepted") {
		t.Fatalf("an unconfirmed row the destination never saw settled %q %q", row.State, row.Answer)
	}

	// After acceptance the same unknown answer means the receipt is gone,
	// which is a different word to the agent that is waiting.
	lost := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: lost, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestAsk,
		TargetComputerID: pair.computer, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("insert the accepted row: %v", err)
	}
	pair.poll(t)
	row = pair.request(t, lost)
	if row.State != store.ThreadRequestErrored || !strings.Contains(string(row.Answer), "no longer knows") {
		t.Fatalf("an accepted row the destination lost settled %q %q", row.State, row.Answer)
	}
}

// TestThreadToolsLostAdmissionAnswerIsUnconfirmedWithItsToken pins what a
// call whose answer never came back owes the model: the token, so the work
// is checked rather than started again, and a row the poller reconciles.
// The poller waits until nothing is in flight for that token before it
// treats the destination's "unknown" as a refusal.
func TestThreadToolsLostAdmissionAnswerIsUnconfirmedWithItsToken(t *testing.T) {
	pair := newReachPair(t)
	// The call is cancelled before it can be answered, which is what a
	// lost reply looks like from the source: the destination may have
	// accepted it, and this computer cannot tell.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ack, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
		Prompt: "work whose answer never came back", ComputerID: pair.computer, ProjectID: pair.project,
	})
	if err != nil {
		t.Fatalf("a lost reply reached the model as an error: %v", err)
	}
	if ack.Token == "" || ack.State != store.ThreadRequestUnconfirmed || ack.Outcome != threadtools.OutcomeUnconfirmed {
		t.Fatalf("ack = %+v, want an unconfirmed request carrying its token", ack)
	}
	row := pair.request(t, ack.Token)
	if row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("the source row settled %q on a call whose answer was lost", row.State)
	}

	// A row still being admitted is fenced out of the poll, and the fence
	// covers every attempt the admission can make.
	if threadRequestAdmissionFence < threadPeerCallTimeout*threadPeerAdmissionAttempts {
		t.Fatalf("the admission fence %s does not cover %d attempts of %s",
			threadRequestAdmissionFence, threadPeerAdmissionAttempts, threadPeerCallTimeout)
	}
	inFlight := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: inFlight, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSpawn,
		TargetComputerID: pair.computer, State: store.ThreadRequestUnconfirmed,
		NextCheck: time.Now().Add(threadRequestAdmissionFence).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert the row being admitted: %v", err)
	}
	pair.source.pollRemoteThreadRequests(time.Now())
	pair.source.threadRequestsWG.Wait()
	if state := pair.request(t, inFlight).State; state != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll refused a request that was still being admitted: %q", state)
	}
	if state := pair.request(t, ack.Token).State; state != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll refused the request before its next check: %q", state)
	}

	// Once the source is done trying, the poll is the recovery path: the
	// destination holds no receipt, so the request was never accepted.
	pair.poll(t)
	row = pair.request(t, ack.Token)
	if row.State != store.ThreadRequestRefused || !strings.Contains(string(row.Answer), "never accepted") {
		t.Fatalf("the poll settled the lost request as %q %q, want refused", row.State, row.Answer)
	}
}

// TestThreadToolsAdmissionIsRetriedWithOneTokenAfterALostReply proves the
// source retries the acceptance itself rather than handing the model an
// error it would answer by starting the work again, and that the retries
// carry the one token, so the destination runs the work once.
func TestThreadToolsAdmissionIsRetriedWithOneTokenAfterALostReply(t *testing.T) {
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	// The destination accepts every call; the first two answers never
	// reach the source.
	faults.dropReplies.Store(int32(threadPeerAdmissionAttempts - 1))

	ack := pair.spawnThere(t, "accepted once, answered on the third try", nil)
	if got := faults.calls.Load(); got != int32(threadPeerAdmissionAttempts) {
		t.Fatalf("the destination answered %d calls, want %d attempts of one token", got, threadPeerAdmissionAttempts)
	}
	if ack.State != store.ThreadRequestAccepted && ack.State != store.ThreadRequestRunning {
		t.Fatalf("ack = %+v, want the acceptance the destination kept", ack)
	}
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" {
		t.Fatalf("the destination accepted %s without a thread: %+v", ack.Token, receipt)
	}
	receipts, err := pair.dest.store.ListThreadRequestReceiptsForThread(receipt.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("the retries left %d receipts on the destination", len(receipts))
	}
	items, err := pair.dest.store.ListItems(receipt.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	prompts := 0
	for _, item := range items {
		if item.Kind == "user_text" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Fatalf("the retries queued the prompt %d times", prompts)
	}
}

// TestThreadToolsStatusWatchesAThreadOnAnotherComputer proves a thread is
// addressed by its id wherever it lives: thread_status with thread_ids
// answers for a thread this computer never held, and its wait ends when
// that thread is at rest there.
func TestThreadToolsStatusWatchesAThreadOnAnotherComputer(t *testing.T) {
	pair := newReachPair(t)
	remote := reachThread(t, pair.dest, uuid.NewString(), "Watched from here")

	result := pair.call(t, "thread_status", `{"thread_ids":["`+remote.ID+`"],"wait_seconds":10}`)
	threads, _ := result["threads"].([]any)
	if len(threads) != 1 {
		t.Fatalf("thread_status reported %d threads, want the one it was asked about: %+v", len(threads), result)
	}
	row, _ := threads[0].(map[string]any)
	if row["thread_id"] != remote.ID {
		t.Fatalf("thread_status answered about %v, want %s", row["thread_id"], remote.ID)
	}
	if row["computer_id"] != pair.computer {
		t.Fatalf("the row is stamped %v, want the destination %s", row["computer_id"], pair.computer)
	}
	if row["title"] != remote.Title {
		t.Fatalf("the row is titled %v, want %q", row["title"], remote.Title)
	}
	if resting, _ := row["resting"].(bool); !resting {
		t.Fatalf("a thread doing nothing on the destination is reported %+v", row)
	}
	// A thread already at rest ends the wait rather than running it out.
	if woke, _ := result["woke_on"].(string); woke != remote.ID {
		t.Fatalf("the wait ended on %q, want the thread that is resting", woke)
	}
	if timedOut, _ := result["timed_out"].(bool); timedOut {
		t.Fatal("the wait timed out on a thread that is already resting")
	}
}

// TestThreadToolsPollVisitsDestinationsConcurrently proves one asleep
// computer costs the poll one of its slots and not the whole pass: the
// healthy destination's row settles while the other call is still open.
func TestThreadToolsPollVisitsDestinationsConcurrently(t *testing.T) {
	pair := newReachPair(t)

	asleep, _ := setupE2EApp(t)
	asleep.configDir = t.TempDir()
	asleep.initIdentity("thread-tools-asleep")
	wire, faults := serveFaultyApp(t, asleep)
	publishReachIdentity(t, asleep, uuid.NewString())
	faults.stall = make(chan struct{})
	stalled := attachDestination(t, pair, wire, asleep)

	// The asleep computer is visited first: it has the earlier check.
	now := time.Now()
	slow := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: slow, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSend,
		TargetComputerID: stalled, State: store.ThreadRequestAccepted,
		NextCheck: now.Add(-time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert the row on the asleep computer: %v", err)
	}
	quick := newThreadRequestToken()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{
		Token: quick, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSend,
		TargetComputerID: pair.computer, State: store.ThreadRequestAccepted,
		NextCheck: now.Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert the row on the healthy computer: %v", err)
	}

	pair.source.pollRemoteThreadRequests(now)
	t.Cleanup(func() {
		close(faults.stall)
		pair.source.threadRequestsWG.Wait()
	})
	// The healthy destination holds no receipt for this token, so it
	// settles as soon as it is asked. Waiting for it proves the pass did
	// not queue behind the call that is still open.
	waitUntilE2E(t, 10*time.Second, "the healthy destination's row settles", func() bool {
		row, found, err := pair.source.store.GetThreadRequest(quick)
		return err == nil && found && threadRequestSettled(row)
	})
	if row := pair.request(t, slow); threadRequestSettled(row) {
		t.Fatalf("the stalled destination's row settled %q while its call was still open", row.State)
	}
}

// TestThreadToolsUncollectedRemoteAnswerExpires proves the destination
// drops an answer nobody collected and the source reads that verdict.
func TestThreadToolsUncollectedRemoteAnswerExpires(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeReplies(t, pair.dest, "nobody collects this")
	spawn := pair.spawnThere(t, "answer into the void", nil)
	waitUntilE2E(t, 30*time.Second, "the destination settles the spawn", func() bool {
		receipt, found, err := pair.dest.store.GetThreadRequestReceipt(spawn.Token)
		return err == nil && found && receipt.SettledAt != 0
	})
	settled := pair.receipt(t, spawn.Token)
	if settled.ExpiresAt == 0 {
		t.Fatal("a settled receipt carries no hold")
	}

	pair.dest.expireThreadRequests(time.UnixMilli(settled.ExpiresAt))
	expired := pair.receipt(t, spawn.Token)
	if expired.State != store.ThreadReceiptExpired || len(expired.Answer) != 0 || len(expired.LateReply) != 0 {
		t.Fatalf("the hold ran out and the receipt is %q with %d answer bytes", expired.State, len(expired.Answer))
	}

	pair.poll(t)
	row := pair.request(t, spawn.Token)
	if row.State != store.ThreadRequestExpired {
		t.Fatalf("the source collected an expired answer as %q", row.State)
	}
	if row.SettledAt != expired.SettledAt {
		t.Fatalf("the source settled at %d, the destination at %d", row.SettledAt, expired.SettledAt)
	}
}

// TestThreadToolsRemoteAnswerSurvivesARestartOfEitherSide proves both
// halves of the ledger are durable: a destination that restarts settles
// what it interrupted, and a source that restarts collects it with no
// memory of the call that started it.
func TestThreadToolsRemoteAnswerSurvivesARestartOfEitherSide(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "interrupted by a restart", nil)
	pair.poll(t)
	if row := pair.request(t, spawn.Token); threadRequestSettled(row) {
		t.Fatalf("the request settled before the restart: %+v", row)
	}

	// The destination restarts. Its boot sweep settles the receipt it can
	// no longer finish.
	pair.dest.sweepThreadRequestsAtBoot()
	if receipt := pair.receipt(t, spawn.Token); receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("the destination's boot sweep left %s as %q", spawn.Token, receipt.State)
	}

	// The source restarts: a second App over the same database, with none
	// of the first one's memory of the request.
	restarted, _ := setupE2EAppOn(t, pair.dbPath, pair.settingsDir)
	restarted.configDir = pair.source.configDir
	restarted.backends = pair.manager
	publishReachIdentity(t, restarted, pair.sourceID)
	pair.pollOn(t, restarted)

	row, found, err := restarted.store.GetThreadRequest(spawn.Token)
	if err != nil || !found {
		t.Fatalf("the restarted source lost the request: found=%v err=%v", found, err)
	}
	if row.State != store.ThreadRequestInterrupted || !strings.Contains(string(row.Answer), "restarted") {
		t.Fatalf("the restarted source collected %q %q, want the interruption", row.State, row.Answer)
	}
}

// TestThreadToolsOpenRequestsSettleWhenThePairingEnds proves a revoked
// pairing is terminal: the requests against it are settled rather than
// retried against a computer that will never answer.
func TestThreadToolsOpenRequestsSettleWhenThePairingEnds(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "revoked mid-flight", nil)
	revokeReachDevice(t, pair.dest, "source")

	pair.poll(t)
	row := pair.request(t, spawn.Token)
	if row.State != store.ThreadRequestErrored {
		t.Fatalf("a request against an ended pairing is %q, want errored", row.State)
	}
	if !strings.Contains(strings.ToLower(string(row.Answer)), "reconnect") {
		t.Fatalf("the settlement does not say how to recover: %q", row.Answer)
	}
}

// revokeReachDevice is the destination's owner ending one pairing from
// their own settings pane.
func revokeReachDevice(t *testing.T, app *App, label string) {
	t.Helper()
	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("read the access overview: %v", err)
	}
	revoked := 0
	for _, device := range overview.Devices {
		if device.Label != label {
			continue
		}
		if _, err := app.RevokeAccessDevice(device.ID); err != nil {
			t.Fatalf("revoke %s: %v", label, err)
		}
		revoked++
	}
	if revoked != 1 {
		t.Fatalf("revoked %d devices labelled %q, want one", revoked, label)
	}
}

// TestThreadToolsMovedTargetSettlesOnTheComputerThatAcceptedIt proves a
// thread handed to another computer settles its open receipts where the
// work was accepted, and that the remote source collects that verdict.
func TestThreadToolsMovedTargetSettlesOnTheComputerThatAcceptedIt(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "moved out from under the request", nil)
	target := pair.receipt(t, spawn.Token).TargetThreadID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sourceID, _ := pair.source.backendIdentity()
	if err := pair.dest.stopThreadRequestsForMovedThread(ctx, target, sourceID); err != nil {
		t.Fatalf("settle the moved thread's requests: %v", err)
	}
	receipt := pair.receipt(t, spawn.Token)
	if receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("a moved target left its receipt %q, want interrupted", receipt.State)
	}

	pair.poll(t)
	row := pair.request(t, spawn.Token)
	if row.State != store.ThreadRequestInterrupted {
		t.Fatalf("the source collected the move as %q, want interrupted", row.State)
	}
}

// TestThreadToolsRemoteBlockedStateReachesTheWaitingCaller proves the
// destination's live blocked state travels on the poll reply, so a remote
// request reads `blocked` with no second state model behind it.
func TestThreadToolsRemoteBlockedStateReachesTheWaitingCaller(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`,
	}})
	spawn := pair.spawnThere(t, "ask me before running anything", map[string]any{
		"runtime_mode": string(provider.RuntimeApprovalRequired),
	})
	target := pair.receipt(t, spawn.Token).TargetThreadID
	waitUntilE2E(t, 30*time.Second, "the destination's thread blocks on an approval", func() bool {
		live, err := pair.dest.threadToolsAdapter().LiveState(context.Background(), target)
		return err == nil && live.Blocked()
	})

	pair.poll(t)
	row := pair.request(t, spawn.Token)
	if !pair.source.threadRequestBlocked(row) {
		t.Fatal("the destination's blocked state did not reach the source row")
	}
	state := pair.adapter().requestState(context.Background(), row)
	if state.State != threadtools.RequestBlocked {
		t.Fatalf("thread_status reports %q for a blocked remote request", state.State)
	}
	if threadRequestOutcome(row, pair.source.threadRequestBlocked(row)) != threadtools.OutcomeBlocked {
		t.Fatal("a wait on a remote request cannot end on blocked")
	}
}

// TestThreadToolsShowToFileCopiesTheExportAcrossComputers proves a path is
// only useful on the computer that can open it: the destination renders
// the window, the bytes cross in chunks under their digest, and the model
// is given a path in its own export directory.
func TestThreadToolsShowToFileCopiesTheExportAcrossComputers(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeReplies(t, pair.dest, "a transcript worth exporting")
	spawn := pair.spawnThere(t, "write something worth exporting", nil)
	target := pair.collect(t, spawn.Token, "a transcript worth exporting")

	result := pair.call(t, "thread_show", `{"thread_id":"`+target+`","window":"all","to_file":true,"include":["all"]}`)
	file, _ := result["file"].(map[string]any)
	path, _ := file["path"].(string)
	local := filepath.Join(pair.source.configDir, threadExportDirName)
	if !strings.HasPrefix(path, local) {
		t.Fatalf("to_file returned %q, want a path under this computer's %s", path, local)
	}
	if transcript, _ := result["transcript"].(string); transcript != "" {
		t.Error("to_file also rendered the window inline")
	}
	if note, _ := result["note"].(string); !strings.Contains(note, "copied from") {
		t.Errorf("the note does not say the file was copied here: %q", note)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the copied export: %v", err)
	}
	digest := sha256.Sum256(data)
	if got, _ := file["sha256"].(string); got != hex.EncodeToString(digest[:]) {
		t.Fatalf("the copy's digest is %s, the reply promised %v", hex.EncodeToString(digest[:]), file["sha256"])
	}
	if size, _ := file["size"].(float64); int64(size) != int64(len(data)) {
		t.Fatalf("the copy is %d bytes, the reply promised %v", len(data), file["size"])
	}
	if !strings.Contains(string(data), "a transcript worth exporting") {
		t.Fatal("the copied transcript does not hold the thread's own text")
	}
	if strings.Contains(string(data), pair.dest.configDir) {
		t.Fatal("the copy names the destination's own directory")
	}

	// The destination's copy is a file awaiting a transfer that has now
	// happened; it goes a day later, and a transcript this computer's own
	// agent asked for does not.
	exports := filepath.Join(pair.dest.configDir, threadExportDirName)
	if peers := countReachExports(t, exports, threadPeerExportPrefix); peers != 1 {
		t.Fatalf("the destination holds %d peer exports, want the one it rendered", peers)
	}
	pair.dest.sweepThreadExports(time.Now().Add(2 * threadRequestExportAge))
	if peers := countReachExports(t, exports, threadPeerExportPrefix); peers != 0 {
		t.Fatalf("%d peer export(s) survived the sweep", peers)
	}
}

func countReachExports(t *testing.T, dir, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			found++
		}
	}
	return found
}

// TestThreadToolsExportRefusesAWindowNoTransferCanCarry proves the size
// ceiling is answered with the number and what to narrow, not a bare
// failure.
func TestThreadToolsExportRefusesAWindowNoTransferCanCarry(t *testing.T) {
	pair := newReachPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := pair.source.fetchThreadExport(ctx, threadtools.Computer{ID: pair.computer, Name: "destination"},
		threadtools.ExportFile{ExportID: threadPeerExportName(pair.caller.ID), Size: remoteArtifactMaxBytes + 1})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("an oversized export = %q, want %q", code, threadtools.CodeInvalidRequest)
	}
	_, message, _ := errorsx.PublicDetails(err)
	if !strings.Contains(message, "Narrow the window") || !strings.Contains(message, "include") {
		t.Fatalf("the refusal does not say what to narrow: %q", message)
	}
	// An export id this computer did not mint is refused before any
	// transfer starts.
	if _, err := pair.source.readThreadExportChunk(ctx, "not-an-export", 0); err == nil {
		t.Fatal("a fabricated export id was served")
	}
}

// TestForgettingAComputerRefusesOnceThenAbandonsItsRequests proves the
// confirmation: the open requests are listed and the pairing is kept, and
// only an explicit abandon settles them and forgets it.
func TestForgettingAComputerRefusesOnceThenAbandonsItsRequests(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "open when the computer is forgotten", map[string]any{"notify": true})

	err := pair.source.RemoveBackend(pair.computer, false)
	if err == nil {
		t.Fatal("a computer with open requests was forgotten without confirmation")
	}
	// Public: the listing has to survive the dispatcher's redaction to
	// reach a connected browser, and the confirm affordance keys on the
	// code rather than on the prose.
	code, message, public := errorsx.PublicDetails(err)
	if !public || code != "thread_requests_open" {
		t.Fatalf("the refusal reaches a remote caller as %q: %v", code, err)
	}
	if !strings.Contains(message, spawn.Token) || !strings.Contains(message, "Confirm") {
		t.Fatalf("the refusal does not list the open requests: %v", err)
	}
	if rows, listErr := pair.manager.List(); listErr != nil || len(rows) != 1 {
		t.Fatalf("the refused removal forgot the computer anyway: %d rows, %v", len(rows), listErr)
	}
	if row := pair.request(t, spawn.Token); threadRequestSettled(row) {
		t.Fatalf("the refused removal settled the request: %+v", row)
	}

	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("abandon the open requests and forget the computer: %v", err)
	}
	row := pair.request(t, spawn.Token)
	if row.State != store.ThreadRequestErrored || !strings.Contains(string(row.Answer), "was not told") {
		t.Fatalf("an abandoned request settled %q %q", row.State, row.Answer)
	}
	if row.Notify || row.DeliveredAt != 0 {
		t.Fatalf("the abandoned request kept its wake: notify=%v deliveredAt=%d", row.Notify, row.DeliveredAt)
	}
	if rows, listErr := pair.manager.List(); listErr != nil || len(rows) != 0 {
		t.Fatalf("the confirmed removal kept the pairing: %d rows, %v", len(rows), listErr)
	}
	// The other computer was not told, so its receipt is untouched and the
	// thread it started is still there.
	if receipt := pair.receipt(t, spawn.Token); threadReceiptSettledState(receipt.State) {
		t.Fatalf("forgetting the computer settled its receipt as %q", receipt.State)
	}
}

// TestCancellingARemoteRequestNamesItsSource: a forwarded cancel is a
// forwarded request like any other, and the destination refuses one that
// cannot name the thread and computer it came from.
func TestCancellingARemoteRequestNamesItsSource(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	spawn := pair.spawnThere(t, "work to be stopped", map[string]any{"notify": true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := pair.adapter().Cancel(ctx, pair.callerIdentity(), threadtools.CancelCall{Token: spawn.Token})
	if err != nil {
		t.Fatalf("cancel a request running on the other computer: %v", err)
	}
	if report.ComputerID != pair.computer || report.Effect == "" {
		t.Fatalf("cancel report = %+v, want an effect on %s", report, pair.computer)
	}
	// The destination settled its own receipt, which is the only side that
	// could have stopped the work.
	receipt := pair.receipt(t, spawn.Token)
	if receipt.State != store.ThreadReceiptCancelled {
		t.Fatalf("the destination's receipt is %q, want cancelled", receipt.State)
	}
	if row := pair.request(t, spawn.Token); row.Notify {
		t.Fatalf("a cancelled request kept its wake: %+v", row)
	}
}

// TestRemoteSpawnWithoutAProjectIsRefusedWithTheDestinationsProjects
// proves the read half of the reach works too: the refusal is built from
// what the destination answered about itself.
func TestRemoteSpawnWithoutAProjectIsRefusedWithTheDestinationsProjects(t *testing.T) {
	pair := newReachPair(t)
	_, err := pair.callRaw(t, "thread_spawn",
		`{"prompt":"no project named","computer_id":"`+pair.computer+`"}`)
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("a remote spawn with no project = %q, want %q", code, threadtools.CodeInvalidRequest)
	}
	_, message, _ := errorsx.PublicDetails(err)
	if !strings.Contains(message, pair.project) {
		t.Fatalf("the refusal does not carry the destination's projects: %q", message)
	}
}

// TestThreadOperationErrorNamesTheComputerAndKeepsTheDestinationsCode
// pins the refusal shape every peer failure reaches the model in.
func TestThreadOperationErrorNamesTheComputerAndKeepsTheDestinationsCode(t *testing.T) {
	app := &App{}
	computer := uuid.NewString()
	if err := app.threadOperationError("send", computer, "t-1", nil); err != nil {
		t.Fatalf("a nil cause became %v", err)
	}

	// A refusal the destination wrote with a thread tools code is reviewed
	// prose already and crosses unchanged.
	refusal := app.threadOperationError("send", computer, "t-1",
		errorsx.Public(threadtools.CodeNotFound, "No thread matches that id.", nil))
	code, message, ok := errorsx.PublicDetails(refusal)
	if !ok || code != threadtools.CodeNotFound {
		t.Fatalf("the destination's code became %q (public=%v)", code, ok)
	}
	if !strings.Contains(message, computer) || !strings.Contains(message, "t-1") || !strings.Contains(message, "No thread matches that id.") {
		t.Fatalf("the refusal does not name the operation, computer and thread: %q", message)
	}

	// Wrapping is idempotent: the second pass adds no second prefix.
	if again := app.threadOperationError("send", computer, "t-1", refusal); again != refusal {
		t.Fatalf("re-wrapping produced %v", again)
	}

	// Anything else goes through the remote classification, so pairing and
	// connection failures read the same on both surfaces.
	plain := app.threadOperationError("status", computer, "", errors.New("dial tcp: connection refused"))
	code, message, ok = errorsx.PublicDetails(plain)
	if !ok || code != "remote_internal_error" {
		t.Fatalf("an unreviewed cause = %q (public=%v)", code, ok)
	}
	if strings.Contains(message, "connection refused") {
		t.Fatalf("the raw cause reached the model: %q", message)
	}
	if !strings.Contains(message, "Thread status on computer "+computer) {
		t.Fatalf("the refusal does not name the operation and computer: %q", message)
	}

	// A tool name already says "thread", so it is not prefixed with the
	// word again.
	tool := app.threadOperationError("thread_spawn", computer, "",
		errorsx.Public(threadtools.CodeInvalidRequest, "Neither this computer nor origin has that branch.", nil))
	if _, message, _ = errorsx.PublicDetails(tool); !strings.HasPrefix(message, "thread_spawn on computer "+computer+": Neither") {
		t.Fatalf("a tool name refusal reads %q", message)
	}
}

// TestThreadToolsLocalWorkStaysLocalWhileAComputerIsPaired: in the paired
// shape every resolved row carries the computer that owns it, this one
// included. A send or a cancel against a thread on the caller's own computer
// must still run here; forwarding it would ask the pairing set for a peer it
// never holds, because a computer is not paired with itself.
func TestThreadToolsLocalWorkStaysLocalWhileAComputerIsPaired(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.source, [][]string{{mockClaudeInitLine}})
	target, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create the local target: %v", err)
	}

	sent := pair.call(t, "thread_send", `{"thread_id":"`+target.ID+`","message":"work on this here"}`)
	token, _ := sent["token"].(string)
	if token == "" {
		t.Fatalf("send returned no token: %v", sent)
	}
	row := pair.request(t, token)
	if row.TargetComputerID != "" {
		t.Fatalf("a local send was recorded against computer %q", row.TargetComputerID)
	}
	if row.TargetThreadID != target.ID {
		t.Fatalf("send target = %q, want the local thread", row.TargetThreadID)
	}
	items, err := pair.source.store.ListItems(target.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var landed bool
	for _, item := range items {
		if strings.Contains(item.Summary, "work on this here") {
			landed = true
		}
	}
	if !landed {
		t.Fatalf("the message never reached the local thread: %+v", items)
	}

	// A cancel by thread id takes the same route.
	stopped := pair.call(t, "thread_cancel", `{"thread_id":"`+target.ID+`"}`)
	if stopped["effect"] == nil {
		t.Fatalf("cancel returned no effect: %v", stopped)
	}
}

// TestThreadToolsFooterOffersTheWayBackOnlyWhenItExists: pairing is
// directional. The source holds a credential for the destination; the
// destination holds none for the source, so a request that lands there
// cannot offer thread_show or thread_send back to the sender. A request one
// of the destination's own threads makes always can.
func TestThreadToolsFooterOffersTheWayBackOnlyWhenItExists(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

	ack := pair.spawnThere(t, "work for the other computer", map[string]any{"wait_seconds": 0, "notify": true})
	receipt := pair.receipt(t, ack.Token)
	foreign := reachRequestMessage(t, pair.dest, receipt.TargetThreadID, ack.Token)
	if !strings.Contains(foreign, "Agent request from thread") {
		t.Fatalf("the forwarded message carries no footer: %q", foreign)
	}
	if strings.Contains(foreign, "thread_show with its id") || strings.Contains(foreign, "thread_send with its id") {
		t.Fatalf("the footer offers a route back to a computer this one cannot reach: %q", foreign)
	}

	// The destination's own thread asking one of its own: the sender is on
	// this computer, so the return navigation applies.
	local, err := createTestThread(t, pair.dest, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create a local target on the destination: %v", err)
	}
	caller, err := createTestThread(t, pair.dest, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create a local caller on the destination: %v", err)
	}
	destID, _ := pair.dest.backendIdentity()
	own, err := threadToolsApp{app: pair.dest}.Send(t.Context(),
		threadtools.Caller{ThreadID: caller.ID, Title: caller.Title, ComputerID: destID, ComputerName: "destination"},
		threadtools.SendCall{ThreadID: local.ID, Message: "work for you", Notify: true})
	if err != nil {
		t.Fatalf("local send on the destination: %v", err)
	}
	home := reachRequestMessage(t, pair.dest, local.ID, own.Token)
	if !strings.Contains(home, "thread_show with its id") {
		t.Fatalf("a sender on this computer is reachable and the footer must say so: %q", home)
	}
}

// reachRequestMessage reads the user message one request dispatched.
func reachRequestMessage(t *testing.T, app *App, threadID, token string) string {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	for _, item := range items {
		if item.Kind == "user_text" && strings.Contains(item.Summary, token) {
			return item.Summary
		}
	}
	t.Fatalf("no message in %s carries request %s: %+v", threadID, token, items)
	return ""
}

// TestThreadPeerRepliesLargerThanAMegabyteCrossTheWire: a peer answers with
// whole content. A thread_item read is a megabyte before JSON escaping, an
// answer or a late reply can be one on its own, and the poller repeats its
// request every pass, so a client bound below the transport's own inbound
// bound turns one large answer into a closed connection that closes again
// on every retry.
func TestThreadPeerRepliesLargerThanAMegabyteCrossTheWire(t *testing.T) {
	pair := newReachPair(t)
	remote := reachThread(t, pair.dest, "cccccccc-0000-4000-8000-000000000001", "Large answer")
	// Quotes double under JSON escaping, so the reply crosses as more than
	// twice the bytes the model asked for.
	body := strings.Repeat(`"`, threadtools.MaxItemBytes)
	if err := pair.dest.store.InsertTurn(store.Turn{
		TurnID: remote.ID + "-turn", ThreadID: remote.ID, TurnIndex: 0, StartedAt: 1_000,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := pair.dest.store.UpsertItem(store.Item{
		ID: "large-output", ThreadID: remote.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash",
		Summary: "Bash", PayloadID: "large-payload", PayloadKind: "tool_result",
		CreatedAt: 1_000, UpdatedAt: 1_000,
	}, &store.Payload{ID: "large-payload", Kind: "tool_result", Data: []byte(body), CreatedAt: 1_000}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	result := pair.call(t, "thread_item",
		`{"thread_id":"`+remote.ID+`","item_id":"large-output","offset":0,"max_bytes":`+
			strconv.Itoa(threadtools.MaxItemBytes)+`}`)
	text, _ := result["text"].(string)
	if len(text) != len(body) {
		t.Fatalf("the reply came back %d bytes for a %d byte read", len(text), len(body))
	}
	if text != body {
		t.Fatal("the bytes the destination read did not survive the crossing")
	}
}

// TestThreadToolsLateCollectionStatesTheAnswersAge proves what a caller that
// was away reads when it comes back: the wake names how old the answer is,
// so an agent that resumes an hour later is not told a stale reply is news.
//
// The hour is injected as the destination's settled-at, which is the only
// clock the age is measured against (threadWakeBody). Nothing else is
// arranged: the destination holds the answer in its own receipt, the source
// never polled while it was written, and the pass that collects it is the
// production poller.
func TestThreadToolsLateCollectionStatesTheAnswersAge(t *testing.T) {
	pair := newReachPair(t)
	// The destination's turn stays open, so its own observer settles
	// nothing and the answer below is the receipt's first settlement.
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	installMockClaudeReplies(t, pair.source, "noted")
	// The caller is mid-turn, so the wake queues at the turn boundary and
	// stays there to be read instead of racing a session start.
	if err := pair.source.store.InsertTurn(store.Turn{
		TurnID: pair.caller.ID + "-open", ThreadID: pair.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	spawn := pair.spawnThere(t, "how many rows did the backfill touch?", map[string]any{"notify": true})
	waitUntilE2E(t, 30*time.Second, "the destination starts the spawn", func() bool {
		receipt, found, err := pair.dest.store.GetThreadRequestReceipt(spawn.Token)
		return err == nil && found && receipt.State == store.ThreadReceiptRunning
	})

	answeredAt := time.Now().Add(-time.Hour).UnixMilli()
	settled, err := pair.dest.store.SettleThreadRequestReceipt(spawn.Token, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{
			State:      store.ThreadReceiptReplied,
			Answer:     []byte("the backfill touched 412 rows"),
			AnswerKind: store.ThreadAnswerReply,
			SettledAt:  answeredAt,
		})
	if err != nil || !settled {
		t.Fatalf("settle the receipt an hour ago: settled=%v err=%v", settled, err)
	}

	pair.poll(t)

	row := pair.request(t, spawn.Token)
	if row.State != store.ThreadRequestReplied || row.SettledAt != answeredAt {
		t.Fatalf("the source collected %q settled at %d, want the destination's %d",
			row.State, row.SettledAt, answeredAt)
	}
	var wake store.FlushQueueItem
	waitUntilE2E(t, 10*time.Second, "the answer is queued for the caller", func() bool {
		for _, queued := range durableQueueRows(t, pair.source, pair.caller.ID) {
			if queued.SendID == threadWakeSendID(spawn.Token) {
				wake = queued
				return true
			}
		}
		return false
	})
	if !strings.Contains(wake.Message, ", answered 1h ago)") {
		t.Errorf("wake status line = %q, want the answer's age", wake.Message)
	}
	if !strings.Contains(wake.Message, "the backfill touched 412 rows") {
		t.Errorf("wake body = %q, want the answer itself", wake.Message)
	}
}
