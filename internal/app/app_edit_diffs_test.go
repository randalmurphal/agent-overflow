package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/highlight"
	"agent-overflow/internal/store"
)

// editDiffFixture seeds a thread with two turns of edit tool calls:
// turn 1 has one edit; turn 2 has two edits touching the same file
// plus a non-diff tool_result payload (empty data) that must be
// excluded from the edits list.
func editDiffFixture(t *testing.T, app *App) string {
	t.Helper()
	thread := testThread("thread-edit-diffs")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()

	seedItem := func(id string, turnIndex, itemIndex int, kind, role, summary, payloadID string, payload *store.Payload) {
		t.Helper()
		item := store.Item{
			ID:        id,
			ThreadID:  thread.ID,
			TurnIndex: turnIndex,
			ItemIndex: itemIndex,
			Kind:      kind,
			Role:      role,
			Status:    "completed",
			Summary:   summary,
			PayloadID: payloadID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if payload != nil {
			if err := app.store.InsertItemWithPayload(item, *payload); err != nil {
				t.Fatalf("seed %s: %v", id, err)
			}
			return
		}
		if _, err := app.store.UpsertItem(item, nil); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	patch := func(path, oldLine, newLine string) string {
		return strings.Join([]string{
			"diff --git a/" + path + " b/" + path,
			"--- a/" + path,
			"+++ b/" + path,
			"@@ -1 +1 @@",
			"-" + oldLine,
			"+" + newLine,
			"",
		}, "\n")
	}
	editPayload := func(id, path, oldLine, newLine string) store.Payload {
		return store.Payload{
			ID:   id,
			Kind: "tool_result",
			Meta: `{"itemType":"file_change","title":"Edited ` + path + `","inlineDiff":{"availability":"full","files":[{"path":"` + path + `"}],"insertions":1,"deletions":1}}`,
			Data: []byte(patch(path, oldLine, newLine)),
		}
	}

	seedItem("user:1", 1, 0, "user_text", "user", "fix the parser", "", nil)
	p1 := editPayload("pl-edit-1", "parser.go", "old parser", "new parser")
	seedItem("tool:1", 1, 1, "tool_call", "assistant", "Edited parser.go", "pl-edit-1", &p1)

	seedItem("user:2", 2, 0, "user_text", "user", "now the lexer", "", nil)
	p2 := editPayload("pl-edit-2", "lexer.go", "alpha", "beta")
	seedItem("tool:2a", 2, 1, "tool_call", "assistant", "Edited lexer.go", "pl-edit-2", &p2)
	p3 := editPayload("pl-edit-3", "lexer.go", "beta", "gamma")
	seedItem("tool:2b", 2, 2, "tool_call", "assistant", "Edited lexer.go", "pl-edit-3", &p3)
	// A tool_result payload with no diff bytes (e.g. a merge that never
	// captured one) is not an edit.
	empty := store.Payload{ID: "pl-empty", Kind: "tool_result", Meta: "{}", Data: []byte{}}
	seedItem("tool:2c", 2, 3, "tool_call", "assistant", "No diff", "pl-empty", &empty)

	// Legacy Claude EventDiff attach: payload kind `diff`, DiffMeta meta.
	seedItem("user:3", 3, 0, "user_text", "user", "legacy turn", "", nil)
	legacy := store.Payload{
		ID:   "pl-legacy",
		Kind: "diff",
		Meta: `{"filePath":"legacy.go","changeKind":"modified","insertions":2,"deletions":1}`,
		Data: []byte(patch("legacy.go", "before", "after")),
	}
	seedItem("tool:3", 3, 1, "tool_call", "assistant", "diff", "pl-legacy", &legacy)

	return thread.ID
}

func TestListThreadEditDiffs(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)

	list, err := app.ListThreadEditDiffs(threadID)
	if err != nil {
		t.Fatalf("ListThreadEditDiffs() error = %v", err)
	}
	if len(list.Entries) != 4 {
		t.Fatalf("expected 4 edit entries, got %d: %+v", len(list.Entries), list.Entries)
	}
	first := list.Entries[0]
	if first.ItemID != "tool:1" || first.PayloadID != "pl-edit-1" || first.TurnIndex != 1 {
		t.Fatalf("first entry = %+v", first)
	}
	if first.Title != "Edited parser.go" || len(first.Paths) != 1 || first.Paths[0] != "parser.go" {
		t.Fatalf("first entry label = %+v", first)
	}
	if first.Insertions != 1 || first.Deletions != 1 {
		t.Fatalf("first entry counts = %+v", first)
	}
	if list.Entries[1].ItemID != "tool:2a" || list.Entries[2].ItemID != "tool:2b" {
		t.Fatalf("expected timeline order, got %+v", list.Entries)
	}

	legacy := list.Entries[3]
	if legacy.ItemID != "tool:3" || legacy.Title != "Edited legacy.go" {
		t.Fatalf("legacy diff-kind entry = %+v", legacy)
	}
	if len(legacy.Paths) != 1 || legacy.Paths[0] != "legacy.go" || legacy.Insertions != 2 || legacy.Deletions != 1 {
		t.Fatalf("legacy diff-kind projection = %+v", legacy)
	}

	labels := map[int]string{}
	for _, label := range list.TurnLabels {
		labels[label.TurnIndex] = label.Label
	}
	if labels[1] != "fix the parser" || labels[2] != "now the lexer" || labels[3] != "legacy turn" {
		t.Fatalf("turn labels = %+v", list.TurnLabels)
	}
}

// Selector labels feed a NATIVE <select> popup that sizes to its
// longest label — an uncapped pasted-stack-trace prompt once stretched
// the popup across three monitors.
func TestListThreadEditDiffsCapsSelectorLabels(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)

	longPrompt := "first line with an error pasted in\n" +
		strings.Repeat("svelte-vendor.js:1517 Uncaught Error: each_key_duplicate ", 40)
	item := store.Item{
		ID: "user:1", ThreadID: threadID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed", Summary: longPrompt,
	}
	if _, err := app.store.UpsertItem(item, nil); err != nil {
		t.Fatalf("UpsertItem() error = %v", err)
	}

	list, err := app.ListThreadEditDiffs(threadID)
	if err != nil {
		t.Fatalf("ListThreadEditDiffs() error = %v", err)
	}
	var label string
	for _, turnLabel := range list.TurnLabels {
		if turnLabel.TurnIndex == 1 {
			label = turnLabel.Label
		}
	}
	if got := len([]rune(label)); got > maxEditSelectorLabelRunes {
		t.Fatalf("label length = %d runes, want <= %d: %q", got, maxEditSelectorLabelRunes, label)
	}
	if strings.ContainsAny(label, "\n\t") || strings.Contains(label, "  ") {
		t.Fatalf("label whitespace not collapsed: %q", label)
	}
	if !strings.HasPrefix(label, "first line with an error pasted in") || !strings.HasSuffix(label, "...") {
		t.Fatalf("label = %q, want collapsed prefix + ellipsis", label)
	}
	// Entry titles ride the same popup; they get the same cap.
	for _, entry := range list.Entries {
		if got := len([]rune(entry.Title)); got > maxEditSelectorLabelRunes {
			t.Fatalf("entry title length = %d runes: %q", got, entry.Title)
		}
	}
}

func TestListThreadEditDiffsEmptyThread(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-no-edits")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	list, err := app.ListThreadEditDiffs(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadEditDiffs() error = %v", err)
	}
	if list.Entries == nil || len(list.Entries) != 0 {
		t.Fatalf("expected empty non-nil entries, got %#v", list.Entries)
	}
	if list.TurnLabels == nil || len(list.TurnLabels) != 0 {
		t.Fatalf("expected empty non-nil labels, got %#v", list.TurnLabels)
	}
}

// fixturePatch is editDiffFixture's patch for one edit.
func fixturePatch(path, oldLine, newLine string) string {
	return "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path +
		"\n@@ -1 +1 @@\n-" + oldLine + "\n+" + newLine + "\n"
}

func TestOpenTurnEditsDiffJoinsTheTurnsPayloadsInOrder(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)
	ctx, _ := reviewDiffConn(t)

	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 2)
	if err != nil {
		t.Fatalf("OpenTurnEditsDiff() error = %v", err)
	}
	// Both same-file edits appear as sequential sections, with no blank
	// line between them and nothing from turn 1.
	want := fixturePatch("lexer.go", "alpha", "beta") + fixturePatch("lexer.go", "beta", "gamma")
	if opened.ID != "" || !opened.Chunk.EOF || opened.Chunk.Data != want {
		t.Fatalf("opened = %+v, want one final chunk %q", opened, want)
	}
	if got := strings.Join(opened.PayloadIDs, ","); got != "pl-edit-2,pl-edit-3" {
		t.Fatalf("PayloadIDs = %q, want the turn's two edit payloads in order", got)
	}

	// A turn with no edits is an empty diff, not an error.
	empty, err := app.OpenTurnEditsDiff(ctx, threadID, 7)
	if err != nil {
		t.Fatalf("OpenTurnEditsDiff(no edits) error = %v", err)
	}
	if empty.ID != "" || !empty.Chunk.EOF || empty.Chunk.Data != "" || len(empty.PayloadIDs) != 0 {
		t.Fatalf("edit-less turn = %+v, want an empty final chunk", empty)
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held for patches that fit their first chunk", n)
	}
}

func TestOpenEditDiffReadsOnePayloadOfTheThread(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)
	ctx, _ := reviewDiffConn(t)

	opened, err := app.OpenEditDiff(ctx, threadID, "pl-legacy")
	if err != nil {
		t.Fatalf("OpenEditDiff() error = %v", err)
	}
	if want := fixturePatch("legacy.go", "before", "after"); opened.Chunk.Data != want || !opened.Chunk.EOF {
		t.Fatalf("chunk = %+v, want %q", opened.Chunk, want)
	}
	if len(opened.PayloadIDs) != 1 || opened.PayloadIDs[0] != "pl-legacy" {
		t.Fatalf("PayloadIDs = %v", opened.PayloadIDs)
	}

	other := testThread("thread-other")
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if _, err := app.OpenEditDiff(ctx, other.ID, "pl-legacy"); err == nil {
		t.Fatal("another thread opened this thread's payload")
	}
	if _, err := app.OpenEditDiff(ctx, threadID, "pl-missing"); err == nil {
		t.Fatal("an unknown payload opened")
	}
}

// seedEditPayloads adds one turn of edit payloads with the given bodies to
// a new thread.
func seedEditPayloads(t *testing.T, app *App, turnIndex int, bodies ...string) string {
	t.Helper()
	thread := testThread("thread-large-edits")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	for i, body := range bodies {
		id := fmt.Sprintf("edit-%d", i)
		item := store.Item{
			ID: "tool:" + id, ThreadID: thread.ID, TurnIndex: turnIndex, ItemIndex: i + 1,
			Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: id,
			CreatedAt: now, UpdatedAt: now,
		}
		payload := store.Payload{ID: id, Kind: "tool_result", Meta: "{}", Data: []byte(body)}
		if err := app.store.InsertItemWithPayload(item, payload); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	return thread.ID
}

// generatedPatch is a patch of one added file with lines lines.
func generatedPatch(path string, lines int) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + path + "\n")
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", lines)
	for i := range lines {
		fmt.Fprintf(&b, "+%s line %d, long enough to fill a chunk before long\n", path, i)
	}
	return b.String()
}

func TestEditsDiffReadsALargeTurnInChunks(t *testing.T) {
	app := newTestAppWithStore(t)
	big := generatedPatch("big.go", 60000)
	bodies := []string{
		generatedPatch("a.go", 3) + "\n\n\n",
		big,
		"\n\n",
		strings.TrimSuffix(generatedPatch("c.go", 2), "\n"),
		generatedPatch("d.go", 20000) + strings.Repeat("\n", editsDiffTailBytes+10),
	}
	threadID := seedEditPayloads(t, app, 4, bodies...)
	want := generatedPatch("a.go", 3) + big + generatedPatch("c.go", 2) + generatedPatch("d.go", 20000)
	if len(want) < 3*reviewDiffFirstChunkBytes {
		t.Fatalf("fixture is %d bytes, want several chunks", len(want))
	}
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 4)
	if err != nil {
		t.Fatalf("OpenTurnEditsDiff() error = %v", err)
	}
	if got := strings.Join(opened.PayloadIDs, ","); got != "edit-0,edit-1,edit-3,edit-4" {
		t.Fatalf("PayloadIDs = %q, want every payload with a diff", got)
	}
	if opened.ID == "" {
		t.Fatal("a patch past its first chunk must hold a handle")
	}

	var chunks []ReviewDiffChunk
	patch := opened.Chunk.Data
	for chunk := opened.Chunk; !chunk.EOF; {
		chunk, err = app.ReadReviewDiff(ctx, opened.ID, chunk.NextOffset, 256<<10)
		if err != nil {
			t.Fatalf("ReadReviewDiff() error = %v", err)
		}
		if !chunk.EOF && !strings.HasSuffix(chunk.Data, "\n") {
			t.Fatalf("chunk at %d ends inside a line", chunk.Offset)
		}
		chunks = append(chunks, chunk)
		patch += chunk.Data
	}
	if patch != want {
		t.Fatalf("joined patch is %d bytes, want %d; payloads must join without blank lines", len(patch), len(want))
	}

	// A chunk read again from its offset is the same chunk.
	again, err := app.ReadReviewDiff(ctx, opened.ID, chunks[1].Offset, 256<<10)
	if err != nil || again != chunks[1] {
		t.Fatalf("re-read = %+v, %v; want the chunk served first", again.Offset, err)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, chunks[1].Offset+1, 256<<10); err == nil {
		t.Fatal("a read from inside a chunk was served")
	}
	if err := app.ReleaseReviewDiff(ctx, opened.ID); err != nil {
		t.Fatalf("ReleaseReviewDiff() error = %v", err)
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held after release", n)
	}
}

func TestEditsDiffRefusesAPayloadThatChangedSinceTheOpen(t *testing.T) {
	app := newTestAppWithStore(t)
	body := generatedPatch("big.go", 60000)
	threadID := seedEditPayloads(t, app, 1, body)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff() = %+v, %v", opened.ID, err)
	}
	next, err := app.ReadReviewDiff(ctx, opened.ID, opened.Chunk.NextOffset, 256<<10)
	if err != nil {
		t.Fatalf("ReadReviewDiff() error = %v", err)
	}

	// The same length with other bytes: the first read of the rest cannot
	// tell, but the chunk already served can.
	changed := []byte(strings.ReplaceAll(body, "line", "LINE"))
	if err := app.store.ReplacePayloadData(threadID, "edit-0", changed, "{}", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ReplacePayloadData() error = %v", err)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, next.Offset, 256<<10); err == nil || !strings.Contains(err.Error(), "the diff changed since it was opened") {
		t.Fatalf("re-read of a changed chunk: err = %v, want a changed diff", err)
	}

	// A new length fails every read.
	if err := app.store.ReplacePayloadData(threadID, "edit-0", []byte(body+body), "{}", time.Now().UnixMilli()); err != nil {
		t.Fatalf("ReplacePayloadData() error = %v", err)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, next.NextOffset, 256<<10); err == nil || !strings.Contains(err.Error(), "the diff changed since it was opened") {
		t.Fatalf("read of a resized payload: err = %v, want a changed diff", err)
	}
}

func TestGetPayloadPatchSpansReturnsThePersistedSeeds(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)

	// One of turn 2's payloads has persist-time spans; the other never
	// got a blob (dropped burst).
	seed := PatchSpanSeed{
		Path:       "lexer.go",
		ContentKey: "ck-lexer",
		Lines:      []highlight.EncodedLine{{Runs: []uint16{4, 1}}},
		Primed:     true,
	}
	blob, err := json.Marshal(PersistedPatchSpans{Version: highlight.SchemaVersion(), Files: []PatchSpanSeed{seed}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdatePayloadSpans(threadID, "pl-edit-2", "", string(blob)); err != nil {
		t.Fatalf("UpdatePayloadSpans() error = %v", err)
	}

	spans, err := app.GetPayloadPatchSpans(threadID, "pl-edit-2")
	if err != nil {
		t.Fatalf("GetPayloadPatchSpans() error = %v", err)
	}
	if len(spans) != 1 || spans[0].Path != "lexer.go" || spans[0].ContentKey != "ck-lexer" || !spans[0].Primed {
		t.Fatalf("spans = %+v, want the one persisted seed", spans)
	}
	if spans, err := app.GetPayloadPatchSpans(threadID, "pl-edit-3"); err != nil || len(spans) != 0 {
		t.Fatalf("payload without spans = %+v, %v", spans, err)
	}
	if _, err := app.GetPayloadPatchSpans(threadID, "pl-missing"); err == nil {
		t.Fatal("an unknown payload answered")
	}
}
