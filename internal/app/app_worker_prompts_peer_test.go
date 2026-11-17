package app

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// The computer that holds a worker a paired computer's spawn owns: the
// prompts it routes to that spawn's thread, held for that computer.

// linkedWorkers is a computer with workers a paired computer's lead spawned
// here, each owned through its own spawn's link, and a local worker a
// thread here spawned.
type linkedWorkers struct {
	app    *App
	events *emitRecorder
	device string
	lead   string
	// workers and spawns are the linked workers and their spawn tokens.
	workers []store.Thread
	spawns  []string
	// local is a worker a thread here spawned.
	local store.Thread
}

const linkedLeadComputer, linkedLeadComputerName = "desk-computer", "Desk"

func newLinkedWorkers(t *testing.T, n int) *linkedWorkers {
	t.Helper()
	return newLinkedWorkersAt(t, n, 0)
}

// newLinkedWorkersAt is newLinkedWorkers with links accepted at createdAt,
// zero for now.
func newLinkedWorkersAt(t *testing.T, n int, createdAt int64) *linkedWorkers {
	t.Helper()
	a, events := newAppForFlushQueueRPC(t)
	a.triage = triage.NewRouter(a.store, a.emit)
	a.configureTriageQueueCallbacks()
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	a.startSessionFn = func(string) error { return errors.New("no provider in this test") }
	f := &linkedWorkers{app: a, events: events, device: reachableDevice(t, a.store), lead: "6f1c2a4e-9d3b-4c1a-8e2f-0a1b2c3d4e5f"}
	for i := range n {
		worker := noticeTestThread(t, a, "linked-worker-"+string(rune('a'+i)))
		token := "linked-spawn-" + string(rune('a'+i))
		if _, _, err := a.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
			Token: token, OwnerDeviceID: f.device, SourceComputerID: linkedLeadComputer, SourceComputerName: linkedLeadComputerName,
			SourceThreadID: f.lead, SourceThreadTitle: "the lead", Kind: store.ThreadRequestSpawn,
			SpawnLink: true, State: store.ThreadReceiptRunning, CreatedAt: createdAt,
		}); err != nil {
			t.Fatal(err)
		}
		if named, _, err := a.store.SetThreadReceiptTarget(token, worker.ID, true); err != nil || !named {
			t.Fatalf("name the link: %v, %v", named, err)
		}
		f.workers, f.spawns = append(f.workers, worker), append(f.spawns, token)
	}
	spawner := noticeTestThread(t, a, "local-spawner")
	f.local = noticeTestThread(t, a, "local-worker")
	if err := a.store.InsertThreadRequest(store.ThreadRequest{
		Token: "local-spawn", CallerThreadID: spawner.ID, TargetThreadID: f.local.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *linkedWorkers) drain(t *testing.T) {
	t.Helper()
	for range 3 {
		if err := f.app.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		f.app.waitAgentNoticeWork()
	}
}

// ask opens an approval on worker i and returns the token it is held under.
func (f *linkedWorkers) ask(t *testing.T, i int) string {
	t.Helper()
	askOn(t, f.app, f.workers[i].ID, "approval-1", false)
	f.drain(t)
	held := f.app.workerPrompts.heldFor(f.spawns[i], f.workers[i].ID)
	if len(held) != 1 {
		t.Fatalf("worker %d holds %d prompts for its spawner there, want one", i, len(held))
	}
	return held[0].token
}

// route is where worker's approval-1 is routed now, and whether it is open.
func (f *linkedWorkers) route(t *testing.T, worker string) *provider.PromptRoute {
	t.Helper()
	pending := f.app.triage.PendingInteractiveRequests(worker).Approvals
	if len(pending) != 1 {
		t.Fatalf("%s has approvals %+v, want one open", worker, pending)
	}
	return pending[0].RoutedTo
}

// list is agent_rows for worker i as its spawn's computer asks it.
func (f *linkedWorkers) list(t *testing.T, i int, held ...string) []ThreadAgentPrompt {
	t.Helper()
	prompts, err := f.app.peerPromptsOf(f.device, f.lead, f.workers[i].Title,
		threadAgentPeerArgs{ThreadID: f.workers[i].ID, Spawn: f.spawns[i], Held: held})
	if err != nil {
		t.Fatal(err)
	}
	return prompts
}

// call is agent_prompt_reply or agent_prompt_return as a paired computer
// sends it.
func (f *linkedWorkers) call(t *testing.T, device, source, tool string, args threadPromptPeerArgs) error {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.app.servePeerPrompt(t.Context(), device, ThreadPeerCall{
		Tool: tool, Args: encoded, Source: threadtools.Caller{ThreadID: source},
	})
	return err
}

// handedToPerson reports whether the events carry worker's approval
// announced again with no route: the announcement that notifies the person.
func (f *linkedWorkers) handedToPerson(worker string) bool {
	for _, call := range f.events.snapshot() {
		if call.Channel != string(eventchan.ProviderApproval) {
			continue
		}
		if event, ok := call.Data.(provider.ApprovalEvent); ok && event.Action == "request" &&
			event.ThreadID == worker && event.Request != nil && event.Request.RoutedTo == nil {
			return true
		}
	}
	return false
}

// A linked worker's approval is routed to its spawner on the paired
// computer, named with that computer, and held for it: nothing is queued
// here, the person is not asked, and only the link's own agent_rows, from
// its device and for its source thread, lists it.
func TestALinkedWorkersPromptIsHeldForItsSpawnerThere(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	token := f.ask(t, 0)
	worker := f.workers[0].ID
	route := f.route(t, worker)
	if route == nil || route.ThreadID != f.lead || route.ComputerID != linkedLeadComputer || route.ComputerName != linkedLeadComputerName {
		t.Fatalf("the approval is routed to %+v, want the lead on Desk", route)
	}
	if f.handedToPerson(worker) {
		t.Fatal("the person was asked about a prompt routed to the spawner there")
	}
	if waiting := f.app.threadWaitingOn(worker); waiting.routedTo() != "the lead on Desk" {
		t.Fatalf("the worker's row reads routed to %q, want the lead on Desk", waiting.routedTo())
	}
	listed := f.list(t, 0)
	if len(listed) != 1 || listed[0].Token != token || listed[0].Title != f.workers[0].Title ||
		listed[0].Approval == nil || listed[0].Approval.ToolName != "Bash" || listed[0].Approval.Description != "make deploy" {
		t.Fatalf("the link's listing = %+v, want the approval under %s", listed, token)
	}
	for name, args := range map[string]struct {
		device, source string
		call           threadAgentPeerArgs
	}{
		"no spawn named":   {f.device, f.lead, threadAgentPeerArgs{ThreadID: worker}},
		"another spawn":    {f.device, f.lead, threadAgentPeerArgs{ThreadID: worker, Spawn: "another-spawn"}},
		"another device":   {"other-device", f.lead, threadAgentPeerArgs{ThreadID: worker, Spawn: f.spawns[0]}},
		"another source":   {f.device, "6f1c2a4e-9d3b-4c1a-8e2f-000000000000", threadAgentPeerArgs{ThreadID: worker, Spawn: f.spawns[0]}},
		"another thread's": {f.device, f.lead, threadAgentPeerArgs{ThreadID: f.local.ID, Spawn: f.spawns[0]}},
	} {
		prompts, err := f.app.peerPromptsOf(args.device, args.source, "", args.call)
		if err != nil || len(prompts) != 0 {
			t.Errorf("%s: listed %+v, %v; want nothing", name, prompts, err)
		}
	}
}

// A held prompt goes to the person here once its lease runs out after it
// was registered or last held, with the announcement that notifies them;
// one the paired computer keeps naming as held stays routed there, and
// listing it without naming it renews nothing.
func TestAHeldPromptGoesToThePersonWhenItsLeaseRunsOut(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 2)
	f.app.workerPromptLeaseOverride = 400 * time.Millisecond
	kept := f.ask(t, 0)
	f.ask(t, 1)
	deadline := time.Now().Add(10 * time.Second)
	for f.route(t, f.workers[1].ID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("a prompt nobody held is still routed to the spawner there past its lease")
		}
		f.list(t, 0, kept)
		f.list(t, 1)
		time.Sleep(50 * time.Millisecond)
	}
	if !f.handedToPerson(f.workers[1].ID) {
		t.Fatal("the prompt whose lease ran out was not announced to the person")
	}
	if route := f.route(t, f.workers[0].ID); route == nil || route.ThreadID != f.lead {
		t.Fatalf("the prompt held throughout is routed to %+v, want the lead there", route)
	}
	// No longer named as held, it goes too.
	waitUntil(t, 10*time.Second, func() bool { return f.route(t, f.workers[0].ID) == nil })
}

// Only the link's own device, for its own source thread and spawn, may
// renew, answer or give back a held prompt. Each refusal leaves the prompt
// as it was; the binding's own call gives it back to the person.
func TestTheBindingAdmitsOnlyTheLinksOwnPrompt(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 2)
	f.app.workerPromptLeaseOverride = 300 * time.Millisecond
	first, second := f.ask(t, 0), f.ask(t, 1)
	askOn(t, f.app, f.local.ID, "approval-1", false)
	f.drain(t)
	local := f.app.workerPrompts.current[workerPromptKey{workerID: f.local.ID, requestID: "approval-1"}]
	if local == "" {
		t.Fatal("the local worker's prompt was not registered")
	}
	other := "6f1c2a4e-9d3b-4c1a-8e2f-000000000000"
	for name, tc := range map[string]struct {
		device, source string
		args           threadPromptPeerArgs
	}{
		"another worker's token": {f.device, f.lead, threadPromptPeerArgs{Spawn: f.spawns[0], Token: second}},
		"a wrong spawn":          {f.device, f.lead, threadPromptPeerArgs{Spawn: "another-spawn", Token: first}},
		"another source thread":  {f.device, other, threadPromptPeerArgs{Spawn: f.spawns[0], Token: first}},
		"another device":         {"other-device", f.lead, threadPromptPeerArgs{Spawn: f.spawns[0], Token: first}},
		"a local prompt's token": {f.device, f.lead, threadPromptPeerArgs{Spawn: f.spawns[0], Token: local}},
	} {
		for _, tool := range []string{threadAgentPeerPromptReturn, threadAgentPeerPromptReply} {
			args := tc.args
			if tool == threadAgentPeerPromptReply {
				args.Decision = threadtools.DecisionAllow
			}
			if err := f.call(t, tc.device, tc.source, tool, args); !errors.Is(err, errPeerPromptNotYours) {
				t.Errorf("%s, %s: %v; want it refused as not yours", name, tool, err)
			}
		}
		if prompts, err := f.app.peerPromptsOf(tc.device, tc.source, "", threadAgentPeerArgs{
			ThreadID: f.workers[0].ID, Spawn: tc.args.Spawn, Held: []string{tc.args.Token},
		}); err != nil || (len(prompts) > 0 && tc.args.Spawn != f.spawns[0]) {
			t.Errorf("%s: listed %+v, %v", name, prompts, err)
		}
	}
	f.drain(t)
	for i, token := range []string{first, second} {
		if prompt, _ := f.app.workerPrompts.get(token); prompt.state != workerPromptRouted {
			t.Fatalf("prompt %d is %v after the refused calls, want it still routed", i, prompt.state)
		}
	}
	if route := f.route(t, f.local.ID); route == nil || route.ComputerID != "" {
		t.Fatalf("the local prompt is routed to %+v after the refused calls", route)
	}
	// None of the refused renewals renewed: the lease runs out on its own.
	waitUntil(t, 10*time.Second, func() bool { return f.route(t, f.workers[1].ID) == nil })

	if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{Spawn: f.spawns[0], Token: first}); err != nil {
		t.Fatalf("the link's own return: %v", err)
	}
	if f.route(t, f.workers[0].ID) != nil || !f.handedToPerson(f.workers[0].ID) {
		t.Fatal("the returned prompt did not go to the person here")
	}
}

// A link that ends gives its prompts to the person at once: a revoked
// device's, and one the retention pass releases because its owner was
// silent past the floor.
func TestALinkThatEndsHandsItsPromptsBack(t *testing.T) {
	t.Parallel()
	t.Run("revoked", func(t *testing.T) {
		t.Parallel()
		f := newLinkedWorkers(t, 1)
		f.ask(t, 0)
		revocation, err := f.app.store.RevokeDevice(f.device, time.Now().UnixMilli())
		if err != nil || !slices.Contains(revocation.ReleasedThreads, f.workers[0].ID) {
			t.Fatalf("revoke: %+v, %v", revocation, err)
		}
		f.app.handBackLinkPrompts(revocation.ReleasedThreads)
		f.drain(t)
		if f.route(t, f.workers[0].ID) != nil || !f.handedToPerson(f.workers[0].ID) {
			t.Fatal("the revoked device's prompt is still routed to its spawner there")
		}
	})
	t.Run("silent past the floor", func(t *testing.T) {
		t.Parallel()
		f := newLinkedWorkersAt(t, 1, 1)
		f.ask(t, 0)
		f.app.expireThreadRequests(time.Now())
		f.drain(t)
		if f.route(t, f.workers[0].ID) != nil || !f.handedToPerson(f.workers[0].ID) {
			t.Fatal("the released link's prompt is still routed to its spawner there")
		}
	})
}

// A linked Codex worker's async questions are one prompt held for its
// spawner there, routed again when this computer restarts and its lease
// counted from the boot; the spawner's answer, written through the
// binding, is the worker's message from the lead on that computer.
func TestALinkedWorkersAsyncQuestionIsHeldForItsSpawnerAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	worker := f.workers[0].ID
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: worker, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: twoAsyncQuestions,
	}); err != nil {
		t.Fatal(err)
	}
	f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": worker})
	f.drain(t)
	listed := f.list(t, 0)
	if len(listed) != 1 || len(listed[0].Questions) != 2 || listed[0].Questions[0].Question != "Which scope?" {
		t.Fatalf("the link lists %+v, want the two questions as one prompt", listed)
	}
	old := listed[0].Token
	// The restart: memory goes, the queue is restored and the questions
	// seeded as the boot does.
	f.app.workerPrompts.mu.Lock()
	f.app.workerPrompts.byToken, f.app.workerPrompts.current = nil, nil
	f.app.workerPrompts.mu.Unlock()
	f.app.workerPrompts.async.mu.Lock()
	f.app.workerPrompts.async.open = nil
	f.app.workerPrompts.async.mu.Unlock()
	booted := time.Now()
	f.app.restoreDurableFlushQueueAtBoot()
	f.app.seedAsyncQuestionPrompts()
	f.drain(t)
	held := f.app.workerPrompts.heldFor(f.spawns[0], worker)
	if len(held) != 1 || held[0].token == old || !held[0].async || held[0].remote.registeredAt.Before(booted) {
		t.Fatalf("after the restart the link holds %+v, want the questions again under a new token, leased from the boot", held)
	}
	if detail, route, open := f.app.asyncQuestionBlocked(worker); !open || route == nil || route.ComputerID != linkedLeadComputer || detail != "Which scope?" {
		t.Fatalf("the questions are routed to %+v (open %v), want the lead on Desk", route, open)
	}
	if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReply, threadPromptPeerArgs{
		Spawn: f.spawns[0], Token: held[0].token, Answers: []string{"One", "tonight"},
	}); err != nil {
		t.Fatalf("the spawner's answer: %v", err)
	}
	var payload flushQueuePayload
	for _, row := range durableQueueRows(t, f.app, worker) {
		if row.SendID == workerAnswerSendIDRoot+held[0].token {
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if payload.OriginThread == nil || payload.OriginThread.ThreadID != f.lead || payload.OriginThread.Title != "the lead" ||
		payload.OriginThread.ComputerID != linkedLeadComputer || payload.OriginThread.ComputerName != linkedLeadComputerName {
		t.Fatalf("the worker's answer is attributed %+v, want the lead on Desk", payload.OriginThread)
	}
}
