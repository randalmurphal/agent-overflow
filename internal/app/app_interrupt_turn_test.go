package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/workflowhost"
)

// interruptAndRevertIfCleanMethodID is the method's FNV-1a id, which an
// older client calls by number.
const interruptAndRevertIfCleanMethodID = 753394581

// An older client calls InterruptAndRevertIfClean for a Stop on a message
// it just sent, with {expectedSendId, draft} as the second argument and []
// as the third. The call decodes through the real dispatcher under its old
// id, interrupts the turn exactly once, keeps the message and answers
// reverted:false, which that client reads as a plain interrupt.
func TestInterruptAndRevertIfCleanDecodesAnOlderClientsCall(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	insertUserItem(t, f.app.store, f.thread.ID, "u:1", 1, "never mind")
	dispatcher := transport.NewDispatcher()
	if _, err := dispatcher.Register(f.app, transport.RegisterOptions{Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList()}); err != nil {
		t.Fatal(err)
	}
	method, ok := dispatcher.LookupID(interruptAndRevertIfCleanMethodID)
	if !ok || method.Name != "InterruptAndRevertIfClean" {
		t.Fatalf("method %d = %+v (found %v), want InterruptAndRevertIfClean", interruptAndRevertIfCleanMethodID, method, ok)
	}
	params := []json.RawMessage{
		json.RawMessage(strconv.Quote(f.thread.ID)),
		json.RawMessage(`{"expectedSendId":"send-1","draft":{"content":"never mind","attachmentIds":["att-1"],"terminalChips":[{"id":"term"}],"sourceProposedPlan":{"threadId":"t","itemId":"p"}}}`),
		json.RawMessage(`[]`),
	}
	raw, frame := dispatcher.Invoke(context.Background(), method, params)
	if frame != nil {
		t.Fatalf("older client's Stop: frame %+v", frame)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	if reverted, present := result["reverted"]; !present || reverted != false {
		t.Fatalf("result = %s, want reverted:false present", raw)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
	if _, found, err := f.app.store.GetThreadItem(f.thread.ID, "u:1"); err != nil || !found {
		t.Fatalf("the message must stay: found=%v err=%v", found, err)
	}
	if _, found, err := f.app.store.GetThreadDraft(f.thread.ID); err != nil || found {
		t.Fatalf("the Stop wrote a composer draft: found=%v err=%v", found, err)
	}
}

// claudeTUINotices counts the thread's discarded-message notices.
func claudeTUINotices(t *testing.T, st *store.Store, threadID string) int {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	n := 0
	for _, item := range items {
		if item.Kind == "notification" && item.Summary == claudeTUIDiscardedMessageNotice {
			n++
		}
	}
	return n
}

// openTurnIndex is the turn an interrupt would stop now.
func openTurnIndex(t *testing.T, app *App, threadID string) int {
	t.Helper()
	turn := app.triage.InterruptTurnIndex(threadID)
	if turn < 0 {
		t.Fatal("no open turn")
	}
	return turn
}

// The TUI discards a message stopped before it replies, whoever presses
// Esc, so the thread says so once. Agent output in the turn, or a provider
// that keeps the message, gets no notice. The Claude recorder session stands
// in for every provider: the decision is the live session's provider.
func TestInterruptNoticesAClaudeTUIMessageStoppedBeforeAReply(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider provider.ProviderKind
		agent    *store.Item
		stop     func(f *agentKillFixture) error
		want     int
	}{
		{name: "claude-tui think-only", provider: provider.ClaudeTUI, want: 1,
			stop: func(f *agentKillFixture) error { return f.app.InterruptTurn(f.thread.ID, nil) }},
		{name: "claude-tui think-only, workflow takeover", provider: provider.ClaudeTUI, want: 1,
			stop: func(f *agentKillFixture) error {
				return f.app.interruptTurnForWorkflow(context.Background(), f.thread.ID, workflowhost.InterruptForTakeover)
			}},
		{name: "claude-tui think-only, older client", provider: provider.ClaudeTUI, want: 1,
			stop: func(f *agentKillFixture) error {
				_, err := f.app.InterruptAndRevertIfClean(f.thread.ID, InterruptRevertOptions{}, nil)
				return err
			}},
		{name: "claude-tui with a reply", provider: provider.ClaudeTUI, want: 0,
			agent: &store.Item{ID: "a:0", Kind: "assistant_text", Summary: "Sure"},
			stop:  func(f *agentKillFixture) error { return f.app.InterruptTurn(f.thread.ID, nil) }},
		{name: "claude-tui with a tool call", provider: provider.ClaudeTUI, want: 0,
			agent: &store.Item{ID: "tool:0", Kind: "tool_call", Status: "running", Summary: "Read"},
			stop:  func(f *agentKillFixture) error { return f.app.InterruptTurn(f.thread.ID, nil) }},
		{name: "headless claude think-only", provider: provider.Claude, want: 0,
			stop: func(f *agentKillFixture) error { return f.app.InterruptTurn(f.thread.ID, nil) }},
		{name: "codex think-only", provider: provider.Codex, want: 0,
			stop: func(f *agentKillFixture) error { return f.app.InterruptTurn(f.thread.ID, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newClaudeAgentKillFixture(t)
			sess, _ := f.app.sessionManager().get(f.thread.ID)
			sess.Provider = string(tc.provider)
			f.app.sessionManager().put(f.thread.ID, sess)
			turn := openTurnIndex(t, f.app, f.thread.ID)
			insertUserItem(t, f.app.store, f.thread.ID, "u:open", turn, "do the thing")
			appendTurnRow(t, f.app.store, store.Item{ID: "think:open", ThreadID: f.thread.ID, TurnIndex: turn, ItemIndex: 1, Kind: "thinking"})
			if tc.agent != nil {
				row := *tc.agent
				row.ThreadID, row.TurnIndex, row.ItemIndex = f.thread.ID, turn, 2
				appendTurnRow(t, f.app.store, row)
			}

			if err := tc.stop(f); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if n := f.interrupts(t); n != 1 {
				t.Fatalf("interrupts = %d, want 1", n)
			}
			if got := claudeTUINotices(t, f.app.store, f.thread.ID); got != tc.want {
				t.Fatalf("notices = %d, want %d", got, tc.want)
			}
		})
	}
}

// Rows that never reach the model with an unanswered prompt keep the notice;
// any other kind is something the TUI kept, and a turn without the reader's
// own message has nothing to discard.
func TestClaudeTUIStopDiscardsMessageReadsTheTurnRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []store.Item
		want bool
	}{
		{name: "message only", want: true},
		{name: "api_retry", rows: []store.Item{{Kind: "api_retry", Role: "system"}}, want: true},
		{name: "api_error", rows: []store.Item{{Kind: "api_error"}}, want: true},
		{name: "error", rows: []store.Item{{Kind: "error", Role: "system"}}, want: true},
		{name: "steered messages", rows: []store.Item{{Kind: "user_text", Role: "user"}}, want: true},
		{name: "assistant_text", rows: []store.Item{{Kind: "assistant_text"}}},
		{name: "notification", rows: []store.Item{{Kind: "notification", Role: "system"}}},
		{name: "compaction", rows: []store.Item{{Kind: "compaction", Role: "system"}}},
		{name: "wire-only user_text", rows: []store.Item{{Kind: "user_text", Role: "user", Meta: `{"wire_only":true}`}}},
		{name: "subagent prompt", rows: []store.Item{{Kind: "user_text", Role: "user", ParentID: "task:0"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t)
			thread := createAppTestThread(t, app, "tui-discard", string(provider.ClaudeTUI), t.TempDir())
			insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
			for i, row := range tc.rows {
				row.ID = "row:" + strconv.Itoa(i)
				row.ThreadID = thread.ID
				row.ItemIndex = i + 1
				if row.ParentID != "" {
					appendTurnRow(t, app.store, store.Item{ID: row.ParentID, ThreadID: thread.ID, ItemIndex: 0, Kind: "tool_call", ToolName: "Task"})
				}
				appendTurnRow(t, app.store, row)
			}
			sess := session{Provider: string(provider.ClaudeTUI)}
			if got := app.claudeTUIStopDiscardsMessage(thread.ID, sess, 0, -1); got != tc.want {
				t.Fatalf("discards = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("no open turn", func(t *testing.T) {
		t.Parallel()
		app := newTestApp(t)
		thread := createAppTestThread(t, app, "tui-no-turn", string(provider.ClaudeTUI), t.TempDir())
		insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
		if app.claudeTUIStopDiscardsMessage(thread.ID, session{Provider: string(provider.ClaudeTUI)}, anyOpenTurn, -1) {
			t.Fatal("a stop with no open turn noticed a discarded message")
		}
	})
	// A queued message's row waits for its echo, so its pending send is the
	// only sign of the message until then.
	t.Run("pending send whose row is deferred", func(t *testing.T) {
		t.Parallel()
		app := newTestApp(t)
		thread := createAppTestThread(t, app, "tui-deferred", string(provider.ClaudeTUI), t.TempDir())
		insertUserItem(t, app.store, thread.ID, "u:0", 0, "answered long ago")
		if !app.claudeTUIStopDiscardsMessage(thread.ID, session{Provider: string(provider.ClaudeTUI)}, anyOpenTurn, 1) {
			t.Fatal("a stop of a pending send with no row yet noticed nothing")
		}
	})
	t.Run("pending send on a turn with agent output", func(t *testing.T) {
		t.Parallel()
		app := newTestApp(t)
		thread := createAppTestThread(t, app, "tui-pending-output", string(provider.ClaudeTUI), t.TempDir())
		insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
		appendTurnRow(t, app.store, store.Item{ID: "a:0", ThreadID: thread.ID, ItemIndex: 1, Kind: "assistant_text"})
		if app.claudeTUIStopDiscardsMessage(thread.ID, session{Provider: string(provider.ClaudeTUI)}, anyOpenTurn, 0) {
			t.Fatal("a pending send on a turn the agent answered noticed a discarded message")
		}
	})
	// The pending send is sampled first, so a turn that opened since is the
	// one read.
	t.Run("an open turn outranks the pending send", func(t *testing.T) {
		t.Parallel()
		app := newTestApp(t)
		thread := createAppTestThread(t, app, "tui-open-wins", string(provider.ClaudeTUI), t.TempDir())
		insertUserItem(t, app.store, thread.ID, "u:0", 0, "hello")
		appendTurnRow(t, app.store, store.Item{ID: "a:0", ThreadID: thread.ID, ItemIndex: 1, Kind: "assistant_text"})
		if app.claudeTUIStopDiscardsMessage(thread.ID, session{Provider: string(provider.ClaudeTUI)}, 0, 1) {
			t.Fatal("the pending send's turn was read while a turn was open")
		}
	})
	t.Run("no message", func(t *testing.T) {
		t.Parallel()
		app := newTestApp(t)
		thread := createAppTestThread(t, app, "tui-no-message", string(provider.ClaudeTUI), t.TempDir())
		appendTurnRow(t, app.store, store.Item{ID: "think:0", ThreadID: thread.ID, Kind: "thinking"})
		if app.claudeTUIStopDiscardsMessage(thread.ID, session{Provider: string(provider.ClaudeTUI)}, 0, -1) {
			t.Fatal("a turn without the reader's message noticed a discarded message")
		}
	})
}

// newClaudeTUIInterruptThread is a claude-tui thread with an open think-only
// turn and a live TUI session over a mock binary. closed closes the session
// first, so its Esc cannot be delivered.
func newClaudeTUIInterruptThread(t *testing.T, closed bool) (*App, store.Thread) {
	t.Helper()
	app := newTestApp(t)
	app.triage = triage.NewRouter(app.store, app.emit)
	dir := t.TempDir()
	thread := createAppTestThread(t, app, "tui-interrupt", string(provider.ClaudeTUI), dir)
	if err := app.triage.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: "turn-0", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	turn := openTurnIndex(t, app, thread.ID)
	insertUserItem(t, app.store, thread.ID, "u:0", turn, "the prompt")
	appendTurnRow(t, app.store, store.Item{ID: "think:0", ThreadID: thread.ID, TurnIndex: turn, ItemIndex: 1, Kind: "thinking"})

	binary := filepath.Join(dir, "mock-claude-tui")
	mockexec.Write(t, binary, "#!/bin/sh\nwhile IFS= read -r _; do :; done\n")
	sess, err := claudetui.NewSession(context.Background(), thread.ID, claudetui.Config{
		Binary:  binary,
		WorkDir: dir,
		Env:     []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claudetui.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if closed {
		if err := sess.Close(); err != nil {
			t.Fatalf("close TUI session: %v", err)
		}
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.ClaudeTUI), Token: "tui", ClaudeTUI: sess})
	return app, thread
}

// Through a real TUI session: the Esc reaches the terminal, the turn is
// recorded as stopped, the message stays, and the notice follows the stop.
func TestInterruptTurnClaudeTUINoticeFollowsTheStop(t *testing.T) {
	t.Parallel()
	app, thread := newClaudeTUIInterruptThread(t, false)

	if err := app.InterruptTurn(thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	stopped, noticed := -1, -1
	for i, item := range items {
		switch {
		case item.ID == "u:0":
		case item.Kind == triage.ItemKindError && item.Summary == triage.StoppedByUser:
			stopped = i
		case item.Kind == "notification" && item.Summary == claudeTUIDiscardedMessageNotice:
			if noticed >= 0 {
				t.Fatalf("notice written twice: %+v", items)
			}
			noticed = i
		}
	}
	if stopped < 0 || noticed < 0 || noticed < stopped {
		t.Fatalf("rows = %+v, want the stop row then one notice", items)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "u:0"); err != nil || !found {
		t.Fatalf("the message must stay: found=%v err=%v", found, err)
	}
}

// An Esc that cannot be delivered stopped nothing, so the TUI kept the
// message: the call fails, the message stays, and there is no notice.
func TestInterruptTurnClaudeTUIFailedEscWritesNoNotice(t *testing.T) {
	t.Parallel()
	app, thread := newClaudeTUIInterruptThread(t, true)

	if err := app.InterruptTurn(thread.ID, nil); err == nil {
		t.Fatal("InterruptTurn succeeded with no terminal to write the Esc to")
	}
	if got := claudeTUINotices(t, app.store, thread.ID); got != 0 {
		t.Fatalf("notices = %d after a failed Esc, want none", got)
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "u:0"); err != nil || !found {
		t.Fatalf("the message must stay: found=%v err=%v", found, err)
	}
}

// With no open turn, only a send still awaiting its echo is in flight for
// the TUI to discard: its prompt is submitted and its turn has not opened.
func TestInterruptTurnClaudeTUIWithNoOpenTurnNoticesOnlyAPendingSend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		pending bool
		want    int
	}{
		{name: "idle thread", want: 0},
		{name: "send awaiting its echo", pending: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newClaudeAgentKillFixture(t)
			f.app.triage = triage.NewRouter(f.app.store, func(eventchan.Channel, any) {})
			sess, _ := f.app.sessionManager().get(f.thread.ID)
			sess.Provider = string(provider.ClaudeTUI)
			f.app.sessionManager().put(f.thread.ID, sess)
			insertUserItem(t, f.app.store, f.thread.ID, "u:0", 0, "answered long ago")
			if tc.pending {
				insertUserItem(t, f.app.store, f.thread.ID, "u:1", 1, "just sent")
				f.app.triage.RegisterPendingSendWithExpectation(f.thread.ID, "u:1", 1, triage.PendingSendExpectation{})
			}

			if err := f.app.InterruptTurn(f.thread.ID, nil); err != nil {
				t.Fatalf("InterruptTurn: %v", err)
			}
			if got := claudeTUINotices(t, f.app.store, f.thread.ID); got != tc.want {
				t.Fatalf("notices = %d, want %d", got, tc.want)
			}
		})
	}
}

// An interrupt that entered while a Codex thread/revert owned the app
// thread lock names the old history generation and must not wake afterward to
// send a startup interrupt against the reloaded thread.
func TestInterruptTurnDropsIntentThatWaitedAcrossCodexRevert(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	workspace := t.TempDir()
	thread := createAppTestThread(t, app, "codex-stale-interrupt", "codex", workspace)
	thread.SessionRef = "provider-codex-stale-interrupt"
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}

	interruptLog := filepath.Join(t.TempDir(), "interrupt.log")
	binary := writeCodexForkAtBinary(t, codexForkMock{
		resumedThreadID:  thread.SessionRef,
		forkedThreadID:   "unused-fork",
		userAgent:        "codex_cli_rs/0.149.0 (Linux; x86_64)",
		historyMode:      "paginated",
		interruptLogPath: interruptLog,
		activeTurnID:     "turn-live",
	})
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         binary,
		Model:          "test-model",
		WorkDir:        workspace,
		ResumeThreadID: thread.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "codex-stale-interrupt-token",
		Codex:    sess,
	})

	unlock := app.threadLocks().Lock(thread.ID)
	interruptDone := make(chan error, 1)
	go func() { interruptDone <- app.InterruptTurn(thread.ID, nil) }()
	waitForThreadLockRefs(t, app.threadLocks(), thread.ID, 2)
	if _, err := sess.Revert(context.Background(), "turn-live"); err != nil {
		unlock()
		t.Fatalf("Revert: %v", err)
	}
	unlock()

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("queued InterruptTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued InterruptTurn did not settle after revert")
	}
	if data, err := os.ReadFile(interruptLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("stale interrupt reached reloaded thread: %s", data)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read interrupt log: %v", err)
	}
}

// TestInterruptTurnDoesNotRetargetAReplacementSession closes the wider form
// of the same race. An interrupt belongs to the provider process visible when
// it enters. If that process is replaced while the call waits on the thread
// lock, the interrupt must not hit the replacement's unrelated work.
func TestInterruptTurnDoesNotRetargetAReplacementSession(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "interrupt-replacement", "codex", t.TempDir())
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "old-session",
	})

	unlock := app.threadLocks().Lock(thread.ID)
	interruptDone := make(chan error, 1)
	go func() { interruptDone <- app.InterruptTurn(thread.ID, nil) }()
	waitForThreadLockRefs(t, app.threadLocks(), thread.ID, 2)
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "replacement-session",
	})
	unlock()

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("queued InterruptTurn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued InterruptTurn did not settle after session replacement")
	}
}
