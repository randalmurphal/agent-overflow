package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A worker on a paired computer that this computer's thread spawned, owned
// there through the spawn's link: its questions and approvals reach the
// spawner here, over the real wire, by its owner wait, and the spawner's reply answers them there.

const (
	remoteApproveLine = `{"type":"control_request","request_id":"req-allow","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make deploy"}}}`
	remoteDenyLine    = `{"type":"control_request","request_id":"req-deny","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf build"}}}`
	remoteAskLine     = `{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[` +
		`{"question":"Which module?","header":"Scope","options":[{"label":"parser","description":""},{"label":"lexer","description":""}],"multiSelect":false},` +
		`{"question":"Ship it?","header":"Ship","options":[{"label":"yes","description":""},{"label":"no","description":""}],"multiSelect":false}]}}}`
)

// remotePromptPair is a spawner here whose worker on the paired computer
// asks what its first turn's lines ask.
type remotePromptPair struct {
	*reachPair
	spawn  store.ThreadRequest
	worker string

	mu       sync.Mutex
	started  []string
	startErr error
}

func newRemotePromptPair(t *testing.T, lines ...string) *remotePromptPair {
	t.Helper()
	remotetest.Require(t)
	return newRemotePromptPairOn(t, newReachPair(t), lines...)
}

// newRemotePromptPairOn is newRemotePromptPair on a pair the caller built.
func newRemotePromptPairOn(t *testing.T, pair *reachPair, lines ...string) *remotePromptPair {
	t.Helper()
	routeWorkerPromptsOn(pair.dest, pair.destBus)
	pair.source.triage.SetPromptRouter(pair.source.routeWorkerPrompt)
	p := &remotePromptPair{reachPair: pair}
	// A start for a wake here starts no provider: the wake stays queued for
	// the spawner, as one a session has not taken in yet.
	pair.source.startSessionFn = func(threadID string) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.started = append(p.started, threadID)
		return p.startErr
	}
	installMockClaudeTurns(t, pair.dest, [][]string{append([]string{mockClaudeInitLine}, lines...)})
	ack := pair.spawnThere(t, "work over there", map[string]any{"runtime_mode": string(provider.RuntimeApprovalRequired)})
	p.spawn = pair.request(t, ack.Token)
	p.worker = pair.receipt(t, ack.Token).TargetThreadID
	t.Cleanup(func() { _ = pair.dest.StopSession(p.worker) })
	return p
}

// routeWorkerPromptsOn sends the fixture's router events through the same
// prompt and Agents observers as a running destination.
func routeWorkerPromptsOn(app *App, bus *capturedEventBus) {
	app.triage.SetPromptRouter(app.routeWorkerPrompt)
	bus.mu.Lock()
	bus.tap = app.emit
	bus.mu.Unlock()
}

// sweep runs this computer's background sweep, the production pace
// shortened, as the one thing that asks the paired computer.
func (p *remotePromptPair) sweep() {
	p.source.threadPollOverride = 100 * time.Millisecond
	p.source.startThreadRequestSweeps()
}

func (p *remotePromptPair) readOwnerState(t *testing.T, owner *App) {
	t.Helper()
	if err := owner.readOwnerChanges(t.Context(), p.computer, p.dest.transportServer.Load().LaunchID(), []string{p.spawn.Token}, nil); err != nil {
		t.Fatal(err)
	}
}

func (p *remotePromptPair) drain(t *testing.T) {
	t.Helper()
	for _, app := range []*App{p.source, p.dest} {
		for range 3 {
			if err := app.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			app.waitAgentNoticeWork()
		}
	}
}

// openThere waits until the worker's n prompts are open there, routed to
// the spawner here and held for it, and returns them by request id. Triage
// routes a prompt before the prompts' queue registers it held, which is
// what an agent_rows call there lists.
func (p *remotePromptPair) openThere(t *testing.T, n int) map[string]provider.PromptRoute {
	t.Helper()
	routes := map[string]provider.PromptRoute{}
	waitUntil(t, 15*time.Second, func() bool {
		if len(p.dest.workerPrompts.heldFor(p.spawn.Token, p.worker)) != n {
			return false
		}
		pending := p.dest.triage.PendingInteractiveRequests(p.worker)
		clear(routes)
		for _, request := range pending.Approvals {
			if request.RoutedTo != nil {
				routes[request.RequestID] = *request.RoutedTo
			}
		}
		for _, request := range pending.UserInputs {
			if request.RoutedTo != nil {
				routes[request.RequestID] = *request.RoutedTo
			}
		}
		return len(routes) == n
	})
	return routes
}

// routeThere is where the worker's prompt under requestID is routed there,
// nil when it is the person's; open is false once it is answered.
func (p *remotePromptPair) routeThere(requestID string) (route *provider.PromptRoute, open bool) {
	pending := p.dest.triage.PendingInteractiveRequests(p.worker)
	for _, request := range pending.Approvals {
		if request.RequestID == requestID {
			return request.RoutedTo, true
		}
	}
	for _, request := range pending.UserInputs {
		if request.RequestID == requestID {
			return request.RoutedTo, true
		}
	}
	return nil, false
}

// remoteWakes is the remote prompt wakes threadID's queue holds.
func remoteWakes(t *testing.T, app *App, threadID string) []workerPromptWake {
	t.Helper()
	durable, _ := queuedSendIDs(t, app, threadID)
	var wakes []workerPromptWake
	for _, id := range durable {
		if wake, ok := parseWorkerPromptWake(id); ok && wake.delivery != "" {
			wakes = append(wakes, wake)
		}
	}
	return wakes
}

// wakesFor waits until threadID's queue holds n remote prompt wakes.
func (p *remotePromptPair) wakesFor(t *testing.T, threadID string, n int) []workerPromptWake {
	t.Helper()
	var wakes []workerPromptWake
	waitUntil(t, 15*time.Second, func() bool {
		p.drain(t)
		wakes = remoteWakes(t, p.source, threadID)
		return len(wakes) == n
	})
	return wakes
}

// requestOf is the request id of the prompt held there under token.
func (p *remotePromptPair) requestOf(t *testing.T, token string) string {
	t.Helper()
	prompt, found := p.dest.workerPrompts.get(token)
	if !found {
		t.Fatalf("no prompt is held there under %s", token)
	}
	return prompt.requestID
}

func (p *remotePromptPair) reply(t *testing.T, caller threadtools.Caller, call threadtools.ReplyCall) (threadtools.ReplyAck, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return threadToolsApp{app: p.source}.Reply(ctx, caller, call)
}

// Driven by the background sweep alone, with no listing read: each of the
// worker's prompts there wakes its spawner here with a wake that names the
// worker's computer, and the spawner's thread_reply answers them there,
// an approval allowed, one denied and a two-part question.
func TestARemoteWorkersPromptsWakeItsSpawnerAndItsRepliesAnswerThem(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine, remoteDenyLine, remoteAskLine)
	routes := p.openThere(t, 3)
	for id, route := range routes {
		if route.ThreadID != p.caller.ID || route.ComputerID != p.sourceID || route.ComputerName != "source" {
			t.Fatalf("%s is routed to %+v, want the spawner on this computer", id, route)
		}
	}
	p.sweep()
	wakes := p.wakesFor(t, p.caller.ID, 3)
	p.mu.Lock()
	if !slices.Contains(p.started, p.caller.ID) {
		t.Errorf("the spawner was not started for its wakes: %q", p.started)
	}
	p.mu.Unlock()
	byRequest := map[string]string{}
	for _, wake := range wakes {
		byRequest[p.requestOf(t, wake.token)] = wake.token
	}
	queued, err := p.source.GetQueueState(p.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, item := range queued {
		if wake, ok := parseWorkerPromptWake(item.SendID); !ok || wake.token != byRequest["req-allow"] {
			continue
		}
		checked++
		name := p.source.remoteComputerNames()[p.computer]
		if !strings.Contains(item.Message, ") on \""+name+"\", which you spawned") || !strings.Contains(item.Message, "token "+byRequest["req-allow"]) {
			t.Errorf("the wake reads %q, want it to name the worker on %q and the token", item.Message, name)
		}
		if item.OriginWake == nil || item.OriginWake.ThreadID != p.worker || item.OriginWake.ComputerID != p.computer || item.OriginWake.Computer != name {
			t.Errorf("the wake's reporter = %+v, want the worker on %s", item.OriginWake, p.computer)
		}
	}
	if checked != 1 {
		t.Fatalf("the queue holds %d wakes for the approval, want one", checked)
	}
	caller := p.callerIdentity()
	for _, call := range []threadtools.ReplyCall{
		{Token: byRequest["req-allow"], Decision: threadtools.DecisionAllow, Text: "go ahead"},
		{Token: byRequest["req-deny"], Decision: threadtools.DecisionDeny, Text: "not that"},
		{Token: byRequest["req-ask"], Answers: []string{"parser", "yes"}, Text: "parser, then ship"},
	} {
		ack, err := p.reply(t, caller, call)
		// The worker's thread is there, named by that computer's id here.
		if err != nil || !ack.Accepted || !ack.Prompt || ack.SourceThreadID != p.worker || ack.SourceComputerID != p.spawn.TargetComputerID {
			t.Fatalf("the reply on %s = %+v, %v", call.Token, ack, err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for left := p.dest.triage.PendingInteractiveRequests(p.worker); len(left.Approvals)+len(left.UserInputs) > 0; left = p.dest.triage.PendingInteractiveRequests(p.worker) {
		if time.Now().After(deadline) {
			t.Fatalf("the worker still waits on %+v after the replies", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Answered, none is held here any more, and the worker lists none.
	if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
		t.Fatalf("this computer still holds %q", held)
	}
	listing, _, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil || len(listing.Prompts) != 0 {
		t.Fatalf("the worker lists %+v, %v; want no prompts", listing.Prompts, err)
	}
}

// A prompt the spawner's computer keeps asking about but never takes, its
// own registry stopped, stays its spawner's there: the person there is not
// asked.
func TestAPromptItsSpawnerNeverTakesStaysItsSpawnersThere(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.source.workerPrompts.gate.Lock()
	p.source.workerPrompts.closed = true
	p.source.workerPrompts.gate.Unlock()
	waitUntil(t, 15*time.Second, func() bool { return p.announcedRoutedHere("req-allow") })
	p.sweep()
	time.Sleep(3 * time.Second)
	if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID || p.announcedToPerson("req-allow") {
		t.Fatalf("the prompt there is routed to %+v (open %v); want it its spawner's, the person not asked", route, open)
	}
}

// An answer read late that leaves out a prompt an earlier answer listed
// closes it, and the next answer that lists it again puts it to the
// spawner again under a new delivery.
func TestARemotePromptLeftOutOfALateAnswerIsDeliveredAgain(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	listing, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil || len(listing.Prompts) != 1 {
		t.Fatalf("the listing there: %+v, %v; want one prompt", listing.Prompts, err)
	}
	reconcile := func(listed []ThreadAgentPrompt) {
		done := p.source.noteRemotePrompts(p.source.ownerReads.start(t.Context(), p.computer, nil), origin, p.spawn, ThreadAgentRows{Prompts: listed}, nil)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		p.drain(t)
	}
	reconcile(listing.Prompts)
	first := p.wakesFor(t, p.caller.ID, 1)[0]
	reconcile(nil)
	p.wakesFor(t, p.caller.ID, 0)
	reconcile(listing.Prompts)
	again := p.wakesFor(t, p.caller.ID, 1)[0]
	if again.token != first.token || again.delivery == first.delivery {
		t.Fatalf("the prompt listed again was delivered as %+v after %+v, want the same token under a new delivery", again, first)
	}
}

// Only a prompt's current delivery is live, takes a hold, or records the
// thread its wake moved to: an earlier delivery's callbacks change nothing.
func TestOnlyARemotePromptsCurrentDeliveryCounts(t *testing.T) {
	t.Parallel()
	key := remotePromptKey{computerID: "desk", token: "prompt-1"}
	r := &remoteWorkerPrompts{byKey: map[remotePromptKey]*remoteWorkerPrompt{
		key: {key: key, holder: "the-holder", delivery: "boot-2", state: remotePromptRouted},
	}}
	if r.live(key, "boot-1") || !r.live(key, "boot-2") {
		t.Fatal("live does not follow the current delivery")
	}
	r.hold(key, "boot-1")
	r.moveTo(key, "boot-1", "another-thread")
	if prompt := r.byKey[key]; prompt.held || prompt.holder != "the-holder" {
		t.Fatalf("an earlier delivery changed the prompt: %+v", prompt)
	}
	r.hold(key, "boot-2")
	r.moveTo(key, "boot-2", "the-successor")
	if prompt := r.byKey[key]; !prompt.held || prompt.holder != "the-successor" {
		t.Fatalf("the current delivery did not take: %+v", prompt)
	}
}

// The person answers there: the spawner's wake is withdrawn here, and its
// reply, before this computer heard and after, is refused as closed.
func TestAPersonsAnswerThereWithdrawsTheWakeAndRefusesALateReply(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	wake := p.wakesFor(t, p.caller.ID, 1)[0]
	prompt, _ := p.dest.workerPrompts.get(wake.token)
	if err := p.dest.RespondToApproval(t.Context(), p.worker, provider.ApprovalResponse{
		RequestID: "req-allow", Occurrence: prompt.occurrence, Decision: "accept",
	}); err != nil {
		t.Fatalf("the person's answer there: %v", err)
	}
	call := threadtools.ReplyCall{Token: wake.token, Decision: threadtools.DecisionDeny, Text: "no"}
	if _, err := p.reply(t, p.callerIdentity(), call); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("a reply after the person answered: %v, want it refused as closed", err)
	}
	p.wakesFor(t, p.caller.ID, 0)
	if _, err := p.reply(t, p.callerIdentity(), call); !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("a reply once the wake was withdrawn: %v, want it refused as closed", err)
	}
}

// The spawner's session cannot start after its wake was queued: the
// prompt stays its spawner's there, and its wake stays queued here for the
// spawner's next session, as a local worker's does; this computer starts
// nothing more for it while that computer keeps asking.
func TestAPromptWhoseSpawnerCannotStartWaitsForItsNextSession(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.startErr = errors.New("the provider binary is missing")
	p.openThere(t, 1)
	p.sweep()
	waitUntil(t, 15*time.Second, func() bool {
		p.drain(t)
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.started) > 0
	})
	p.mu.Lock()
	starts := len(p.started)
	p.mu.Unlock()
	for range 3 {
		p.readOwnerState(t, p.source)
		p.drain(t)
	}
	if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID || p.announcedToPerson("req-allow") {
		t.Fatalf("the prompt is routed to %+v (open %v) while this computer keeps asking, want it its spawner's", route, open)
	}
	p.mu.Lock()
	again := len(p.started)
	p.mu.Unlock()
	if again != starts {
		t.Fatalf("the spawner was started %d more times for a prompt it did not take", again-starts)
	}
	p.wakesFor(t, p.caller.ID, 1)
}

// handOffOn runs New Thread, Same Assistant on from.
func handOffOn(t *testing.T, app *App, from string) store.Thread {
	t.Helper()
	next, err := app.nextAssistantThread(context.Background(), from, false)
	if err != nil {
		t.Fatalf("New Thread, Same Assistant: %v", err)
	}
	return next
}

// announcedToPerson reports whether the paired computer announced the
// worker's prompt under requestID with no route: what notifies its person.
func (p *remotePromptPair) announcedToPerson(requestID string) bool {
	p.destBus.mu.Lock()
	defer p.destBus.mu.Unlock()
	for _, event := range p.destBus.all {
		if approval, ok := event.Data.(provider.ApprovalEvent); ok && approval.Action == "request" && approval.Request != nil &&
			approval.Request.RequestID == requestID && approval.Request.ThreadID == p.worker && approval.Request.RoutedTo == nil {
			return true
		}
	}
	return false
}

// announcedRoutedHere reports whether the paired computer announced the
// worker's prompt under requestID routed to the spawner here.
func (p *remotePromptPair) announcedRoutedHere(requestID string) bool {
	p.destBus.mu.Lock()
	defer p.destBus.mu.Unlock()
	for _, event := range p.destBus.all {
		if approval, ok := event.Data.(provider.ApprovalEvent); ok && approval.Action == "request" && approval.Request != nil &&
			approval.Request.RequestID == requestID && approval.Request.ThreadID == p.worker &&
			approval.Request.RoutedTo != nil && approval.Request.RoutedTo.ThreadID == p.caller.ID {
			return true
		}
	}
	return false
}

func (p *remotePromptPair) keyOf(token string) remotePromptKey {
	return remotePromptKey{computerID: p.computer, token: token}
}

// The prompt follows an assistant's handoffs here: one that lands before
// this computer picked it up delivers it to the successor, and two after
// it was delivered move it on under new deliveries. A late hand-back of an
// earlier delivery changes nothing, the earlier threads' replies are
// refused, the last successor's answers it there, and the person there is
// never asked.
func TestARemoteWorkersPromptFollowsTheAssistantsHandoffs(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	testAssistant(t, p.source, p.caller.ID)
	p.openThere(t, 1)
	second := handOffOn(t, p.source, p.caller.ID)
	p.readOwnerState(t, p.source)
	first := p.wakesFor(t, second.ID, 1)[0]
	third := handOffOn(t, p.source, second.ID)
	p.drain(t)
	fourth := handOffOn(t, p.source, third.ID)
	p.readOwnerState(t, p.source)
	last := p.wakesFor(t, fourth.ID, 1)[0]
	if last.token != first.token || last.delivery == first.delivery {
		t.Fatalf("the successor's wake is %+v, want the same prompt under a new delivery than %+v", last, first)
	}
	for _, earlier := range []string{p.caller.ID, second.ID, third.ID} {
		if wakes := remoteWakes(t, p.source, earlier); len(wakes) != 0 {
			t.Fatalf("an earlier thread %s still holds %+v", earlier, wakes)
		}
	}
	// A delayed withdrawal or start failure of the first delivery.
	p.source.handBackWorkerPromptWake(workerPromptSendIDRoot + first.token + ":" + first.delivery)
	p.source.remotePromptNotTaken(p.keyOf(first.token), first.delivery)
	p.drain(t)
	if wakes := remoteWakes(t, p.source, fourth.ID); len(wakes) != 1 || wakes[0] != last {
		t.Fatalf("after a late hand-back of the first delivery the successor holds %+v", wakes)
	}
	if held := p.source.heldRemotePrompts(p.spawn); !slices.Equal(held, []string{last.token}) {
		t.Fatalf("this computer holds %q, want the prompt", held)
	}
	if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID {
		t.Fatalf("the prompt there is routed to %+v (open %v), want the spawner here", route, open)
	}
	call := threadtools.ReplyCall{Token: last.token, Decision: threadtools.DecisionAllow, Text: "go"}
	for _, earlier := range []string{p.caller.ID, second.ID} {
		if _, err := p.reply(t, threadtools.Caller{ThreadID: earlier}, call); err == nil || !strings.Contains(err.Error(), "another thread") {
			t.Fatalf("an earlier thread's reply: %v, want it refused", err)
		}
	}
	if ack, err := p.reply(t, threadtools.Caller{ThreadID: fourth.ID}, call); err != nil || !ack.Accepted {
		t.Fatalf("the successor's reply = %+v, %v", ack, err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, open := p.routeThere("req-allow")
		return !open
	})
	if p.announcedToPerson("req-allow") {
		t.Fatal("the person there was asked")
	}
}

// A holder that can no longer answer gives the prompt back at once: the
// person there gets it, and nothing is left queued here. A rest stops the
// worker there too, which closes it, and the next read of that computer's
// prompts ends its hold here.
func TestAPromptGoesBackWhenItsHolderCannotAnswer(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*testing.T, *remotePromptPair){
		"archived": func(t *testing.T, p *remotePromptPair) {
			if _, err := p.source.ArchiveThread(p.caller.ID); err != nil {
				t.Fatal(err)
			}
		},
		"deleted": func(t *testing.T, p *remotePromptPair) {
			if _, err := p.source.DeleteThread(p.caller.ID); err != nil {
				t.Fatal(err)
			}
		},
		"brought to rest": func(t *testing.T, p *remotePromptPair) {
			var result ThreadAgentStop
			p.source.stopThreadAgentTarget(t.Context(), p.caller.ID, map[string]bool{p.caller.ID: true}, &result)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newRemotePromptPair(t, remoteApproveLine)
			p.openThere(t, 1)
			p.readOwnerState(t, p.source)
			wake := p.wakesFor(t, p.caller.ID, 1)[0]
			end(t, p)
			waitUntil(t, 15*time.Second, func() bool {
				route, _ := p.routeThere("req-allow")
				return route == nil
			})
			if _, open := p.routeThere("req-allow"); open && !p.announcedToPerson("req-allow") {
				t.Fatal("the prompt went back without being announced to the person there")
			}
			p.readOwnerState(t, p.source)
			p.drain(t)
			if held := p.source.heldRemotePrompts(p.spawn); len(held) != 0 {
				t.Fatalf("this computer still holds %q", held)
			}
			if _, err := p.reply(t, threadtools.Caller{ThreadID: p.caller.ID}, threadtools.ReplyCall{Token: wake.token, Decision: threadtools.DecisionAllow}); err == nil {
				t.Fatal("a reply to the prompt given back was taken")
			}
		})
	}
}

// A prompt that arrives after its spawner here was archived, owing the
// worker nothing (as when its computer predates owner operations), is
// given back at once: nothing is queued to the archived thread.
func TestAPromptForAnArchivedSpawnerGoesBackAtOnce(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	if _, _, err := p.source.store.ArchiveThread(p.caller.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		p.readOwnerState(t, p.source)
		p.drain(t)
		route, open := p.routeThere("req-allow")
		return open && route == nil && p.announcedToPerson("req-allow")
	})
	if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
		t.Fatalf("the archived spawner holds wakes %+v", wakes)
	}
}

// This computer restarts after the spawner's wake was queued and before it
// replied, inside the lease: the spawner is asked again under a new
// delivery and its reply answers the prompt there.
func TestARemotePromptIsAskedAgainAfterTheSpawnersComputerRestarts(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	before := p.wakesFor(t, p.caller.ID, 1)[0]

	restarted, _ := setupE2EAppOn(t, p.dbPath, p.settingsDir)
	restarted.configDir = p.source.configDir
	restarted.backends = p.manager
	publishReachIdentity(t, restarted, p.sourceID)
	restarted.startSessionFn = func(string) error { return nil }
	restarted.restoreDurableFlushQueueAtBoot()
	if wakes := remoteWakes(t, restarted, p.caller.ID); len(wakes) != 0 {
		t.Fatalf("the boot kept the previous boot's wakes %+v", wakes)
	}
	var after workerPromptWake
	waitUntil(t, 15*time.Second, func() bool {
		p.readOwnerState(t, restarted)
		if err := restarted.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		restarted.waitAgentNoticeWork()
		wakes := remoteWakes(t, restarted, p.caller.ID)
		if len(wakes) == 1 {
			after = wakes[0]
		}
		return len(wakes) == 1
	})
	if after.token != before.token || after.delivery == before.delivery {
		t.Fatalf("after the restart the wake is %+v, want %s under a new delivery", after, before.token)
	}
	ack, err := threadToolsApp{app: restarted}.Reply(t.Context(), threadtools.Caller{ThreadID: p.caller.ID},
		threadtools.ReplyCall{Token: after.token, Decision: threadtools.DecisionAllow, Text: "go"})
	if err != nil || !ack.Accepted {
		t.Fatalf("the spawner's reply after the restart = %+v, %v", ack, err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, open := p.routeThere("req-allow")
		return !open
	})
}

// The paired computer takes the spawner's answer and the reply to the call
// is lost: the call is tried again, which that computer acknowledges as the
// answer it already has.
func TestARemoteAnswerWhoseAcknowledgementIsLostIsAcknowledgedOnTheRetry(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	p := newRemotePromptPairOn(t, pair, remoteApproveLine)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	wake := p.wakesFor(t, p.caller.ID, 1)[0]
	before := faults.calls.Load()
	faults.dropReplies.Store(before + 1)
	ack, err := p.reply(t, p.callerIdentity(), threadtools.ReplyCall{Token: wake.token, Decision: threadtools.DecisionAllow, Text: "go"})
	if err != nil {
		t.Fatalf("the reply whose acknowledgement was lost: %v", err)
	}
	if got := faults.calls.Load() - before; got != 2 {
		t.Fatalf("the paired computer answered %d calls, want the reply and its retry", got)
	}
	if !ack.Prompt || ack.Accepted {
		t.Fatalf("the retry's ack = %+v, want the answer acknowledged as already taken", ack)
	}
	waitUntil(t, 15*time.Second, func() bool {
		_, open := p.routeThere("req-allow")
		return !open
	})
}

// A wake the spawner did not take in, here withdrawn from its queue by the
// person, ends that delivery and keeps the prompt for its holder's next
// turn; the hand-back of a delivery a handoff replaced ends nothing.
func TestAWithdrawnRemoteWakeEndsItsOwnHold(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	testAssistant(t, p.source, p.caller.ID)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	first := p.wakesFor(t, p.caller.ID, 1)[0]
	next := handOffOn(t, p.source, p.caller.ID)
	p.drain(t)
	current := p.wakesFor(t, next.ID, 1)[0]
	if _, err := p.source.removeQueuedItem(t.Context(), next.ID, workerPromptSendIDRoot+first.token+":"+first.delivery); err != nil {
		t.Fatal(err)
	}
	p.source.handBackWorkerPromptWake(workerPromptSendIDRoot + first.token + ":" + first.delivery)
	p.drain(t)
	if held := p.source.heldRemotePrompts(p.spawn); !slices.Equal(held, []string{current.token}) {
		t.Fatalf("after the replaced delivery's hand-back this computer holds %q, want the prompt", held)
	}
	if removal, err := p.source.withdrawQueuedMessage(t.Context(), next.ID, workerPromptSendIDRoot+current.token+":"+current.delivery,
		queuedWithdrawal{byPerson: true}); err != nil || removal != queueRemovalRemoved {
		t.Fatalf("the person's withdrawal: %v, %v", removal, err)
	}
	p.drain(t)
	p.readOwnerState(t, p.source)
	p.drain(t)
	if wakes := remoteWakes(t, p.source, next.ID); len(wakes) != 0 {
		t.Fatalf("after its wake was withdrawn the holder was given %+v before its next turn", wakes)
	}
	if held := p.source.heldRemotePrompts(p.spawn); !slices.Equal(held, []string{current.token}) {
		t.Fatalf("after its wake was withdrawn this computer holds %q, want the prompt kept for its holder", held)
	}
	if route, open := p.routeThere("req-allow"); !open || route == nil || route.ThreadID != p.caller.ID {
		t.Fatalf("the prompt there is routed to %+v (open %v), want its spawner here", route, open)
	}
	// The holder's next turn puts it to the holder again, as a new delivery.
	// This fixture's spawner computer has no event bus, so its turn start is
	// handed to the prompts' observer as its bus would.
	p.source.tapWorkerPrompt(eventchan.ProviderTurnStarted, triage.TurnStartedEvent{ThreadID: next.ID, TurnID: next.ID + ":next"})
	p.drain(t)
	if again := p.wakesFor(t, next.ID, 1)[0]; again.token != current.token || again.delivery == current.delivery {
		t.Fatalf("the holder's next turn was given %+v after %+v, want the prompt under a new delivery", again, current)
	}
}

// While assistants are off here, an assistant's worker there still asks the
// assistant: the person there is never asked, a prompt asked during the
// pause waits in the assistant's queue, and turning assistants on starts the
// assistant for what waits there.
func TestAPausedAssistantsWorkerThereWaitsForIt(t *testing.T) {
	t.Parallel()
	p := newRemotePromptPair(t, remoteApproveLine)
	testAssistant(t, p.source, p.caller.ID)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	p.wakesFor(t, p.caller.ID, 1)
	switchAssistants(t, p.source, false)
	p.source.convergeAssistants()
	askOn(t, p.dest, p.worker, "approval-2", false)
	waitUntil(t, 15*time.Second, func() bool {
		p.readOwnerState(t, p.source)
		p.drain(t)
		return slices.ContainsFunc(remoteWakes(t, p.source, p.caller.ID), func(wake workerPromptWake) bool {
			return p.requestOf(t, wake.token) == "approval-2"
		})
	})
	for _, request := range []string{"req-allow", "approval-2"} {
		if route, open := p.routeThere(request); !open || route == nil || route.ThreadID != p.caller.ID || p.announcedToPerson(request) {
			t.Fatalf("%s is routed to %+v (open %v) while assistants are off, want it the paused assistant's", request, route, open)
		}
	}
	p.mu.Lock()
	before := len(p.started)
	p.mu.Unlock()
	switchAssistants(t, p.source, true)
	p.source.convergeAssistants()
	waitUntil(t, 15*time.Second, func() bool {
		p.drain(t)
		p.mu.Lock()
		defer p.mu.Unlock()
		return slices.Contains(p.started[before:], p.caller.ID)
	})
}

// A send to the worker after it rested, which then asks something, is
// wakes its spawner even after the original request left the poll.
func TestASendThatBlocksARestedRemoteWorkerWakesItsSpawner(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	routeWorkerPromptsOn(pair.dest, pair.destBus)
	pair.source.startSessionFn = func(string) error { return nil }
	installMockClaudeTurns(t, pair.dest, [][]string{
		{mockClaudeInitLine, `{"type":"result","subtype":"success","is_error":false}`},
		{remoteApproveLine},
	})
	ack := pair.spawnThere(t, "work over there", map[string]any{"runtime_mode": string(provider.RuntimeApprovalRequired)})
	worker := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 15*time.Second, func() bool {
		pair.poll(t)
		return threadRequestSettled(pair.request(t, ack.Token))
	})
	// Past its late-reply window, the spawn is no longer polled.
	pair.source.retireThreadRequestPoll(ack.Token)
	startReconciledOwner(t, pair)
	send, err := threadToolsApp{app: pair.source}.Send(t.Context(), pair.callerIdentity(), threadtools.SendCall{
		ThreadID: worker, ComputerID: pair.computer, Message: "one more thing",
	})
	if err != nil {
		t.Fatalf("the send: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		pending := pair.dest.triage.PendingInteractiveRequests(worker).Approvals
		return len(pending) == 1 && pending[0].RoutedTo != nil
	})
	waitUntil(t, 15*time.Second, func() bool {
		pair.poll(t)
		if err := pair.source.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		pair.source.waitAgentNoticeWork()
		return len(remoteWakes(t, pair.source, pair.caller.ID)) == 1
	})
	if row := pair.request(t, send.Token); threadRequestSettled(row) {
		t.Fatalf("the send settled while its worker waits: %+v", row)
	}
	_ = pair.dest.StopSession(worker)
}
