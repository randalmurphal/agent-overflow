package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The edit-and-resend saga is a four-step sequence with three durable
// seams between them:
//
//	stage crash copy → rollback (provider cut + truncate) → resend → settle draft
//
// Every step boundary is a crash window with a DIFFERENT correct durable
// state, and the whole point of the crash copy is that the user never
// loses text at any of them. The tests below inject a failure at each
// seam and assert exactly what survives — the sibling
// app_revert_and_resend_test.go covers the happy path and the
// resend-failed seam, so these start one step earlier and end one step
// later.

// TestRevertAndResendStagingFailureLeavesEverythingUntouched is seam 1:
// the crash copy itself fails to stage. Nothing destructive has run yet,
// so the correct durable state is "as if the call never happened" —
// timeline intact, session unchanged, and critically the draft row NOT
// half-written, because a partially merged draft would show the user
// text they never sent while the message they clicked edit on is still
// in the conversation.
//
// A composer draft whose attachment JSON is corrupt is the real
// mechanism: composerdraft.MergeParts decodes the current row's ids
// before it can merge anything, so the failure lands inside the staging
// call with the row untouched.
func TestRevertAndResendStagingFailureLeavesEverythingUntouched(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, workspace := seedResendThread(t, app, "t-resend-stage-fail")

	corrupt := store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "half-typed follow-up",
		Attachments:   `{"not":"an array"`,
		TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if _, err := app.store.UpsertThreadDraft(corrupt); err != nil {
		t.Fatalf("seed corrupt draft: %v", err)
	}

	err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"})
	if err == nil {
		t.Fatal("revert and resend succeeded with an undecodable draft row, want the staging step to fail")
	}
	if !strings.Contains(err.Error(), "revert and resend: stage edited message") {
		t.Fatalf("error = %q, want the staging-step prefix", err)
	}

	// Nothing destructive ran.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("items after a failed staging = %+v, want all 4 preserved", items)
	}
	if ref := mustGetThread(t, app, thread.ID).SessionRef; ref != resendSourceSessionID {
		t.Fatalf("session ref = %q, want the untouched source session", ref)
	}
	assertClaudeSessionText(t, workspace, resendSourceSessionID, []string{"first", "second"}, nil)

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft != corrupt {
		t.Fatalf("draft after a failed staging = %+v, want the row untouched (%+v)", draft, corrupt)
	}
	for _, e := range bus.allEvents() {
		if e.Name == "user_message:reverted" {
			t.Fatal("user_message:reverted fired despite the staging failure")
		}
	}
}

// TestRevertAndResendRollbackFailureAfterStagingKeepsCrashCopy is seam
// 2: the crash copy is staged and the ROLLBACK then fails. This is the
// window the crash copy exists for in its purest form — the user's
// message is still in the conversation AND their edit is parked in the
// draft row, so a process death here loses nothing and a live frontend
// can recover from either copy.
//
// A missing session file is the mechanism: rollbackClaudeThreadToMessage
// fails at LocateSessionFile, before it writes a slice or touches the
// thread row, so the conversation stays whole while the staged draft
// stands.
func TestRevertAndResendRollbackFailureAfterStagingKeepsCrashCopy(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, workspace := seedResendThread(t, app, "t-resend-rollback-fail")

	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "half-typed follow-up",
		TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	sessionPath, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), resendSourceSessionID, workspace)
	if err != nil {
		t.Fatalf("resolve claude session path: %v", err)
	}
	if err := os.Remove(sessionPath); err != nil {
		t.Fatalf("remove claude session file: %v", err)
	}

	const edited = "rewritten prompt"
	err = app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: edited})
	if err == nil {
		t.Fatal("revert and resend succeeded with no session file, want the provider rollback to fail")
	}
	if !strings.Contains(err.Error(), "revert and resend") || strings.Contains(err.Error(), "resend failed") {
		t.Fatalf("error = %q, want a rollback-stage failure, NOT the post-commit resend-failed prefix", err)
	}

	// The conversation is untouched: the revert never committed.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("items after a failed rollback = %+v, want all 4 preserved", items)
	}
	if ref := mustGetThread(t, app, thread.ID).SessionRef; ref != resendSourceSessionID {
		t.Fatalf("session ref = %q, want the untouched source session", ref)
	}
	for _, e := range bus.allEvents() {
		if e.Name == "user_message:reverted" {
			t.Fatal("user_message:reverted fired despite the rollback failure")
		}
	}

	// ...and the crash copy stands: edited text first, untouched WIP
	// second, the WIP's chips carried through.
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != edited+"\n\nhalf-typed follow-up" {
		t.Fatalf("draft content = %q, want the edited text merged ahead of the WIP", draft.Content)
	}
	if draft.TerminalChips != `[{"id":"chip-1","label":"npm test"}]` {
		t.Fatalf("draft terminal chips = %q, want the WIP's chips carried through the merge", draft.TerminalChips)
	}
}

// TestRevertAndResendConvergesOnRetryAfterCommittedProviderCut is the
// crash window with no clean rollback: the provider cut COMMITTED
// (session sliced, SessionRef repointed, provider ids remapped) and the
// SQLite truncation did not. The durable state is a timeline that still
// holds rows the provider transcript no longer has.
//
// The fixture reproduces that state exactly by committing the provider
// half on its own — the same call rollbackConversationLocked makes,
// stopped one step short — and then runs the FULL saga on top, which is
// what the user's retry (or the frontend's) does. Convergence is the
// contract: writeClaudeSessionSlice must recognize an already-cut
// transcript through the anchor's remapped parent uuid and re-slice
// through it, rather than resurrecting the cut-away prompt with a full
// clone or failing forever.
//
// A mid-turn anchor is deliberate: it is the case that takes the
// parent-uuid detector (a turn-initial anchor falls back to the ordinal
// walk, which converges for a different reason).
func TestRevertAndResendConvergesOnRetryAfterCommittedProviderCut(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, workspace := seedResendMidTurnThread(t, app, "t-resend-converge")

	item, found, err := app.store.GetThreadItem(thread.ID, "user:steer")
	if err != nil || !found {
		t.Fatalf("load anchor item: found=%v err=%v", found, err)
	}
	anchor, _, err := app.store.GetMessageAnchor(thread.ID, "user:steer")
	if err != nil {
		t.Fatalf("load message anchor: %v", err)
	}

	// Commit the provider half only — a truncation failure's durable
	// leftovers, byte for byte.
	if err := app.rollbackClaudeThreadToMessage(thread, anchor, item); err != nil {
		t.Fatalf("seed the committed provider cut: %v", err)
	}
	cutRef := mustGetThread(t, app, thread.ID).SessionRef
	if cutRef == resendSourceSessionID {
		t.Fatal("precondition: the provider cut did not repoint SessionRef")
	}
	assertClaudeSessionText(t, workspace, cutRef, []string{"first"}, []string{"steer"})
	if items, err := app.store.ListItems(thread.ID); err != nil || len(items) != 4 {
		t.Fatalf("precondition: items = %+v (%v), want the un-truncated timeline", items, err)
	}

	// The retry.
	const edited = "rewritten steer"
	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:steer", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("retried revert and resend: %v", err)
	}

	// Timeline and transcript agree afterwards: the kept prefix on both
	// sides, the rolled-back prompt on neither, the replacement only in
	// the timeline (the mock provider never echoes it back).
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 || items[0].ID != "user:0" || items[1].ID != "asst:0" {
		t.Fatalf("items after the retry = %+v, want the kept prefix plus the replacement", items)
	}
	if items[2].Summary != edited {
		t.Fatalf("replacement row = %+v, want %q", items[2], edited)
	}
	convergedRef := mustGetThread(t, app, thread.ID).SessionRef
	if convergedRef == cutRef {
		t.Fatal("retry reused the already-cut session file instead of re-slicing through the anchor parent")
	}
	assertClaudeSessionText(t, workspace, convergedRef, []string{"first"}, []string{"steer"})
	if _, ev := findRevertedEvent(t, bus); !ev.DraftPendingResend {
		t.Fatal("retried reverted event draftPendingResend = false, want true")
	}
}

// TestRevertAndResendRestoresChipsAndPlanLinkByteIdentical pins the
// merge→restore round-trip on the RICHEST work-in-progress the composer
// can hold: text, attachment ids, terminal chips, and a
// sourceProposedPlan link to a plan in another thread. Those last two
// are carried by composerdraft.MergeParts rather than rebuilt, so a
// regression there is silent — the send succeeds and the user simply
// loses the plan linkage that marks the original plan Accepted.
func TestRevertAndResendRestoresChipsAndPlanLinkByteIdentical(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-plan-link")

	wip := store.ThreadDraft{
		ThreadID:                  thread.ID,
		Content:                   "half-typed follow-up",
		Attachments:               `["att-wip"]`,
		TerminalChips:             `[{"id":"chip-1","label":"npm test","preview":"ok","content":"PASS","createdAt":7}]`,
		PendingPlanImplementation: `{"threadId":"plan-thread","itemId":"plan-item"}`,
		UpdatedAt:                 time.Now().UnixMilli(),
	}
	if _, err := app.store.UpsertThreadDraft(wip); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft != wip {
		t.Fatalf("draft after resend = %+v, want the pre-saga WIP restored byte-identical (%+v)", draft, wip)
	}
}

// TestRevertAndResendStagedCrashCopyKeepsChipsAndPlanLink is the same
// payload observed MID-saga, which is the state a process crash would
// actually leave behind. The staged row must carry the edit ahead of the
// WIP text AND keep the chips and plan link, because that row is the
// only copy of the composer's context until the saga settles it.
func TestRevertAndResendStagedCrashCopyKeepsChipsAndPlanLink(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-stage-rich")

	const chips = `[{"id":"chip-1","label":"npm test","preview":"ok","content":"PASS","createdAt":7}]`
	const plan = `{"threadId":"plan-thread","itemId":"plan-item"}`
	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:                  thread.ID,
		Content:                   "half-typed follow-up",
		Attachments:               `["att-wip"]`,
		TerminalChips:             chips,
		PendingPlanImplementation: plan,
		UpdatedAt:                 time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(
		context.Background(),
		thread.ID, "user:1",
		RevertAndResendOptions{Content: edited, AttachmentIDs: []string{"att-edited"}},
	); err == nil {
		t.Fatal("revert and resend succeeded with an unresolvable attachment, want the resend to fail")
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != edited+"\n\nhalf-typed follow-up" {
		t.Fatalf("staged content = %q, want the edit merged ahead of the WIP", draft.Content)
	}
	if got := decodeDraftAttachmentIDs(t, draft.Attachments); len(got) != 2 || got[0] != "att-edited" || got[1] != "att-wip" {
		t.Fatalf("staged attachments = %v, want the edited ids first then the WIP's", got)
	}
	if draft.TerminalChips != chips {
		t.Fatalf("staged terminal chips = %q, want the WIP's chips verbatim", draft.TerminalChips)
	}
	if draft.PendingPlanImplementation != plan {
		t.Fatalf("staged plan link = %q, want the WIP's link verbatim", draft.PendingPlanImplementation)
	}
}

// TestRevertAndResendSerializesConcurrentSendAfterReplacement is the
// lock contract the whole saga rests on. A send issued while the saga
// runs must land STRICTLY AFTER the replacement message — a send that
// slipped into the window between the truncation and the resend would
// be answered with the pre-edit context and then be re-ordered behind a
// message the model had not seen.
//
// Determinism comes from the thread lock's own reference counter: the
// concurrent caller increments it before blocking on the mutex, so
// observing refs==2 while the saga holds the lock proves the send is
// parked in Lock() and cannot proceed until the saga releases. The
// window is opened from the `user_message:reverted` emission, which
// fires on the saga's goroutine after the truncation commits — the most
// dangerous point in the sequence.
func TestRevertAndResendSerializesConcurrentSendAfterReplacement(t *testing.T) {
	app, bus := newResendTestApp(t)
	app.configureTriageQueueCallbacks()
	t.Cleanup(func() { app.flushDispatch.wg.Wait() })
	thread, _ := seedResendThread(t, app, "t-resend-lock")

	sendErr := make(chan error, 1)
	var once sync.Once
	app.testEmitHook = func(name string, data any) {
		bus.emit(name, data)
		if name != "user_message:reverted" {
			return
		}
		once.Do(func() {
			go func() {
				_, err := app.SendMessageWithOptions(t.Context(), thread.ID, "concurrent follow-up", SendMessageOptions{SendID: "concurrent-follow-up", ReconcileBySendID: true})
				sendErr <- err
			}()
			// Blocks until the racing send is provably parked on this
			// thread's action lock.
			waitForThreadLockRefs(t, app.threadLocks(), thread.ID, 2)
		})
	}

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}
	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("concurrent send: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent send never completed after the saga released the thread lock")
	}

	// This fixture's provider is deliberately silent. The replacement is
	// still awaiting turn/start, so the concurrent public send must use the
	// queue rather than invent another direct turn. Its row appears only
	// when the provider echoes it, after consuming the replacement.
	for _, content := range []string{edited, "concurrent follow-up"} {
		if content == "concurrent follow-up" {
			flushed := bus.nextOfKind(t, "provider:queue_flushed", 10*time.Second)
			flushEvent, ok := flushed.Data.(QueueFlushedEvent)
			if !ok || len(flushEvent.Items) != 1 || flushEvent.Items[0].Message != content {
				t.Fatalf("concurrent queued send = %#v", flushed.Data)
			}
		}
		head, ok := app.triage.PeekPendingSendHeadForTest(thread.ID)
		if !ok {
			t.Fatalf("no pending send for %q", content)
		}
		meta, err := json.Marshal(map[string]string{"provider_item_id": head.ExpectedProviderItemID})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range []provider.ProviderEvent{
			{Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnIndex: head.TurnIndex},
			{Kind: provider.EventUserText, ThreadID: thread.ID, TurnIndex: head.TurnIndex, ItemID: head.AOItemID, Content: content, Meta: meta},
			{Kind: provider.EventTurnComplete, ThreadID: thread.ID, TurnIndex: head.TurnIndex, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}},
		} {
			event.Timestamp = time.Now()
			if err := app.triage.Handle(event); err != nil {
				t.Fatalf("provider event %s for %q: %v", event.Kind, content, err)
			}
		}
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var userTexts []string
	for _, item := range items {
		if item.Kind == "user_text" {
			userTexts = append(userTexts, item.Summary)
		}
	}
	want := []string{"first", edited, "concurrent follow-up"}
	if len(userTexts) != len(want) {
		t.Fatalf("user rows = %v, want %v", userTexts, want)
	}
	for i := range want {
		if userTexts[i] != want[i] {
			t.Fatalf("user rows = %v, want %v (the racing send must land after the replacement)", userTexts, want)
		}
	}
}

// TestRevertAndResendSettlesDraftAgainstMidSagaComposerSaves is seam 4:
// the settle. The composer stays typeable for the whole saga — only
// sending is suspended, and SaveDraft takes no thread lock — so a
// debounced save can land between the staged crash copy and the settle.
// Blindly restoring the pre-saga snapshot over it would silently destroy
// text the user typed, which is the exact loss the crash copy exists to
// prevent.
//
// All four sequences run, because the settle is a decision ABOUT a
// transition (did the row move?) and state coverage would miss it: with
// and without a mid-saga save, crossed with a composer that did and did
// not already hold work-in-progress.
//
// The save is driven from the send seam because that is the one instant
// inside the saga a test can reach deterministically — the lock is held,
// the crash copy is staged, the settle has not run.
func TestRevertAndResendSettlesDraftAgainstMidSagaComposerSaves(t *testing.T) {
	for _, tc := range []struct {
		name        string
		priorWIP    bool
		midSagaSave bool
	}{
		{name: "no save, WIP restored", priorWIP: true},
		{name: "no save, empty composer cleared"},
		{name: "save over WIP wins", priorWIP: true, midSagaSave: true},
		{name: "save into an empty composer wins", midSagaSave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := newResendTestApp(t)
			thread, _ := seedResendThread(t, app, "t-resend-settle")

			wip := store.ThreadDraft{
				ThreadID:      thread.ID,
				Content:       "half-typed follow-up",
				Attachments:   `["att-wip"]`,
				TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
				UpdatedAt:     time.Now().UnixMilli(),
			}
			if tc.priorWIP {
				if _, err := app.store.UpsertThreadDraft(wip); err != nil {
					t.Fatalf("seed WIP draft: %v", err)
				}
			}

			// The saved row is read back inside the saga so the assertion
			// compares the FULL row the composer wrote, not just its text.
			var midSagaRow store.ThreadDraft
			app.sendMessageFn = func(string, string, []string) error {
				if !tc.midSagaSave {
					return nil
				}
				if err := app.SaveDraft(t.Context(), thread.ID, "typed while the saga ran", nil, nil, nil); err != nil {
					t.Errorf("mid-saga composer save: %v", err)
					return nil
				}
				row, ok, err := app.store.GetThreadDraft(thread.ID)
				if err != nil || !ok {
					t.Errorf("read back mid-saga save: ok=%v err=%v", ok, err)
				}
				midSagaRow = row
				return nil
			}

			if err := app.RevertConversationAndResendMessage(
				context.Background(),
				thread.ID, "user:1",
				RevertAndResendOptions{Content: "rewritten prompt"},
			); err != nil {
				t.Fatalf("revert and resend: %v", err)
			}

			draft, ok, err := app.store.GetThreadDraft(thread.ID)
			if err != nil {
				t.Fatalf("get draft: %v", err)
			}
			switch {
			case tc.midSagaSave:
				if !ok {
					t.Fatal("the settle deleted a composer save that landed mid-saga")
				}
				if draft != midSagaRow {
					t.Fatalf("draft after resend = %+v, want the mid-saga save %+v left alone", draft, midSagaRow)
				}
			case tc.priorWIP:
				if !ok || draft != wip {
					t.Fatalf("draft after resend = %+v (ok=%v), want the pre-saga WIP %+v restored", draft, ok, wip)
				}
			default:
				if ok {
					t.Fatalf("draft after resend = %+v, want the staged crash copy cleared", draft)
				}
			}
		})
	}
}

// TestRevertAndResendReportsSuccessWhenTheSettleFails pins the one
// asymmetry in the tail: the send already went out, so a failure to
// retire the crash copy must not be reported as a failed revert — the
// caller would tell the user their message did not send when it did.
// Nothing is lost either way; the row still holds both texts.
//
// The store closing under the saga is the real shape of this failure
// (shutdown racing the tail after the send committed), and it is the
// only injection that does not require a seam the production code has no
// other use for.
func TestRevertAndResendReportsSuccessWhenTheSettleFails(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-settle-fail")

	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:  thread.ID,
		Content:   "half-typed follow-up",
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	app.sendMessageFn = func(string, string, []string) error {
		if err := app.store.Close(); err != nil {
			t.Errorf("close store mid-saga: %v", err)
		}
		return nil
	}

	if err := app.RevertConversationAndResendMessage(
		context.Background(),
		thread.ID, "user:1",
		RevertAndResendOptions{Content: "rewritten prompt"},
	); err != nil {
		t.Fatalf("revert and resend = %v, want nil: the send completed, only the draft settle failed", err)
	}
	if !strings.Contains(logs.String(), "revert and resend") {
		t.Fatalf("settle failure was swallowed silently; log output = %q", logs.String())
	}
}

// TestRevertAndResendProceedsOncePendingSendResolves is the release side
// of the echo-gap guard (TestRevertAndResendRefusesPendingSend covers
// the refusal). The marker is transient state, not a mode: once the
// in-flight send resolves, the very same call must go through — a guard
// that leaked its marker would wedge the edit affordance for the rest of
// the thread's life.
func TestRevertAndResendProceedsOncePendingSendResolves(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-pending-release")

	app.triage.RegisterPendingSendWithExpectation(thread.ID, "user:1", 1, triage.PendingSendExpectation{ProviderItemID: ""})
	err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"})
	if err == nil || !strings.Contains(err.Error(), "awaiting provider confirmation") {
		t.Fatalf("error = %v, want the pending-send refusal", err)
	}

	// The send resolved (here: it failed and released its marker, the
	// same clear app_send.go performs). The revert is now admissible.
	app.triage.ClearPendingSendForFailure(thread.ID, "user:1")
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("precondition: the pending-send marker survived the clear")
	}

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("revert and resend after the pending send resolved: %v", err)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 || items[0].ID != "user:0" || items[1].ID != "asst:0" || items[2].Summary != edited {
		t.Fatalf("items after the released revert = %+v, want the kept prefix plus the replacement", items)
	}
}

// --- Codex ---

// TestRevertAndResendCodexForksAndResends is the Codex happy path. The
// provider half is completely different from Claude's — stop the
// session, `thread/fork` at the last provider turn before the anchor,
// repoint SessionRef, and truncate SQLite at WHOLE TURN granularity
// rather than at the message — so the saga's contract has to be
// re-proved on it rather than inherited.
func TestRevertAndResendCodexForksAndResends(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit

	workspace := t.TempDir()
	thread := e2eThread("t-resend-codex", string(provider.Codex), workspace)
	thread.SessionRef = "codex-source-thread"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertAssistantTextItem(t, app.store, thread.ID, "asst:0", 0, "reply 0")
	insertUserItemWithMeta(t, app.store, thread.ID, "user:1", 1, "second", `{"provider_item_id":"provider-user-1"}`)
	insertAssistantTextItem(t, app.store, thread.ID, "asst:1", 1, "reply 1")
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	forkLog := filepath.Join(t.TempDir(), "fork-requests.jsonl")
	binary := writeCodexResendBinary(t, codexResendMock{
		forkedThreadID: "codex-forked-thread",
		forkRequestLog: forkLog,
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("install mock codex binary: %v", err)
	}

	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "half-typed follow-up",
		TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("codex revert and resend: %v", err)
	}

	// The fork cut at the last provider turn BEFORE the anchor's turn.
	if req := readCodexForkRequest(t, forkLog); !strings.Contains(req, `"lastTurnId":"turn-a"`) ||
		!strings.Contains(req, `"threadId":"codex-source-thread"`) {
		t.Fatalf("fork request = %s, want codex-source-thread cut at turn-a", req)
	}
	if ref := mustGetThread(t, app, thread.ID).SessionRef; ref != "codex-forked-thread" {
		t.Fatalf("session ref = %q, want the fork", ref)
	}

	// Codex truncates the WHOLE anchor turn, so the assistant row that
	// shared turn 1 goes with it and the event carries no kept-set.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 || items[0].ID != "user:0" || items[1].ID != "asst:0" || items[2].Summary != edited {
		t.Fatalf("items after codex resend = %+v, want the turn-0 prefix plus the replacement", items)
	}
	revertedIndex, ev := findRevertedEvent(t, bus)
	if !ev.DraftPendingResend {
		t.Fatal("codex reverted event draftPendingResend = false, want true")
	}
	if len(ev.KeptAnchorTurnItemIDs) != 0 {
		t.Fatalf("codex kept-set = %v, want empty (the whole turn is cut)", ev.KeptAnchorTurnItemIDs)
	}
	if resentIndex := findResentItemEventIndex(t, bus, edited); revertedIndex >= resentIndex {
		t.Fatalf("user_message:reverted emitted at %d, after the resent item event at %d", revertedIndex, resentIndex)
	}

	// The WIP comes back, same as the Claude path.
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != "half-typed follow-up" {
		t.Fatalf("draft after codex resend = %q, want the untouched WIP restored", draft.Content)
	}
}

// TestRevertAndResendCodexRollbackFailureKeepsCrashCopy is the Codex
// half of seam 2. A fork whose surviving tail is not the turn AO asked
// to cut at means the app-server and AO disagree about provider history,
// so the rollback refuses — and the crash copy must stand exactly as it
// does on the Claude path, with the conversation untouched.
func TestRevertAndResendCodexRollbackFailureKeepsCrashCopy(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit

	workspace := t.TempDir()
	thread := e2eThread("t-resend-codex-fail", string(provider.Codex), workspace)
	thread.SessionRef = "codex-source-thread"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:0", 0, "first", `{"provider_item_id":"provider-user-0"}`)
	insertUserItemWithMeta(t, app.store, thread.ID, "user:1", 1, "second", `{"provider_item_id":"provider-user-1"}`)
	insertCodexTurn(t, app.store, thread.ID, 0, "turn-a")
	insertCodexTurn(t, app.store, thread.ID, 1, "turn-b")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "", "")

	binary := writeCodexResendBinary(t, codexResendMock{
		forkedThreadID: "codex-forked-thread",
		forkTailTurnID: "turn-wrong",
	})
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
		t.Fatalf("install mock codex binary: %v", err)
	}
	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:  thread.ID,
		Content:   "half-typed follow-up",
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	const edited = "rewritten prompt"
	err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, "user:1", RevertAndResendOptions{Content: edited})
	if err == nil || !strings.Contains(err.Error(), "expected anchor") {
		t.Fatalf("error = %v, want the fork tail mismatch to abort the rollback", err)
	}
	if strings.Contains(err.Error(), "resend failed") {
		t.Fatalf("error = %q, want a rollback-stage failure, not the post-commit prefix", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items after a failed codex rollback = %+v, want both preserved", items)
	}
	if ref := mustGetThread(t, app, thread.ID).SessionRef; ref != "codex-source-thread" {
		t.Fatalf("session ref = %q, want the untouched source thread", ref)
	}
	for _, e := range bus.allEvents() {
		if e.Name == "user_message:reverted" {
			t.Fatal("user_message:reverted fired despite the codex rollback failure")
		}
	}
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != edited+"\n\nhalf-typed follow-up" {
		t.Fatalf("draft content = %q, want the crash copy staged ahead of the WIP", draft.Content)
	}
}

// --- fixtures ---

// seedResendMidTurnThread builds a single-turn thread whose LAST user
// message sits mid-turn (a steered prompt at item_index 2), with the
// anchor carrying both the message uuid and its transcript parent — the
// pair the already-cut retry detector reads.
func seedResendMidTurnThread(t *testing.T, app *App, id string) (store.Thread, string) {
	t.Helper()
	workspace := t.TempDir()
	writeClaudeProjectSession(t, os.Getenv("HOME"), workspace, resendSourceSessionID,
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"steer"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := e2eThread(id, string(provider.Claude), workspace)
	thread.SessionRef = resendSourceSessionID
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, row := range []store.Item{
		{ID: "user:0", Kind: "user_text", Role: "user", ItemIndex: 0, Summary: "first"},
		{ID: "asst:0", Kind: "assistant_text", Role: "assistant", ItemIndex: 1, Summary: "reply 0"},
		{ID: "user:steer", Kind: "user_text", Role: "user", ItemIndex: 2, Summary: "steer"},
		{ID: "asst:1", Kind: "assistant_text", Role: "assistant", ItemIndex: 3, Summary: "reply 1"},
	} {
		row.ThreadID = thread.ID
		row.TurnIndex = 0
		row.Status = "completed"
		row.CreatedAt = now
		row.UpdatedAt = now
		if _, err := app.store.AppendItem(row); err != nil {
			t.Fatalf("append %s: %v", row.ID, err)
		}
	}
	seedMessageAnchor(t, app.store, thread.ID, "user:steer", 0, "u1", "a0")
	return thread, workspace
}

// codexResendMock configures writeCodexResendBinary. Unlike the
// rollback-only mock next door, this one has to survive the RESEND that
// follows the fork: it echoes back whichever thread id is resumed (the
// source thread for the fork's throwaway session, the fork for the
// resend) and answers turn/start.
type codexResendMock struct {
	forkedThreadID string
	forkRequestLog string
	// forkTailTurnID overrides the fork tail exposed by thread/turns/list so
	// the anchor cross-check fails.
	forkTailTurnID string
}

func writeCodexResendBinary(t *testing.T, mock codexResendMock) string {
	t.Helper()
	logForkRequest := ":"
	if mock.forkRequestLog != "" {
		logForkRequest = fmt.Sprintf(`/bin/echo "$line" >> '%s'`, mock.forkRequestLog)
	}
	tailExpr := `"$cut"`
	if mock.forkTailTurnID != "" {
		tailExpr = fmt.Sprintf(`'%s'`, mock.forkTailTurnID)
	}
	script := fmt.Sprintf(`#!/bin/sh
turn=0
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        %s
        cut=$(/bin/echo "$line" | /usr/bin/grep -o '"lastTurnId":"[^"]*"' | /usr/bin/cut -d'"' -f4)
        tail=%s
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s","turns":[]}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/turns/list"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"data":[{"id":"%%s"}],"nextCursor":null}}\n' "$id" "$tail"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        resumed=$(/bin/echo "$line" | /usr/bin/grep -o '"threadId":"[^"]*"' | /usr/bin/head -1 | /usr/bin/cut -d'"' -f4)
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%%s","turns":[]}}}\n' "$id" "$resumed"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"codex-fresh-thread"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/read"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"status":{"type":"idle"}}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
        turn=$((turn+1))
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-resent-%%s"}}}\n' "$id" "$turn"
        continue
    fi
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, logForkRequest, tailExpr, mock.forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-resend.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock codex binary: %v", err)
	}
	return path
}
