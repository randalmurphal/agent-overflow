package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
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
	// dbPath is the store's file, for a second handle that rewrites a link.
	dbPath string
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
	a, dbPath := newTestAppWithStorePath(t)
	events := wireLinkedWorkersApp(a)
	f := &linkedWorkers{app: a, events: events, device: reachableDevice(t, a.store), lead: "6f1c2a4e-9d3b-4c1a-8e2f-0a1b2c3d4e5f", dbPath: dbPath}
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

// wireLinkedWorkersApp gives a the fixture's triage, prompt router and
// failing session starts, and returns the recorder of its events.
func wireLinkedWorkersApp(a *App) *emitRecorder {
	events := &emitRecorder{}
	a.testEmitHook = events.capture
	a.triage = triage.NewRouter(a.store, a.emit)
	a.configureTriageQueueCallbacks()
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	a.startSessionFn = func(string) error { return errors.New("no provider in this test") }
	return events
}

// restart is the computer after a restart: a second App over the same
// database, whose store is a new run, with nothing in memory. Nothing of
// its boot has run yet.
func (f *linkedWorkers) restart(t *testing.T) *linkedWorkers {
	t.Helper()
	after := *f
	after.app = newTestAppAtStorePath(t, f.dbPath)
	after.events = wireLinkedWorkersApp(after.app)
	return &after
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
	prompts, err := f.app.peerPromptsOf(t.Context(), f.device, f.lead, f.workers[i].Title,
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
	// A thread here that calls itself the lead cannot answer it with its
	// own thread_reply: only the binding answers a held prompt.
	_, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.lead},
		threadtools.ReplyCall{Token: token, Decision: threadtools.DecisionAllow})
	if code, _, _ := errorsx.PublicDetails(err); !handled || code != threadtools.CodeRequestNotYours {
		t.Fatalf("a local thread_reply on the held prompt: handled %v, %v; want it refused as not yours", handled, err)
	}
	if prompt, _ := f.app.workerPrompts.get(token); prompt.state != workerPromptRouted {
		t.Fatalf("the held prompt is %v after a refused local reply", prompt.state)
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
		prompts, err := f.app.peerPromptsOf(t.Context(), args.device, args.source, "", args.call)
		if err != nil || len(prompts) != 0 {
			t.Errorf("%s: listed %+v, %v; want nothing", name, prompts, err)
		}
	}
}

// A held prompt stays its spawner's there while its link stands, however
// long that computer goes without asking or without naming it as held: the
// person here is not asked.
func TestAHeldPromptStaysWithItsSpawnerThereWhileItsLinkStands(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 2)
	named, unnamed := f.ask(t, 0), f.ask(t, 1)
	f.list(t, 0, named)
	f.list(t, 1)
	time.Sleep(3 * time.Second)
	f.app.drainWorkerPrompts(t.Context(), 10*time.Second)
	for i, token := range []string{named, unnamed} {
		if route := f.route(t, f.workers[i].ID); route == nil || route.ThreadID != f.lead {
			t.Fatalf("the prompt under %s is routed to %+v, want the lead there", token, route)
		}
		if f.handedToPerson(f.workers[i].ID) {
			t.Fatalf("the person was asked about the prompt under %s", token)
		}
	}
}

// Only the link's own device, for its own source thread and spawn, may
// answer or give back a held prompt. Each refusal leaves the prompt
// as it was; the binding's own call gives it back to the person.
func TestTheBindingAdmitsOnlyTheLinksOwnPrompt(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 2)
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
		if prompts, err := f.app.peerPromptsOf(t.Context(), tc.device, tc.source, "", threadAgentPeerArgs{
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

	// The link's own return, from a spawner there that no thread can hold
	// it for, gives it to the person here.
	if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReturn, threadPromptPeerArgs{
		Spawn: f.spawns[0], Token: first, Reason: peerPromptNoHolder,
	}); err != nil {
		t.Fatalf("the link's own return: %v", err)
	}
	if f.route(t, f.workers[0].ID) != nil || !f.handedToPerson(f.workers[0].ID) {
		t.Fatal("the returned prompt did not go to the person here")
	}
}

// The binding holds a call to both the link a prompt was held through and
// the link that owns its worker now: after the link changes, neither the
// old link's identity nor the new one's may renew, answer or give back the
// prompt.
func TestTheBindingHoldsACallToTheLinkThenAndNow(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]struct {
		column, value string
	}{
		"spawn":         {"token", "relinked-spawn"},
		"device":        {"owner_device_id", "other-device"},
		"source thread": {"source_thread_id", "6f1c2a4e-9d3b-4c1a-8e2f-000000000000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkedWorkers(t, 1)
			token := f.ask(t, 0)
			raw, err := sql.Open("sqlite", f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`UPDATE thread_request_receipts SET `+change.column+` = ? WHERE token = ?`, change.value, f.spawns[0]); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			then := struct{ device, source, spawn string }{f.device, f.lead, f.spawns[0]}
			now := then
			switch change.column {
			case "token":
				now.spawn = change.value
			case "owner_device_id":
				now.device = change.value
			default:
				now.source = change.value
			}
			for which, id := range map[string]struct{ device, source, spawn string }{"then": then, "now": now} {
				for _, tool := range []string{threadAgentPeerPromptReturn, threadAgentPeerPromptReply} {
					err := f.call(t, id.device, id.source, tool, threadPromptPeerArgs{Spawn: id.spawn, Token: token, Decision: threadtools.DecisionAllow})
					if !errors.Is(err, errPeerPromptNotYours) {
						t.Errorf("the link %s, %s: %v; want it refused as not yours", which, tool, err)
					}
				}
			}
			if prompt, _ := f.app.workerPrompts.get(token); prompt.state != workerPromptRouted {
				t.Fatalf("the prompt is %v after the refused calls, want it still routed", prompt.state)
			}
		})
	}
}

// A prompt announced routed through a link that no longer routes there by
// the time it is held, the link replaced or gone, goes to the person at
// once.
func TestAPromptRoutedThroughAStaleLinkGoesToThePersonAtOnce(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	worker := f.workers[0].ID
	f.app.triage.SetPromptRouter(func(threadID string) *provider.PromptRoute {
		if threadID == worker {
			return &provider.PromptRoute{ThreadID: "6f1c2a4e-9d3b-4c1a-8e2f-0000000000aa", Title: "a lead before", ComputerID: linkedLeadComputer, ComputerName: linkedLeadComputerName}
		}
		return f.app.routeWorkerPrompt(threadID)
	})
	askOn(t, f.app, worker, "approval-1", false)
	waitUntil(t, 10*time.Second, func() bool {
		f.drain(t)
		return f.route(t, worker) == nil && f.handedToPerson(worker)
	})
	if held := f.app.workerPrompts.heldFor(f.spawns[0], worker); len(held) != 0 {
		t.Fatalf("the worker holds %d prompts for a link that does not route there", len(held))
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
		f.app.announceReleasedWorkers(revocation.ReleasedThreads)
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
// spawner there, and the spawner's answer, written through the binding, is
// the worker's message from the lead on that computer. A question asked
// before this computer restarted is the person's: the link holds nothing
// for it.
func TestALinkedWorkersAsyncQuestionIsHeldForItsSpawner(t *testing.T) {
	t.Parallel()
	f := newLinkedWorkers(t, 1)
	worker := f.workers[0].ID
	record := func(itemID string) {
		t.Helper()
		if _, err := f.app.store.RecordAsyncQuestions(store.Item{
			ID: itemID, ThreadID: worker, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: twoAsyncQuestions,
		}); err != nil {
			t.Fatal(err)
		}
		f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": worker})
		f.drain(t)
	}
	record("question:old")
	if listed := f.list(t, 0); len(listed) != 1 {
		t.Fatalf("before the restart the link lists %+v, want the question", listed)
	}
	// record and everything below act on the restarted computer.
	f = f.restart(t)
	f.app.restoreDurableFlushQueueAtBoot()
	f.app.syncAsyncQuestionPrompts(worker)
	f.drain(t)
	if listed := f.list(t, 0); len(listed) != 0 {
		t.Fatalf("after the restart the link lists %+v, want nothing", listed)
	}
	if _, route, open := f.app.asyncQuestionBlocked(worker); open {
		t.Fatalf("after the restart the question is routed to %+v, want it the person's", route)
	}

	record("question:ask")
	listed := f.list(t, 0)
	if len(listed) != 1 || len(listed[0].Questions) != 2 || listed[0].Questions[0].Question != "Which scope?" {
		t.Fatalf("the link lists %+v, want the two questions as one prompt", listed)
	}
	if detail, route, open := f.app.asyncQuestionBlocked(worker); !open || route == nil || route.ComputerID != linkedLeadComputer || detail != "Which scope?" {
		t.Fatalf("the questions are routed to %+v (open %v), want the lead on Desk", route, open)
	}
	if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReply, threadPromptPeerArgs{
		Spawn: f.spawns[0], Token: listed[0].Token, Answers: []string{"One", "tonight"},
	}); err != nil {
		t.Fatalf("the spawner's answer: %v", err)
	}
	var payload flushQueuePayload
	for _, row := range durableQueueRows(t, f.app, worker) {
		if row.SendID == workerAnswerSendIDRoot+listed[0].Token {
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
