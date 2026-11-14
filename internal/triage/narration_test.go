package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// narrationStopMeta is a thinking block's stop as the Claude parser emits
// it for a block whose signature carries the narration tag.
var narrationStopMeta = json.RawMessage(`{"index":0,"blockType":"thinking","narration":true}`)

// narrationHead is a word that sits only in the first runes of
// narrationText, outside the tail a thinking row keeps as its summary.
const narrationHead = "quokka"

// narrationText is longer than a thinking row's summary and names one real
// workspace path, so a row holding it whole is told from one holding its
// tail, and its path refs from none.
func narrationText(t *testing.T) string {
	t.Helper()
	text := narrationHead + " opens the note. See src/foo.ts for the change.\n\n" +
		strings.Repeat("The summary keeps going for a while. ", 20) + "The end."
	if len([]rune(text)) <= thinkingPreviewRunes || strings.Contains(ThinkingSummaryPreview(text), narrationHead) {
		t.Fatal("fixture must outrun the thinking preview with its head outside it")
	}
	return text
}

// narrationThread creates thread t1 in a workspace holding src/foo.ts.
func narrationThread(t *testing.T, st *store.Store) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID: "t1", ProjectID: triageTestProjectID, Title: "narration", Provider: "claude",
		WorkspacePath: root, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

// echoCodeSpans makes the router's code spans name the text they were
// built from.
func echoCodeSpans(t *testing.T, router *Router) {
	t.Helper()
	router.SetCodeSpanEnricher(func(text string) json.RawMessage {
		blob, err := json.Marshal(map[string]string{"got": text})
		if err != nil {
			t.Fatalf("marshal echo: %v", err)
		}
		return blob
	})
}

// streamThinking streams text as thinking deltas in scope without
// stopping the block.
func streamThinking(t *testing.T, router *Router, scope, providerItemID, text string) {
	t.Helper()
	for len(text) > 0 {
		n := min(len(text), 97)
		handleOrFatal(t, router, provider.ProviderEvent{
			Kind: provider.EventThinking, ThreadID: "t1", ParentToolUseID: scope, ItemID: providerItemID,
			Content: text[:n], Timestamp: time.Now(),
		})
		text = text[n:]
	}
}

type narrationMeta struct {
	Narration      bool                `json:"narration"`
	PathRefs       []pathlinks.PathRef `json:"pathRefs"`
	CodeSpans      map[string]string   `json:"codeSpans"`
	ProviderItemID string              `json:"provider_item_id"`
}

func decodeNarrationMeta(t *testing.T, raw string) narrationMeta {
	t.Helper()
	var meta narrationMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("meta %q: %v", raw, err)
	}
	return meta
}

// assertNarrationRow checks a stored or pushed row is the settled prose
// row of a narration block.
func assertNarrationRow(t *testing.T, what string, row store.Item, text string) narrationMeta {
	t.Helper()
	if row.Kind != itemKindAssistantText || row.Role != "assistant" || row.Status != statusCompleted {
		t.Fatalf("%s is %s/%s/%s, want completed assistant text", what, row.Kind, row.Role, row.Status)
	}
	if row.Summary != text {
		t.Fatalf("%s summary is %d bytes, want the whole %d-byte text", what, len(row.Summary), len(text))
	}
	if row.PayloadKind != itemKindAssistantText || row.PayloadMeta != "{}" {
		t.Fatalf("%s payload is %q %q, want the assistant text shape", what, row.PayloadKind, row.PayloadMeta)
	}
	meta := decodeNarrationMeta(t, row.Meta)
	if !meta.Narration {
		t.Fatalf("%s meta carries no narration key: %s", what, row.Meta)
	}
	return meta
}

func assertNoErrorRows(t *testing.T, st *store.Store) {
	t.Helper()
	for _, item := range mustListItems(t, st, "t1") {
		if item.Kind == "error" || item.Kind == "api_error" {
			t.Fatalf("an error row was written: %+v", item)
		}
	}
}

// A streamed thinking block whose stop carries the narration marker
// settles as assistant text: one upsert of the same row, holding the whole
// text and what a settled prose row carries.
func TestNarrationStopSettlesStreamedThinkingAsAssistantText(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	echoCodeSpans(t, router)
	text := narrationText(t)

	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStart, ThreadID: "t1", Timestamp: time.Now(),
		Meta: json.RawMessage(`{"index":0,"blockType":"thinking","apiMessageId":"msg_01"}`),
	})
	streamThinking(t, router, "", "", text)
	streaming := firstItemByKind(t, st, "t1", itemKindThinking)
	if streaming.Status != statusStreaming {
		t.Fatalf("the block is %s before its stop, want streaming thinking", streaming.Status)
	}

	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()

	items := mustListItems(t, st, "t1")
	if len(items) != 1 {
		t.Fatalf("the settle left %d rows, want the one row: %+v", len(items), items)
	}
	row := items[0]
	meta := assertNarrationRow(t, "stored row", row, text)
	if row.ID != streaming.ID || row.PayloadID != streaming.PayloadID || row.TurnIndex != streaming.TurnIndex ||
		row.ItemIndex != streaming.ItemIndex || row.ParentID != streaming.ParentID || row.CreatedAt != streaming.CreatedAt {
		t.Fatalf("the settle moved the row:\n before %+v\n after  %+v", streaming, row)
	}
	if len(meta.PathRefs) != 1 || meta.PathRefs[0].Path != "src/foo.ts" {
		t.Fatalf("path refs = %+v, want src/foo.ts", meta.PathRefs)
	}
	if meta.CodeSpans["got"] != text {
		t.Fatalf("code spans were built from %d bytes, want the whole text", len(meta.CodeSpans["got"]))
	}
	if !strings.Contains(row.Meta, `"apiMessageId":"msg_01"`) && !strings.Contains(row.Meta, "msg_01") {
		t.Fatalf("the settle dropped the row's stream block meta: %s", row.Meta)
	}
	data, err := st.GetPayloadData("t1", row.PayloadID)
	if err != nil || string(data) != text {
		t.Fatalf("payload data = %d bytes (%v), want the whole text", len(data), err)
	}

	events := itemStreamEventsFor(t, emissions, row.ID)
	last := events[len(events)-1]
	if last.Action != itemStreamActionUpsert || last.Item == nil {
		t.Fatalf("the settle's push is %+v, want an upsert", last)
	}
	assertNarrationRow(t, "pushed row", *last.Item, text)
	if last.Item.Meta != row.Meta || last.Item.ItemIndex != row.ItemIndex || last.Item.CreatedAt != row.CreatedAt {
		t.Fatalf("the pushed row is not the stored row:\n pushed %+v\n stored %+v", *last.Item, row)
	}
	for i, evt := range events[:len(events)-1] {
		switch evt.Action {
		case itemStreamActionUpsert:
			if evt.Item.Kind != itemKindThinking || evt.Item.Status != statusStreaming {
				t.Fatalf("event %d pushed %s/%s before the stop", i, evt.Item.Kind, evt.Item.Status)
			}
		case itemStreamActionDelta:
			if evt.Kind != itemKindThinking {
				t.Fatalf("event %d is a %s delta, want thinking", i, evt.Kind)
			}
		default:
			t.Fatalf("event %d is a %s; the settle is one upsert", i, evt.Action)
		}
	}
	if router.hasActiveStreamingItem("t1") {
		t.Fatal("the settle left the thread streaming")
	}
}

// The search index holds assistant text and no thinking, so the settled
// narration row enters it with its whole text.
func TestNarrationRowIsIndexedForSearch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	streamThinking(t, router, "", "", text)
	search := func() []store.ThreadSearchHit {
		t.Helper()
		hits, err := st.SearchThreads(narrationHead, store.ThreadSearchFilter{ThreadIDs: []string{"t1"}})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		return hits
	}
	if hits := search(); len(hits) != 0 {
		t.Fatalf("a streaming thinking row is indexed: %+v", hits)
	}
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()
	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	hits := search()
	if len(hits) != 1 || hits[0].ItemID != row.ID || hits[0].Kind != store.ThreadSearchKindAssistant {
		t.Fatalf("search hits = %+v, want the narration row as assistant text", hits)
	}
}

// A stop without the marker, or with it false, settles thinking as before:
// a patch of the thinking row.
func TestUnmarkedStopLeavesThinkingAsThinking(t *testing.T) {
	for name, meta := range map[string]string{
		"no key":     `{"index":0,"blockType":"thinking"}`,
		"false":      `{"index":0,"blockType":"thinking","narration":false}`,
		"not a bool": `{"index":0,"blockType":"thinking","narration":"true"}`,
	} {
		t.Run(name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			narrationThread(t, st)
			text := narrationText(t)
			streamThinking(t, router, "", "", text)
			handleOrFatal(t, router, provider.ProviderEvent{
				Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(), Meta: json.RawMessage(meta),
			})
			router.WaitForPendingSettles()
			row := firstItemByKind(t, st, "t1", itemKindThinking)
			if row.Status != statusCompleted || row.Summary != ThinkingSummaryPreview(text) || row.PayloadKind != itemKindThinking {
				t.Fatalf("thinking row = %s %q %s", row.Status, row.Summary, row.PayloadKind)
			}
			if strings.Contains(row.Meta, "narration") {
				t.Fatalf("thinking row meta names narration: %s", row.Meta)
			}
			events := itemStreamEventsFor(t, emissions, row.ID)
			if last := events[len(events)-1]; last.Action != itemStreamActionPatch {
				t.Fatalf("the settle's push is a %s, want a patch", last.Action)
			}
		})
	}
}

// A narration block that ends without its stop stays a thinking row, and
// a stop that arrives after the row settled changes nothing.
func TestNarrationBlockWithoutItsStopStaysThinking(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	streamThinking(t, router, "", "", text)
	// A tool start is a timeline boundary: it settles the scope's open
	// thinking.
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "toolu_1", ItemType: "Read", Timestamp: time.Now(),
		Meta: json.RawMessage(`{"toolName":"Read","input":{"file_path":"README.md"}}`),
	})
	router.WaitForPendingSettles()
	row := firstItemByKind(t, st, "t1", itemKindThinking)
	if row.Status != statusCompleted {
		t.Fatalf("thinking row is %s after the boundary, want completed", row.Status)
	}

	emissions.reset()
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()
	after := mustItem(t, st, "t1", row.ID)
	if after.Kind != itemKindThinking || after.Summary != row.Summary || after.Rev != row.Rev {
		t.Fatalf("a late stop rewrote the settled row: %+v", after)
	}
	if events := itemStreamEventsFor(t, emissions, row.ID); len(events) != 0 {
		t.Fatalf("a late stop pushed the settled row: %+v", events)
	}
}

// The settle leaves a row another write ended between its read and its
// write. The code span enricher runs in that gap.
func TestNarrationSettleLeavesARowEndedAfterItsRead(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	streamThinking(t, router, "", "", text)
	row := firstItemByKind(t, st, "t1", itemKindThinking)
	var stopped string
	router.SetCodeSpanEnricher(func(string) json.RawMessage {
		current := mustItem(t, st, "t1", row.ID)
		stopped = stoppedSummary(current.Summary)
		if _, changed, err := st.ErrorActiveItemIfRevision("t1", row.ID, current.Rev, stopped, time.Now().UnixMilli(), nil); err != nil || !changed {
			t.Errorf("flip: changed=%v err=%v", changed, err)
		}
		return nil
	})
	emissions.reset()
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()

	got := mustItem(t, st, "t1", row.ID)
	if got.Kind != itemKindThinking || got.Status != statusErrored || got.Summary != stopped || got.PayloadKind != itemKindThinking {
		t.Fatalf("row = %s %s %q %s, want the flip's errored thinking row", got.Kind, got.Status, got.Summary, got.PayloadKind)
	}
	if events := itemStreamEventsFor(t, emissions, row.ID); len(events) != 0 {
		t.Fatalf("the settle pushed a row it did not write: %+v", events)
	}
}

// A top-level narration block that arrives complete is written as
// assistant text under the thinking id it would have had, and streams its
// wire projection as a recovered text block does.
func TestRecoveredNarrationBlockIsWrittenAsAssistantText(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	echoCodeSpans(t, router)
	text := narrationText(t)
	recover := func(itemID, content string, meta json.RawMessage) {
		t.Helper()
		handleOrFatal(t, router, provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: itemID,
			Content: content, ContentPresent: true, Meta: meta, Timestamp: time.Now(),
		})
		router.WaitForPendingSettles()
	}
	handleOrFatal(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()})
	recover("msg_retry#0", text, narrationStopMeta)

	items := mustListItems(t, st, "t1")
	if len(items) != 1 {
		t.Fatalf("rows = %+v, want one", items)
	}
	row := items[0]
	meta := assertNarrationRow(t, "stored row", row, text)
	if row.ID != ThinkingItemID(0, "", 0) || row.PayloadID != ThinkingPayloadID(row.ID) {
		t.Fatalf("row ids = %s / %s, want the thinking ids", row.ID, row.PayloadID)
	}
	if meta.ProviderItemID != "msg_retry#0" || len(meta.PathRefs) != 1 || meta.CodeSpans["got"] != text {
		t.Fatalf("meta = %s", row.Meta)
	}
	events := itemStreamEventsFor(t, emissions, row.ID)
	if len(events) != 3 {
		t.Fatalf("got %d item events, want upsert, delta, patch: %+v", len(events), events)
	}
	upsert, delta, patch := events[0], events[1], events[2]
	if upsert.Action != itemStreamActionUpsert || upsert.Item.Kind != itemKindAssistantText ||
		upsert.Item.Status != statusStreaming || upsert.Item.Summary != "" || !decodeNarrationMeta(t, upsert.Item.Meta).Narration {
		t.Fatalf("first event = %+v, want a blank streaming assistant text upsert carrying the narration key", upsert)
	}
	if delta.Action != itemStreamActionDelta || delta.Kind != itemKindAssistantText || delta.Delta != text {
		t.Fatalf("second event = %+v, want the whole text as an assistant text delta", delta)
	}
	if patch.Action != itemStreamActionPatch || patch.Patch.Status == nil || *patch.Patch.Status != statusCompleted ||
		patch.Patch.Summary == nil || *patch.Patch.Summary != text {
		t.Fatalf("third event = %+v, want the completed patch with the whole text", patch)
	}

	// The same block again, marked or not, changes nothing.
	emissions.reset()
	recover("msg_retry#0", text, narrationStopMeta)
	recover("msg_retry#0", text, json.RawMessage(`{"index":0,"blockType":"thinking"}`))
	if again := mustListItems(t, st, "t1"); len(again) != 1 || again[0].Rev != row.Rev || again[0].Kind != itemKindAssistantText {
		t.Fatalf("a repeat of the block rewrote history: %+v", again)
	}
	if events := emissions.snapshot(); len(events) != 0 {
		t.Fatalf("a repeat of the block emitted %d events", len(events))
	}
	assertNoErrorRows(t, st)

	// The narration row took a thinking block index.
	recover("msg_retry#1", "plain reasoning", json.RawMessage(`{"index":1,"blockType":"thinking"}`))
	if next := firstItemByKind(t, st, "t1", itemKindThinking); next.ID != ThinkingItemID(0, "", 1) {
		t.Fatalf("the next thinking block is %s, want %s", next.ID, ThinkingItemID(0, "", 1))
	}

	// Different text under the same block is a history conflict.
	recover("msg_retry#0", text+" changed", narrationStopMeta)
	if got := mustItem(t, st, "t1", row.ID); got.Summary != text {
		t.Fatalf("a conflicting repeat replaced the row's text")
	}
	conflicts := 0
	for _, item := range mustListItems(t, st, "t1") {
		if strings.Contains(item.Summary, "attempted to replace completed thinking item "+row.ID) {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("a conflicting repeat reported %d conflicts, want 1", conflicts)
	}
}

// A subagent's narration block arrives complete and is one settled
// assistant text upsert under its parent. A repeat of it, marked or not,
// names the same row: an unmarked repeat shares the row's scoped id and
// would otherwise overwrite it with a thinking row.
func TestScopedNarrationBlockSurvivesItsRepeat(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	recover := func(meta json.RawMessage) {
		t.Helper()
		handleOrFatal(t, router, provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "tu-a", ItemID: "msg_sub#0",
			Content: text, ContentPresent: true, Meta: meta, Timestamp: time.Now(),
		})
		router.WaitForPendingSettles()
	}
	emissions.reset()
	recover(narrationStopMeta)

	wantID := scopedStreamItemID("think", 1, "tu-a", "msg_sub#0", "")
	row := mustItem(t, st, "t1", wantID)
	assertNarrationRow(t, "stored row", row, text)
	if row.ParentID != "tu-a" {
		t.Fatalf("row parent = %q, want the agent", row.ParentID)
	}
	events := itemStreamEventsFor(t, emissions, wantID)
	if len(events) != 1 || events[0].Action != itemStreamActionUpsert {
		t.Fatalf("a scoped block pushed %+v, want one upsert", events)
	}
	assertNarrationRow(t, "pushed row", *events[0].Item, text)

	emissions.reset()
	recover(narrationStopMeta)
	recover(json.RawMessage(`{"index":0,"blockType":"thinking"}`))
	if got := mustItem(t, st, "t1", wantID); got.Rev != row.Rev || got.Kind != itemKindAssistantText || got.Summary != text {
		t.Fatalf("a repeat of the block rewrote the row: %+v", got)
	}
	if rows := findItemsByKind(t, st, "t1", itemKindThinking); len(rows) != 0 {
		t.Fatalf("a repeat of the block wrote a thinking row: %+v", rows)
	}
	if events := itemStreamEventsFor(t, emissions, wantID); len(events) != 0 {
		t.Fatalf("a repeat of the block pushed the row: %+v", events)
	}
	assertNoErrorRows(t, st)
}

// A transcript snapshot of a narration block is written as assistant
// text, and its re-assert names the same row.
func TestSnapshotNarrationBlockIsWrittenAsAssistantText(t *testing.T) {
	router, st, _ := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	meta, err := json.Marshal(map[string]any{
		provider.MetaTranscriptSnapshotKey: true, provider.MetaNarrationKey: true, "signature": "sig",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := provider.ProviderEvent{
		Kind: provider.EventThinking, ThreadID: "t1", ItemID: "mirror-think",
		Content: text, ContentPresent: true, Meta: meta, Timestamp: time.Now(),
	}
	handleOrFatal(t, router, snapshot)
	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	assertNarrationRow(t, "stored row", row, text)
	handleOrFatal(t, router, snapshot)
	if items := mustListItems(t, st, "t1"); len(items) != 1 || items[0].Rev != row.Rev {
		t.Fatalf("the snapshot's re-assert rewrote history: %+v", items)
	}
	if router.hasActiveStreamingItem("t1") {
		t.Fatal("a snapshot opened streaming state")
	}
	assertNoErrorRows(t, st)
}

// A streamed block that names its provider item settles with the stop's
// own content, and a repeat of that stop finds the assistant text row.
func TestNarrationStopWithContentSettlesAndRepeats(t *testing.T) {
	router, st, _ := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	streamThinking(t, router, "", "block-1", text[:120])
	stop := provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: "block-1",
		Content: text, ContentPresent: true, Meta: narrationStopMeta, Timestamp: time.Now(),
	}
	handleOrFatal(t, router, stop)
	router.WaitForPendingSettles()
	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	assertNarrationRow(t, "stored row", row, text)
	data, err := st.GetPayloadData("t1", row.PayloadID)
	if err != nil || string(data) != text {
		t.Fatalf("payload data = %d bytes (%v), want the stop's content", len(data), err)
	}

	handleOrFatal(t, router, stop)
	router.WaitForPendingSettles()
	if items := mustListItems(t, st, "t1"); len(items) != 1 || items[0].Rev != row.Rev {
		t.Fatalf("a repeat of the stop rewrote history: %+v", items)
	}
	assertNoErrorRows(t, st)
}

// A narration row under an agent keeps its place in the agent's card, and
// the agent's end finds no stream to settle.
func TestScopedNarrationSettleKeepsTheCardAndTheAgentEnd(t *testing.T) {
	router, st, _ := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	agentRead(t, router, "t1", "tu-a-read", "tu-a")
	streamThinking(t, router, "tu-a", "", text)
	if n := scopeStreams(router, "t1", "tu-a"); n != 1 {
		t.Fatalf("the agent holds %d streams, want 1", n)
	}
	card := func() string {
		t.Helper()
		router.flushSubagentCards("t1")
		return mustItem(t, st, "t1", "tu-a").Meta
	}
	if before := card(); !strings.Contains(before, `"subagentDescendantCount":2`) {
		t.Fatalf("card before the settle: %s", before)
	}

	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "tu-a", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()
	texts := agentTexts(t, st, "t1", "tu-a")
	if len(texts) != 1 {
		t.Fatalf("the agent's assistant text rows: %+v", texts)
	}
	assertNarrationRow(t, "stored row", texts[0], text)
	if after := card(); !strings.Contains(after, `"subagentDescendantCount":2`) || !strings.Contains(after, `"subagentLatestChildSummary"`) {
		t.Fatalf("card after the settle: %s", after)
	}
	if n := scopeStreams(router, "t1", "tu-a"); n != 0 {
		t.Fatalf("the settle left %d streams counted", n)
	}
	streams, err := st.AgentStreams("t1", "tu-a")
	if err != nil || len(streams) != 0 {
		t.Fatalf("the agent's open streams after the settle: %+v %v", streams, err)
	}

	parkStop(t, router, "t1", "tu-a", "task-a", "Done.", "u-final")
	router.WaitForPendingSettles()
	got := mustItem(t, st, "t1", texts[0].ID)
	if got.Rev != texts[0].Rev || got.Kind != itemKindAssistantText || got.Summary != text {
		t.Fatalf("the agent's end rewrote the narration row: %+v", got)
	}
	if open := openRows(t, st, "t1"); len(open) != 0 {
		t.Fatalf("rows left open: %v", open)
	}
}

// An agent's end settles a thinking stream it still holds as thinking: no
// stop arrived, so no marker did.
func TestAgentEndSettlesAnOpenNarrationStreamAsThinking(t *testing.T) {
	router, st, _ := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	parkHandle(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1})
	parkLaunchAgent(t, router, "t1", "tu-a", "task-a", "")
	streamThinking(t, router, "tu-a", "", text)
	parkStop(t, router, "t1", "tu-a", "task-a", "Done.", "u-final")
	router.WaitForPendingSettles()
	row := firstItemByKind(t, st, "t1", itemKindThinking)
	if row.Status != statusCompleted || row.Summary != ThinkingSummaryPreview(text) {
		t.Fatalf("the agent's end left the stream %s %q", row.Status, row.Summary)
	}
	if n := scopeStreams(router, "t1", "tu-a"); n != 0 {
		t.Fatalf("the agent's end left %d streams counted", n)
	}

	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: "tu-a", Timestamp: time.Now(), Meta: narrationStopMeta,
	})
	router.WaitForPendingSettles()
	if got := mustItem(t, st, "t1", row.ID); got.Kind != itemKindThinking || got.Rev != row.Rev {
		t.Fatalf("a stop after the agent's end rewrote the row: %+v", got)
	}
}

// The summarizer's reasoning is never prose for the user: its stop does
// not read the marker, and the row settle converts thinking rows only.
func TestCompactionReasoningIgnoresTheNarrationMarker(t *testing.T) {
	text := narrationText(t)
	assertReasoning := func(t *testing.T, st *store.Store) {
		t.Helper()
		items := mustListItems(t, st, "t1")
		if len(items) != 1 || items[0].Kind != itemKindCompactionReasoning || items[0].Status != statusCompleted ||
			items[0].Summary != ThinkingSummaryPreview(text) || strings.Contains(items[0].Meta, "narration") {
			t.Fatalf("compaction reasoning rows = %+v", items)
		}
	}
	t.Run("stop", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		narrationThread(t, st)
		streamThinking(t, router, provider.CompactionReasoningScope, "", text)
		handleOrFatal(t, router, provider.ProviderEvent{
			Kind: provider.EventContentBlockStop, ThreadID: "t1", ParentToolUseID: provider.CompactionReasoningScope,
			Timestamp: time.Now(), Meta: narrationStopMeta,
		})
		router.WaitForPendingSettles()
		assertReasoning(t, st)
	})
	t.Run("row settle", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		narrationThread(t, st)
		streamThinking(t, router, provider.CompactionReasoningScope, "", text)
		row := firstItemByKind(t, st, "t1", itemKindCompactionReasoning)
		if err := router.settleStreamingThinkingRow("t1", row.ID, statusCompleted, interruptedSummary, "", false, true); err != nil {
			t.Fatalf("row settle: %v", err)
		}
		assertReasoning(t, st)
	})
}

// A narration row keeps its response stamp, so the record of a response
// the CLI abandoned names it and its page read carries the record.
func TestAbandonedResponseMarksItsNarrationRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	narrationThread(t, st)
	text := narrationText(t)
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStart, ThreadID: "t1", Timestamp: time.Now(),
		Meta: json.RawMessage(`{"index":0,"blockType":"thinking","apiMessageId":"msg_01"}`),
	})
	streamThinking(t, router, "", "", text)
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", Timestamp: time.Now(),
		Meta: json.RawMessage(`{"index":0,"blockType":"thinking","apiMessageId":"msg_01","narration":true}`),
	})
	router.WaitForPendingSettles()
	router.DrainWireItemRefresh()
	row := firstItemByKind(t, st, "t1", itemKindAssistantText)
	if !store.ItemReadIsDecorated(row) {
		t.Fatalf("the narration row lost its response stamp: %s", row.Meta)
	}

	abandonBlocks(t, router, "t1", "", "msg_01", 0)
	notes := abandonedNotifications(t, st, "t1")
	if len(notes) != 1 {
		t.Fatalf("abandoned notifications: %+v", notes)
	}
	emissions.reset()
	router.DrainWireItemRefresh()
	var pushed *store.Item
	for _, evt := range itemStreamEventsFor(t, emissions, row.ID) {
		if evt.Action == itemStreamActionUpsert {
			pushed = evt.Item
		}
	}
	if pushed == nil || pushed.DiscardedBy != notes[0].ID || pushed.Kind != itemKindAssistantText {
		t.Fatalf("the refresh pushed %+v, want the narration row marked by %s", pushed, notes[0].ID)
	}
}
