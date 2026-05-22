package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

func TestSendMessageHappyPath(t *testing.T) {
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
	app.sessions[thread.ID] = session{provider: string(provider.Codex)}

	if err := app.SendMessage(thread.ID, "Hello", nil); err != nil {
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
		app.mu.Lock()
		app.sessions[threadID] = session{provider: string(provider.Codex), token: "lazy"}
		app.mu.Unlock()
		return nil
	}

	// Don't use sendMessageFn: it short-circuits before the lazy-start
	// check, so we'd never hit the code under test.
	_ = app.SendMessage(thread.ID, "Hello", nil)
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d, want 1 (lazy-start must fire for session-less thread)", startCalls)
	}

	// A second send on the now-populated session must not re-trigger
	// lazy-start — the session is already live.
	_ = app.SendMessage(thread.ID, "Second", nil)
	if startCalls != 1 {
		t.Fatalf("startSessionFn calls = %d after second send, want 1 (no double-start)", startCalls)
	}
}

func TestSendMessagePersistsUserItemBeforeLazyStartCompletes(t *testing.T) {
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
		app.mu.Lock()
		app.sessions[threadID] = session{provider: string(provider.Codex), token: "lazy"}
		app.mu.Unlock()
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.SendMessage(thread.ID, "visible now", nil)
	}()

	select {
	case <-startEntered:
	case <-time.After(time.Second):
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
	case <-time.After(time.Second):
		t.Fatal("SendMessage did not return after lazy start was released")
	}
}

func TestSendMessagePersistsUserItemAndErrorWhenLazyStartFails(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-start-fail")
	thread.Provider = string(provider.Codex)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.startSessionFn = func(string) error {
		return errors.New("provider boot exploded")
	}

	err := app.SendMessage(thread.ID, "still visible", nil)
	if err == nil || !strings.Contains(err.Error(), "provider boot exploded") {
		t.Fatalf("SendMessage() error = %v, want lazy-start failure", err)
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
		app.mu.Lock()
		app.sessions[threadID] = session{provider: string(provider.Codex), token: "lazy"}
		app.mu.Unlock()
		return nil
	}

	_, err := app.SendMessageWithOptions(thread.ID, "Hello", SendMessageOptions{
		RuntimeMode: string(provider.RuntimeFullAccess),
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

// Regression: implement-time plan→chat flip must be atomic with
// persisting the user message. Splitting the two on the frontend used
// to leave plan-mode threads stuck reading "chat" when send failed.
func TestSendMessageWithOptionsImplementSwitchesPlanModeToChat(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-implement-plan-mode")
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
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "test-token", claude: sess}

	updated, err := app.SendMessageWithOptions(thread.ID, "Implement the plan.", SendMessageOptions{
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
	state, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState: %v", err)
	}
	if !found || state.ImplementedAt == 0 {
		t.Errorf("plan state = %+v, want implemented_at > 0", state)
	}
}

// Pins the click-time mark contract: once SendMessageWithOptions
// persists the user_text row, the plan is implemented even if the
// downstream sendToProvider call fails. The mark is sticky on
// failure — the user already committed to the implementation by
// clicking, and the sibling failed-send error row makes the failure
// visible for a retry. Also confirms the mode flip stays committed.
func TestSendMessageWithOptionsImplementSendFailureKeepsPlanAccepted(t *testing.T) {
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
	app.sessions[thread.ID] = session{provider: string(provider.Codex), token: "no-provider"}

	_, err := app.SendMessageWithOptions(thread.ID, "Implement the plan.", SendMessageOptions{
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
	// The seeded plan row sits at turnIndex 0, so the implement send
	// is turnIndex 1 — the persisted user_text row is `user:1`.
	if state.ImplementedByItemID != "user:1" {
		t.Errorf("ImplementedByItemID = %q, want %q (the persisted user_text row id)", state.ImplementedByItemID, "user:1")
	}
}

// Pins the re-click contract: a second implement-the-plan send against
// the same plan must NOT overwrite the original attribution. The user
// might click "implement" twice (network retry, double-click); the
// mark is gated on `WHERE implemented_at = 0` so the first click owns
// the badge forever.
func TestSendMessageWithOptionsImplementOnAlreadyImplementedPlanKeepsFirstAttribution(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-implement-replay")
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
		Meta:      `{"title":"Plan","preview":"x","lineCount":1,"charCount":1}`,
		Data:      []byte("# Plan"),
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
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "test-token", claude: sess}

	if _, err := app.SendMessageWithOptions(thread.ID, "Implement the plan.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	}); err != nil {
		t.Fatalf("first SendMessageWithOptions() error = %v", err)
	}

	first, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil || !found {
		t.Fatalf("first GetProposedPlanState err=%v found=%v", err, found)
	}
	if first.ImplementedAt == 0 || first.ImplementedByItemID == "" {
		t.Fatalf("first send did not mark plan: %+v", first)
	}

	if _, err := app.SendMessageWithOptions(thread.ID, "Implement again.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	}); err != nil {
		t.Fatalf("second SendMessageWithOptions() error = %v", err)
	}

	second, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil || !found {
		t.Fatalf("second GetProposedPlanState err=%v found=%v", err, found)
	}
	if second.ImplementedAt != first.ImplementedAt {
		t.Errorf("ImplementedAt = %d, want %d (must not be overwritten by re-click)", second.ImplementedAt, first.ImplementedAt)
	}
	if second.ImplementedByItemID != first.ImplementedByItemID {
		t.Errorf("ImplementedByItemID = %q, want %q (re-click must not steal attribution)", second.ImplementedByItemID, first.ImplementedByItemID)
	}
}

func TestSendMessageWithOptionsPersistsSourceProposedPlan(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-source-plan")
	thread.Provider = string(provider.Claude)
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
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{provider: string(provider.Claude), token: "test-token", claude: sess}

	_, err = app.SendMessageWithOptions(thread.ID, "Implement the plan.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	})
	if err != nil {
		t.Fatalf("SendMessageWithOptions() error = %v", err)
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

	_, err = app.SendMessageWithOptions(thread.ID, "Implement the plan again.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ItemID: "plan-item"},
	})
	if err != nil {
		t.Fatalf("second SendMessageWithOptions() error = %v", err)
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

	state, found, err := app.store.GetProposedPlanState(thread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState after second send: %v", err)
	}
	if !found {
		t.Fatal("plan state missing after second send")
	}
	if state.ImplementedByItemID != userItem.ID {
		t.Fatalf("ImplementedByItemID after second send = %q, want first implementation item %q", state.ImplementedByItemID, userItem.ID)
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
		claude.Config{Binary: writeClaudePassthroughBinary(t), WorkDir: targetThread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[targetThread.ID] = session{provider: string(provider.Claude), token: "test-token", claude: sess}

	_, err = app.SendMessageWithOptions(targetThread.ID, "Implement the plan.", SendMessageOptions{
		SourceProposedPlan: &SourceProposedPlan{ThreadID: sourceThread.ID, ItemID: "plan-item"},
	})
	if err != nil {
		t.Fatalf("SendMessageWithOptions() error = %v", err)
	}

	state, found, err := app.store.GetProposedPlanState(sourceThread.ID, "plan-item")
	if err != nil {
		t.Fatalf("GetProposedPlanState() error = %v", err)
	}
	if !found || state.ImplementedByThreadID != targetThread.ID || state.ImplementedByItemID != "user:0" {
		t.Fatalf("state = %+v, want implemented by target user turn", state)
	}
}

func TestSendMessageReturnsLazyStartError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-lazy-start-fail")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	app.startSessionFn = func(threadID string) error {
		return fmt.Errorf("synthetic start failure")
	}

	err := app.SendMessage(thread.ID, "Hello", nil)
	if err == nil {
		t.Fatal("SendMessage() error = nil, want lazy-start error")
	}
	if !strings.Contains(err.Error(), "start session") || !strings.Contains(err.Error(), "synthetic start failure") {
		t.Fatalf("SendMessage() error = %v, want wrapped start-session failure", err)
	}
}

func TestSendMessageIncrementsTurnIndex(t *testing.T) {
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Next message", nil); err != nil {
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

func TestSendMessageCapturesCheckpointBeforeEachUserMessage(t *testing.T) {
	app := newTestAppWithStore(t)
	app.checkpoints = checkpoint.NewStore()
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	workspace := initCheckpointRepo(t)
	thread := testThread("thread-send-first-checkpoint")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "first turn", nil); err != nil {
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

	if _, ok, err := app.store.GetCheckpointByUserItemID(thread.ID, userItem.ID); err != nil || !ok {
		t.Fatalf("checkpoint for first user item missing after first send: ok=%v err=%v", ok, err)
	}

	writeFile(t, workspace, "agent-output.txt", "created during first turn\n")
	if err := app.SendMessage(thread.ID, "second turn", nil); err != nil {
		t.Fatalf("SendMessage(second) error = %v", err)
	}
	if _, ok, err := app.store.GetCheckpointByUserItemID(thread.ID, "user:1"); err != nil || !ok {
		t.Fatalf("checkpoint for second user item missing after second send: ok=%v err=%v", ok, err)
	}
}

func TestSendMessageGeneratesClaudeThreadTitleOnFirstTurn(t *testing.T) {
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

	emitted := make(chan store.Thread, 4)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		updated, ok := data.(store.Thread)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want store.Thread", data)
		}
		emitted <- updated
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume", nil); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	foundTitle := false
	for !foundTitle {
		select {
		case updated := <-emitted:
			if updated.Title == "Reconnect spinner resume fix" {
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

	renamedByGenerator := make(chan store.Thread, 4)
	app.emitEventFn = func(name string, data any) {
		if name != "thread:updated" {
			return
		}
		updated, ok := data.(store.Thread)
		if !ok {
			t.Fatalf("thread:updated payload type = %T, want store.Thread", data)
		}
		// Only record events where the title matches the generated value —
		// the user-driven rename in this test sets a different title and
		// we only care about catching a stale generator write here.
		if updated.Title == "Generated title" {
			renamedByGenerator <- updated
		}
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume", nil); err != nil {
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
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)

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

	worktreePath, err := app.GitCreateWorktree(thread.ID, "")
	if err != nil {
		t.Fatalf("GitCreateWorktree() error = %v", err)
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: worktreePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "Fix reconnect spinner on resume", nil); err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	stored, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if stored.Branch != "ao-feature-reconnect-spinner" {
		t.Fatalf("stored Branch = %q, want ao-feature-reconnect-spinner", stored.Branch)
	}

	status, err := app.GetGitStatus(thread.ID)
	if err != nil {
		t.Fatalf("GetGitStatus() error = %v", err)
	}
	if status.Branch != "ao-feature-reconnect-spinner" {
		t.Fatalf("status.Branch = %q, want ao-feature-reconnect-spinner", status.Branch)
	}
}

func TestRespondToApprovalNoActiveSessionError(t *testing.T) {
	app := newTestAppWithStore(t)

	err := app.RespondToApproval("nonexistent-thread", provider.ApprovalResponse{
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	err = app.RespondToApproval(thread.ID, provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if !errors.Is(err, provider.ErrStaleInteractiveRequest) {
		t.Fatalf("RespondToApproval() error = %v, want stale interactive request", err)
	}
}

func TestRespondToApprovalNoProviderError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-approval-no-provider")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Session exists but has no provider set -- both claude and codex are nil.
	app.sessions[thread.ID] = session{
		provider: "unknown",
		token:    "test-token",
	}

	err := app.RespondToApproval(thread.ID, provider.ApprovalResponse{
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
// erroring. This matches runPlainInterruptLocked's behavior and
// closes the user-visible regression where a session that died on
// the readLoop's "disconnected" path produced a "No active session
// for thread X" banner the moment the user clicked Stop — with no
// session to stop, the action is a successful no-op and Reconnect
// is the recovery path. The previous string-matching guard
// (`strings.Contains(err.Error(), "no active session")`) was the
// only surface check; if InterruptTurn grows a new failure mode
// that should surface, route it through a distinct error rather
// than re-introducing this one.
func TestInterruptTurnMissingSessionIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)

	if err := app.InterruptTurn("nonexistent-thread"); err != nil {
		t.Fatalf("InterruptTurn(nonexistent) error = %v, want nil", err)
	}
}

func TestInterruptTurnHappyPathClaude(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-interrupt-claude")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Use the interrupt-responder binary so Interrupt takes the clean
	// control_request → control_response round-trip instead of falling
	// through to the 10s kill timeout.
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudeInterruptResponderBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	start := time.Now()
	err = app.InterruptTurn(thread.ID)
	if err != nil {
		t.Fatalf("InterruptTurn() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("InterruptTurn took %s — should have completed quickly via the round-trip, not via the kill fallback", elapsed)
	}
	// Clean ack path keeps the session in the registry — only the
	// timeout-fallback kill evicts it. Pin that behaviour so a future
	// regression in the round-trip path doesn't silently force every
	// interrupt onto the cold-start --resume path.
	app.mu.Lock()
	_, stillThere := app.sessions[thread.ID]
	app.mu.Unlock()
	if !stillThere {
		t.Fatal("session was evicted from registry after a clean interrupt; should only happen on kill fallback")
	}
}

// TestInterruptCreatesStoppedSystemError covers spec behavior: a user
// interrupt flips running/streaming items to errored with a " — stopped"
// suffix and records a new "Stopped by user" system error row. The
// existing markTurnItemsErrored path uses " — interrupted" for fatal
// crash / truncation; these suffixes must NOT collapse into one —
// "stopped" is user-initiated, "interrupted" is everything else.
func TestInterruptCreatesStoppedSystemError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-interrupt-stopped")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	app.triage = triage.NewRouter(app.store, func(string, any) {})

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
			Binary:  writeClaudeInterruptResponderBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	if err := app.InterruptTurn(thread.ID); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
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
// the full InterruptTurn → MarkUserInterrupt → store flip chain for
// BOTH providers (Claude and Codex) so a future refactor doesn't break
// the exemption on one path while keeping it on the other.
func TestInterrupt_LeavesBackgroundTasksRunning(t *testing.T) {
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

			app.triage = triage.NewRouter(app.store, func(string, any) {})

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
					Binary:  writeClaudeInterruptResponderBinary(t),
					WorkDir: thread.WorkspacePath,
				},
				func(provider.ProviderEvent) {},
			)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			app.sessions[thread.ID] = session{
				provider: string(provider.Claude),
				token:    "interrupt-bg-token",
				claude:   sess,
			}

			if err := app.InterruptTurn(thread.ID); err != nil {
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	app.mu.Lock()
	_, exists := app.sessions[thread.ID]
	app.mu.Unlock()
	if exists {
		t.Fatalf("sessions[%s] still present after StopSession", thread.ID)
	}
}

func TestStopSessionNoSessionIsNoOp(t *testing.T) {
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
func TestSendMessageSerialPerThread(t *testing.T) {
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	const N = 5
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := app.SendMessage(thread.ID, fmt.Sprintf("msg-%d", i), nil); err != nil {
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
	// Each SendMessage should have inserted exactly one user item with
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: threadA.WorkspacePath,
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
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: threadB.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession B: %v", err)
	}
	t.Cleanup(func() { _ = sessB.Close() })

	app.sessions[threadA.ID] = session{provider: string(provider.Claude), token: "a", claude: sessA}
	app.sessions[threadB.ID] = session{provider: string(provider.Claude), token: "b", claude: sessB}

	if err := app.SendMessage(threadA.ID, "a", nil); err != nil {
		t.Fatalf("send A: %v", err)
	}
	if err := app.SendMessage(threadB.ID, "b", nil); err != nil {
		t.Fatalf("send B: %v", err)
	}

	itemsA, _ := app.store.ListItems(threadA.ID)
	itemsB, _ := app.store.ListItems(threadB.ID)
	if len(itemsA) != 1 || len(itemsB) != 1 {
		t.Fatalf("per-thread isolation broken: A=%d B=%d", len(itemsA), len(itemsB))
	}
}

// TestSendMessagePersistsUserItemAndErrorWhenProviderSendFails exercises the
// current optimistic-send contract: the user_text lands first so the turn is
// visible immediately, and a follow-up error row records the failed provider
// send.
func TestSendMessagePersistsUserItemAndErrorWhenProviderSendFails(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-fail")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Create a real claude session, then close it so the next Send's
	// WriteLine fails with "process already exited".
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Logf("close returned %v (expected)", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	err = app.SendMessage(thread.ID, "doomed content", nil)
	if err == nil {
		t.Fatal("expected Send to fail with a closed session, got nil")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userFound, errorFound bool
	for _, item := range items {
		if item.Role == "user" && item.Kind == "user_text" {
			userFound = true
		}
		if item.Kind == "error" {
			errorFound = true
		}
	}
	if !userFound || !errorFound {
		t.Fatalf("expected user_text + error after provider send failure, got %+v", items)
	}
}

// TestSendMessageGoesThroughRouter asserts that both the optimistic
// user_text item and the send-failure error row flow through
// triage.Router.PersistItem (the single persistence chokepoint) rather
// than bypassing it via a raw store.UpsertItem call. We exercise the
// failure path because it hits both persistence sites in the same run;
// success alone would miss the send-failure branch. Regression mode:
// if a future refactor re-introduces the direct store.UpsertItem call,
// the items wouldn't emit through the registered emit func, and the
// error:<turn>:<seq> id would collide with a fresh provider error on
// the same turn.
func TestSendMessageGoesThroughRouter(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-router")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Record every emit so we can assert the items flowed through the
	// router (which emits on persist) and not via a direct
	// store.UpsertItem (which wouldn't call emit at all).
	var mu sync.Mutex
	var emissions []string
	var upsertedIDs []string
	app.triage = triage.NewRouter(app.store, func(name string, data any) {
		mu.Lock()
		emissions = append(emissions, name)
		if name == "provider:item_event" {
			if event, ok := data.(triage.ItemStreamEvent); ok && event.Action == "upsert" && event.Item != nil {
				upsertedIDs = append(upsertedIDs, event.Item.ID)
			}
		}
		mu.Unlock()
	})

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// Close the session so Send fails and we exercise both paths.
	if err := sess.Close(); err != nil {
		t.Logf("close: %v (expected)", err)
	}
	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	// Expected to fail because the session is closed.
	if err := app.SendMessage(thread.ID, "will-fail", nil); err == nil {
		t.Fatal("expected send failure on closed session")
	}

	mu.Lock()
	defer mu.Unlock()

	// Both the user item and the error item must have been emitted as
	// provider:item_event — that only happens when the router's
	// persistItem runs, not when we call store.UpsertItem directly.
	wantUserID := "user:0"
	var sawUser, sawError bool
	for _, id := range upsertedIDs {
		if id == wantUserID {
			sawUser = true
		}
		if strings.HasPrefix(id, "error:0:") {
			sawError = true
		}
	}
	if !sawUser {
		t.Errorf("router did not emit user item upsert (ids=%v)", upsertedIDs)
	}
	if !sawError {
		t.Errorf("router did not emit send-failure error upsert (ids=%v)", upsertedIDs)
	}

	// The send-failure error id MUST use the router's sequence counter —
	// not a hardcoded :0 suffix. First error on turn 0 → seq 0 → error:0:0.
	// If a future refactor reverts to a hardcoded :0 this still matches,
	// so the distinguishing signal is the prefix format (error:<turn>:<seq>)
	// rather than a numeric collision check alone.
	sendFailureID := ""
	for _, id := range upsertedIDs {
		if strings.HasPrefix(id, "error:") {
			sendFailureID = id
			break
		}
	}
	if sendFailureID != "error:0:0" {
		t.Errorf("first send-failure error id = %q, want error:0:0", sendFailureID)
	}
}

// TestSendMessagePersistsUserItemOnSuccess confirms the happy path is
// preserved: a successful Send still results in the user item landing
// in the store. Regression would be moving the InsertItem call past a
// success branch by mistake.
func TestSendMessagePersistsUserItemOnSuccess(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-send-success")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "test-token",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "hello world", nil); err != nil {
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
}

// TestSendMessageDoesNotEmitSyntheticTurnStart pins the Phase F deletion
// of the synthetic EventTurnStart. Pre-Phase-F, a successful Claude
// send drove turn-start by manually calling triage.Handle with
// EventTurnStart; post-Phase-F, both providers derive turn-start from
// native wire events (Claude's system/init, Codex's turn/started) so
// the synthetic emission is gone. A regression that re-introduces the
// synthetic emission would emit provider:turn_started before the wire
// init arrived, breaking the wire-driven contract this phase
// established.
func TestSendMessageDoesNotEmitSyntheticTurnStart(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-synthetic-turn-start")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var mu sync.Mutex
	var emissions []string
	app.triage = triage.NewRouter(app.store, func(name string, _ any) {
		mu.Lock()
		emissions = append(emissions, name)
		mu.Unlock()
	})

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range emissions {
		if name == "provider:turn_started" {
			t.Fatalf("provider:turn_started emitted on bare send — Phase F removed the synthetic EventTurnStart, turn-start must wait for wire system/init (emissions=%v)", emissions)
		}
	}
}

// TestSendMessageRegistersPendingSend pins the post-Phase-F send
// contract: a successful Claude send must register a pending-send
// marker BEFORE returning from sendMessage. Without the marker,
// triage.handleInit can't tell a fresh AO send from an idle reconnect
// when the wire system/init arrives, and turn-start would never fire
// for Claude.
func TestSendMessageRegistersPendingSend(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-pending-send-registered")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("setup: thread should have no pending sends before SendMessage")
	}
	if err := app.SendMessage(thread.ID, "hello", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatalf("RegisterPendingSend was not called by the send path — Phase F's wire-driven turn-start can't run without the marker")
	}
}

// TestSendMessageClearsPendingSendOnFailure pins the cleanup contract:
// when sendToProvider fails, the send path must drop the pending-send
// marker via ClearPendingSendForFailure. Without this, the marker
// would persist; a later wire init for a different (or orphaned) send
// could mis-route through the handleTurnStart path on stale state.
func TestSendMessageClearsPendingSendOnFailure(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-pending-send-cleared-on-failure")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	app.triage = triage.NewRouter(app.store, func(string, any) {})

	// Closed Claude session so sendToProvider's WriteLine returns "process
	// already exited". Mirrors TestSendMessagePersistsUserItemAndError-
	// WhenProviderSendFails.
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{
			Binary:  writeClaudePassthroughBinary(t),
			WorkDir: thread.WorkspacePath,
		},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Logf("close: %v (expected)", err)
	}

	app.sessions[thread.ID] = session{
		provider: string(provider.Claude),
		token:    "tok",
		claude:   sess,
	}

	if err := app.SendMessage(thread.ID, "doomed", nil); err == nil {
		t.Fatal("expected SendMessage to fail with closed session")
	}

	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Errorf("ClearPendingSendForFailure was not called by the send-failure path — marker leaked into idle state")
	}

	// Existing optimistic-send contract must still hold: the error item
	// is persisted alongside the orphan user_text. Pinning this here so
	// the cleanup change can't accidentally short-circuit the existing
	// failure persistence.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var sawError bool
	for _, item := range items {
		if item.Kind == "error" {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Error("expected error item after failed send — existing optimistic-send contract regressed")
	}
}
