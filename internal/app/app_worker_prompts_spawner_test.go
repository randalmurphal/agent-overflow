package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// workerPromptTokens is the token of each worker prompt wake threadID's
// queue holds.
func workerPromptTokens(t *testing.T, a *App, threadID string) []string {
	t.Helper()
	durable, _ := queuedSendIDs(t, a, threadID)
	var tokens []string
	for _, id := range durable {
		if token, ok := strings.CutPrefix(id, workerPromptSendIDRoot); ok {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// A worker an ordinary thread spawned puts its approval and its question to
// that thread, not the user: both are stamped with the spawner, wake it
// (queued in its thread, its session started for them), and its
// thread_reply answers each in the worker's live provider session.
func TestAnOrdinarySpawnersWorkerPromptsWakeItAndItsReplyAnswersThem(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	app.triage.SetPromptRouter(app.routeWorkerPrompt)
	bus.mu.Lock()
	bus.tap = app.tapWorkerPrompt
	bus.mu.Unlock()
	spawner, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateThreadRuntimeMode(context.Background(), worker.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-worker", CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var woken []string
	app.startSessionFn = func(threadID string) error {
		if threadID == worker.ID {
			return app.startSessionNow(threadID)
		}
		mu.Lock()
		woken = append(woken, threadID)
		mu.Unlock()
		// The spawner's provider is not what this test is about; its
		// wakes stay queued for a session that never comes.
		return errors.New("no provider for the spawner in this test")
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-worker","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"control_request","request_id":"req-approve","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make deploy"}}}`,
		`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which module?","header":"Scope","options":[{"label":"parser","description":""},{"label":"lexer","description":""}],"multiSelect":false}]}}}`,
	}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.StartSession(worker.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(worker.ID, "go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	var tokens []string
	waitUntil(t, 10*time.Second, func() bool {
		if err := app.drainWorkerPrompts(context.Background(), 5*time.Second); err != nil {
			t.Fatal(err)
		}
		app.waitAgentNoticeWork()
		tokens = workerPromptTokens(t, app, spawner.ID)
		return len(tokens) == 2
	})
	pending := app.triage.PendingInteractiveRequests(worker.ID)
	if len(pending.Approvals) != 1 || len(pending.UserInputs) != 1 {
		t.Fatalf("the worker's prompts = %+v", pending)
	}
	for _, route := range []*provider.PromptRoute{pending.Approvals[0].RoutedTo, pending.UserInputs[0].RoutedTo} {
		if route == nil || route.ThreadID != spawner.ID || route.Title != spawner.Title {
			t.Fatalf("a prompt is routed to %+v, want the spawner %s", route, spawner.ID)
		}
	}
	mu.Lock()
	if !slices.Contains(woken, spawner.ID) {
		t.Errorf("the spawner's session was not started for its wakes: %q", woken)
	}
	mu.Unlock()

	caller := threadtools.Caller{ThreadID: spawner.ID}
	byPrompt := map[string]string{}
	for _, token := range tokens {
		prompt, ok := app.workerPrompts.get(token)
		if !ok {
			t.Fatalf("no prompt under %s", token)
		}
		byPrompt[prompt.requestID] = token
	}
	ack, handled, err := app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: byPrompt["req-approve"], Decision: threadtools.DecisionAllow, Text: "go ahead"})
	if err != nil || !handled || !ack.Accepted {
		t.Fatalf("the spawner's approval = %+v handled %v, %v", ack, handled, err)
	}
	ack, handled, err = app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: byPrompt["req-ask"], Text: "parser"})
	if err != nil || !handled || !ack.Accepted {
		t.Fatalf("the spawner's answer = %+v handled %v, %v", ack, handled, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		left := app.triage.PendingInteractiveRequests(worker.ID)
		return len(left.Approvals) == 0 && len(left.UserInputs) == 0
	})
	_ = app.StopSession(worker.ID)
}

// The user answering in an ordinary spawner's worker first settles the
// prompt: the spawner's queued wake is withdrawn and its later reply is
// refused as closed.
func TestAnOrdinarySpawnersReplyAfterTheUserAnsweredIsRefused(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "user-first", false)
	f.drain(t)
	sendID := f.queuedPrompt(t, f.assistant.ID)
	token := strings.TrimPrefix(sendID, workerPromptSendIDRoot)
	f.app.settleWorkerPrompt(f.worker.ID, f.request.RequestID, openPromptRef(t, f.app.triage, f.worker.ID, f.request.RequestID).Occurrence)
	if durable, _ := queuedSendIDs(t, f.app, f.assistant.ID); slices.Contains(durable, sendID) {
		t.Fatalf("the spawner still holds the wake of a prompt the user answered: %q", durable)
	}
	_, handled, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.assistant.ID},
		threadtools.ReplyCall{Token: token, Decision: threadtools.DecisionAllow, Text: "go"})
	if !handled || !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the reply after the user answered: handled %v, %v; want closed", handled, err)
	}
}

// A prompt whose wake an ordinary spawner never took goes back to the
// user: there is no successor thread to hand it to.
func TestAnOrdinarySpawnersWithdrawnWakeHandsThePromptBack(t *testing.T) {
	t.Parallel()
	f := newWorkerPromptFixture(t, "withdrawn", false)
	f.drain(t)
	sendID := f.queuedPrompt(t, f.assistant.ID)
	stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.assistant.ID, sendID)
	if err != nil || stop.Outcome != threadAgentStopped {
		t.Fatalf("withdraw = %+v, %v", stop, err)
	}
	f.drain(t)
	if pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals; len(pending) != 1 || pending[0].RoutedTo != nil {
		t.Fatalf("the worker's prompt = %+v after its wake was withdrawn, want it open and back with the user", pending)
	}
	if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) != 0 {
		t.Fatalf("the spawner still holds prompt wakes %q", tokens)
	}
}

// A prompt goes to the thread one spawn up: a worker's worker asks the
// worker, not the thread above it, and a worker whose spawner is archived
// asks the user.
func TestAWorkerPromptRoutesOneSpawnUpUnlessTheSpawnerIsArchived(t *testing.T) {
	t.Parallel()
	a, _ := newAppForFlushQueueRPC(t)
	a.startSessionFn = func(string) error { return errors.New("the provider binary is missing") }
	a.triage.SetPromptRouter(a.routeWorkerPrompt)
	lead := noticeTestThread(t, a, "route-lead")
	worker := noticeTestThread(t, a, "route-worker")
	grand := noticeTestThread(t, a, "route-grand")
	for _, spawn := range []struct{ token, caller, target string }{
		{"lead-worker", lead.ID, worker.ID}, {"worker-grand", worker.ID, grand.ID},
	} {
		if err := a.store.InsertThreadRequest(store.ThreadRequest{
			Token: spawn.token, CallerThreadID: spawn.caller, TargetThreadID: spawn.target,
			Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ask := func(threadID, requestID string) *provider.PromptRoute {
		t.Helper()
		request := provider.ApprovalRequest{RequestID: requestID, ThreadID: threadID, ToolName: "Bash", Kind: "command"}
		meta, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventApprovalRequest, ThreadID: threadID, ItemID: requestID, Meta: meta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		for _, pending := range a.triage.PendingInteractiveRequests(threadID).Approvals {
			if pending.RequestID == requestID {
				return pending.RoutedTo
			}
		}
		t.Fatalf("%s of %s is not pending", requestID, threadID)
		return nil
	}
	if route := ask(grand.ID, "grand-1"); route == nil || route.ThreadID != worker.ID {
		t.Fatalf("the worker's worker asks %+v, want the worker %s", route, worker.ID)
	}
	if route := ask(worker.ID, "worker-1"); route == nil || route.ThreadID != lead.ID {
		t.Fatalf("the worker asks %+v, want the lead %s", route, lead.ID)
	}
	if _, _, err := a.store.ArchiveThread(worker.ID); err != nil {
		t.Fatal(err)
	}
	if route := ask(grand.ID, "grand-2"); route != nil {
		t.Fatalf("a worker whose spawner is archived asks %+v, want the user", route)
	}
}

// The first message of a spawn an ordinary thread made says the new
// thread's prompts go to it, on a line of its own; a send to a thread the
// sender did not spawn says nothing of a route.
func TestAnOrdinarySpawnersFooterSaysItTakesTheWorkersPrompts(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "on it", "checked")
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "review the parser change", WaitSeconds: 20})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	const routes = "\nThe sender spawned this thread, so your questions and approval requests go to it, not to the user.\n"
	if first := f.userRow(t, ack.ThreadID, ack.Token).Summary; !strings.Contains(first, routes) || strings.Contains(first, "standing agent") {
		t.Fatalf("the spawn's footer:\n%s", first)
	}
	other, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: other.ID, Message: "check this too", WaitSeconds: 20})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if message := f.userRow(t, other.ID, sent.Token).Summary; strings.Contains(message, "spawned this thread") {
		t.Fatalf("a send to a thread the sender did not spawn routes its prompts:\n%s", message)
	}
}

// A prompt token names the prompt one session of the worker asked, not
// whichever prompt holds its request id: after the worker's session ends
// unanswered and a new session asks again under the same request id, the
// old token answers nothing, its wake is gone, and the new token answers the
// new prompt.
func TestAnOldPromptTokenCannotAnswerAReplacementSessionsPrompt(t *testing.T) {
	t.Parallel()
	app, bus := setupE2EApp(t)
	app.triage.SetPromptRouter(app.routeWorkerPrompt)
	bus.mu.Lock()
	bus.tap = app.tapWorkerPrompt
	bus.mu.Unlock()
	spawner, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.UpdateThreadRuntimeMode(context.Background(), worker.ID, string(provider.RuntimeApprovalRequired)); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-worker", CallerThreadID: spawner.ID, TargetThreadID: worker.ID,
		Kind: store.ThreadRequestSpawn, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	app.startSessionFn = func(threadID string) error {
		if threadID == worker.ID {
			return app.startSessionNow(threadID)
		}
		return errors.New("no provider for the spawner in this test")
	}
	// Every process the script runs asks the same request id.
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-worker","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"control_request","request_id":"req-approve","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make deploy"}}}`,
	}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	start := func(message string) {
		t.Helper()
		if err := app.StartSession(worker.ID); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		if err := app.SendMessage(worker.ID, message, nil); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	}
	queuedOtherThan := func(not string) string {
		t.Helper()
		var token string
		waitUntil(t, 10*time.Second, func() bool {
			if err := app.drainWorkerPrompts(context.Background(), 5*time.Second); err != nil {
				t.Fatal(err)
			}
			app.waitAgentNoticeWork()
			for _, queued := range workerPromptTokens(t, app, spawner.ID) {
				if queued != not {
					token = queued
				}
			}
			return token != ""
		})
		return token
	}
	start("go")
	first := queuedOtherThan("")
	ended, _ := app.workerPrompts.get(first)
	if err := app.StopSession(worker.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return len(app.triage.PendingInteractiveRequests(worker.ID).Approvals) == 0
	})
	// The registry's queue is held, so the replacement's prompt is open in
	// the worker before it is registered: only the session that asked
	// tells the two prompts apart.
	release := make(chan struct{})
	app.workerPrompts.queue.Go(func() { <-release })
	start("go again")
	t.Cleanup(func() { _ = app.StopSession(worker.ID) })
	waitUntil(t, 10*time.Second, func() bool {
		return len(app.triage.PendingInteractiveRequests(worker.ID).Approvals) == 1
	})
	stillOpen := func(after string) {
		t.Helper()
		if pending := app.triage.PendingInteractiveRequests(worker.ID).Approvals; len(pending) != 1 {
			t.Fatalf("the replacement session's prompt = %+v after %s, want it still open", pending, after)
		}
	}
	caller := threadtools.Caller{ThreadID: spawner.ID}
	_, handled, err := app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: first, Decision: threadtools.DecisionAllow, Text: "go ahead"})
	if !handled || !errors.Is(err, errWorkerPromptClosed) {
		t.Fatalf("the ended session's token: handled %v, %v; want it closed", handled, err)
	}
	stillOpen("the old token's reply")
	// An answer bound to the ended session is refused at the provider
	// write, whatever holds the request id now.
	if err := app.respondToApproval(t.Context(), worker.ID, ended.occurrence, provider.ApprovalResponse{RequestID: ended.requestID, Decision: "accept"}); !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("an answer for the ended session: %v, want already handled", err)
	}
	stillOpen("an answer for the ended session")
	close(release)

	second := queuedOtherThan(first)
	if tokens := workerPromptTokens(t, app, spawner.ID); !slices.Equal(tokens, []string{second}) {
		t.Fatalf("the spawner holds prompt wakes %q, want only the replacement's %s", tokens, second)
	}
	ack, handled, err := app.answerWorkerPrompt(t.Context(), caller, threadtools.ReplyCall{Token: second, Decision: threadtools.DecisionAllow, Text: "go ahead"})
	if err != nil || !handled || !ack.Accepted {
		t.Fatalf("the replacement's token: %+v handled %v, %v", ack, handled, err)
	}
}

// A paired computer reports where its target's prompt went, and the row on
// the computer that asked says so: a prompt routed to a thread there is
// not counted as waiting on the person, and one handed back to the user
// there is.
func TestARemoteRowSaysWhenItsTargetsPromptWentToAThreadThere(t *testing.T) {
	t.Parallel()
	there := newWorkerPromptFixture(t, "peer-routed", false)
	there.drain(t)
	token := "peer-routed-send"
	if _, _, err := there.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "source-device", Kind: store.ThreadRequestSend,
		TargetThreadID: there.worker.ID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	here := newRequestFixture(t)
	computer := "peer-routed-computer"
	if err := here.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: here.caller.ID, Kind: store.ThreadRequestSend, State: store.ThreadRequestUnconfirmed,
		TargetComputerID: computer, TargetThreadID: there.worker.ID, Notify: true,
	}); err != nil {
		t.Fatal(err)
	}
	// report carries the destination's reading across the wire, as a poll
	// does.
	report := func() {
		t.Helper()
		state, err := there.app.threadPeerRequestState(t.Context(), "source-device", token)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var answer ThreadPeerRequest
		if err := json.Unmarshal(wire, &answer); err != nil {
			t.Fatal(err)
		}
		if _, _, err := here.app.applyThreadPeerRequest(token, computer, answer); err != nil {
			t.Fatal(err)
		}
	}
	want := func(routedTo string, needYou int) {
		t.Helper()
		listing, err := here.app.ListThreadAgentRows(t.Context(), here.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, ok := agentRowByToken(listing.Rows, token)
		if !ok || row.State != threadtools.RequestBlocked || row.BlockedOn != "person" || row.BlockedRoutedTo != routedTo {
			t.Fatalf("the remote row = %+v (listed %v), want blocked on the person routed to %q", row, ok, routedTo)
		}
		summary, err := here.app.GetThreadWorkerSummary(t.Context(), here.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Open != 1 || summary.NeedYou != needYou {
			t.Fatalf("summary = %+v, want one open row, %d waiting on the user", summary, needYou)
		}
	}
	frames := make(chan ThreadAgentRowChange, 64)
	here.app.testEmitHook = func(name string, data any) {
		if change, ok := data.(ThreadAgentRowChange); ok && name == string(eventchan.ThreadAgentRowsChanged) && change.ID == token {
			frames <- change
		}
	}
	// framed waits for a frame naming the row, then for the frames already
	// owed to go out, and empties what came.
	framed := func(after string) {
		t.Helper()
		select {
		case <-frames:
		case <-time.After(10 * time.Second):
			t.Fatalf("no frame for the remote row after %s", after)
		}
		waitUntil(t, 10*time.Second, func() bool {
			here.app.threadAgentEmits.mu.Lock()
			defer here.app.threadAgentEmits.mu.Unlock()
			return !here.app.threadAgentEmits.draining
		})
		for len(frames) > 0 {
			<-frames
		}
	}
	report()
	framed("it became blocked")
	want(there.assistant.Title, 0)

	if there.app.triage.ReroutePrompts(there.assistant.ID, openPromptRef(t, there.app.triage, there.worker.ID, there.request.RequestID)) != 1 {
		t.Fatal("the prompt did not go back to the user there")
	}
	// Still blocked, now on the person: the row changes all the same.
	report()
	framed("its prompt went back to the user")
	want("", 1)
}
