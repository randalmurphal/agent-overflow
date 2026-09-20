package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// The request ledger end to end: a thread asks another thread for work, the
// work runs on a mock provider, and the answer comes back the way the ack
// promised it would.
//
// Every fixture here runs against a real store, the real send path and the
// real turn observer. The only thing that is not real is the provider, which
// is a mock script (kerneltest isolation is what keeps a real one out).

type requestFixture struct {
	app    *App
	bus    *capturedEventBus
	caller store.Thread
}

func newRequestFixture(t *testing.T) *requestFixture {
	t.Helper()
	return newRequestFixtureIn(t, t.TempDir())
}

// newRequestFixtureIn puts the caller thread in a named workspace, which is
// what a test asserting project or worktree behavior needs.
func newRequestFixtureIn(t *testing.T, workspace string) *requestFixture {
	t.Helper()
	app, bus := setupE2EApp(t)
	// Exports and answer files land under the data directory; a fixture
	// without one would write into the repository working directory.
	app.configDir = t.TempDir()
	caller, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create caller thread: %v", err)
	}
	app.installThreadRequestObserver()
	return &requestFixture{app: app, bus: bus, caller: caller}
}

// mockClaude installs a provider that answers each user message with one
// assistant message and ends the turn.
func (f *requestFixture) mockClaude(t *testing.T, replies ...string) {
	t.Helper()
	installMockClaudeReplies(t, f.app, replies...)
}

// installMockClaudeReplies gives one app a provider that answers each user
// message with one assistant message and ends the turn.
func installMockClaudeReplies(t *testing.T, app *App, replies ...string) {
	t.Helper()
	turns := make([][]string, 0, len(replies))
	for _, reply := range replies {
		turns = append(turns, []string{
			mockClaudeInitLine,
			`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":` + quoteJSON(reply) + `}]}}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		})
	}
	installMockClaudeTurns(t, app, turns)
}

// mockClaudeInitLine opens a mock turn. It is shared so a test that needs
// a turn the mock leaves running writes only the lines it cares about.
const mockClaudeInitLine = `{"type":"system","subtype":"init","session_id":"sess-request","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`

// installMockClaudeTurns installs a mock provider whose n-th user message
// produces the n-th batch of wire lines verbatim.
func installMockClaudeTurns(t *testing.T, app *App, turns [][]string) {
	t.Helper()
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), turns)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
}

func quoteJSON(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func (f *requestFixture) callerIdentity() threadtools.Caller {
	return threadtools.Caller{ThreadID: f.caller.ID, Title: f.caller.Title, ComputerID: "local-computer", ComputerName: "This Mac"}
}

func (f *requestFixture) adapter() threadToolsApp { return threadToolsApp{app: f.app} }

func (f *requestFixture) request(t *testing.T, token string) store.ThreadRequest {
	t.Helper()
	row, found, err := f.app.store.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("GetThreadRequest(%s): found=%v err=%v", token, found, err)
	}
	return row
}

func (f *requestFixture) receipt(t *testing.T, token string) store.ThreadRequestReceipt {
	t.Helper()
	row, found, err := f.app.store.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("GetThreadRequestReceipt(%s): found=%v err=%v", token, found, err)
	}
	return row
}

// userRow returns the request's own user message in the target thread.
func (f *requestFixture) userRow(t *testing.T, threadID, token string) store.Item {
	t.Helper()
	items, err := f.app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Kind != "user_text" {
			continue
		}
		var meta usermessage.Meta
		if json.Unmarshal([]byte(item.Meta), &meta) == nil && meta.OriginThread != nil && meta.OriginThread.Token == token {
			return item
		}
	}
	t.Fatalf("no user row in %s carries request %s: %+v", threadID, token, items)
	return store.Item{}
}

// TestThreadSpawnRunsTheWorkAndSettlesOnItsOwnTurn is the whole happy path:
// the spawn inherits the caller's settings, the message carries the footer
// and the attribution, the mock answers, and the turn that consumed the
// message settles the request with that answer.
func TestThreadSpawnRunsTheWorkAndSettlesOnItsOwnTurn(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the launcher builds clean now")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "check the windows build", Title: "Windows build", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if ack.Outcome != threadtools.OutcomeSettled || ack.State != store.ThreadRequestFinished {
		t.Fatalf("ack = %+v, want a settled finished request", ack)
	}
	if ack.AnswerKind != threadtools.AnswerFinal || !strings.Contains(ack.Answer, "launcher builds clean") {
		t.Fatalf("answer = %q (%s), want the turn's final message", ack.Answer, ack.AnswerKind)
	}
	if ack.Delivered != store.ThreadWakeInline {
		t.Errorf("delivered = %q, want the reply itself to count as delivery", ack.Delivered)
	}

	spawned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("spawned thread: %v", err)
	}
	if spawned.ProjectID != f.caller.ProjectID || spawned.Provider != f.caller.Provider ||
		spawned.Model != f.caller.Model || spawned.RuntimeMode != f.caller.RuntimeMode {
		t.Errorf("spawn did not inherit the caller: %+v vs %+v", spawned, f.caller)
	}
	if spawned.Title != "Windows build" {
		t.Errorf("spawn title = %q", spawned.Title)
	}

	// The message the spawned thread read: the prompt, the footer both ends
	// parse, and the attribution the origin chip renders.
	row := f.userRow(t, ack.ThreadID, ack.Token)
	if !strings.Contains(row.Summary, "check the windows build") {
		t.Errorf("user row lost the prompt: %q", row.Summary)
	}
	if !strings.Contains(row.Summary, "Agent request from thread") || !strings.Contains(row.Summary, ack.Token) {
		t.Errorf("user row carries no request footer: %q", row.Summary)
	}
	var meta usermessage.Meta
	if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
		t.Fatalf("decode user meta: %v", err)
	}
	if meta.Origin != usermessage.OriginAgentThread || meta.OriginThread == nil ||
		meta.OriginThread.ThreadID != f.caller.ID || meta.OriginThread.Token != ack.Token {
		t.Fatalf("origin attribution = %+v", meta)
	}

	// The receipt names the turn it was consumed by, and the request is
	// settled once, with no wake owed.
	receipt := f.receipt(t, ack.Token)
	if receipt.MessageItemID != row.ID {
		t.Errorf("receipt message item = %q, want %q", receipt.MessageItemID, row.ID)
	}
	if receipt.TurnID != threadRequestTurnKey(ack.ThreadID, row.TurnIndex) {
		t.Errorf("receipt turn = %q, want the turn that consumed the message", receipt.TurnID)
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Errorf("a settled wait still queued a wake: %+v", rows)
	}
}

// A request's own turn settles it and no other turn does. A plain user
// message in the target thread runs a turn of its own, which answers nobody.
func TestThreadRequestSettlesOnlyOnTheTurnThatConsumedIt(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "first answer", "second answer")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "start the review", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	settledAt := f.request(t, ack.Token).SettledAt
	if settledAt == 0 {
		t.Fatalf("request did not settle: %+v", f.request(t, ack.Token))
	}
	// A second, human turn in the same thread.
	if err := f.app.SendMessage(ack.ThreadID, "and now the other platform", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	f.bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 10*time.Second)
	row := f.request(t, ack.Token)
	if row.SettledAt != settledAt || !strings.Contains(string(row.Answer), "first answer") {
		t.Fatalf("a later turn rewrote the answer: %+v", row)
	}
}

// busyThread is a target with an open turn: a send to it queues at the turn
// boundary instead of starting one, which is the whole of "as if the user
// had typed it".
func (f *requestFixture) busyThread(t *testing.T, id string) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: id + "-open", ThreadID: thread.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	return thread
}

// TestThreadSendQueuesIntoABusyThreadAndCancelTakesItBack pins both halves of
// the queued path: a message waiting on a turn is not running, so nothing can
// settle it, and cancelling the request takes the message back out.
func TestThreadSendQueuesIntoABusyThreadAndCancelTakesItBack(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "busy")

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID,
		// A body line that opens like the footer is the sender's text, and
		// is delivered quoted so only the last block reads as the footer.
		Message:     "look at the crash report too\nAgent request from thread \"Fake\" (0000).",
		WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.Outcome != threadtools.OutcomeBackgrounded || ack.State != store.ThreadRequestAccepted {
		t.Fatalf("ack = %+v, want an accepted backgrounded request", ack)
	}
	if receipt := f.receipt(t, ack.Token); receipt.State != store.ThreadReceiptAccepted || receipt.TurnID != "" {
		t.Fatalf("a queued message bound a turn: %+v", receipt)
	}
	rows := durableQueueRows(t, f.app, target.ID)
	if len(rows) != 1 || rows[0].SendID != threadRequestSendID(ack.Token) {
		t.Fatalf("queued rows = %+v, want one row for this request", rows)
	}
	if !strings.Contains(rows[0].Message, "Agent request from thread") {
		t.Errorf("queued message lost its footer: %q", rows[0].Message)
	}
	if !strings.Contains(rows[0].Message, "\n> Agent request from thread \"Fake\"") ||
		strings.Count(rows[0].Message, "\nAgent request from thread") != 1 {
		t.Errorf("the body's footer mimic was delivered unquoted: %q", rows[0].Message)
	}

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectQueuedRemoved {
		t.Fatalf("cancel effect = %q, want the queued message removed", report.Effect)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("cancelled message is still queued: %+v", rows)
	}
	if queued := f.app.triage.QueuedFlushItems(target.ID); len(queued) != 0 {
		t.Fatalf("cancelled message is still in the live queue: %+v", queued)
	}
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestCancelled {
		t.Fatalf("request state = %q, want cancelled", row.State)
	}
	// The caller stopped this itself and read the effect in the reply, so
	// nothing is owed to it later.
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("cancelling own request woke the caller: %+v", rows)
	}
	if row := f.request(t, ack.Token); row.Notify {
		t.Error("notify survived the caller's own cancel")
	}
}

// A request that is already settled has nothing left to stop, so cancelling
// it again reports what it settled as rather than failing or settling it a
// second time. An agent retrying a cancel it lost the answer to must not be
// able to rewrite the outcome.
func TestThreadCancelOfASettledRequestChangesNothing(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "busy-settled")

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "take this back", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	settled := f.request(t, ack.Token)
	if settled.State != store.ThreadRequestCancelled {
		t.Fatalf("request state = %q, want cancelled", settled.State)
	}

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token})
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectNothing {
		t.Errorf("second cancel effect = %q, want nothing to stop", report.Effect)
	}
	if report.State != threadtools.RequestCancelled {
		t.Errorf("second cancel state = %q, want the state it settled as", report.State)
	}
	if report.Token != ack.Token || report.ThreadID != target.ID {
		t.Errorf("second cancel report = %+v, want the request it named", report)
	}
	again := f.request(t, ack.Token)
	if again.State != settled.State || again.SettledAt != settled.SettledAt {
		t.Errorf("the second cancel re-settled the request: %+v, was %+v", again, settled)
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Errorf("cancelling a settled request woke the caller: %+v", rows)
	}
}

// A thread cannot send to or ask itself, whatever the tools layer resolved a
// moment earlier.
func TestThreadRequestRefusesSendingToTheCallersOwnThread(t *testing.T) {
	f := newRequestFixture(t)
	_, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: f.caller.ID, Message: "hello me"})
	if code := publicCode(t, err); code != threadtools.CodeSelfSend {
		t.Fatalf("send-to-self code = %q", code)
	}
	if _, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{ThreadID: f.caller.ID, Question: "what am I doing"}); err != nil {
		if code := publicCode(t, err); code != threadtools.CodeSelfSend {
			t.Fatalf("ask-self code = %q", code)
		}
	} else {
		t.Fatal("asking itself was allowed")
	}
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListThreadRequestsByCaller: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused call left rows behind: %+v", rows)
	}
}

// TestThreadAskForksAReadOnlyScratchThreadAndDeletesIt pins what makes an ask
// safe: the question goes to a hidden read-only fork of the target's tail,
// and the fork is gone once the answer is stored somewhere that outlives it.
func TestThreadAskForksAReadOnlyScratchThreadAndDeletesIt(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the retry budget is three attempts")
	target := f.forkableThread(t, "ask-source")

	ack, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{
		ThreadID: target.ID, Question: "what is the retry budget?", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	scratchID := ack.ThreadID
	if scratchID == target.ID {
		t.Fatal("the question went into the target thread itself")
	}
	if ack.Outcome != threadtools.OutcomeSettled || !strings.Contains(ack.Answer, "retry budget") {
		t.Fatalf("ack = %+v", ack)
	}
	// The target never saw the question.
	items, err := f.app.store.ListItems(target.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "what is the retry budget") {
			t.Fatalf("the ask landed in the target's transcript: %+v", item)
		}
	}
	// The scratch fork is gone, and its row with it. The deletion runs with
	// the token's settle lock released, so it is not ordered against the
	// answer reaching the caller.
	waitUntil(t, 10*time.Second, func() bool {
		_, err := f.app.store.GetThread(scratchID)
		return err != nil
	})
	if _, found, err := f.app.store.GetScratchThread(scratchID); err != nil || found {
		t.Fatalf("scratch row survived: found=%v err=%v", found, err)
	}
	// The whole answer is still readable after the thread that wrote it is
	// gone, which is the point of storing it on the request.
	row := f.request(t, ack.Token)
	if !strings.Contains(string(row.Answer), "retry budget") {
		t.Fatalf("answer lost with the scratch thread: %+v", row)
	}
}

// Every value a request row can hold has to arrive at the model as a word
// its tool schema declares. The literals here are that schema: a rename on
// either side of the mapping, or a new stored state nobody translated, has
// to break this test rather than reach an agent as an unknown word.
func TestEveryStoredRequestValueHasAToolWord(t *testing.T) {
	states := map[string]string{
		store.ThreadRequestUnconfirmed: "unconfirmed",
		store.ThreadRequestAccepted:    "accepted",
		store.ThreadRequestRunning:     "running",
		store.ThreadRequestReplied:     "replied",
		store.ThreadRequestFinished:    "finished",
		store.ThreadRequestErrored:     "errored",
		store.ThreadRequestCancelled:   "cancelled",
		store.ThreadRequestInterrupted: "interrupted",
		store.ThreadRequestExpired:     "expired",
		store.ThreadRequestRefused:     "refused",
	}
	if len(threadRequestStateWords) != len(states) {
		t.Errorf("the state vocabulary has %d entries, this test pins %d", len(threadRequestStateWords), len(states))
	}
	for stored, want := range states {
		if got := threadRequestStateWord(stored); got != want {
			t.Errorf("state %q reads as %q, want %q", stored, got, want)
		}
	}
	// blocked is derived from the target's live state at read time, so it is
	// a word with no row behind it.
	if _, mapped := threadRequestStateWords[threadtools.RequestBlocked]; mapped {
		t.Error("blocked is stored as a request state, but nothing writes it")
	}

	kinds := map[string]string{
		"":                      "",
		store.ThreadAnswerReply: "reply",
		store.ThreadAnswerFinal: "final",
		store.ThreadAnswerError: "error",
		store.ThreadAnswerNote:  "note",
	}
	if len(threadAnswerKindWords) != len(kinds) {
		t.Errorf("the answer vocabulary has %d entries, this test pins %d", len(threadAnswerKindWords), len(kinds))
	}
	for stored, want := range kinds {
		if got := threadAnswerKindWord(stored); got != want {
			t.Errorf("answer kind %q reads as %q, want %q", stored, got, want)
		}
	}

	// A value the maps do not know is still the caller's best evidence, so
	// it passes through instead of arriving blank.
	if got := threadRequestStateWord("from-a-newer-build"); got != "from-a-newer-build" {
		t.Errorf("an unmapped state read as %q, want it passed through", got)
	}
	if got := threadAnswerKindWord("from-a-newer-build"); got != "from-a-newer-build" {
		t.Errorf("an unmapped answer kind read as %q, want it passed through", got)
	}
}

// The scratch fork runs read-only whatever the source runs: a question must
// not be able to act on a workspace. Driven through Ask itself, because the
// mode is chosen on the way to the fork and a test that forks directly would
// not notice the tool handing it a different one.
func TestThreadAskScratchForkIsAlwaysReadOnly(t *testing.T) {
	f := newRequestFixture(t)
	// The turn stays open, so the fork is still there to read: an ask that
	// settles deletes its scratch thread.
	f.mockClaudeHoldingTheTurn(t, "reading the workspace")
	target := f.forkableThread(t, "readonly-source")
	if err := f.app.store.UpdateRuntimeMode(target.ID, string(provider.RuntimeFullAccess)); err != nil {
		t.Fatalf("UpdateRuntimeMode: %v", err)
	}

	ack, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{
		ThreadID: target.ID, Question: "what does this workspace do?",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	fork, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread(scratch): %v", err)
	}
	if fork.RuntimeMode != string(provider.RuntimeReadOnly) {
		t.Errorf("scratch runtime mode = %q, want read-only", fork.RuntimeMode)
	}
	if fork.Mode != threadmode.ModeScratch {
		t.Errorf("scratch mode = %q", fork.Mode)
	}
	if !strings.HasPrefix(fork.Title, "Ask: ") {
		t.Errorf("scratch title = %q", fork.Title)
	}
	row, found, err := f.app.store.GetScratchThread(fork.ID)
	if err != nil || !found || row.RequestToken != ack.Token || row.SourceThreadID != target.ID {
		t.Fatalf("scratch row = %+v found=%v err=%v", row, found, err)
	}
}

// The autonomy gate stands on the thread tools' write paths too.
//
// A call one of this computer's own agents makes carries no session and is
// unaffected, which is the case the last subtest pins: the tools' MCP server
// is its own listener and its calls are in-process. A call a paired computer
// forwarded arrives on that computer's session, and a session that may
// operate threads but not act without approval must not be able to start
// autonomous work through thread_spawn or thread_send.
func TestThreadToolsWritesNeedAutonomyForAnAutonomousThread(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "on it")
	f.app.initIdentity("backend-under-test")
	limited := pairSessionWithScopes(t, f.app, "thumb-thread-tools", []identity.Scope{
		identity.ScopeThreadsRead, identity.ScopeThreadsOperate, identity.ScopeTerminalOperate,
	})
	forwarded := callFrom(limited.ID, false)

	source := f.forkableThread(t, "autonomy-source")
	// The precondition, asserted rather than assumed: these threads resolve
	// to full-access, which is the mode the gate is about.
	if f.caller.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Fatalf("caller runtime mode = %q, want full-access; this test no longer covers what it was written for", f.caller.RuntimeMode)
	}

	t.Run("fresh spawn", func(t *testing.T) {
		_, err := f.adapter().Spawn(forwarded, f.callerIdentity(), threadtools.SpawnCall{Prompt: "start"})
		wantScopeRefusal(t, err, transport.ScopeThreadsAutonomy)
	})
	t.Run("from_thread spawn", func(t *testing.T) {
		_, err := f.adapter().Spawn(forwarded, f.callerIdentity(), threadtools.SpawnCall{
			FromThread: source.ID, Prompt: "carry on",
		})
		wantScopeRefusal(t, err, transport.ScopeThreadsAutonomy)
	})
	t.Run("send", func(t *testing.T) {
		_, err := f.adapter().Send(forwarded, f.callerIdentity(), threadtools.SendCall{
			ThreadID: source.ID, Message: "keep going",
		})
		wantScopeRefusal(t, err, transport.ScopeThreadsAutonomy)
	})
	t.Run("no threads were started", func(t *testing.T) {
		threads := threadIDsInStore(t, f.app)
		if len(threads) != 2 {
			t.Fatalf("threads = %v, want only the caller and the source", threads)
		}
	})
	t.Run("a local agent is unaffected", func(t *testing.T) {
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
			FromThread: source.ID, Prompt: "carry on",
		})
		if err != nil {
			t.Fatalf("Spawn from an in-process caller: %v", err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread(spawned): %v", err)
		}
		if spawned.RuntimeMode != string(provider.RuntimeFullAccess) {
			t.Fatalf("spawned runtime mode = %q, want the caller's full-access", spawned.RuntimeMode)
		}
	})
}

// A receipt accepted whose thread was never created is settled here, not
// left for the next boot: the retry that finds it reads a refusal, the
// receipt reads interrupted, and no second thread is created for it.
func TestThreadSpawnSettlesAReceiptWhoseThreadWasNeverCreated(t *testing.T) {
	f := newRequestFixture(t)
	adapter := f.adapter()
	origin, err := adapter.localOrigin(f.callerIdentity())
	if err != nil {
		t.Fatalf("localOrigin: %v", err)
	}

	// The state a crash between the two writes leaves behind: an accepted
	// receipt with no target thread.
	const token = "half-created-token"
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token:            token,
		OwnerDeviceID:    threadReceiptLocalOwner,
		SourceComputerID: "local-computer",
		SourceThreadID:   f.caller.ID,
		Kind:             store.ThreadRequestSpawn,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	before := threadIDsInStore(t, f.app)

	_, err = adapter.acceptSpawn(t.Context(), origin, token, threadtools.SpawnCall{Prompt: "try again"})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("retry of a half-created request: code = %q, err = %v", code, err)
	}
	if receipt := f.receipt(t, token); receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt state = %q, want interrupted", receipt.State)
	}
	if after := threadIDsInStore(t, f.app); len(after) != len(before) {
		t.Fatalf("threads after the refusal = %v, want the %v it started with", after, before)
	}
}

// A spawn that cannot join the group it named leaves no thread behind. The
// caller reads a refusal and makes the request again, and a retry that found
// the first thread still there would be spawning the same work twice.
func TestThreadSpawnRemovesTheThreadWhenItsGroupPatchFails(t *testing.T) {
	f := newRequestFixture(t)
	// A thread with no project has nowhere to create a group, which is the
	// refusal that can only be reached once the new thread exists.
	source := testThread("spawn-group-failure-source")
	source.ProjectID = ""
	source.Provider = string(provider.Claude)
	source.SessionRef = "spawn-group-failure"
	source.WorkspacePath = t.TempDir()
	if err := f.app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, f.app.store, source.ID)
	before := threadIDsInStore(t, f.app)

	_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", Group: "Release",
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("spawn into an impossible group: code = %q, err = %v", code, err)
	}
	if after := threadIDsInStore(t, f.app); len(after) != len(before) {
		t.Fatalf("the refused spawn left a thread behind: %v, started with %v", after, before)
	}
}

// threadIDsInStore is every thread row, for a test asserting that a refused
// call created none.
func threadIDsInStore(t *testing.T, app *App) []string {
	t.Helper()
	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		ids = append(ids, thread.ID)
	}
	return ids
}

// forkableThread builds a Claude thread an ask can fork: a session file on
// disk, a session ref on the row, and a stamped user message to cut at.
func (f *requestFixture) forkableThread(t *testing.T, sessionID string) store.Thread {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve isolated home: %v", err)
	}
	workspace := t.TempDir()
	writeClaudeProjectSession(t, home, workspace, sessionID, `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"`+sessionID+`","message":{"role":"user","content":"what is the retry policy"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"`+sessionID+`","message":{"role":"assistant","content":[{"type":"text","text":"three attempts"}]}}
`)
	thread, err := createTestThread(t, f.app, string(provider.Claude), workspace, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create forkable thread: %v", err)
	}
	thread.SessionRef = sessionID
	if err := f.app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update session ref: %v", err)
	}
	insertUserItemWithMeta(t, f.app.store, thread.ID, thread.ID+":u0", 0, "what is the retry policy", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, f.app.store, thread.ID, thread.ID+":a0", 0, "three attempts")
	updated, err := f.app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return updated
}

// awaitRequestState polls until the request reaches one of the wanted states.
// Settlement runs off the provider read loop, so a test that is not parked on
// a wait has to wait for it the way any other observer would.
func (f *requestFixture) awaitRequestState(t *testing.T, token string, want ...string) store.ThreadRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last store.ThreadRequest
	for time.Now().Before(deadline) {
		last = f.request(t, token)
		for _, state := range want {
			if last.State == state {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("request %s stayed in %q, want one of %v", token, last.State, want)
	return last
}

// TestThreadRequestWithNoWaitArrivesAsAMessage covers the other half of the
// promise the ack makes: nobody is parked, so the answer is queued into the
// caller's thread as a message it will read on its next turn.
func TestThreadRequestWithNoWaitArrivesAsAMessage(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the migration is reversible")
	// The caller is mid-turn, so the wake queues at the turn boundary and
	// stays there to be read instead of racing a session start.
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	// A person's half-written message must survive an agent's wake.
	if _, err := f.app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID: f.caller.ID, Content: "half a sentence", UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "is the migration reversible?", Title: "Migration", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if ack.Outcome != threadtools.OutcomeBackgrounded {
		t.Fatalf("ack outcome = %q, want backgrounded", ack.Outcome)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if !strings.Contains(string(row.Answer), "reversible") {
		t.Fatalf("answer = %q", row.Answer)
	}

	var wake store.FlushQueueItem
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				wake = queued
			}
		}
		if wake.SendID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if wake.SendID == "" {
		t.Fatalf("no wake was queued for %s: %+v", ack.Token, durableQueueRows(t, f.app, f.caller.ID))
	}
	if !strings.Contains(wake.Message, "reversible") || !strings.Contains(wake.Message, ack.Token) {
		t.Errorf("wake body = %q, want the answer and the token", wake.Message)
	}
	if draft, found, err := f.app.store.GetThreadDraft(f.caller.ID); err != nil || !found || draft.Content != "half a sentence" {
		t.Errorf("the wake consumed the person's draft: %+v found=%v err=%v", draft, found, err)
	}
	var payload flushqueue.Payload
	if err := json.Unmarshal(wake.Payload, &payload); err != nil {
		t.Fatalf("decode wake payload: %v", err)
	}
	if payload.Origin != string(usermessage.OriginAgentThread) || payload.OriginThread == nil {
		t.Errorf("wake payload = %+v, want the agent-thread attribution", payload)
	}
	delivered := f.request(t, ack.Token)
	if delivered.DeliveredHow != store.ThreadWakeQueued || delivered.DeliveredAt == 0 {
		t.Errorf("delivery = %q at %d, want a queued delivery", delivered.DeliveredHow, delivered.DeliveredAt)
	}
}

// A wait that runs out returns the work as still running and arms the wake,
// so the answer is never dropped between the two delivery paths.
func TestThreadRequestWaitTimeoutArmsTheWake(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "slow")

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "take your time", WaitSeconds: 1, Notify: false,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.Outcome != threadtools.OutcomeBackgrounded {
		t.Fatalf("ack outcome = %q, want backgrounded", ack.Outcome)
	}
	if !ack.Notify {
		t.Error("ack does not say the answer will arrive as a message")
	}
	if row := f.request(t, ack.Token); !row.Notify {
		t.Error("the timed-out wait left the request with no way to deliver")
	}
}

// An interrupt of the caller's turn ends the calls it was blocking without
// touching the work they were waiting for.
func TestThreadRequestWaitEndsWhenTheCallersTurnIsInterrupted(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "unhurried")

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "whenever you can", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	type result struct {
		report threadtools.StatusReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
			Tokens: []string{ack.Token}, WaitSeconds: 30,
		})
		done <- result{report, err}
	}()
	waitUntil(t, 10*time.Second, func() bool { return f.app.requestWaitActive(ack.Token) })

	if err := f.app.interruptTurnCtx(t.Context(), f.caller.ID); err != nil {
		t.Fatalf("interruptTurnCtx: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("RequestStates: %v", got.err)
		}
		if len(got.report.Requests) != 1 || got.report.Requests[0].State != store.ThreadRequestAccepted {
			t.Fatalf("report = %+v, want the request still running", got.report)
		}
		if got.report.WokeOn != "" || got.report.TimedOut {
			t.Errorf("report = %+v, want neither a settlement nor a timeout", got.report)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupt did not end the parked status call")
	}
	if receipt := f.receipt(t, ack.Token); receipt.State != store.ThreadReceiptAccepted {
		t.Fatalf("the interrupt disturbed the request itself: %+v", receipt)
	}
}

// TestThreadStatusReattachesAndCountsAsDelivery is the re-attach path: the
// agent left, the answer landed, and reading it in a status reply is what
// delivers it. No message is owed afterwards.
func TestThreadStatusReattachesAndCountsAsDelivery(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the index rebuild took nine minutes")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "how long did the rebuild take?", WaitSeconds: 0, Notify: false,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)

	report, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{
		Tokens: []string{ack.Token}, WaitSeconds: 5,
	})
	if err != nil {
		t.Fatalf("RequestStates: %v", err)
	}
	if len(report.Requests) != 1 {
		t.Fatalf("report = %+v", report)
	}
	state := report.Requests[0]
	if state.State != store.ThreadRequestFinished || !strings.Contains(state.Answer, "nine minutes") {
		t.Fatalf("state = %+v", state)
	}
	if report.WokeOn != ack.Token {
		t.Errorf("woke_on = %q, want the settled token", report.WokeOn)
	}
	if state.Delivered != store.ThreadWakeInline {
		t.Errorf("delivered = %q, want the status reply to count as delivery", state.Delivered)
	}
	if state.WakeQueued {
		t.Error("a message was queued for an answer the agent just read")
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("a wake was queued anyway: %+v", rows)
	}
	if row := f.request(t, ack.Token); row.DeliveredHow != store.ThreadWakeInline || row.DeliveredAt == 0 {
		t.Errorf("delivery was not recorded: %q at %d", row.DeliveredHow, row.DeliveredAt)
	}
}

// mockClaudeHoldingTheTurn answers and then keeps the turn open, which is the
// state a request is in while its agent is still working: the receipt is
// running and a reply can still settle it.
func (f *requestFixture) mockClaudeHoldingTheTurn(t *testing.T, text string) {
	t.Helper()
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-hold","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}})
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}
}

// runningRequest spawns a thread whose turn stays open, so the test holds a
// receipt in exactly the state a reply settles.
func (f *requestFixture) runningRequest(t *testing.T, prompt string) threadtools.RequestAck {
	t.Helper()
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: prompt, WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
	return ack
}

func (f *requestFixture) targetIdentity(t *testing.T, threadID string) threadtools.Caller {
	t.Helper()
	thread, err := f.app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	return threadtools.Caller{ThreadID: thread.ID, Title: thread.Title, ComputerID: "local-computer", ComputerName: "This Mac"}
}

// TestThreadReplyAnswersOnceAndRefusesASecondAnswer pins the whole reply
// contract: the answer settles the request, the same text again is the same
// answer, different text is a refusal, and a token belonging to another
// thread is not answerable here at all.
func TestThreadReplyAnswersOnceAndRefusesASecondAnswer(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "starting on it")
	ack := f.runningRequest(t, "how many rows did the backfill touch?")
	responder := f.targetIdentity(t, ack.ThreadID)

	reply, err := f.adapter().Reply(t.Context(), responder, threadtools.ReplyCall{Token: ack.Token, Text: "41,220 rows"})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !reply.Accepted || reply.State != threadtools.RequestReplied || reply.Late {
		t.Fatalf("reply ack = %+v", reply)
	}
	row := f.request(t, ack.Token)
	if row.State != store.ThreadRequestReplied || string(row.Answer) != "41,220 rows" {
		t.Fatalf("request = %+v", row)
	}
	if row.AnswerKind != store.ThreadAnswerReply {
		t.Errorf("answer kind = %q, want a reply", row.AnswerKind)
	}

	again, err := f.adapter().Reply(t.Context(), responder, threadtools.ReplyCall{Token: ack.Token, Text: "41,220 rows"})
	if err != nil {
		t.Fatalf("repeated Reply: %v", err)
	}
	if again.Accepted {
		t.Error("a retry of the same reply was counted as a second answer")
	}
	if again.State != threadtools.RequestReplied {
		t.Errorf("repeated reply state = %q", again.State)
	}

	_, err = f.adapter().Reply(t.Context(), responder, threadtools.ReplyCall{Token: ack.Token, Text: "on reflection, 41,221"})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("second answer code = %q", code)
	}

	_, err = f.adapter().Reply(t.Context(), f.callerIdentity(), threadtools.ReplyCall{Token: ack.Token, Text: "me again"})
	if code := publicCode(t, err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("answering another thread's request = %q", code)
	}
	_, err = f.adapter().Reply(t.Context(), responder, threadtools.ReplyCall{Token: "no-such-token", Text: "hello"})
	if code := publicCode(t, err); code != threadtools.CodeRequestUnknown {
		t.Fatalf("unknown token code = %q", code)
	}
}

// A reply that arrives after the turn ended is a revision, not a refusal:
// the sender was already told the turn produced no answer, so the late text
// is stored and delivered as a second wake.
func TestThreadLateReplyArrivesAsASecondWake(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "looking into it")
	// The caller is mid-turn so both wakes stay in the queue to be read.
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "caller-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "what did the profiler say?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	settled := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, ack.Token).DeliveredAt != 0 })

	late, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "the profiler blames the JSON decode",
	})
	if err != nil {
		t.Fatalf("late Reply: %v", err)
	}
	if !late.Accepted || !late.Late {
		t.Fatalf("late reply ack = %+v", late)
	}
	if late.Revision <= settled.Revision {
		t.Errorf("late revision = %d, want past the settled revision %d", late.Revision, settled.Revision)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if !strings.Contains(string(row.LateReply), "JSON decode") || row.LateReplyAt == 0 {
		t.Fatalf("late reply was not stored: %+v", row)
	}
	// The first answer is untouched: a revision adds to the record, it does
	// not rewrite it.
	if !strings.Contains(string(row.Answer), "looking into it") {
		t.Errorf("the late reply overwrote the first answer: %q", row.Answer)
	}
	waitUntil(t, 10*time.Second, func() bool { return f.request(t, ack.Token).LateDeliveredAt != 0 })
	var sendIDs []string
	for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
		sendIDs = append(sendIDs, queued.SendID)
	}
	wantFirst, wantLate := threadWakeSendID(ack.Token), threadWakeLateSendID(ack.Token)
	if len(sendIDs) != 2 || sendIDs[0] != wantFirst || sendIDs[1] != wantLate {
		t.Fatalf("queued wakes = %v, want %q then %q", sendIDs, wantFirst, wantLate)
	}
}

// A turn that fails settles the request as errored with what the provider
// said, so the sender learns the work did not happen.
func TestThreadRequestSettlesErroredWhenTheTurnFails(t *testing.T) {
	f := newRequestFixture(t)
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
		`{"type":"system","subtype":"init","session_id":"sess-error","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["the sandbox denied the write"]}`,
	}})
	if _, err := f.app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("set claude binary: %v", err)
	}

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "write the release notes", WaitSeconds: 20, Notify: false,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if ack.State != store.ThreadRequestErrored {
		t.Fatalf("ack = %+v, want an errored request", ack)
	}
	if ack.AnswerKind != threadtools.AnswerError {
		t.Errorf("answer kind = %q, want the error kind", ack.AnswerKind)
	}
	row := f.request(t, ack.Token)
	if row.State != store.ThreadRequestErrored || len(row.Answer) == 0 {
		t.Fatalf("request = %+v, want an errored row with an account of it", row)
	}
}

// A target that has stopped to ask a person something ends the wait without
// settling: the person owns the answer now.
func TestThreadStatusReturnsBlockedWhenTheTargetNeedsAPerson(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "waiting-on-a-person")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "rerun the deploy", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	approval, err := json.Marshal(provider.ApprovalRequest{RequestID: "approval-1", Kind: "permission"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventApprovalRequest, ThreadID: target.ID, ItemID: "approval-1",
		Meta: approval, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("approval request: %v", err)
	}

	report, err := f.adapter().RequestStates(t.Context(), f.callerIdentity(), threadtools.StatusCall{
		Tokens: []string{ack.Token}, WaitSeconds: 10,
	})
	if err != nil {
		t.Fatalf("RequestStates: %v", err)
	}
	if report.WokeOn != ack.Token || report.TimedOut {
		t.Fatalf("report = %+v, want the blocked target to end the wait", report)
	}
	if len(report.Requests) != 1 || report.Requests[0].State != threadtools.RequestBlocked {
		t.Fatalf("requests = %+v, want a blocked state", report.Requests)
	}
	// Blocked is not settled: the request is still the target's work.
	if row := f.request(t, ack.Token); row.State != store.ThreadRequestAccepted {
		t.Fatalf("request state = %q, want it still open", row.State)
	}
}

// A request belongs to the thread that made it. Another thread can neither
// read it nor stop it.
func TestThreadRequestsAreScopedToTheirCaller(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "shared")
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "mine", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	stranger := f.targetIdentity(t, target.ID)

	_, err = f.adapter().RequestStates(t.Context(), stranger, threadtools.StatusCall{Tokens: []string{ack.Token}})
	if code := publicCode(t, err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("reading another thread's request = %q", code)
	}
	_, err = f.adapter().Cancel(t.Context(), stranger, threadtools.CancelCall{Token: ack.Token})
	if code := publicCode(t, err); code != threadtools.CodeRequestNotYours {
		t.Fatalf("cancelling another thread's request = %q", code)
	}
	// By thread id, the caller may only stop a thread it started work in.
	other, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create unrelated thread: %v", err)
	}
	_, err = f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: other.ID})
	if code := publicCode(t, err); code != threadtools.CodeNotYours {
		t.Fatalf("cancelling an unrelated thread = %q", code)
	}
	// The thread this caller did send to is cancellable by id.
	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{ThreadID: target.ID})
	if err != nil {
		t.Fatalf("Cancel by thread id: %v", err)
	}
	if report.ThreadID != target.ID {
		t.Fatalf("cancel report = %+v", report)
	}
}

// TestThreadSpawnCutsAWorktreeFromTheBaseItIsGiven: base names the branch
// the worktree starts from, origin's head of it by default so the thread
// works on what was pushed, and the local head with base_local so unpushed
// commits are in it. A base the project does not have is refused before a
// request exists.
func TestThreadSpawnCutsAWorktreeFromTheBaseItIsGiven(t *testing.T) {
	repo, bare := testutil.InitGitRepoWithOrigin(t)
	// A release branch origin has and this clone does not, and a local
	// commit on main that is not pushed.
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", bare, ".")
	testutil.RunGit(t, sibling, "checkout", "-b", "release/2.4")
	testutil.RunGit(t, sibling, "push", "origin", "release/2.4")
	releaseTip := gitRevParse(t, sibling, "HEAD")
	writeFile(t, repo, "unpushed.txt", "local only\n")
	runGit(t, repo, "add", "unpushed.txt")
	runGit(t, repo, "commit", "-m", "unpushed")
	localMain := gitRevParse(t, repo, "main")
	originMain := gitRevParse(t, repo, "origin/main")

	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")
	spawnAt := func(call threadtools.SpawnCall) store.Thread {
		t.Helper()
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if err != nil {
			t.Fatalf("Spawn(%+v): %v", call, err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		if spawned.WorktreePath == "" {
			t.Fatalf("spawn %+v cut no worktree", call)
		}
		return spawned
	}

	fromOrigin := spawnAt(threadtools.SpawnCall{Prompt: "review", WorktreeBranch: "review-main", WorktreeBase: "main"})
	if head := gitRevParse(t, fromOrigin.WorktreePath, "HEAD"); head != originMain {
		t.Errorf("base main starts at %s, want origin's head %s (local main is %s)", head, originMain, localMain)
	}
	fromLocal := spawnAt(threadtools.SpawnCall{Prompt: "review", WorktreeBranch: "review-local", WorktreeBase: "main", WorktreeBaseLocal: true})
	if head := gitRevParse(t, fromLocal.WorktreePath, "HEAD"); head != localMain {
		t.Errorf("base_local main starts at %s, want the local head %s", head, localMain)
	}
	fromRelease := spawnAt(threadtools.SpawnCall{Prompt: "review", WorktreeBranch: "review-release", WorktreeBase: "release/2.4"})
	if head := gitRevParse(t, fromRelease.WorktreePath, "HEAD"); head != releaseTip {
		t.Errorf("base release/2.4 starts at %s, want origin's %s", head, releaseTip)
	}

	countRequests := func() int {
		rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0)
		if err != nil {
			t.Fatalf("ListThreadRequestsByCaller: %v", err)
		}
		return len(rows)
	}
	before := countRequests()
	for name, call := range map[string]threadtools.SpawnCall{
		"unknown base":           {Prompt: "x", WorktreeBranch: "b", WorktreeBase: "nowhere"},
		"origin-only base local": {Prompt: "x", WorktreeBranch: "b", WorktreeBase: "release/2.4", WorktreeBaseLocal: true},
		"flag-shaped base":       {Prompt: "x", WorktreeBranch: "b", WorktreeBase: "--output=x"},
	} {
		_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
			t.Errorf("%s: code = %q (%v), want invalid request", name, code, err)
		}
	}
	if after := countRequests(); after != before {
		t.Errorf("refused spawns left %d request rows behind", after-before)
	}
}

// TestThreadSpawnOverridesWhatItIsToldAndCutsAWorktree covers the five
// inherited settings, the refusal a bad one earns, and the worktree door: a
// spawn that names a branch gets a fresh checkout of the caller's project.
func TestThreadSpawnOverridesWhatItIsToldAndCutsAWorktree(t *testing.T) {
	f := newRequestFixtureIn(t, initGitRepo(t))
	f.mockClaude(t, "on it")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "port the fix", Title: "Port the fix", Model: "claude-haiku-4-5",
		RuntimeMode: string(provider.RuntimeReadOnly), WorktreeBranch: "port-the-fix",
		Group: "Port sweep", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	spawned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if spawned.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want the override", spawned.Model)
	}
	// The group named by the call exists in the new thread's project and
	// holds it, created by the spawn itself.
	group, err := f.app.store.GetThreadGroup(spawned.GroupID)
	if err != nil {
		t.Fatalf("GetThreadGroup(%q): %v", spawned.GroupID, err)
	}
	if group.Name != "Port sweep" || group.ProjectID != spawned.ProjectID {
		t.Errorf("group = %+v, want %q in project %q", group, "Port sweep", spawned.ProjectID)
	}
	// A second spawn naming the same group joins it rather than making
	// another of the same name.
	again, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "port the other fix", Group: "Port sweep", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Spawn(again): %v", err)
	}
	if second, err := f.app.store.GetThread(again.ThreadID); err != nil || second.GroupID != spawned.GroupID {
		t.Errorf("second spawn group = %q (%v), want %q", second.GroupID, err, spawned.GroupID)
	}
	// The caller runs an effort its model offers and the override's does
	// not. The inherited value is dropped rather than carried across, and
	// thread creation's own model policy settles what replaces it.
	if spawned.ReasoningEffort == f.caller.ReasoningEffort {
		t.Errorf("effort = %q, want the caller's effort dropped with its model", spawned.ReasoningEffort)
	}
	if spawned.RuntimeMode != string(provider.RuntimeReadOnly) {
		t.Errorf("runtime mode = %q, want the override", spawned.RuntimeMode)
	}
	if spawned.ProjectID != f.caller.ProjectID {
		t.Errorf("project = %q, want the caller's %q", spawned.ProjectID, f.caller.ProjectID)
	}
	if spawned.WorktreePath == "" || spawned.WorktreePath == projectPathForThread(t, f.app, f.caller) {
		t.Fatalf("worktree path = %q, want a fresh checkout", spawned.WorktreePath)
	}
	if !strings.Contains(spawned.Branch, "port-the-fix") {
		t.Errorf("branch = %q, want the requested name", spawned.Branch)
	}
	if info, err := os.Stat(spawned.WorktreePath); err != nil || !info.IsDir() {
		t.Fatalf("worktree was not created at %s: %v", spawned.WorktreePath, err)
	}

	// A model this computer does not have is refused with the list, not
	// silently replaced.
	_, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "x", Model: "claude-imaginary-9"})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("unknown model code = %q", code)
	}
	if !strings.Contains(err.Error(), "claude-haiku-4-5") {
		t.Errorf("refusal does not list the models it has: %v", err)
	}
	// An effort a model does not have is refused the same way.
	_, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "x", Model: "claude-haiku-4-5", Effort: "max",
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("unknown effort code = %q", code)
	}
}

// A spawn from a thread carries that thread's history: the new agent starts
// with the conversation, not a description of it.
func TestThreadSpawnFromThreadForksTheHistoryIntoAVisibleThread(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "reproduced it")
	source := f.forkableThread(t, "spawn-source")

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on from here", Title: "Carry on", Group: "Approaches", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	fork, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if fork.ID == source.ID {
		t.Fatal("the spawn ran in the source thread")
	}
	// The group lives in the fork's own project, which is the source's.
	if group, err := f.app.store.GetThreadGroup(fork.GroupID); err != nil || group.Name != "Approaches" || group.ProjectID != source.ProjectID {
		t.Errorf("fork group = %+v (%v), want %q in the source's project", group, err, "Approaches")
	}
	if fork.Mode == threadmode.ModeScratch {
		t.Error("a spawned fork is a thread the person can see, not a scratch thread")
	}
	if fork.Title != "Carry on" {
		t.Errorf("title = %q, want the requested one", fork.Title)
	}
	items, err := f.app.store.ListItems(fork.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var carried, prompt bool
	for _, item := range items {
		if strings.Contains(item.Summary, "what is the retry policy") {
			carried = true
		}
		if strings.Contains(item.Summary, "carry on from here") {
			prompt = true
		}
	}
	if !carried {
		t.Errorf("the fork did not carry the source's history: %+v", items)
	}
	if !prompt {
		t.Errorf("the prompt never reached the fork: %+v", items)
	}
	// The source is untouched by the fork.
	sourceItems, err := f.app.store.ListItems(source.ID)
	if err != nil {
		t.Fatalf("ListItems(source): %v", err)
	}
	for _, item := range sourceItems {
		if strings.Contains(item.Summary, "carry on from here") {
			t.Fatalf("the spawn wrote into the source thread: %+v", item)
		}
	}
}

// A send to an idle thread starts it. The queue path is for a thread that is
// busy; an idle one gets the message now.
func TestThreadSendStartsAnIdleThread(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "reading it now")
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "read the design doc", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ack.State != store.ThreadRequestFinished {
		t.Fatalf("ack = %+v, want the work to have run", ack)
	}
	if rows := durableQueueRows(t, f.app, target.ID); len(rows) != 0 {
		t.Fatalf("an idle thread queued the message instead of running it: %+v", rows)
	}
	item := f.userRow(t, target.ID, ack.Token)
	if !strings.Contains(item.Summary, "read the design doc") {
		t.Errorf("user row = %q", item.Summary)
	}
	var meta usermessage.Meta
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.SendID != threadRequestSendID(ack.Token) {
		t.Errorf("send id = %q, want the request's own", meta.SendID)
	}
	if meta.Origin != usermessage.OriginAgentThread || meta.OriginThread.ThreadID != f.caller.ID {
		t.Errorf("attribution = %+v, want the caller thread", meta)
	}
}

// restart builds a second App over the same database, which is what a
// restart is to everything durable: the rows survive, the process state does
// not.
func (f *requestFixture) restart(t *testing.T) *requestFixture {
	t.Helper()
	bus := newCapturedEventBus()
	app := &App{store: f.app.store, settings: f.app.settings}
	app.triage = triage.NewRouter(app.store, bus.emitChannel)
	app.triage.SetEventHook(bus.observeRouterEvent)
	app.providerDiscoveryCaches = f.app.providerDiscoveryCaches
	app.textGenerationExecutor = f.app.textGenerationExecutor
	app.keepAwakeApply = f.app.keepAwakeApply
	app.configDir = f.app.configDir
	app.appCtx, app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		app.appCancel()
		app.threadRequestsWG.Wait()
	})
	app.installThreadRequestObserver()
	return &requestFixture{app: app, bus: bus, caller: f.caller}
}

// holdCallerTurn keeps the caller mid-turn so wakes queue where the test can
// read them instead of racing a session start.
func (f *requestFixture) holdCallerTurn(t *testing.T) {
	t.Helper()
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: f.caller.ID + "-open", ThreadID: f.caller.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
}

// A reminder is a message to the thread's own agent, due at a time. It is a
// row and nothing else, so a restart between setting it and its due time
// changes nothing.
func TestThreadReminderFiresAfterARestart(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)

	ack, err := f.adapter().Remind(t.Context(), f.callerIdentity(), threadtools.RemindCall{
		DueAtUnixMs: time.Now().Add(-time.Second).UnixMilli(), Note: "check whether the deploy finished",
	})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	if ack.State != store.ThreadRequestAccepted || !ack.Notify {
		t.Fatalf("ack = %+v, want an accepted reminder that will arrive as a message", ack)
	}
	if row := f.request(t, ack.Token); row.Kind != store.ThreadRequestRemind || row.DueAt == 0 {
		t.Fatalf("request = %+v, want a due reminder", row)
	}

	after := f.restart(t)
	after.app.fireDueThreadReminders(time.Now())

	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if !strings.Contains(string(row.Answer), "deploy finished") {
		t.Fatalf("reminder answer = %q, want the note", row.Answer)
	}
	wake := awaitQueuedWake(t, after.app, f.caller.ID, threadWakeSendID(ack.Token))
	if !strings.Contains(wake.Message, "deploy finished") {
		t.Fatalf("reminder message = %q, want the note", wake.Message)
	}
}

// awaitQueuedWake polls for a request's wake in its caller's durable queue.
// A wake is delivered off the sweep's goroutine because it takes the caller
// thread's own lock and can start its session, so the sweep returning is not
// the message having arrived.
func awaitQueuedWake(t *testing.T, app *App, threadID, sendID string) store.FlushQueueItem {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows := durableQueueRows(t, app, threadID)
		for _, queued := range rows {
			if queued.SendID == sendID {
				return queued
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no wake %s arrived: %+v", sendID, rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Reading a request is a delivery only when there is an answer to deliver.
// A reminder stores its note the moment it is armed, and listing the
// thread's requests must not count that as the wake the reminder still owes.
func TestListingRequestsDoesNotDisarmAPendingReminder(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)

	ack, err := f.adapter().Remind(t.Context(), f.callerIdentity(), threadtools.RemindCall{
		DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "check whether the deploy finished",
	})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}

	// thread_status with no arguments: the listing every agent makes to
	// find its own tokens.
	listing, err := f.adapter().ListRequests(t.Context(), f.callerIdentity(), threadtools.ListCall{})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	for _, request := range listing.Requests {
		if request.Token == ack.Token && request.Delivered != "" {
			t.Fatalf("listing an armed reminder reported it delivered %q", request.Delivered)
		}
	}
	if row := f.request(t, ack.Token); row.DeliveredAt != 0 {
		t.Fatalf("listing an armed reminder marked it delivered at %d", row.DeliveredAt)
	}

	// The reminder still fires as a message when its time comes.
	f.app.fireDueThreadReminders(time.Now().Add(2 * time.Hour))
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	waitUntil(t, 10*time.Second, func() bool {
		for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
			if queued.SendID == threadWakeSendID(ack.Token) {
				return true
			}
		}
		return false
	})
}

// A reminder the caller disarmed is settled and nothing else. Every other
// delivery goes through the collector, which is where the notify flag is
// read, and firing one has to use the same door or an archived thread comes
// back for a message nobody is owed.
func TestADisarmedReminderIsNotDeliveredAndLeavesTheThreadArchived(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")

	ack, err := f.adapter().Remind(t.Context(), f.callerIdentity(), threadtools.RemindCall{
		DueAtUnixMs: time.Now().Add(-time.Second).UnixMilli(), Note: "stand up",
	})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	// Archiving the caller disarms every wake it is owed.
	if err := f.app.ArchiveThread(f.caller.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	if row := f.request(t, ack.Token); row.Notify {
		t.Fatal("archiving the caller left the reminder armed")
	}

	f.app.fireDueThreadReminders(time.Now())
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	if row.DeliveredAt != 0 {
		t.Errorf("a disarmed reminder was delivered as %q", row.DeliveredHow)
	}
	for _, queued := range durableQueueRows(t, f.app, f.caller.ID) {
		if queued.SendID == threadWakeSendID(ack.Token) {
			t.Fatal("a disarmed reminder queued a wake")
		}
	}
	thread, err := f.app.store.GetThread(f.caller.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !thread.Archived {
		t.Fatal("a disarmed reminder brought the archived thread back")
	}
}

// Archiving through the agents' organize patch is the same archive as the
// sidebar's: the thread's parked calls end and its wakes are disarmed, or
// the next answer brings it back out of the archive the user put it in.
func TestArchivingThroughTheOrganizePatchStopsTheCallersRequests(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	spawn := f.runningRequest(t, "how big is the index?")
	if row := f.request(t, spawn.Token); !row.Notify {
		t.Fatal("the request under test is not owed a message")
	}

	archived := true
	if _, err := f.app.applyThreadOrganizePatch(t.Context(), f.caller.ID, threadapp.OrganizePatch{Archived: &archived}); err != nil {
		t.Fatalf("applyThreadOrganizePatch: %v", err)
	}
	if row := f.request(t, spawn.Token); row.Notify {
		t.Error("a thread archived by thread_update is still owed a message")
	}
}

// The sweep ticker is what fires a reminder with nobody watching. The nudge
// is what keeps one due sooner than the next tick from waiting it out.
func TestThreadReminderSweepFiresOnItsOwn(t *testing.T) {
	f := newRequestFixture(t)
	// The reminder wakes the thread, and waking a thread starts its agent.
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)
	f.app.appCtx, f.app.appCancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		f.app.appCancel()
		f.app.threadRequestsWG.Wait()
	})
	f.app.startThreadRequestSweeps()

	ack, err := f.adapter().Remind(t.Context(), f.callerIdentity(), threadtools.RemindCall{
		DueAtUnixMs: time.Now().UnixMilli(), Note: "stand up",
	})
	if err != nil {
		t.Fatalf("Remind: %v", err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		return f.request(t, ack.Token).State == store.ThreadRequestFinished
	})
}

// A caller that goes away stops asking. Archiving is the reversible half:
// the asks it started are stopped and nothing is owed to it while it is out
// of sight.
func TestThreadRequestsStopWhenTheCallerIsArchived(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	ask := f.runningRequest(t, "how big is the index?")

	if err := f.app.ArchiveThread(f.caller.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}
	row := f.request(t, ask.Token)
	if row.Notify {
		t.Error("an archived thread is still owed a message")
	}
}

// Deleting the caller takes its requests with it: the ledger is the caller's
// record, and a record with no owner is nothing to collect. The hidden work
// it started goes too; the threads a person can see do not.
func TestDeletingTheCallerTakesItsRequests(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	spawn := f.runningRequest(t, "how big is the index?")
	target := f.forkableThread(t, "delete-caller-source")
	ask, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{
		ThreadID: target.ID, Question: "what did you decide?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if err := f.app.DeleteThread(f.caller.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	for _, token := range []string{spawn.Token, ask.Token} {
		if _, found, err := f.app.store.GetThreadRequest(token); err != nil || found {
			t.Fatalf("request %s outlived its caller: found=%v err=%v", token, found, err)
		}
	}
	// The scratch thread was the caller's private workspace; it goes with it.
	if _, err := f.app.store.GetThread(ask.ThreadID); err == nil {
		t.Errorf("the ask's scratch thread %s outlived its caller", ask.ThreadID)
	}
	// The spawned thread is work a person can see, and deleting the thread
	// that asked for it is not a reason to destroy it.
	if _, err := f.app.store.GetThread(spawn.ThreadID); err != nil {
		t.Errorf("the spawned thread was deleted with its caller: %v", err)
	}
}

// A target deleted mid-request cannot answer, and saying so is the only
// honest settlement.
func TestDeletingTheTargetErrorsTheRequest(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "starting")
	ack := f.runningRequest(t, "what does the log say?")

	if err := f.app.DeleteThread(ack.ThreadID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	row := f.awaitRequestState(t, ack.Token, store.ThreadRequestErrored)
	if !strings.Contains(string(row.Answer), "deleted") {
		t.Fatalf("answer = %q, want an account of the deletion", row.Answer)
	}
}

// A restart interrupts whatever was in flight. The boot sweep says so, once,
// instead of leaving requests that can never settle.
func TestBootSweepSettlesWhatTheRestartInterrupted(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "keep going")
	scratch := f.forkableThread(t, "boot-scratch")
	if err := f.app.store.InsertScratchThread(store.ScratchThread{
		ThreadID: scratch.ID, SourceThreadID: f.caller.ID, RequestToken: "stale-token",
		ReturnMode: scratchReturnMode(threadmode.ModeChat), CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertScratchThread: %v", err)
	}

	after := f.restart(t)
	after.app.sweepThreadRequestsAtBoot()

	row := after.awaitRequestState(t, ack.Token, store.ThreadRequestInterrupted)
	if row.State != store.ThreadRequestInterrupted {
		t.Fatalf("request = %+v", row)
	}
	receipt, found, err := after.app.store.GetThreadRequestReceipt(ack.Token)
	if err != nil || !found || receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt = %+v found=%v err=%v", receipt, found, err)
	}
	// A scratch thread is a thread nobody can see; one left by a restart is
	// swept with its row rather than kept forever.
	if _, err := after.app.store.GetThread(scratch.ID); err == nil {
		t.Errorf("scratch thread %s survived the boot sweep", scratch.ID)
	}
	if _, found, err := after.app.store.GetScratchThread(scratch.ID); err != nil || found {
		t.Errorf("scratch row survived the boot sweep: found=%v err=%v", found, err)
	}
}

// A status call waits on all of its tokens at once and returns on whichever
// settles first, not on whichever was listed first.
func TestThreadStatusEndsOnWhicheverRequestSettlesFirst(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "thinking")
	// Three tokens, and the one that settles is neither the first nor the
	// last listed: a wait that returned on list order rather than on a
	// settlement would pass on two tokens and fail here.
	first := f.runningRequest(t, "the first one")
	fast := f.runningRequest(t, "the fast one")
	last := f.runningRequest(t, "the last one")

	type result struct {
		report threadtools.StatusReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
			Tokens: []string{first.Token, fast.Token, last.Token}, WaitSeconds: 30,
		})
		done <- result{report, err}
	}()
	waitUntil(t, 10*time.Second, func() bool { return f.app.requestWaitActive(fast.Token) })

	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, fast.ThreadID), threadtools.ReplyCall{
		Token: fast.Token, Text: "done first",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("RequestStates: %v", got.err)
		}
		if got.report.WokeOn != fast.Token {
			t.Fatalf("woke on %q, want the token that settled", got.report.WokeOn)
		}
		if len(got.report.Requests) != 3 {
			t.Fatalf("report lists %d requests, want all three", len(got.report.Requests))
		}
		for _, index := range []int{0, 2} {
			open := got.report.Requests[index]
			if open.State != store.ThreadRequestRunning && open.State != store.ThreadRequestAccepted {
				t.Errorf("the unsettled request %d reads as %q", index, open.State)
			}
			if open.Answer != "" {
				t.Errorf("the unsettled request %d carries an answer: %+v", index, open)
			}
		}
		if got.report.Requests[1].Answer != "done first" {
			t.Errorf("the settled request = %+v", got.report.Requests[1])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the settlement did not end the parked status call")
	}
}

// Two calls parked on the same token both return: a wake is a broadcast, not
// a handoff to whoever got there first.
func TestTwoWaitersOnOneRequestBothReturn(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "thinking")
	ack := f.runningRequest(t, "who won?")

	done := make(chan threadtools.StatusReport, 2)
	for range 2 {
		go func() {
			report, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
				Tokens: []string{ack.Token}, WaitSeconds: 30,
			})
			if err != nil {
				report = threadtools.StatusReport{}
			}
			done <- report
		}()
	}
	waitUntil(t, 10*time.Second, func() bool { return f.waitersOn(ack.Token) == 2 })

	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "the cache did",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	for range 2 {
		select {
		case report := <-done:
			if report.WokeOn != ack.Token {
				t.Fatalf("a waiter returned without the settlement: %+v", report)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a waiter never returned")
		}
	}
	// The answer was read by a wait, so no message is owed on top of it.
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("a wake was queued for an answer two calls just read: %+v", rows)
	}
}

// thread_status watches threads as well as tokens: a thread coming to rest
// is what an agent waiting on somebody else's work is actually waiting for.
func TestThreadStatusWatchesAThreadUntilItRests(t *testing.T) {
	f := newRequestFixture(t)
	target := f.busyThread(t, "watched")
	// Whether a thread is running is live state, so the watch is driven
	// through the router that owns it.
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: target.ID, TurnID: "watched-open", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	type result struct {
		report threadtools.StatusReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
			ThreadIDs: []string{target.ID}, WaitSeconds: 30,
		})
		done <- result{report, err}
	}()
	waitUntil(t, 10*time.Second, func() bool { return f.app.requestWaitActive(threadWatchKey(target.ID)) })

	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: target.ID, TurnID: "watched-open",
		TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("RequestStates: %v", got.err)
		}
		if len(got.report.Threads) != 1 || got.report.Threads[0].ThreadID != target.ID {
			t.Fatalf("report = %+v, want the watched thread", got.report)
		}
		if !got.report.Threads[0].Resting {
			t.Errorf("the watched thread still reads as busy: %+v", got.report.Threads[0])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the thread coming to rest did not end the watch")
	}
}

// Cancelling a running request stops that turn and nothing else, and the
// interrupted turn cannot then be read as the answer.
func TestThreadCancelInterruptsTheRequestsOwnTurn(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working on it")
	ack := f.runningRequest(t, "rebuild the index")

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if report.Effect != threadtools.EffectInterrupted {
		t.Fatalf("effect = %q, want the turn interrupted", report.Effect)
	}
	row := f.request(t, ack.Token)
	if row.State != store.ThreadRequestCancelled {
		t.Fatalf("request = %+v, want cancelled", row)
	}
	// The interrupt's own turn end cannot overwrite the cancellation with an
	// answer nobody asked for, and that is structural rather than a matter
	// of timing: settling took the receipt out of the observer's gate, so
	// the turn end has no token to look at, and the receipt it would settle
	// is already settled.
	if tokens, armed := f.app.runningReceiptTokens(ack.ThreadID); armed {
		t.Fatalf("the cancelled request is still armed for its turn end: %v", tokens)
	}
	if receipt := f.receipt(t, ack.Token); receipt.State != store.ThreadReceiptCancelled {
		t.Fatalf("receipt = %q, want cancelled", receipt.State)
	}
	if again := f.request(t, ack.Token); again.State != store.ThreadRequestCancelled {
		t.Fatalf("the interrupted turn re-settled the request as %q", again.State)
	}
}

// The ledger is readable without a token: a thread can list what it asked
// for, open work first, and read a whole answer out to a file.
func TestThreadRequestsListAndExport(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the answer is 42")
	settled, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "what is the answer?", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	open, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: f.busyThread(t, "listed").ID, Message: "and the question?", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	list, err := f.adapter().ListRequests(t.Context(), f.callerIdentity(), threadtools.ListCall{Limit: 10})
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(list.Requests) != 2 || list.More {
		t.Fatalf("list = %+v, want both requests and no more", list)
	}
	if list.Requests[0].Token != open.Token {
		t.Errorf("list order = %q first, want the open request", list.Requests[0].Token)
	}

	export, err := f.adapter().ExportAnswer(t.Context(), f.callerIdentity(), settled.Token)
	if err != nil {
		t.Fatalf("ExportAnswer: %v", err)
	}
	body, err := os.ReadFile(export.Path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if !strings.Contains(string(body), "the answer is 42") {
		t.Fatalf("export = %q", body)
	}
	if export.Size != int64(len(body)) || export.SHA256 == "" {
		t.Errorf("export = %+v, want the size and digest of what it wrote", export)
	}
	if !strings.HasPrefix(export.Path, f.app.configDir) {
		t.Errorf("export path %q is outside the data directory", export.Path)
	}
}

// A wake the previous process never delivered is restored into the composer
// at boot, and the request says so rather than claiming the message was
// delivered.
func TestRestoredWakeIsRecordedAsADraft(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "the backup finished")
	f.holdCallerTurn(t)

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "did the backup finish?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return f.request(t, ack.Token).DeliveredHow == store.ThreadWakeQueued
	})

	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()

	row := after.request(t, ack.Token)
	if row.DeliveredHow != store.ThreadWakeDraft {
		t.Fatalf("delivery = %q, want the draft correction", row.DeliveredHow)
	}
	draft, found, err := after.app.store.GetThreadDraft(f.caller.ID)
	if err != nil || !found || !strings.Contains(draft.Content, "the backup finished") {
		t.Fatalf("draft = %+v found=%v err=%v", draft, found, err)
	}
	if rows := durableQueueRows(t, after.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("the restored row was left in the queue: %+v", rows)
	}
}

// waitersOn counts the calls parked on one key, which is how a test knows
// both of them arrived before the settlement it is about to write.
func (f *requestFixture) waitersOn(key string) int {
	f.app.threadRequests.mu.Lock()
	defer f.app.threadRequests.mu.Unlock()
	count := 0
	for _, wait := range f.app.threadRequests.waits {
		if _, ok := wait.keys[key]; ok {
			count++
		}
	}
	return count
}

// TestTwoQueuedRequestsSettleOnTheOneTurnThatConsumedThem covers the join:
// two messages that waited out a turn are sent as one, so one turn end is
// the answer to both. Settling either alone, or twice, would be wrong.
func TestTwoQueuedRequestsSettleOnTheOneTurnThatConsumedThem(t *testing.T) {
	f := newRequestFixture(t)
	workspace := initGitRepo(t)
	target, err := createTestThread(t, f.app, string(provider.Claude), workspace, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "merge-turn", ThreadID: target.ID, TurnIndex: 0, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	// A session that records what it is given and says nothing back: the
	// dispatch is the behavior under test, not the provider's answer.
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(context.Background(), target.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: workspace},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.app.sessionManager().put(target.ID, session{
		Provider: string(provider.Claude), Token: "tok", Claude: sess,
		Liveness: newSessionLiveness(time.Now()),
	})

	first, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "check the changelog", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send(first): %v", err)
	}
	second, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: target.ID, Message: "and the migration notes", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	queued := f.app.triage.QueuedFlushItems(target.ID)
	if len(queued) != 2 {
		t.Fatalf("queued = %+v, want both requests waiting", queued)
	}
	f.app.dispatchFlush(target.ID, queued)

	firstReceipt, secondReceipt := f.receipt(t, first.Token), f.receipt(t, second.Token)
	if firstReceipt.State != store.ThreadReceiptRunning || secondReceipt.State != store.ThreadReceiptRunning {
		t.Fatalf("receipts = %q / %q, want both running", firstReceipt.State, secondReceipt.State)
	}
	if firstReceipt.TurnID != secondReceipt.TurnID {
		t.Fatalf("turns = %q / %q, want the one turn that consumed both", firstReceipt.TurnID, secondReceipt.TurnID)
	}
	if firstReceipt.MessageItemID != secondReceipt.MessageItemID {
		t.Errorf("message rows = %q / %q, want the one merged message", firstReceipt.MessageItemID, secondReceipt.MessageItemID)
	}

	if err := f.app.store.UpdateTurnCompleted("merge-turn", time.Now().UnixMilli(), "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
	f.app.settleReceiptsForEndedTurn(target.ID)
	for _, token := range []string{first.Token, second.Token} {
		if row := f.request(t, token); row.State != store.ThreadRequestFinished {
			t.Errorf("request %s = %q, want the shared turn to have settled it", token, row.State)
		}
	}
}

// An ask forks a thread that is mid-turn: the question is answered against a
// snapshot, and the thread being asked is never interrupted to answer it.
func TestThreadAskForksAThreadThatIsMidTurn(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "as of right now, three")
	target := f.forkableThread(t, "mid-turn-source")
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertTurn(store.Turn{
		TurnID: "mid-turn-open", ThreadID: target.ID, TurnIndex: 1, StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := f.app.store.AppendItem(store.Item{
		ID: "mid-turn-tool", ThreadID: target.ID, TurnIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "running", Summary: "Bash", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}

	ack, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{
		ThreadID: target.ID, Question: "how many retries are left?", WaitSeconds: 20,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if ack.Outcome != threadtools.OutcomeSettled {
		t.Fatalf("ack = %+v", ack)
	}
	// The thread that was asked is still mid-turn: nothing about answering
	// a question may take its turn away from it.
	turn, found, err := f.app.store.GetActiveTurn(target.ID)
	if err != nil || !found {
		t.Fatalf("the ask ended the target's turn: found=%v err=%v", found, err)
	}
	if turn.TurnID != "mid-turn-open" {
		t.Fatalf("active turn = %q, want the one that was already running", turn.TurnID)
	}
}

// TestThreadSpawnFromThreadTakesTheCallersSettings pins what a fork of
// another thread runs with: the caller's settings, the call's overrides, and
// the source's provider, which a fork resumes and cannot change.
//
// The permission level is the sharp half. A read-only caller forking a
// full-access thread must not end up with a full-access thread, which is
// what inheriting the source's settings silently produced.
func TestThreadSpawnFromThreadTakesTheCallersSettings(t *testing.T) {
	f := newRequestFixture(t)
	f.mockClaude(t, "on it", "on it", "on it")
	f.caller.RuntimeMode = string(provider.RuntimeReadOnly)
	if err := f.app.store.UpdateThread(f.caller); err != nil {
		t.Fatalf("make the caller read-only: %v", err)
	}
	source := f.forkableThread(t, "settings-source")
	source.RuntimeMode = string(provider.RuntimeFullAccess)
	source.Model = "claude-haiku-4-5"
	if err := f.app.store.UpdateThread(source); err != nil {
		t.Fatalf("make the source full-access: %v", err)
	}

	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	fork, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if fork.RuntimeMode != string(provider.RuntimeReadOnly) {
		t.Errorf("fork runtime mode = %q, want the caller's read-only", fork.RuntimeMode)
	}
	if fork.Model != f.caller.Model {
		t.Errorf("fork model = %q, want the caller's %q", fork.Model, f.caller.Model)
	}
	if fork.Provider != source.Provider {
		t.Errorf("fork provider = %q, want the source's %q", fork.Provider, source.Provider)
	}

	// An explicit override is honored, not dropped.
	ack, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", Model: "claude-haiku-4-5",
		RuntimeMode: string(provider.RuntimeApprovalRequired), WaitSeconds: 0,
	})
	if err != nil {
		t.Fatalf("Spawn with overrides: %v", err)
	}
	overridden, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if overridden.Model != "claude-haiku-4-5" || overridden.RuntimeMode != string(provider.RuntimeApprovalRequired) {
		t.Errorf("overrides were dropped: model=%q runtime=%q", overridden.Model, overridden.RuntimeMode)
	}

	// A model this computer does not offer is refused here exactly as it is
	// for a fresh spawn, rather than being ignored.
	_, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", Model: "claude-imaginary-9",
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("unknown model on a fork = %q", code)
	}

	// The provider axis cannot move in a fork, and saying so is the only
	// honest answer: the fork resumes the source's session.
	_, err = f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		FromThread: source.ID, Prompt: "carry on", Provider: string(provider.Codex),
	})
	if code := publicCode(t, err); code != threadtools.CodeInvalidRequest {
		t.Fatalf("a provider change on a fork = %q, want a refusal", code)
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("the refusal does not name the axis: %v", err)
	}
}

// TestThreadStatusSaysTheQueuedMessageIsStillComing is the other half of a
// backgrounded answer: the agent reads it off the token, and the reply tells
// it that the message carrying the same answer is still on its way, so it
// does not read the answer twice without knowing why.
//
// It runs the notice through the tool itself, because the sentence is the
// tool's, and over both halves of the queued path: the wake waiting in the
// durable queue of a caller with nothing to take it, and the wake already
// handed to a live session, which is where an agent calling thread_status
// actually is.
func TestThreadStatusSaysTheQueuedMessageIsStillComing(t *testing.T) {
	type statusReply struct {
		Requests []struct {
			Token      string `json:"token"`
			State      string `json:"state"`
			Answer     string `json:"answer"`
			Delivered  string `json:"delivered"`
			WakeQueued bool   `json:"wake_queued"`
		} `json:"requests"`
		Note string `json:"note"`
	}
	const notice = "A message carrying this answer is already queued in this thread and will still arrive."
	readStatus := func(t *testing.T, f *requestFixture, token string) statusReply {
		t.Helper()
		answer, err := f.app.threadToolsServer().Call(t.Context(), f.callerIdentity(), "thread_status",
			json.RawMessage(`{"tokens":["`+token+`"],"wait_seconds":5}`))
		if err != nil {
			t.Fatalf("thread_status: %v", err)
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatalf("encode thread_status reply: %v", err)
		}
		var reply statusReply
		if err := json.Unmarshal(encoded, &reply); err != nil {
			t.Fatalf("decode thread_status reply %s: %v", encoded, err)
		}
		if len(reply.Requests) != 1 {
			t.Fatalf("reply = %s, want the one request", encoded)
		}
		return reply
	}
	awaitQueuedWake := func(t *testing.T, f *requestFixture, token string) {
		t.Helper()
		waitUntil(t, 10*time.Second, func() bool {
			row, found, err := f.app.store.GetThreadRequest(token)
			return err == nil && found && row.DeliveredHow == store.ThreadWakeQueued
		})
	}

	t.Run("waiting in the queue", func(t *testing.T) {
		f := newRequestFixture(t)
		f.mockClaudeHoldingTheTurn(t, "starting on it")
		// No session on the caller, so the wake sits in the durable queue
		// until the thread has somewhere to put it.
		f.holdCallerTurn(t)
		ack := f.runningRequest(t, "how many rows did the backfill touch?")
		if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
			Token: ack.Token, Text: "the backfill touched 412 rows",
		}); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		awaitQueuedWake(t, f, ack.Token)

		reply := readStatus(t, f, ack.Token)
		request := reply.Requests[0]
		if request.State != store.ThreadRequestReplied || !strings.Contains(request.Answer, "412 rows") {
			t.Fatalf("request = %+v, want the reply read off the token", request)
		}
		if !request.WakeQueued {
			t.Errorf("request = %+v, want the queued wake reported", request)
		}
		if !strings.Contains(reply.Note, notice) {
			t.Errorf("note = %q, want it to name the message still coming", reply.Note)
		}
	})

	t.Run("already handed to the session", func(t *testing.T) {
		f := newRequestFixture(t)
		f.mockClaudeHoldingTheTurn(t, "starting on it")
		// The production queue, which hands a message to a live session
		// instead of holding it until the thread is started again.
		f.app.configureTriageQueueCallbacks()
		t.Cleanup(func() { f.app.flushDispatch.wg.Wait() })
		// The caller is live and mid-turn, which is where every agent that
		// calls thread_status is. Its queue hands the wake straight to the
		// session, where it waits for the turn boundary: no durable queue
		// row is left, and the message has still not arrived.
		if err := f.app.SendMessage(f.caller.ID, "look into the backfill", nil); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		waitUntil(t, 10*time.Second, func() bool {
			_, live := f.app.sessionManager().get(f.caller.ID)
			return live
		})
		ack := f.runningRequest(t, "how many rows did the backfill touch?")
		if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
			Token: ack.Token, Text: "the backfill touched 412 rows",
		}); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		awaitQueuedWake(t, f, ack.Token)
		waitUntil(t, 10*time.Second, func() bool {
			_, found, err := f.app.store.FindFlushQueueItemBySendID(f.caller.ID, threadWakeSendID(ack.Token))
			return err == nil && !found
		})
		wake := f.wakeRow(t, ack.Token)
		if usermessage.ReadProviderItemID(wake.Meta) != "" {
			t.Fatalf("the provider already took the wake up: %+v", wake)
		}

		reply := readStatus(t, f, ack.Token)
		request := reply.Requests[0]
		if request.State != store.ThreadRequestReplied || !strings.Contains(request.Answer, "412 rows") {
			t.Fatalf("request = %+v, want the reply read off the token", request)
		}
		if !request.WakeQueued {
			t.Errorf("request = %+v, want the dispatched wake still reported as coming", request)
		}
		if !strings.Contains(reply.Note, notice) {
			t.Errorf("note = %q, want it to name the message still coming", reply.Note)
		}

		// Once the provider echoes the message back, the model has read it
		// and the notice stops: nothing is owed twice.
		if err := f.app.store.UpdateItemMeta(f.caller.ID, wake.ID, `{"sendId":"`+threadWakeSendID(ack.Token)+`","provider_item_id":"u-taken-up"}`); err != nil {
			t.Fatalf("UpdateItemMeta: %v", err)
		}
		read := readStatus(t, f, ack.Token)
		if read.Requests[0].WakeQueued || strings.Contains(read.Note, notice) {
			t.Errorf("the notice outlived the message: %+v note=%q", read.Requests[0], read.Note)
		}
	})
}

// wakeRow returns the caller's `user_text` row carrying one request's wake.
func (f *requestFixture) wakeRow(t *testing.T, token string) store.Item {
	t.Helper()
	sendID := threadWakeSendID(token)
	var found store.Item
	waitUntil(t, 10*time.Second, func() bool {
		items, err := f.app.store.ListItems(f.caller.ID)
		if err != nil {
			return false
		}
		for _, item := range items {
			if item.Kind == "user_text" && strings.Contains(item.Meta, sendID) {
				found = item
				return true
			}
		}
		return false
	})
	if found.ID == "" {
		t.Fatalf("no wake message was written to the caller for %s", token)
	}
	return found
}
