package app

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

const resendSourceSessionID = "source-session"

const resendSourceSessionJSONL = `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`

// newResendTestApp builds the full E2E app the edit-and-resend saga
// needs: a real store + triage router, an attachment store, and a SILENT
// mock CLI installed over setupE2EApp's poisoned provider binary. The
// resend therefore dispatches for real — session start, stdin write, the
// lot — while the provider answers with nothing, which keeps the
// timeline assertions deterministic. No test here may reach a real
// provider binary or the developer's provider homes; setupE2EApp
// detaches HOME and fails the test if an unmocked spawn slips through.
func newResendTestApp(t *testing.T) (*App, *capturedEventBus) {
	t.Helper()
	app, bus := setupE2EApp(t)
	// One ordered log for BOTH App-level emissions (user_message:reverted)
	// and triage's item stream, so this saga's truncate-before-new-message
	// ordering is observable in a single sequence.
	app.testEmitHook = bus.emit

	attachments, err := attachment.NewStore(
		attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, app.store,
	)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attachments

	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("install mock claude binary: %v", err)
	}
	return app, bus
}

// seedResendThread builds the idle two-turn Claude thread the saga tests
// revert: a sliceable session file under the isolated HOME plus the
// matching SQLite rows and the anchor keying the slice.
func seedResendThread(t *testing.T, app *App, id string) (store.Thread, string) {
	t.Helper()
	workspace := t.TempDir()
	writeClaudeProjectSession(t, os.Getenv("HOME"), workspace, resendSourceSessionID, resendSourceSessionJSONL)
	thread := e2eThread(id, string(provider.Claude), workspace)
	thread.SessionRef = resendSourceSessionID
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "first")
	insertAssistantTextItem(t, app.store, thread.ID, "asst:0", 0, "reply 0")
	insertUserItem(t, app.store, thread.ID, "user:1", 1, "second")
	insertAssistantTextItem(t, app.store, thread.ID, "asst:1", 1, "reply 1")
	seedMessageAnchor(t, app.store, thread.ID, "user:1", 1, "u1", "")
	return thread, workspace
}

// TestRevertAndResendReplacesMessageAndRestoresWIP is the whole saga end
// to end: the tail after the anchor is truncated, the provider session
// is sliced to drop the original prompt, the EDITED replacement is
// persisted and dispatched in its place, and the composer draft comes
// back byte-identical to the work-in-progress the user had before —
// terminal chips included. The `user_message:reverted` event carries
// draftPendingResend so the frontend knows the draft row it just saw was
// saga state, not composer content.
func TestRevertAndResendReplacesMessageAndRestoresWIP(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, workspace := seedResendThread(t, app, "t-resend")

	wip := store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "half-typed follow-up",
		Attachments:   `["att-wip"]`,
		TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if _, err := app.store.UpsertThreadDraft(wip); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(thread.ID, "user:1", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 || items[0].ID != "user:0" || items[1].ID != "asst:0" {
		t.Fatalf("items after resend = %+v, want the turn-0 prefix plus one new user row", items)
	}
	resent := items[2]
	if resent.Kind != "user_text" || resent.Role != "user" || resent.Summary != edited {
		t.Fatalf("resent row = %+v, want a user_text carrying %q", resent, edited)
	}
	if resent.TurnIndex != 1 {
		t.Fatalf("resent row turn index = %d, want 1 (the truncated tail's slot)", resent.TurnIndex)
	}

	// The pre-saga WIP returns untouched: the edited text now lives in
	// the persisted user row, so the transient crash copy is settled away
	// rather than left fused into the composer.
	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft != wip {
		t.Fatalf("draft after resend = %+v, want the pre-saga WIP %+v restored byte-identical", draft, wip)
	}

	// The provider session was sliced at the anchor: the original prompt
	// is gone from the transcript the resend resumes on.
	updated := mustGetThread(t, app, thread.ID)
	if updated.SessionRef == "" || updated.SessionRef == resendSourceSessionID {
		t.Fatalf("session ref = %q, want a recovered fork session", updated.SessionRef)
	}
	assertClaudeSessionText(t, workspace, updated.SessionRef, []string{"first"}, []string{"second"})

	revertedIndex, ev := findRevertedEvent(t, bus)
	if ev.ThreadID != thread.ID || ev.UserItemID != "user:1" || ev.TurnIndex != 1 {
		t.Fatalf("reverted event = %+v, want thread=%s item=user:1 turn=1", ev, thread.ID)
	}
	if !ev.DraftPendingResend {
		t.Fatal("reverted event draftPendingResend = false, want true so the frontend skips composer rehydration")
	}

	// Ordering is load-bearing: one FIFO WebSocket carries both frames,
	// so the frontend must see the truncation before the replacement.
	resentIndex := findResentItemEventIndex(t, bus, edited)
	if revertedIndex >= resentIndex {
		t.Fatalf("user_message:reverted emitted at %d, after the resent item event at %d; the frontend would drop the new row",
			revertedIndex, resentIndex)
	}
}

// TestRevertAndResendKeepsMergedDraftWhenResendFails is the crash-copy
// contract under the one failure that matters: the revert committed and
// the resend did not. The draft row must still hold the edited text
// merged AHEAD of the user's untouched WIP — both texts recoverable —
// and the error must be distinguishable from a guard rejection so the
// caller knows the timeline really was truncated.
func TestRevertAndResendKeepsMergedDraftWhenResendFails(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-fail")

	if _, err := app.store.UpsertThreadDraft(store.ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "half-typed follow-up",
		TerminalChips: `[{"id":"chip-1","label":"npm test"}]`,
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed WIP draft: %v", err)
	}

	// An attachment id that resolves to nothing fails the send inside
	// resolveUserMessageEnvelope — after the rollback committed, which is
	// exactly the window the crash copy exists for.
	const edited = "rewritten prompt"
	err := app.RevertConversationAndResendMessage(
		thread.ID, "user:1",
		RevertAndResendOptions{Content: edited, AttachmentIDs: []string{"ghost-attachment"}},
	)
	if err == nil {
		t.Fatal("revert and resend succeeded with an unresolvable attachment, want the resend to fail")
	}
	if !strings.Contains(err.Error(), "revert and resend: resend failed") {
		t.Fatalf("error = %q, want the distinct resend-failure prefix", err)
	}

	// The revert itself committed.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "user:0" || items[1].ID != "asst:0" {
		t.Fatalf("items after failed resend = %+v, want [user:0 asst:0]", items)
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != edited+"\n\nhalf-typed follow-up" {
		t.Fatalf("draft content = %q, want the edited text merged ahead of the WIP", draft.Content)
	}
	if got := decodeDraftAttachmentIDs(t, draft.Attachments); len(got) != 1 || got[0] != "ghost-attachment" {
		t.Fatalf("draft attachments = %v, want the edited message's ids preserved", got)
	}
	if draft.TerminalChips != `[{"id":"chip-1","label":"npm test"}]` {
		t.Fatalf("draft terminal chips = %q, want the WIP's chips carried through the merge", draft.TerminalChips)
	}
}

// TestRevertAndResendClearsDraftWhenNoWIP is the empty-WIP half of the
// draft contract: with nothing in the composer, the staged crash copy is
// just the edited payload, and a successful resend leaves no draft row
// behind at all.
func TestRevertAndResendClearsDraftWhenNoWIP(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-nowip")

	if err := app.RevertConversationAndResendMessage(thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	if draft, ok, err := app.store.GetThreadDraft(thread.ID); err != nil {
		t.Fatalf("get draft: %v", err)
	} else if ok && strings.TrimSpace(draft.Content) != "" {
		t.Fatalf("draft after resend = %+v, want no leftover composer content", draft)
	}
}

// TestRevertAndResendStagesEditedPayloadWithoutWIP pins the staged crash
// copy itself for the empty-WIP case: the failure branch is the only way
// to observe the row mid-saga, and it must be the edited payload alone —
// no phantom separator, no inherited content.
func TestRevertAndResendStagesEditedPayloadWithoutWIP(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-stage")

	const edited = "rewritten prompt"
	if err := app.RevertConversationAndResendMessage(
		thread.ID, "user:1",
		RevertAndResendOptions{Content: edited, AttachmentIDs: []string{"ghost-attachment"}},
	); err == nil {
		t.Fatal("revert and resend succeeded with an unresolvable attachment, want the resend to fail")
	}

	draft, ok, err := app.store.GetThreadDraft(thread.ID)
	if err != nil || !ok {
		t.Fatalf("get draft: %+v ok=%v err=%v", draft, ok, err)
	}
	if draft.Content != edited {
		t.Fatalf("staged draft content = %q, want exactly the edited text", draft.Content)
	}
}

// TestRevertAndResendKeepsAttachments resends a message carrying the
// anchor's own attachments. Attachment records are thread-scoped, not
// item-scoped, so truncating the conversation must leave them
// resolvable — a send whose ids died with the rolled-back row would fail
// resolveSendMessageAttachments instead of re-sending the images.
func TestRevertAndResendKeepsAttachments(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-attach")

	pngHeader := base64.StdEncoding.EncodeToString([]byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	})
	record, err := app.attachments.Upload(thread.ID, "shot.png", "image/png", pngHeader, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("upload attachment: %v", err)
	}
	meta, err := usermessage.Marshal(usermessage.Input{Attachments: []store.Attachment{record}})
	if err != nil {
		t.Fatalf("marshal user meta: %v", err)
	}
	if err := app.store.UpdateItemMeta(thread.ID, "user:1", meta); err != nil {
		t.Fatalf("stamp attachment meta on the anchor: %v", err)
	}

	if err := app.RevertConversationAndResendMessage(
		thread.ID, "user:1",
		RevertAndResendOptions{Content: "rewritten prompt", AttachmentIDs: []string{record.ID}},
	); err != nil {
		t.Fatalf("revert and resend with attachments: %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	resent := items[len(items)-1]
	resentMeta, err := usermessage.FromItem(resent)
	if err != nil {
		t.Fatalf("decode resent user meta: %v", err)
	}
	if len(resentMeta.Attachments) != 1 || resentMeta.Attachments[0].ID != record.ID {
		t.Fatalf("resent attachments = %+v, want the anchor's attachment %s carried over", resentMeta.Attachments, record.ID)
	}
}

// TestRevertAndResendConfirmedKillClearsBackgroundRows drives the
// consented path: a running background tool call on a turn BEFORE the
// revert point (so its row survives the truncation) is flipped inactive
// during the rollback — dead work must not survive as a stale running
// spinner — and the tray-change event fires.
func TestRevertAndResendConfirmedKillClearsBackgroundRows(t *testing.T) {
	app, bus := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-bg")
	insertRunningBackgroundToolCall(t, app.store, thread.ID, "bg:0", 0, 9)

	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 1 {
		t.Fatalf("precondition: running background tasks = %d (%v), want 1", count, err)
	}

	if err := app.RevertConversationAndResendMessage(thread.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt", KillRunningBackgroundTasks: true}); err != nil {
		t.Fatalf("confirmed revert and resend: %v", err)
	}

	if count, err := app.countRunningBackgroundTasks(thread.ID); err != nil || count != 0 {
		t.Fatalf("running background tasks after confirmed revert = %d (%v), want 0", count, err)
	}
	trayChanged := false
	for _, e := range bus.allEvents() {
		if e.Name == "provider:background_tasks_changed" {
			trayChanged = true
		}
	}
	if !trayChanged {
		t.Fatal("provider:background_tasks_changed did not fire")
	}
}

// TestRevertAndResendMidTurnAnchorEmitsKeptSet resends over a MID-TURN
// user message (a steered prompt at item_index > 0). The item-granular
// cut keeps the turn's prefix in SQLite, and the `user_message:reverted`
// event must carry exactly that kept-set so the frontend trims to match
// instead of hiding rows the backend kept.
func TestRevertAndResendMidTurnAnchorEmitsKeptSet(t *testing.T) {
	app, bus := newResendTestApp(t)
	workspace := t.TempDir()
	writeClaudeProjectSession(t, os.Getenv("HOME"), workspace, resendSourceSessionID,
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"source-session","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"source-session","message":{"role":"user","content":"steer"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"source-session","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`)
	thread := e2eThread("t-resend-midturn", string(provider.Claude), workspace)
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
	seedMessageAnchor(t, app.store, thread.ID, "user:steer", 0, "u1", "")

	if err := app.RevertConversationAndResendMessage(thread.ID, "user:steer", RevertAndResendOptions{Content: "rewritten steer"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	_, ev := findRevertedEvent(t, bus)
	kept := ev.KeptAnchorTurnItemIDs
	if len(kept) != 2 || kept[0] != "user:0" || kept[1] != "asst:0" {
		t.Fatalf("event kept-set = %v, want [user:0 asst:0]", kept)
	}
	assertClaudeSessionText(t, workspace, mustGetThread(t, app, thread.ID).SessionRef,
		[]string{"first"}, []string{"steer"})
}

func mustGetThread(t *testing.T, app *App, threadID string) store.Thread {
	t.Helper()
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("get thread %s: %v", threadID, err)
	}
	return thread
}

// findRevertedEvent returns the ordinal position of the single
// `user_message:reverted` emission plus its payload.
func findRevertedEvent(t *testing.T, bus *capturedEventBus) (int, UserMessageRevertedEvent) {
	t.Helper()
	index := -1
	var found UserMessageRevertedEvent
	for i, e := range bus.allEvents() {
		if e.Name != "user_message:reverted" {
			continue
		}
		if index >= 0 {
			t.Fatalf("user_message:reverted fired more than once")
		}
		ev, ok := e.Data.(UserMessageRevertedEvent)
		if !ok {
			t.Fatalf("user_message:reverted payload = %T, want UserMessageRevertedEvent", e.Data)
		}
		index, found = i, ev
	}
	if index < 0 {
		t.Fatal("user_message:reverted never fired")
	}
	return index, found
}

// findResentItemEventIndex returns the ordinal position of the item
// stream emission that pushed the replacement user row.
func findResentItemEventIndex(t *testing.T, bus *capturedEventBus, content string) int {
	t.Helper()
	for i, e := range bus.allEvents() {
		if e.Name != "provider:item_event" {
			continue
		}
		ev, ok := e.Data.(triage.ItemStreamEvent)
		if !ok || ev.Item == nil {
			continue
		}
		if ev.Item.Kind == "user_text" && ev.Item.Summary == content {
			return i
		}
	}
	t.Fatalf("no provider:item_event carried the resent user row %q", content)
	return -1
}

func decodeDraftAttachmentIDs(t *testing.T, raw string) []string {
	t.Helper()
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatalf("decode draft attachments %q: %v", raw, err)
	}
	return ids
}

// TestRevertAndResendReExpandsComposerCommands pins wire parity with the
// composer's own send for a D31 command edit: the transcript keeps the
// RAW typed text while the provider payload carries the expanded block.
// An edited message naming /workflow must re-expand on resend exactly as
// a fresh composer send would — the expansion is derived state rebuilt
// at send time, never copied from (or lost with) the original message.
func TestRevertAndResendReExpandsComposerCommands(t *testing.T) {
	app, _ := newResendTestApp(t)
	thread, _ := seedResendThread(t, app, "t-resend-expand")

	// Swap the silent mock for one that logs stdin: the expansion lives
	// only in providerContent on the wire, so the capture file is the one
	// place an assertion can see what the CLI was actually given.
	capture := filepath.Join(t.TempDir(), "stdin-capture.jsonl")
	script := "#!/bin/bash\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> '" + capture + "'\ndone\nexit 0\n"
	binary := filepath.Join(t.TempDir(), "mock-claude-capture.sh")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write capturing mock claude: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("install capturing mock claude: %v", err)
	}

	const edited = "/workflow start the release"
	if err := app.RevertConversationAndResendMessage(thread.ID, "user:1", RevertAndResendOptions{Content: edited}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}

	// Transcript stays raw: the row records the typed text and the fact
	// that it was a command invocation, never the expanded block.
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	resent := items[len(items)-1]
	if resent.Summary != edited {
		t.Fatalf("resent summary = %q, want the raw typed text %q", resent.Summary, edited)
	}
	meta, err := usermessage.FromItem(resent)
	if err != nil {
		t.Fatalf("resent meta: %v", err)
	}
	if meta.Command != "workflow" {
		t.Fatalf("resent meta command = %q, want %q", meta.Command, "workflow")
	}

	// The wire payload carries the typed text plus the appended block.
	sent := waitForFileText(t, capture, func(text string) bool {
		return strings.Contains(text, "Agent Overflow workflows are available")
	})
	if !strings.Contains(sent, edited) {
		t.Fatalf("wire payload carries the block but not the typed text: %s", sent)
	}
}
