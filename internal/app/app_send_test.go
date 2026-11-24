package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

func TestSendMessageHappyPath(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var sentThread, sentContent string
	app.sendMessageFn = func(threadID, content string, attachmentIDs []string) error {
		sentThread = threadID
		sentContent = content
		return nil
	}
	// sendMessageFn bypasses session lookup, but sendMessage still checks
	// for it first. Populate the session map so the real codepath up to the
	// sendMessageFn shortcut is exercised.
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex)})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Hello", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if sentThread != thread.ID {
		t.Fatalf("sent threadID = %q, want %q", sentThread, thread.ID)
	}
	if sentContent != "Hello" {
		t.Fatalf("sent content = %q, want Hello", sentContent)
	}
}

// TestSendMessageLazyStartsSession covers the "new thread → type → send"
// path: a freshly-created thread has no provider session yet, so SendMessage
// must kick off startSession before forwarding the user message. This
// replaces the prior "no active session" error test — thread creation no
// longer spawns a provider process, and the UX no longer surfaces a
// disconnected banner while the user is composing their first message.
func TestSendMessageLazyStartsSession(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-lazy-start")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var startCalls int
	app.startSessionFn = func(threadID string) error {
		if threadID != thread.ID {
			t.Errorf("startSessionFn threadID = %q, want %q", threadID, thread.ID)
		}
		startCalls++
		// Register a session entry so the post-start lookup succeeds. The
		// empty session struct will fail at sendToProvider ("no provider"),
		// which is fine — this test only asserts that the lazy-start fired.
		app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "lazy"})
		return nil
	}

	// Don't use sendMessageFn: it short-circuits before the lazy-start
	// check, so we'd never hit the code under test.
	_, _ = app.SendMessageWithOptions(context.Background(), thread.ID, "Hello", pageSendOptions(uuid.NewString()))
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d, want 1 (lazy-start must fire for session-less thread)", startCalls)
	}

	// A second send on the now-populated session must not re-trigger
	// lazy-start — the session is already live.
	_, _ = app.SendMessageWithOptions(context.Background(), thread.ID, "Second", pageSendOptions(uuid.NewString()))
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d after second send, want 1 (no double-start)", startCalls)
	}
}

func TestSendMessagePersistsUserItemBeforeLazyStartCompletes(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-visible-before-start")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	userItemUpserted := make(chan store.Item, 1)
	app.testEmitHook = func(name string, data any) {
		if name != "provider:item_event" {
			return
		}
		event, ok := data.(triage.ItemStreamEvent)
		if !ok || event.Item == nil {
			return
		}
		if event.Item.ThreadID == thread.ID && event.Item.Kind == "user_text" && event.Item.Summary == "visible now" {
			select {
			case userItemUpserted <- *event.Item:
			default:
			}
		}
	}

	startEntered := make(chan struct{})
	allowStartReturn := make(chan struct{})
	var releaseStartOnce sync.Once
	releaseStart := func() {
		releaseStartOnce.Do(func() {
			close(allowStartReturn)
		})
	}
	defer releaseStart()
	app.startSessionFn = func(threadID string) error {
		if threadID != thread.ID {
			t.Errorf("startSessionFn threadID = %q, want %q", threadID, thread.ID)
		}
		close(startEntered)
		<-allowStartReturn
		app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "lazy"})
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := app.SendMessageWithOptions(context.Background(), thread.ID, "visible now", pageSendOptions(uuid.NewString()))
		errCh <- err
	}()

	// The bounds only end a hung test. This package runs its tests in
	// parallel, and a loaded host stalls one for over a second.
	select {
	case <-startEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("SendMessage did not enter lazy start")
	}

	select {
	case <-userItemUpserted:
	default:
		t.Fatal("user_text upsert should emit before lazy start completes")
	}

	releaseStart()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "session has no provider") {
			t.Fatalf("SendMessage() error = %v, want session-has-no-provider after lazy start returns", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendMessage did not return after lazy start was released")
	}
}

func TestSendMessagePersistsUserItemAndErrorWhenLazyStartFails(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-start-fail")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.startSessionFn = func(string) error {
		return errors.New("provider boot exploded")
	}

	_, err := app.SendMessageWithOptions(context.Background(), thread.ID, "still visible", pageSendOptions(uuid.NewString()))
	if err == nil || !strings.Contains(err.Error(), "start session") || !strings.Contains(err.Error(), "provider boot exploded") {
		t.Fatalf("SendMessage() error = %v, want the wrapped start-session failure", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userFound, errorFound bool
	for _, item := range items {
		if item.Kind == "user_text" && item.Role == "user" && item.Summary == "still visible" {
			userFound = true
		}
		if item.Kind == "error" && strings.Contains(item.Summary, "provider boot exploded") {
			errorFound = true
		}
	}
	if !userFound || !errorFound {
		t.Fatalf("expected user_text + error after lazy-start failure, got %+v", items)
	}
}

func TestSendMessageWithOptionsAppliesRuntimeModeBeforeLazyStart(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	emissions := captureEmissions(app)
	thread := testThread("thread-send-runtime-before-start")
	thread.RuntimeMode = string(provider.RuntimeApprovalRequired)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var startCalls int
	app.startSessionFn = func(threadID string) error {
		startCalls++
		if threadID != thread.ID {
			t.Fatalf("startSessionFn threadID = %q, want %q", threadID, thread.ID)
		}
		stored, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatalf("GetThread during lazy start: %v", err)
		}
		if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
			t.Fatalf("lazy start saw runtime mode = %q, want full-access", stored.RuntimeMode)
		}
		app.sessionManager().put(threadID, session{Provider: string(provider.Codex), Token: "lazy"})
		return nil
	}

	_, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Hello", SendMessageOptions{
		SendID:            uuid.NewString(),
		ReconcileBySendID: true,
		RuntimeMode:       string(provider.RuntimeFullAccess),
	})
	if err == nil {
		t.Fatal("SendMessageWithOptions() error = nil, want fake provider send failure")
	}
	if !strings.Contains(err.Error(), "session has no provider") {
		t.Fatalf("SendMessageWithOptions() error = %v, want fake provider send failure", err)
	}
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d, want 1", startCalls)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.RuntimeMode != string(provider.RuntimeFullAccess) {
		t.Fatalf("stored runtime mode = %q, want full-access", stored.RuntimeMode)
	}
	fired := emissionsFor(emissions, "thread:runtime_mode_changed")
	if len(fired) != 1 {
		t.Fatalf("runtime_mode_changed emissions = %d, want 1", len(fired))
	}
}

// Pins the click-time mark contract: once SendMessageWithOptions
// persists the user_text row, the plan is implemented even if the
// downstream sendToProvider call fails. The mark is sticky on
// failure — the user already committed to the implementation by
// clicking, and the sibling failed-send error row makes the failure
// visible for a retry. Also confirms the mode flip stays committed.
func TestSendMessageWithOptionsImplementSendFailureKeepsPlanAccepted(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-implement-mode-revert")
	thread.Provider = string(provider.Codex)
	thread.Mode = "plan"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"x","lineCount":1,"charCount":1}`,
		Data:      []byte("# Plan"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}
	// Codex session with no provider: sendToProvider trips
	// "session has no provider" so the helper-then-send order is
	// observable — the plan-implemented mark from
	// applyProposedPlanAcceptance must already be durable when the
	// provider write fails.
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "no-provider"})

	_, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Implement the plan.", SendMessageOptions{
		SendID:             "implement-plan",
		ReconcileBySendID:  true,
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	})
	if err == nil {
		t.Fatal("SendMessageWithOptions() error = nil, want session-has-no-provider failure")
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Mode != "chat" {
		t.Errorf("stored mode = %q, want chat (mode flip stays committed once user message persists)", stored.Mode)
	}
	state, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState: %v", err)
	}
	if !found {
		t.Fatal("plan state missing")
	}
	if state.ImplementedAt == 0 {
		t.Error("ImplementedAt = 0, want > 0 (mark is sticky once SendMessageWithOptions persists the user_text — provider failure must not revert)")
	}
	row, found, err := app.store.FindUserTextItemBySendID(thread.ID, "implement-plan")
	if err != nil || !found {
		t.Fatalf("the implement send's row: found=%v err=%v", found, err)
	}
	if state.ImplementedByItemID != row.ID {
		t.Errorf("ImplementedByItemID = %q, want %q (the persisted user_text row id)", state.ImplementedByItemID, row.ID)
	}
}

// An implement-the-plan send on a plan-mode thread flips the thread to chat
// atomically with persisting the user message (splitting the two on the
// frontend used to leave plan-mode threads stuck reading "chat" when send
// failed), records the plan on the user row, and marks the plan implemented
// by that row. A second implement send against the same plan (a network
// retry, a double-click) records the plan on its own row but must NOT
// overwrite the original attribution: the mark is gated on
// `WHERE implemented_at = 0` so the first click owns the badge forever.
func TestSendMessageWithOptionsPersistsSourceProposedPlan(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-source-plan")
	thread.Provider = string(provider.Claude)
	thread.Mode = "plan"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Fix plan","preview":"do it","lineCount":1,"charCount":5}`,
		Data:      []byte("# Fix plan\n\nDo it."),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0), Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})

	updated, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Implement the plan.", SendMessageOptions{
		SendID: "first-implementation", ReconcileBySendID: true,
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	})
	if err != nil {
		t.Fatalf("SendMessageWithOptions() error = %v", err)
	}
	if updated.Mode != "chat" {
		t.Errorf("returned thread mode = %q, want chat", updated.Mode)
	}

	// applyProposedPlanAcceptance fires synchronously between PersistItem
	// and sendToProvider, so the implemented mark is durable by the time
	// SendMessageWithOptions returns. No wire-init simulation is needed.
	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if stored.Mode != "chat" {
		t.Errorf("stored thread mode = %q, want chat", stored.Mode)
	}
	if stored.HasActionableProposedPlan {
		t.Error("stored thread.HasActionableProposedPlan = true, want false (plan was just implemented)")
	}
	first, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil || !found {
		t.Fatalf("first GetProposedPlanState err=%v found=%v", err, found)
	}
	if first.ImplementedAt == 0 || first.ImplementedByItemID == "" {
		t.Fatalf("first send did not mark plan: %+v", first)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
			break
		}
	}
	if userItem.Meta == "" {
		t.Fatal("user item meta is empty")
	}
	var meta userMessageMeta
	if err := json.Unmarshal([]byte(userItem.Meta), &meta); err != nil {
		t.Fatalf("unmarshal user meta: %v", err)
	}
	if meta.SourceProposedPlan == nil {
		t.Fatal("sourceProposedPlan missing")
	}
	if meta.SourceProposedPlan.ItemID != "plan-item" || meta.SourceProposedPlan.PayloadID != "plan-payload" {
		t.Fatalf("sourceProposedPlan = %+v, want plan item/payload", meta.SourceProposedPlan)
	}

	plans, err := app.store.ListThreadProposedPlans(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadProposedPlans: %v", err)
	}
	if len(plans) != 1 || !strings.Contains(plans[0].Meta, `"planImplementedByItemId":"`+userItem.ID+`"`) {
		t.Fatalf("plan meta = %v, want implemented by %s", plans, userItem.ID)
	}

	// The passthrough mock emits no echoes/results. Complete its first input
	// so the second click exercises persisted plan attribution, not only
	// queue admission while the first implementation is still pending.
	acknowledgeMockClaudeSend(t, app, thread.ID)
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Implement the plan again.", SendMessageOptions{
		SendID: "second-implementation", ReconcileBySendID: true,
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	}); err != nil {
		t.Fatalf("second SendMessageWithOptions() error = %v", err)
	}
	if _, found, err := app.store.FindUserTextItemBySendID(thread.ID, "second-implementation"); err != nil || !found {
		t.Fatalf("second implementation did not persist: found=%v err=%v", found, err)
	}

	items, err = app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems after second send: %v", err)
	}
	var secondUserItem store.Item
	for _, item := range items {
		if item.Role == "user" && item.Summary == "Implement the plan again." {
			if secondUserItem.ID != "" {
				t.Fatalf("found multiple second implementation user items: %q and %q", secondUserItem.ID, item.ID)
			}
			secondUserItem = item
		}
	}
	if secondUserItem.ID == "" {
		t.Fatal("second user item missing")
	}
	if secondUserItem.Meta == "" {
		t.Fatal("second user item meta is empty")
	}
	var secondMeta userMessageMeta
	if err := json.Unmarshal([]byte(secondUserItem.Meta), &secondMeta); err != nil {
		t.Fatalf("unmarshal second user meta: %v", err)
	}
	if secondMeta.SourceProposedPlan == nil || secondMeta.SourceProposedPlan.ItemID != "plan-item" {
		t.Fatalf("second sourceProposedPlan = %+v, want plan-item", secondMeta.SourceProposedPlan)
	}

	second, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil || !found {
		t.Fatalf("second GetProposedPlanState err=%v found=%v", err, found)
	}
	if second.ImplementedAt != first.ImplementedAt {
		t.Errorf("ImplementedAt = %d, want %d (must not be overwritten by re-click)", second.ImplementedAt, first.ImplementedAt)
	}
	if second.ImplementedByItemID != userItem.ID || first.ImplementedByItemID != userItem.ID {
		t.Errorf("ImplementedByItemID = %q then %q, want the first implementation item %q (re-click must not steal attribution)", first.ImplementedByItemID, second.ImplementedByItemID, userItem.ID)
	}
	threadAfterSecondSend, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after second send: %v", err)
	}
	if threadAfterSecondSend.HasActionableProposedPlan {
		t.Fatal("HasActionableProposedPlan = true after second send, want accepted plan to stay non-actionable")
	}
}

func TestSendMessageWithOptionsPersistsCrossThreadSourceProposedPlan(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	sourceThread := testThread("thread-source-plan")
	sourceThread.Provider = string(provider.Claude)
	targetThread := testThread("thread-implement-plan")
	targetThread.Provider = string(provider.Claude)
	targetThread.WorkspacePath = t.TempDir()
	for _, thread := range []store.Thread{sourceThread, targetThread} {
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", thread.ID, err)
		}
	}

	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  sourceThread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Fix plan","preview":"do it","lineCount":1,"charCount":5}`,
		Data:      []byte("# Fix plan\n\nDo it."),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(sourceThread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		targetThread.ID,
		claude.Config{Reaper: procutiltest.Reaper(), Retention: claude.UserHomeRetention(0), Binary: writeClaudePassthroughBinary(t), WorkDir: targetThread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(targetThread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})

	_, err = app.SendMessageWithOptions(context.Background(), targetThread.ID, "Implement the plan.", SendMessageOptions{
		SendID:             "implement-plan",
		ReconcileBySendID:  true,
		SourceProposedPlan: &SourceProposedPlan{ThreadID: sourceThread.ID, ItemID: "plan-item"},
	})
	if err != nil {
		t.Fatalf("SendMessageWithOptions() error = %v", err)
	}

	state, found, err := app.store.GetProposedPlanState(sourceThread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState() error = %v", err)
	}
	row, sent, err := app.store.FindUserTextItemBySendID(targetThread.ID, "implement-plan")
	if err != nil || !sent {
		t.Fatalf("the implement send's row: found=%v err=%v", sent, err)
	}
	if !found || state.ImplementedByThreadID != targetThread.ID || state.ImplementedByItemID != row.ID {
		t.Fatalf("state = %+v, want implemented by the target's user row %s", state, row.ID)
	}
}

func TestSendMessageIncrementsTurnIndex(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-turn-index")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Seed a prior turn so the next user message should get turn index 1.
	if err := app.store.InsertItem(store.Item{
		ID:        "existing-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Summary:   "prior turn",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertItem() error = %v", err)
	}

	// Use a real claude session backed by a passthrough binary so sendMessage
	// exercises the full code path including item persistence and turn indexing.
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Next message", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}

	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
			break
		}
	}
	if userItem.ID == "" {
		t.Fatal("expected persisted user item")
	}
	if userItem.TurnIndex != 1 {
		t.Fatalf("user item TurnIndex = %d, want 1", userItem.TurnIndex)
	}
	if userItem.Summary != "Next message" {
		t.Fatalf("user item Summary = %q, want Next message", userItem.Summary)
	}
}

func TestSendMessageRecordsMessageAnchorForEachUserMessage(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	workspace := t.TempDir()
	thread := testThread("thread-send-first-anchor")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "first turn", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
			break
		}
	}
	if userItem.ID == "" {
		t.Fatal("expected persisted user item")
	}
	if userItem.TurnIndex != 0 {
		t.Fatalf("first user item TurnIndex = %d, want 0", userItem.TurnIndex)
	}

	// The minted uuid lands on BOTH the row meta AND the anchor at send
	// time, before any echo: a rollback to the message can run before
	// Claude's replay echo arrives, and resolveMessageAnchor returns the
	// anchor as-is when one exists, so an empty id would drop the rollback
	// to the ordinal-walk slice. No provider echo is simulated here.
	var meta map[string]any
	if err := json.Unmarshal([]byte(userItem.Meta), &meta); err != nil {
		t.Fatalf("decode user meta %q: %v", userItem.Meta, err)
	}
	rowID, _ := meta["provider_item_id"].(string)
	if rowID == "" {
		t.Fatalf("user row has no provider_item_id at persist time — send-time pre-stamp did not fire (meta: %q)", userItem.Meta)
	}
	anchor, ok, err := app.store.GetMessageAnchor(thread.ID, userItem.ID)
	if err != nil || !ok {
		t.Fatalf("anchor for first user item missing after first send: ok=%v err=%v", ok, err)
	}
	if anchor.ProviderUserMessageID != rowID {
		t.Fatalf("anchor ProviderUserMessageID = %q, want %q (must mirror the pre-stamped row meta so an early rollback keys on the UUID-slice path)", anchor.ProviderUserMessageID, rowID)
	}

	acknowledgeMockClaudeSend(t, app, thread.ID)
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "second turn", pageSendOptions("second-turn")); err != nil {
		t.Fatalf("SendMessage(second) error = %v", err)
	}
	second, found, err := app.store.FindUserTextItemBySendID(thread.ID, "second-turn")
	if err != nil || !found || second.TurnIndex != 1 {
		t.Fatalf("second user row = %+v, found=%v err=%v, want turn 1", second, found, err)
	}
	if _, ok, err := app.store.GetMessageAnchor(thread.ID, second.ID); err != nil || !ok {
		t.Fatalf("anchor for second user item missing after second send: ok=%v err=%v", ok, err)
	}
}

// TestSendMessageDoesNotPreStampProviderItemIDForCodex pins the Claude
// gate on the send-time-uuid mint. Codex assigns its own item ids and
// ignores a supplied uuid; stamping a fabricated id on a Codex row would
// mis-seed it and trip attachProviderItemIDToUserRow's drift log on every
// echo. The no-provider Codex session makes sendToProvider fail AFTER the
// user row is persisted, so the persisted row reflects the send-time state.
func TestSendMessageDoesNotPreStampProviderItemIDForCodex(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-codex-no-prestamp")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "no-provider"})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "hello codex", pageSendOptions(uuid.NewString())); err == nil {
		t.Fatal("SendMessageWithOptions on no-provider Codex session returned nil, want failure")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, it := range items {
		if it.Role == "user" {
			userItem = it
			break
		}
	}
	if userItem.ID == "" {
		t.Fatal("expected persisted user item even on send failure")
	}
	if userItem.Meta != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(userItem.Meta), &meta); err != nil {
			t.Fatalf("decode meta %q: %v", userItem.Meta, err)
		}
		if id, _ := meta["provider_item_id"].(string); id != "" {
			t.Fatalf("Codex user row carries provider_item_id %q at send time — pre-stamp must be gated on Claude", id)
		}
	}
}

func TestSendMessageGeneratesClaudeThreadTitleOnFirstTurn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-title")
	thread.Title = "New Thread"
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.generateThreadTitleFn = func(thread store.Thread, message string, _ []store.Attachment) (string, error) {
		if message != "Fix reconnect spinner on resume" {
			t.Fatalf("message = %q, want first user turn", message)
		}
		return ` "Reconnect spinner resume fix" `, nil
	}

	emitted := make(chan triage.ThreadUpdateEvent, 4)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
		}
		emitted <- evt
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Fix reconnect spinner on resume", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	foundTitle := false
	for !foundTitle {
		select {
		case evt := <-emitted:
			if evt.Title != nil && *evt.Title == "Reconnect spinner resume fix" {
				foundTitle = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for thread rename event")
		}
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Reconnect spinner resume fix" {
		t.Fatalf("stored title = %q, want generated title", stored.Title)
	}
}

func TestSendMessageDoesNotOverwriteRenamedThreadTitle(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-title-custom")
	thread.Title = "New Thread"
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Channel-gated title generator: the fake generator blocks on
	// generatorGate until the test explicitly releases it, and signals
	// generatorDone when its inner write attempt has landed. That
	// replaces the former 100ms sleep in the generator and 250ms sleep
	// after RenameThread — both were heuristic windows that hid a
	// real ordering contract. With the gate we can enforce the exact
	// scenario: SendMessage kicks off the background title generator,
	// the user renames while the generator is still blocked, we then
	// release the generator and wait for it to settle.
	generatorGate := make(chan struct{})
	generatorDone := make(chan struct{})
	app.generateThreadTitleFn = func(store.Thread, string, []store.Attachment) (string, error) {
		<-generatorGate
		defer close(generatorDone)
		return "Generated title", nil
	}

	renamedByGenerator := make(chan triage.ThreadUpdateEvent, 4)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
		}
		if evt.Title != nil && *evt.Title == "Generated title" {
			renamedByGenerator <- evt
		}
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Fix reconnect spinner on resume", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if err := app.RenameThread(thread.ID, "Keep this custom title"); err != nil {
		t.Fatalf("RenameThread() error = %v", err)
	}

	// Release the generator now that the user rename has already
	// landed; the generator's post-rename write must be suppressed
	// because the thread no longer carries the original "New Thread"
	// title.
	close(generatorGate)
	select {
	case <-generatorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("title generator never completed after gate release")
	}

	select {
	case evt := <-renamedByGenerator:
		t.Fatalf("unexpected generator rename event after user override: %+v", evt)
	default:
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Title != "Keep this custom title" {
		t.Fatalf("stored title = %q, want custom title", stored.Title)
	}
}

func TestSendMessageRenamesTemporaryWorktreeBranchOnFirstTurn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)

	thread := testThread("thread-send-rename-worktree")
	thread.Provider = string(provider.Claude)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	worktreePath, err := worktreePathOf(app.PrepareThreadWorktree(thread.ID, "", "", false))
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}

	thread, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = worktreePath
	if err := app.store.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread() error = %v", err)
	}

	app.generateBranchNameFn = func(thread store.Thread, message string) (string, error) {
		if message != "Fix reconnect spinner on resume" {
			t.Fatalf("message = %q, want first user turn", message)
		}
		return "feature/reconnect-spinner", nil
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   worktreePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "Fix reconnect spinner on resume", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-feature-reconnect-spinner" {
		t.Fatalf("stored Branch = %q, want ao-feature-reconnect-spinner", stored.Branch)
	}

	status, err := app.GetGitStatus(t.Context(), workspaceRefForThread(thread))
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if status.Branch != "ao-feature-reconnect-spinner" {
		t.Fatalf("status.Branch = %q, want ao-feature-reconnect-spinner", status.Branch)
	}
}

func TestRespondToApprovalNoActiveSessionError(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	err := app.RespondToApproval(context.Background(), "nonexistent-thread", provider.ApprovalResponse{
		RequestID: "1",
		Decision:  "accept",
	})
	if err == nil {
		t.Fatal("RespondToApproval() error = nil, want no active session error")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Fatalf("RespondToApproval() error = %v, want no active session", err)
	}
}

func TestRespondToApprovalRejectsUntrackedClaudeRequest(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-approval-claude")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	err = app.RespondToApproval(context.Background(), thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("RespondToApproval() error = %v, want stale interactive request", err)
	}
}

func TestRespondToApprovalNoProviderError(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-approval-no-provider")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// Session exists but has no provider set -- both claude and codex are nil.
	app.sessionManager().put(thread.ID, session{
		Provider: "unknown",
		Token:    "test-token",
	})

	err := app.RespondToApproval(context.Background(), thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if err == nil {
		t.Fatal("RespondToApproval() error = nil, want session has no provider error")
	}
	if !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("RespondToApproval() error = %v, want no provider", err)
	}
}

// TestInterruptTurnMissingSessionIsNoOp pins the tolerant contract:
// interrupting a thread with no live session returns nil instead of
// erroring. It closes the user-visible regression where a session that died on
// the readLoop's "disconnected" path produced a "No active session
// for thread X" banner the moment the user clicked Stop — with no
// session to stop, the action is a successful no-op and Reconnect
// is the recovery path. The previous string-matching guard
// (`strings.Contains(err.Error(), "no active session")`) was the
// only surface check; if InterruptTurn grows a new failure mode
// that should surface, route it through a distinct error rather
// than re-introducing this one.
func TestInterruptTurnMissingSessionIsNoOp(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	if err := app.InterruptTurn("nonexistent-thread", nil); err != nil {
		t.Fatalf("InterruptTurn(nonexistent) error = %v, want nil", err)
	}
}

// TestInterruptCreatesStoppedSystemError covers spec behavior: a user
// interrupt flips running/streaming items to errored with a " — stopped"
// suffix and records a new "Stopped by user" system error row. The
// existing markTurnItemsErrored path uses " — interrupted" for fatal
// crash / truncation; these suffixes must NOT collapse into one —
// "stopped" is user-initiated, "interrupted" is everything else.
func TestInterruptCreatesStoppedSystemError(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-interrupt-stopped")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	// Start a turn and seed a running tool_call in it.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTurnStart,
		ThreadID:  thread.ID,
		TurnIndex: 1,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "sleep 60"},
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  thread.ID,
		ItemID:    "tool-stopped",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudeInterruptResponderBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
	})

	// The interrupt-responder binary answers the control_request, so the
	// interrupt takes the clean round-trip instead of falling through to the
	// 10s kill timeout.
	start := time.Now()
	if err := app.InterruptTurn(thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("InterruptTurn took %s — should have completed quickly via the round-trip, not via the kill fallback", elapsed)
	}
	// The clean ack path keeps the session in the registry — only the
	// timeout-fallback kill evicts it — so an interrupt does not force the
	// next send onto the cold-start --resume path.
	if _, stillThere := app.sessionManager().get(thread.ID); !stillThere {
		t.Fatal("session was evicted from registry after a clean interrupt; should only happen on kill fallback")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}

	var toolCall, sysError store.Item
	for _, it := range items {
		if it.Kind == "tool_call" {
			toolCall = it
		}
		if it.Kind == "error" && it.Role == "system" {
			sysError = it
		}
	}
	if toolCall.ID == "" {
		t.Fatal("tool_call row missing")
	}
	if toolCall.Status != "errored" {
		t.Errorf("tool_call status = %q, want errored", toolCall.Status)
	}
	if !strings.HasSuffix(toolCall.Summary, " — stopped") {
		t.Errorf("tool_call summary %q must end with ' — stopped'", toolCall.Summary)
	}
	// The " — interrupted" suffix is for truncation/crash; never for
	// user interrupts.
	if strings.Contains(toolCall.Summary, " — interrupted") {
		t.Errorf("tool_call summary %q must not carry interrupted suffix", toolCall.Summary)
	}
	if sysError.ID == "" {
		t.Fatal("system error row missing — expected 'Stopped by user'")
	}
	if sysError.Summary != "Stopped by user" {
		t.Errorf("system error summary = %q, want 'Stopped by user'", sysError.Summary)
	}
}

// TestInterrupt_LeavesBackgroundTasksRunning pins Phase-4's interrupt
// contract: a user pressing Esc on the turn must NOT touch
// `is_background=true AND status='running'` rows. The background task
// legitimately outlives the interrupted turn; its completion (or
// failure) is signalled by the provider on a separate rail (Claude's
// task_updated, Codex's item/completed on the backgrounded launchID).
// Flipping it here would race with that signal and leave the timeline
// inconsistent.
//
// This guard exists inside triage.flipTurnItemsErrored
// (`if item.IsBackground && item.Kind == itemKindToolCall { continue }`)
// and has unit coverage at the triage level; this app-level test pins
// the full InterruptTurn → MarkInterrupt → store flip chain for
// BOTH providers (Claude and Codex) so a future refactor doesn't break
// the exemption on one path while keeping it on the other.
func TestInterrupt_LeavesBackgroundTasksRunning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		providerName string
	}{
		{"claude", string(provider.Claude)},
		{"codex", string(provider.Codex)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			thread := testThread("thread-interrupt-bg-" + tc.name)
			thread.Provider = tc.providerName
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}

			app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

			// Open a turn.
			if err := app.triage.Handle(provider.ProviderEvent{
				Kind:      provider.EventTurnStart,
				ThreadID:  thread.ID,
				TurnIndex: 1,
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn start: %v", err)
			}

			// Seed a backgrounded tool_call row (status=running,
			// is_background=true) directly — mirrors a row the
			// projector already stamped before the user interrupts.
			bgID := "tool-interrupt-bg"
			now := time.Now().UnixMilli()
			bg := store.Item{
				ID: bgID, ThreadID: thread.ID, TurnIndex: 1, ItemIndex: 0,
				Kind: "tool_call", Role: "assistant", Status: "running",
				IsBackground: true, Summary: "Bash: long-running script",
				ToolName: "Bash", CreatedAt: now, UpdatedAt: now,
			}
			if err := app.store.InsertItem(bg); err != nil {
				t.Fatalf("seed bg row: %v", err)
			}

			// Install a provider session so InterruptTurn routes
			// through the real app-layer path. We use the passthrough
			// Claude binary for both provider branches — InterruptTurn's
			// Codex path would need a real Codex session, but the
			// exemption we're testing sits in triage.flipTurnItemsErrored
			// which is provider-agnostic. For the Codex case we install
			// the claude session with a stubbed provider string so the
			// App-level dispatch runs; the underlying Interrupt()
			// primitive gets a clean control_response ack from the
			// interrupt-responder binary so we don't pay the kill-fallback
			// timeout per case.
			sess, err := claude.NewSession(
				context.Background(),
				thread.ID,
				claude.Config{
					Reaper:    procutiltest.Reaper(),
					Retention: claude.UserHomeRetention(0),
					Binary:    writeClaudeInterruptResponderBinary(t),
					WorkDir:   thread.WorkspacePath,
				},
				func(provider.ProviderEvent) {},
			)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			app.sessionManager().put(thread.ID, session{
				Provider: string(provider.Claude),
				Token:    "interrupt-bg-token",
				Claude:   sess,
			})

			if err := app.InterruptTurn(thread.ID, nil); err != nil {
				t.Fatalf("InterruptTurn: %v", err)
			}

			// The backgrounded row must be untouched: still running,
			// no " — stopped" / " — interrupted" suffix.
			after, ok, err := app.store.GetThreadItem(thread.ID, bgID)
			if err != nil || !ok {
				t.Fatalf("GetItem(bg) found=%v err=%v", ok, err)
			}
			if after.Status != "running" {
				t.Errorf("%s: bg row status = %q, want running (interrupt must exempt backgrounded rows)",
					tc.name, after.Status)
			}
			if after.Summary != "Bash: long-running script" {
				t.Errorf("%s: bg row summary rewritten: %q", tc.name, after.Summary)
			}
		})
	}
}

func TestStopSessionRemovesFromMap(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-stop")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	_, exists := app.sessionManager().get(thread.ID)
	if exists {
		t.Fatalf("sessions[%s] still present after StopSession", thread.ID)
	}
}

func TestStopSessionNoSessionIsNoOp(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	// StopSession on a thread with no session should not error.
	if err := app.StopSession("nonexistent-thread"); err != nil {
		t.Fatalf("StopSession() error = %v, want nil", err)
	}
}

// TestSendMessageSerialPerThread exercises Bug B11: five concurrent
// SendMessage calls on the same thread must execute strictly serially,
// each with a distinct, monotonically-increasing turn_index. Without the
// per-thread mutex two sends could compute the same lastTurnIndex and
// collide on the UNIQUE(turn_index, item_index) constraint, or silently
// attribute the same user message to two different turns.
func TestInternalSendMessageSerialPerThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-serial")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
	})

	const N = 5
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := app.sendMessage(thread.ID, fmt.Sprintf("msg-%d", i), nil); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("SendMessage: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	// The internal new-turn primitive should insert exactly one user item with
	// a unique turnIndex in [0..N-1]. A regression would produce either
	// duplicate turnIndex (UNIQUE violation aborts the second insert),
	// or a count mismatch.
	seenTurns := make(map[int]bool)
	for _, item := range items {
		if item.Role != "user" {
			continue
		}
		if seenTurns[item.TurnIndex] {
			t.Fatalf("duplicate turnIndex %d (Bug B11 regression): %+v", item.TurnIndex, item)
		}
		seenTurns[item.TurnIndex] = true
	}
	if len(seenTurns) != N {
		t.Fatalf("persisted user turns = %d, want %d", len(seenTurns), N)
	}
	for i := 0; i < N; i++ {
		if !seenTurns[i] {
			t.Fatalf("missing turnIndex %d in persisted items", i)
		}
	}
}

// TestSendMessageParallelDifferentThreads confirms the per-thread mutex
// does NOT serialize across unrelated threads: two threads' sends make
// progress concurrently.
func TestSendMessageParallelDifferentThreads(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)

	threadA := testThread("thread-parallel-A")
	threadA.Provider = string(provider.Claude)
	threadA.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(threadA); err != nil {
		t.Fatalf("CreateThread A: %v", err)
	}
	threadB := testThread("thread-parallel-B")
	threadB.Provider = string(provider.Claude)
	threadB.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(threadB); err != nil {
		t.Fatalf("CreateThread B: %v", err)
	}

	sessA, err := claude.NewSession(
		context.Background(),
		threadA.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   threadA.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession A: %v", err)
	}
	t.Cleanup(func() { _ = sessA.Close() })
	sessB, err := claude.NewSession(
		context.Background(),
		threadB.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   threadB.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession B: %v", err)
	}
	t.Cleanup(func() { _ = sessB.Close() })
	app.sessionManager().put(threadA.ID, session{Provider: string(provider.Claude), Token: "a", Claude: sessA})
	app.sessionManager().put(threadB.ID, session{Provider: string(provider.Claude), Token: "b", Claude: sessB})

	if _, err := app.SendMessageWithOptions(context.Background(), threadA.ID, "a", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("send A: %v", err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), threadB.ID, "b", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("send B: %v", err)
	}

	itemsA, _ := app.store.ListItems(threadA.ID)
	itemsB, _ := app.store.ListItems(threadB.ID)
	if len(itemsA) != 1 || len(itemsB) != 1 {
		t.Fatalf("per-thread isolation broken: A=%d B=%d", len(itemsA), len(itemsB))
	}
}

// TestSendMessagePersistsUserItemAndErrorWhenProviderSendFails exercises the
// optimistic-send contract on a failed provider send: the user_text lands
// first so the turn is visible immediately, and a follow-up error row records
// the failure. Both flow through triage.Router.PersistItem (the single
// persistence chokepoint) rather than a raw store.UpsertItem call, which
// would emit nothing and let the error:<turn>:<seq> id collide with a fresh
// provider error on the same turn. The failure also drops the pending-send
// marker via ClearPendingSendForFailure; a leaked marker would let a later
// wire init for a different (or orphaned) send mis-route through the
// handleTurnStart path on stale state.
func TestSendMessagePersistsUserItemAndErrorWhenProviderSendFails(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-fail")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var mu sync.Mutex
	var upsertedIDs []string
	app.triage = triage.NewRouter(app.store, func(name eventchan.Channel, data any) {
		if name != "provider:item_event" {
			return
		}
		if event, ok := data.(triage.ItemStreamEvent); ok && event.Action == "upsert" && event.Item != nil {
			mu.Lock()
			upsertedIDs = append(upsertedIDs, event.Item.ID)
			mu.Unlock()
		}
	})

	// Create a real claude session, then close it so the next Send's
	// WriteLine fails with "process already exited".
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Logf("close returned %v (expected)", err)
	}
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "doomed content", pageSendOptions(uuid.NewString())); err == nil {
		t.Fatal("expected Send to fail with a closed session, got nil")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userFound, errorFound bool
	var userID string
	for _, item := range items {
		if item.Role == "user" && item.Kind == "user_text" {
			userFound = true
			userID = item.ID
		}
		if item.Kind == "error" {
			errorFound = true
		}
	}
	if !userFound || !errorFound {
		t.Fatalf("expected user_text + error after provider send failure, got %+v", items)
	}

	// The send-failure error id uses the router's sequence counter: first
	// error on turn 0 → seq 0 → error:0:0.
	mu.Lock()
	ids := append([]string(nil), upsertedIDs...)
	mu.Unlock()
	sawUser, sendFailureID := false, ""
	for _, id := range ids {
		if id == userID {
			sawUser = true
		}
		if sendFailureID == "" && strings.HasPrefix(id, "error:") {
			sendFailureID = id
		}
	}
	if !sawUser {
		t.Errorf("router did not emit user item upsert (ids=%v)", ids)
	}
	if sendFailureID != "error:0:0" {
		t.Errorf("first send-failure error upsert = %q, want error:0:0 (ids=%v)", sendFailureID, ids)
	}

	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("ClearPendingSendForFailure was not called by the send-failure path — marker leaked into idle state")
	}
}

// TestSendMessagePersistsUserItemOnSuccess pins the successful Claude send:
// the user item lands in the store (a regression would be moving the
// InsertItem call past a success branch), and a pending-send marker is
// registered before sendMessage returns, without which triage.handleInit
// cannot tell a fresh AO send from an idle reconnect when the wire
// system/init arrives. No provider:turn_started is emitted: both providers
// derive turn-start from native wire events (Claude's system/init, Codex's
// turn/started), and a synthetic EventTurnStart would emit it before the
// wire init arrived.
func TestSendMessagePersistsUserItemOnSuccess(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-success")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var mu sync.Mutex
	var emissions []string
	app.triage = triage.NewRouter(app.store, func(name eventchan.Channel, _ any) {
		mu.Lock()
		emissions = append(emissions, name.String())
		mu.Unlock()
	})

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("setup: thread should have no pending sends before SendMessage")
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "hello world", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" {
			userItem = item
		}
	}
	if userItem.ID == "" {
		t.Fatal("user item missing after successful Send")
	}
	if userItem.Summary != "hello world" {
		t.Fatalf("summary = %q, want hello world", userItem.Summary)
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatalf("RegisterPendingSend was not called by the send path — the wire-driven turn-start can't run without the marker")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range emissions {
		if name == "provider:turn_started" {
			t.Fatalf("provider:turn_started emitted on bare send — turn-start must wait for wire system/init (emissions=%v)", emissions)
		}
	}
}

// The first persisted item is what admits a row to the sidebar list and
// flips its derived isDraft. A send nobody made from a screen (an agent's
// spawn, a queued dispatch) has no client applying its own result, so the
// row must be broadcast as listed once that item exists, and only once.
func TestSendMessageBroadcastsListedRowOnFirstItem(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-first-item")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var mu sync.Mutex
	var listed []store.Thread
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		evt, ok := data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", data)
		}
		if evt.Action != triage.ThreadActionListed || evt.Thread == nil {
			return
		}
		mu.Lock()
		listed = append(listed, *evt.Thread)
		mu.Unlock()
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   sess,
	})

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "first", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	mu.Lock()
	got := append([]store.Thread(nil), listed...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("listed frames after first send = %d, want 1", len(got))
	}
	if got[0].ID != thread.ID || got[0].IsDraft {
		t.Fatalf("listed frame = id %q isDraft %v, want the row with isDraft false", got[0].ID, got[0].IsDraft)
	}

	// A thread that already has an item was listed long ago; its send says
	// nothing about membership.
	seeded := testThread("thread-send-later-item")
	seeded.Provider = string(provider.Claude)
	seeded.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(seeded); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if _, err := app.store.UpsertItem(store.Item{
		ID: "prior", ThreadID: seeded.ID, Kind: "user_text", Role: "user", Status: "completed", Summary: "earlier",
	}, nil); err != nil {
		t.Fatalf("UpsertItem() error = %v", err)
	}
	seededSess, err := claude.NewSession(
		context.Background(),
		seeded.ID,
		claude.Config{
			Reaper:    procutiltest.Reaper(),
			Retention: claude.UserHomeRetention(0),
			Binary:    writeClaudePassthroughBinary(t),
			WorkDir:   seeded.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = seededSess.Close() })
	app.sessionManager().put(seeded.ID, session{
		Provider: string(provider.Claude),
		Token:    "test-token",
		Claude:   seededSess,
	})
	if _, err := app.SendMessageWithOptions(context.Background(), seeded.ID, "later", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	mu.Lock()
	n := len(listed)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("listed frames after a send on a thread with items = %d, want the first thread's only", n)
	}
}

func (a *App) sendMessage(threadID string, content string, attachmentIDs []string) error {
	_, err := a.sendMessageWithOptions(
		context.Background(), threadID, content, sendMessageOptions{AttachmentIDs: attachmentIDs},
	)
	return err
}

// ids are the comment ids the claims marked.
func (cs diffReviewClaims) ids() []string {
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ids...)
	}
	return ids
}
