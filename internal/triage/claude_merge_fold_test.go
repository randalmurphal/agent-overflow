package triage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// Fold tests for the Claude queue-boundary merge. The wire facts they encode
// come from claude-wire.md §Queued-message consumption: a boundary drain of
// N>=2 consecutive prompt commands emits one merged-away echo per member
// carrying that member's OWN blocks, then acknowledges the batch under the
// LAST member's uuid with every member's blocks concatenated.
//
// Every envelope here is hand-written rather than produced by a CLI, and the
// digests come from the real outbound block builder, so the tests pin the
// comparison AO actually makes.

// mergeFoldSend registers one quiet flush send exactly as the dispatcher does:
// a persisted row plus a pending entry carrying the uuid the echo will name
// and the fingerprints of the blocks that went on the wire.
func mergeFoldSend(
	t *testing.T, router *Router, threadID, itemID, uuid, text string,
	attachments []provider.ImageAttachment, meta usermessage.Meta,
	turnIndex, responseTurn int,
) store.Item {
	t.Helper()
	metaJSON, err := usermessage.MarshalMeta(meta)
	if err != nil {
		t.Fatalf("marshal row meta: %v", err)
	}
	digest, err := claude.UserMessageBlockDigest(text, attachments, false)
	if err != nil {
		t.Fatalf("block digest for %s: %v", itemID, err)
	}
	now := time.Now().UnixMilli()
	item := store.Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   text,
		Meta:      metaJSON,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := router.PersistAndRegisterPendingQuietFlushSendWithExpectation(
		threadID, "queue:"+itemID, item, responseTurn, now,
		PendingSendExpectation{ProviderItemID: uuid, ContentBlockDigest: digest},
	); err != nil {
		t.Fatalf("register quiet flush send %s: %v", itemID, err)
	}
	return item
}

// mergeEcho builds the `EventUserText` a replayed Claude envelope produces for
// the given content blocks, by running the real parser over a real envelope.
// The digest in its meta is therefore whatever the parser would report for
// that wire line, not something the test asserts into existence.
func mergeEcho(t *testing.T, threadID, uuid string, blocks []map[string]any, withTimestamp bool) provider.ProviderEvent {
	t.Helper()
	envelope := map[string]any{
		"type":     "user",
		"isReplay": true,
		"uuid":     uuid,
		"message":  map[string]any{"role": "user", "content": blocks},
	}
	if withTimestamp {
		envelope["timestamp"] = "2026-09-16T12:00:00.000Z"
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal echo envelope: %v", err)
	}
	events, err := claude.NewParser().ParseLine(threadID, line)
	if err != nil {
		t.Fatalf("parse echo envelope: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventUserText {
		t.Fatalf("echo envelope produced %d events (%+v), want one EventUserText", len(events), events)
	}
	return events[0]
}

func textBlocks(texts ...string) []map[string]any {
	blocks := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	return blocks
}

func imageBlock(attachment provider.ImageAttachment) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": attachment.MimeType,
			"data":       base64.StdEncoding.EncodeToString(attachment.Data),
		},
	}
}

func rowSummary(t *testing.T, st *store.Store, threadID, itemID string) (store.Item, bool) {
	t.Helper()
	item, found, err := st.GetThreadItem(threadID, itemID)
	if err != nil {
		t.Fatalf("read %s/%s: %v", threadID, itemID, err)
	}
	return item, found
}

func itemStreamEvents(emissions *emissionLog) []ItemStreamEvent {
	var out []ItemStreamEvent
	for _, e := range emissions.snapshot() {
		if e.eventName != "provider:item_event" {
			continue
		}
		if evt, ok := e.data.(ItemStreamEvent); ok {
			out = append(out, evt)
		}
	}
	return out
}

// TestClaudeMergeFold_TwoMessagesBecomeOneRow is the core case: A is flushed
// at one drain and B at a later one, the CLI has not consumed A when B's drain
// reaches the turn boundary, so it merges them. A's echo carries A's own
// blocks; B's carries A's THEN B's. AO must end with one row holding both
// texts, one anchor under B's uuid, and both send ids on the row.
func TestClaudeMergeFold_TwoMessagesBecomeOneRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	rowA := mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", "first queued", nil,
		usermessage.Meta{SendID: "send-a"}, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "second queued", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)

	var anchored []store.Item
	router.SetFlushUserTextConfirmedHook(func(threadID string, item store.Item) {
		anchored = append(anchored, item)
		if err := st.UpsertMessageAnchor(store.MessageAnchor{
			ThreadID: threadID, UserItemID: item.ID, TurnIndex: item.TurnIndex,
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Errorf("anchor %s: %v", item.ID, err)
		}
	})

	// Merged-away echo: A's own blocks, no timestamp.
	if err := router.Handle(mergeEcho(t, "t1", "uuid-a", textBlocks("first queued"), false)); err != nil {
		t.Fatalf("echo A: %v", err)
	}
	// Survivor ack: the concatenated blocks, with a timestamp.
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("first queued", "second queued"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}

	if _, found := rowSummary(t, st, "t1", rowA.ID); found {
		t.Errorf("row %s survived the fold — the transcript has no entry under its uuid", rowA.ID)
	}
	survivor, found := rowSummary(t, st, "t1", rowB.ID)
	if !found {
		t.Fatalf("survivor row %s is gone", rowB.ID)
	}
	want := "first queued" + usermessage.JoinSeparator + "second queued"
	if survivor.Summary != want {
		t.Errorf("survivor summary:\n got %q\nwant %q", survivor.Summary, want)
	}

	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	if !slices.Equal(meta.JoinedSendIDs, []string{"send-a", "send-b"}) {
		t.Errorf("joinedSendIds: got %v, want [send-a send-b]", meta.JoinedSendIDs)
	}
	if meta.SendID != "send-a" {
		t.Errorf("sendId: got %q, want the first member send-a", meta.SendID)
	}
	if got := usermessage.ReadProviderItemID(survivor.Meta); got != "uuid-b" {
		t.Errorf("survivor provider_item_id: got %q, want uuid-b (the uuid the transcript carries)", got)
	}

	// Exactly one anchor, on the survivor.
	anchors, err := st.ListMessageAnchors("t1")
	if err != nil {
		t.Fatalf("ListMessageAnchors: %v", err)
	}
	if len(anchors) != 1 || anchors[0].UserItemID != rowB.ID {
		t.Fatalf("anchors after fold: %+v, want exactly one on %s", anchors, rowB.ID)
	}
	if len(anchored) == 0 {
		t.Error("the confirmed hook never ran, so no anchor would have been recorded in production")
	}

	// The frontend gets the survivor's new content and the removal of the
	// folded row, so a client can never show the text twice.
	var sawSurvivorJoin, sawRemoval bool
	for _, evt := range itemStreamEvents(emissions) {
		if evt.Action == itemStreamActionUpsert && evt.Item != nil &&
			evt.Item.ID == rowB.ID && strings.Contains(evt.Item.Summary, "first queued") {
			sawSurvivorJoin = true
		}
		if evt.Action == itemStreamActionRemove && evt.ItemID == rowA.ID {
			sawRemoval = true
		}
	}
	if !sawSurvivorJoin {
		t.Error("no upsert carried the joined survivor row")
	}
	if !sawRemoval {
		t.Errorf("no remove event for the folded row %s — its Zone 2 marker would never clear", rowA.ID)
	}
}

// foldTwo folds two quiet flush sends into one row as the CLI's boundary
// merge does and returns the survivor's meta.
func foldTwo(t *testing.T, textA, textB string, metaA, metaB usermessage.Meta) usermessage.Meta {
	t.Helper()
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", textA, nil, metaA, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", textB, nil, metaB, 3, 5)
	if err := router.Handle(mergeEcho(t, "t1", "uuid-a", textBlocks(textA), false)); err != nil {
		t.Fatalf("echo A: %v", err)
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks(textA, textB), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}
	survivor, found := rowSummary(t, st, "t1", rowB.ID)
	if !found {
		t.Fatalf("survivor row %s is gone", rowB.ID)
	}
	if want := textA + usermessage.JoinSeparator + textB; survivor.Summary != want {
		t.Fatalf("survivor summary:\n got %q\nwant %q", survivor.Summary, want)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	return meta
}

// A row the CLI folded from an agent's request and the person's message
// records each member's part with its origin, so a Stop or a session end can
// give the person back only their text.
func TestClaudeMergeFold_RecordsEachMembersPartAndOrigin(t *testing.T) {
	request := "Fix the nil check."
	person := "and also update the docs"
	origin := &usermessage.OriginThread{ThreadID: "lead", Title: "Lead", Token: "tok-1", Kind: "send"}
	meta := foldTwo(t, request, person,
		usermessage.Meta{SendID: "thread-request:tok-1", Origin: usermessage.OriginAgentThread, OriginThread: origin},
		usermessage.Meta{SendID: "send-b"})

	want := []usermessage.JoinedPart{
		{SendID: "thread-request:tok-1", Bytes: len(request), Origin: usermessage.OriginAgentThread, OriginThread: origin},
		{SendID: "send-b", Bytes: len(person)},
	}
	if !reflect.DeepEqual(meta.JoinedParts, want) {
		t.Fatalf("joinedParts:\n got %+v\nwant %+v", meta.JoinedParts, want)
	}
	got, agents, ok := usermessage.SplitPersonParts(request+usermessage.JoinSeparator+person, meta)
	if !ok || got != person || len(agents) != 1 {
		t.Fatalf("split: person %q, %d agent parts, ok %v; want %q, 1, true", got, len(agents), ok, person)
	}
}

// A member that is itself a joined row whose recorded parts do not describe
// its text leaves the fold with no parts, so nothing splits it by lengths
// that are wrong.
func TestClaudeMergeFold_RecordsNoPartsAMemberCannotSupport(t *testing.T) {
	stale := usermessage.Meta{
		SendID:        "send-a",
		JoinedSendIDs: []string{"send-a", "send-c"},
		JoinedParts:   []usermessage.JoinedPart{{SendID: "send-a", Bytes: 3}, {SendID: "send-c", Bytes: 99}},
	}
	meta := foldTwo(t, "one"+usermessage.JoinSeparator+"two", "three", stale, usermessage.Meta{SendID: "send-b"})
	if meta.JoinedParts != nil {
		t.Fatalf("joinedParts: got %+v, want none", meta.JoinedParts)
	}
}

// TestClaudeMergeFold_RenumbersImageMarkers pins the attachment half. A
// carries an image at its own `[Image #1]`; after the fold the combined row
// must address A's image as #1 and B's as #2, in that attachment order.
func TestClaudeMergeFold_RenumbersImageMarkers(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	imageA := provider.ImageAttachment{ID: "att-a", MimeType: "image/png", Data: []byte("alpha-bytes")}
	imageB := provider.ImageAttachment{ID: "att-b", MimeType: "image/jpeg", Data: []byte("beta-bytes")}
	metaA := usermessage.Meta{
		SendID:      "send-a",
		Attachments: []usermessage.AttachmentMeta{{ID: "att-a", ThreadID: "t1", MimeType: "image/png", Kind: store.AttachmentKindImage}},
	}
	metaB := usermessage.Meta{
		SendID:      "send-b",
		Attachments: []usermessage.AttachmentMeta{{ID: "att-b", ThreadID: "t1", MimeType: "image/jpeg", Kind: store.AttachmentKindImage}},
	}

	textA := "look at [Image #1] please"
	textB := "and [Image #1] too"
	rowA := mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", textA,
		[]provider.ImageAttachment{imageA}, metaA, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", textB,
		[]provider.ImageAttachment{imageB}, metaB, 3, 5)

	// The blocks the CLI echoes back are the ones Send wrote: the marker is
	// replaced by an inline base64 image between the surrounding text runs.
	blocksA := []map[string]any{
		{"type": "text", "text": "look at "},
		imageBlock(imageA),
		{"type": "text", "text": " please"},
	}
	blocksB := []map[string]any{
		{"type": "text", "text": "and "},
		imageBlock(imageB),
		{"type": "text", "text": " too"},
	}

	if err := router.Handle(mergeEcho(t, "t1", "uuid-a", blocksA, false)); err != nil {
		t.Fatalf("echo A: %v", err)
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", append(append([]map[string]any{}, blocksA...), blocksB...), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}

	if _, found := rowSummary(t, st, "t1", rowA.ID); found {
		t.Errorf("row %s survived the fold", rowA.ID)
	}
	survivor, found := rowSummary(t, st, "t1", rowB.ID)
	if !found {
		t.Fatalf("survivor row %s is gone", rowB.ID)
	}
	want := "look at [Image #1] please" + usermessage.JoinSeparator + "and [Image #2] too"
	if survivor.Summary != want {
		t.Errorf("survivor summary:\n got %q\nwant %q", survivor.Summary, want)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	if len(meta.Attachments) != 2 || meta.Attachments[0].ID != "att-a" || meta.Attachments[1].ID != "att-b" {
		t.Errorf("attachments: got %+v, want att-a then att-b so the renumbered markers index them", meta.Attachments)
	}
}

// TestClaudeMergeFold_ThreeMessages pins the N>2 decomposition: the walk back
// from the survivor must consume the two preceding sends and land exactly on
// an empty prefix.
func TestClaudeMergeFold_ThreeMessages(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	rowA := mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", "one", nil,
		usermessage.Meta{SendID: "send-a"}, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "two", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)
	rowC := mergeFoldSend(t, router, "t1", "user:3:flush:3", "uuid-c", "three", nil,
		usermessage.Meta{SendID: "send-c"}, 3, 6)

	for _, echo := range []struct {
		uuid string
		text string
	}{{"uuid-a", "one"}, {"uuid-b", "two"}} {
		if err := router.Handle(mergeEcho(t, "t1", echo.uuid, textBlocks(echo.text), false)); err != nil {
			t.Fatalf("merged-away echo %s: %v", echo.uuid, err)
		}
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-c", textBlocks("one", "two", "three"), true)); err != nil {
		t.Fatalf("survivor echo: %v", err)
	}

	for _, gone := range []store.Item{rowA, rowB} {
		if _, found := rowSummary(t, st, "t1", gone.ID); found {
			t.Errorf("row %s survived a three-way fold", gone.ID)
		}
	}
	survivor, found := rowSummary(t, st, "t1", rowC.ID)
	if !found {
		t.Fatalf("survivor row %s is gone", rowC.ID)
	}
	want := strings.Join([]string{"one", "two", "three"}, usermessage.JoinSeparator)
	if survivor.Summary != want {
		t.Errorf("survivor summary:\n got %q\nwant %q", survivor.Summary, want)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	if !slices.Equal(meta.JoinedSendIDs, []string{"send-a", "send-b", "send-c"}) {
		t.Errorf("joinedSendIds: got %v, want all three in queue order", meta.JoinedSendIDs)
	}
}

// TestClaudeMergeFold_UnexplainedPrefixLeavesRowsAlone is the refusal. An echo
// longer than the send it names, whose extra leading blocks are NOT what AO
// wrote for the preceding message, is content AO cannot account for. Guessing
// would fold the wrong text into the wrong row, so nothing is folded and the
// rows stay exactly as dispatched — the loud, pre-existing behaviour.
func TestClaudeMergeFold_UnexplainedPrefixLeavesRowsAlone(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	rowA := mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", "first queued", nil,
		usermessage.Meta{SendID: "send-a"}, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "second queued", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)

	if err := router.Handle(mergeEcho(t, "t1", "uuid-a", textBlocks("first queued"), false)); err != nil {
		t.Fatalf("echo A: %v", err)
	}
	// The extra leading block is not A's text. Nothing in the registry
	// explains it.
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b",
		textBlocks("something the app never sent", "second queued"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}

	a, found := rowSummary(t, st, "t1", rowA.ID)
	if !found {
		t.Fatalf("row %s was removed by a fold that should not have happened", rowA.ID)
	}
	if a.Summary != "first queued" {
		t.Errorf("row A summary changed to %q", a.Summary)
	}
	b, found := rowSummary(t, st, "t1", rowB.ID)
	if !found {
		t.Fatalf("row %s is gone", rowB.ID)
	}
	if b.Summary != "second queued" {
		t.Errorf("row B summary:\n got %q\nwant the message AO dispatched, unjoined", b.Summary)
	}
	for _, evt := range itemStreamEvents(emissions) {
		if evt.Action == itemStreamActionRemove {
			t.Errorf("an unexplained echo removed row %s", evt.ItemID)
		}
	}
}

// TestClaudeMergeFold_OrdinaryEchoIsUntouched guards the common path: an echo
// carrying exactly the blocks AO sent must not go anywhere near the fold, and
// two separate messages consumed separately stay two rows.
func TestClaudeMergeFold_OrdinaryEchoIsUntouched(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	rowA := mergeFoldSend(t, router, "t1", "user:3:flush:1", "uuid-a", "first queued", nil,
		usermessage.Meta{SendID: "send-a"}, 3, 4)
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "second queued", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)

	if err := router.Handle(mergeEcho(t, "t1", "uuid-a", textBlocks("first queued"), true)); err != nil {
		t.Fatalf("echo A: %v", err)
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("second queued"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}

	for _, want := range []struct {
		item    store.Item
		summary string
	}{{rowA, "first queued"}, {rowB, "second queued"}} {
		got, found := rowSummary(t, st, "t1", want.item.ID)
		if !found {
			t.Fatalf("row %s is gone — an unmerged pair must stay two rows", want.item.ID)
		}
		if got.Summary != want.summary {
			t.Errorf("row %s summary: got %q, want %q", want.item.ID, got.Summary, want.summary)
		}
	}
	for _, evt := range itemStreamEvents(emissions) {
		if evt.Action == itemStreamActionRemove {
			t.Errorf("ordinary echoes removed row %s", evt.ItemID)
		}
	}
}

// TestClaudeMergeFold_DeferredMemberHasNoRowToDelete covers the member whose
// own echo had not landed yet, so its row was still deferred and exists
// nowhere but the pending entry. There is nothing to delete, but its text and
// send id still belong on the survivor and its send-queue marker still has to
// clear, which only the removal event does.
func TestClaudeMergeFold_DeferredMemberHasNoRowToDelete(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")

	digestA, err := claude.UserMessageBlockDigest("first queued", nil, false)
	if err != nil {
		t.Fatalf("block digest for A: %v", err)
	}
	metaA, err := usermessage.MarshalMeta(usermessage.Meta{SendID: "send-a"})
	if err != nil {
		t.Fatalf("marshal A meta: %v", err)
	}
	now := time.Now().UnixMilli()
	deferredA := store.Item{
		ID: "user:3:flush:1", ThreadID: "t1", TurnIndex: 4,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "first queued", Meta: metaA, CreatedAt: now, UpdatedAt: now,
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", deferredA, now,
		PendingSendExpectation{ProviderItemID: "uuid-a", ContentBlockDigest: digestA})

	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "second queued", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)

	// Only the survivor's ack arrives; A's merged-away echo is still in
	// flight, so its row was never written.
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("first queued", "second queued"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}

	if _, found := rowSummary(t, st, "t1", deferredA.ID); found {
		t.Errorf("deferred member %s was persisted by the fold; it has no transcript entry", deferredA.ID)
	}
	survivor, found := rowSummary(t, st, "t1", rowB.ID)
	if !found {
		t.Fatalf("survivor row %s is gone", rowB.ID)
	}
	want := "first queued" + usermessage.JoinSeparator + "second queued"
	if survivor.Summary != want {
		t.Errorf("survivor summary:\n got %q\nwant %q", survivor.Summary, want)
	}
	meta, err := usermessage.FromItem(survivor)
	if err != nil {
		t.Fatalf("decode survivor meta: %v", err)
	}
	if !slices.Equal(meta.JoinedSendIDs, []string{"send-a", "send-b"}) {
		t.Errorf("joinedSendIds: got %v, want [send-a send-b]", meta.JoinedSendIDs)
	}

	var sawRemoval bool
	for _, evt := range itemStreamEvents(emissions) {
		if evt.Action == itemStreamActionRemove && evt.ItemID == deferredA.ID {
			sawRemoval = true
		}
	}
	if !sawRemoval {
		t.Errorf("no remove event for the deferred member %s — its send-queue marker would never clear", deferredA.ID)
	}
	// The pending entry is consumed, so A's own echo cannot arrive later and
	// stamp a row this fold already absorbed.
	if _, ok := router.peekPendingSendByItemID("t1", deferredA.ID); ok {
		t.Errorf("pending entry for %s survived the fold", deferredA.ID)
	}
}

// TestFlushSendDigestLedgerIsBounded pins the retention rule: the ledger holds
// a fixed window of recent flush sends, so a long-lived thread cannot grow it.
func TestFlushSendDigestLedgerIsBounded(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for i := 0; i < flushSendDigestHistory*3; i++ {
		router.RegisterPendingFlushResendWithExpectation("t1", "user:0:flush:"+string(rune('a'+i)), 0,
			PendingSendExpectation{ContentBlockDigest: []string{"t:" + string(rune('a'+i))}})
	}
	router.mu.Lock()
	got := len(router.threadStateIfPresent("t1").recentFlushDigests)
	router.mu.Unlock()
	if got != flushSendDigestHistory {
		t.Errorf("ledger length: got %d, want the %d-entry bound", got, flushSendDigestHistory)
	}
}

// A merge that absorbs a member whose own echo had not arrived consumes that
// member (settleFoldedMergeState: "the message is provably in provider
// context"). An answer or completion carried by that member must reach the
// consumed hook, or its owner's delivery stays unconfirmed and the next boot
// gives back a message the agent already holds.
func TestAMergeFoldedMemberIsReportedConsumed(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	var consumed []string
	router.SetFlushSendConsumedHook(func(_ string, sendIDs []string) {
		consumed = append(consumed, sendIDs...)
	})

	digestA, err := claude.UserMessageBlockDigest("the completion", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	metaA, err := usermessage.MarshalMeta(usermessage.Meta{SendID: "remote-completion:c:r"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	deferredA := store.Item{
		ID: "user:3:flush:1", ThreadID: "t1", TurnIndex: 4,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "the completion", Meta: metaA, CreatedAt: now, UpdatedAt: now,
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", deferredA, now,
		PendingSendExpectation{ProviderItemID: "uuid-a", ContentBlockDigest: digestA})
	mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "person text", nil,
		usermessage.Meta{SendID: "send-b"}, 3, 5)

	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("the completion", "person text"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}
	if _, ok := router.peekPendingSendByItemID("t1", deferredA.ID); ok {
		t.Fatalf("fixture: the fold did not consume the member")
	}
	if !slices.Contains(consumed, "remote-completion:c:r") {
		t.Fatalf("consumed hook saw %q; the folded member's completion was never confirmed", consumed)
	}
}

// A merge that absorbs several members consumes them all in one settle, so
// every member's owner must reach the consumed hook, not only the first.
// Neither member's own echo arrives first: A is still deferred and B, though
// persisted, was never echoed, so the fold is the only confirmation either
// gets.
func TestEveryMergeFoldedMemberIsReportedConsumed(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	var consumed []string
	router.SetFlushSendConsumedHook(func(_ string, sendIDs []string) {
		consumed = append(consumed, sendIDs...)
	})

	digestA, err := claude.UserMessageBlockDigest("one", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	metaA, err := usermessage.MarshalMeta(usermessage.Meta{SendID: "remote-completion:c:r"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	deferredA := store.Item{
		ID: "user:3:flush:1", ThreadID: "t1", TurnIndex: 4,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "one", Meta: metaA, CreatedAt: now, UpdatedAt: now,
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", deferredA, now,
		PendingSendExpectation{ProviderItemID: "uuid-a", ContentBlockDigest: digestA})
	rowB := mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "two", nil,
		usermessage.Meta{SendID: "thread-wake:tok"}, 3, 5)
	mergeFoldSend(t, router, "t1", "user:3:flush:3", "uuid-c", "three", nil,
		usermessage.Meta{SendID: "send-c"}, 3, 6)

	if err := router.Handle(mergeEcho(t, "t1", "uuid-c", textBlocks("one", "two", "three"), true)); err != nil {
		t.Fatalf("survivor echo: %v", err)
	}
	for _, member := range []string{deferredA.ID, rowB.ID} {
		if _, ok := router.peekPendingSendByItemID("t1", member); ok {
			t.Fatalf("fixture: the fold did not consume %s", member)
		}
	}
	for _, owner := range []string{"remote-completion:c:r", "thread-wake:tok"} {
		if !slices.Contains(consumed, owner) {
			t.Errorf("consumed hook saw %q; folded member owner %q was never confirmed", consumed, owner)
		}
	}
}

// A member a merge fold absorbs was delivered in the survivor's turn: the CLI
// ran them as one message. Its request is reported delivered there, as the
// survivor's is, or it would wait for a delivery that never comes.
func TestAMergeFoldedMemberIsDeliveredInTheSurvivorsTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	log := &deliveryLog{}
	router.SetSendDeliveredHook(log.record)

	digestA, err := claude.UserMessageBlockDigest("check the changelog", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	metaA, err := usermessage.MarshalMeta(usermessage.Meta{SendID: "thread-request:tok-a"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	deferredA := store.Item{
		ID: "user:3:flush:1", ThreadID: "t1", TurnIndex: 4,
		Kind: "user_text", Role: "user", Status: "completed",
		Summary: "check the changelog", Meta: metaA, CreatedAt: now, UpdatedAt: now,
	}
	router.RegisterPendingFlushSendWithExpectation("t1", "queue:1", deferredA, now,
		PendingSendExpectation{ProviderItemID: "uuid-a", ContentBlockDigest: digestA})
	mergeFoldSend(t, router, "t1", "user:3:flush:2", "uuid-b", "and the notes", nil,
		usermessage.Meta{SendID: "thread-request:tok-b"}, 3, 5)

	// The merged turn opens, then the survivor's echo carries both.
	if err := router.Handle(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", Timestamp: time.Now()}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	open, ok := router.openTurnIndex("t1")
	if !ok {
		t.Fatal("fixture: no turn open")
	}
	if err := router.Handle(mergeEcho(t, "t1", "uuid-b", textBlocks("check the changelog", "and the notes"), true)); err != nil {
		t.Fatalf("echo B: %v", err)
	}
	if _, ok := router.peekPendingSendByItemID("t1", deferredA.ID); ok {
		t.Fatal("fixture: the fold did not consume the member")
	}
	turns := map[string]int{}
	for _, delivery := range log.snapshot() {
		for _, id := range delivery.SendIDs {
			turns[id] = delivery.TurnIndex
		}
	}
	for _, id := range []string{"thread-request:tok-a", "thread-request:tok-b"} {
		if turn, ok := turns[id]; !ok || turn != open {
			t.Errorf("delivery of %s = %d (reported %v), want the merged turn %d; all: %+v", id, turn, ok, open, log.snapshot())
		}
	}
}

// A deferred member whose merged-away echo arrives before the survivor's turn
// opens: A's echo, `started` for A and B, then B's echo carrying both, with
// the init that opens the merged turn before B's echo (the documented order)
// or none, when B's echo opens it. A's echo opens turn 1 for A and delivers
// it there; turn 2 runs the merged message. The fold leaves turn 1 with no
// rows, so A's delivery moves to turn 2 and turn 1 is ended, not left open,
// whether or not the opening of turn 2 already completed it.
func TestAMergeFoldedMemberEchoedIntoATurnOfItsOwnMovesToTheSurvivorsTurn(t *testing.T) {
	for _, order := range []string{"init", "no init"} {
		t.Run(order, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			log := &deliveryLog{}
			router.SetSendDeliveredHook(log.record)
			handle := func(evt provider.ProviderEvent) {
				t.Helper()
				evt.ThreadID = "t1"
				evt.Timestamp = time.Now()
				if err := router.Handle(evt); err != nil {
					t.Fatalf("%s: %v", evt.Kind, err)
				}
			}
			handle(provider.ProviderEvent{Kind: provider.EventTurnStart})
			handle(provider.ProviderEvent{Kind: provider.EventTextDelta, Content: "turn 0 response"})
			handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: normalTurnCompleteMeta()})
			for i, send := range []struct{ uuid, text, sendID string }{
				{"uuid-a", "check the changelog", "thread-request:tok-a"},
				{"uuid-b", "and the notes", "thread-request:tok-b"},
			} {
				turn := i + 1
				digest, err := claude.UserMessageBlockDigest(send.text, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				meta, err := usermessage.MarshalMeta(usermessage.Meta{SendID: send.sendID})
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UnixMilli()
				router.RegisterPendingFlushSendWithExpectation("t1", fmt.Sprintf("queue:%d", turn), store.Item{
					ID: fmt.Sprintf("user:%d:flush:%d", turn, turn), ThreadID: "t1", TurnIndex: turn,
					Kind: "user_text", Role: "user", Status: "completed", Summary: send.text, Meta: meta, CreatedAt: now, UpdatedAt: now,
				}, now, PendingSendExpectation{ProviderItemID: send.uuid, ContentBlockDigest: digest})
			}

			handle(mergeEcho(t, "t1", "uuid-a", textBlocks("check the changelog"), false))
			handle(commandLifecycleEvent("t1", "uuid-a", provider.CommandStarted))
			handle(commandLifecycleEvent("t1", "uuid-b", provider.CommandStarted))
			if order == "init" {
				handle(provider.ProviderEvent{Kind: provider.EventInit})
			}
			handle(mergeEcho(t, "t1", "uuid-b", textBlocks("check the changelog", "and the notes"), true))

			var moved *SendDelivery
			turns := map[string][]int{}
			for _, delivery := range log.snapshot() {
				for _, id := range delivery.SendIDs {
					turns[id] = append(turns[id], delivery.TurnIndex)
				}
				if delivery.Rebind {
					moved = &delivery
				}
			}
			if !slices.Equal(turns["thread-request:tok-a"], []int{1, 2}) || !slices.Equal(turns["thread-request:tok-b"], []int{2}) {
				t.Fatalf("deliveries = %+v, want A into turn 1 then moved to 2, B into 2", log.snapshot())
			}
			if moved == nil || moved.ReboundFrom != 1 || moved.TurnIndex != 2 || !moved.OwnTurn || moved.UserItemID != "user:2:flush:2" {
				t.Fatalf("moved delivery = %+v, want A's moved from turn 1 to 2, its own, under the survivor's row", moved)
			}
			turn, found, err := st.GetTurnByThreadIndex("t1", 1)
			if err != nil || !found || turn.CompletedAt == nil {
				t.Fatalf("turn 1 = %+v found=%v err=%v, want it ended", turn, found, err)
			}
			active, found, err := st.GetActiveTurn("t1")
			if err != nil || !found || active.TurnIndex != 2 {
				t.Fatalf("active turn = %+v found=%v err=%v, want turn 2 alone", active, found, err)
			}
		})
	}
}
