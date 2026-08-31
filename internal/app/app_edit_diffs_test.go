package app

import (
	"encoding/json"
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

func TestGetTurnEditsDiffConcatenatesInOrder(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)

	turnDiff, err := app.GetTurnEditsDiff(threadID, 2)
	if err != nil {
		t.Fatalf("GetTurnEditsDiff() error = %v", err)
	}
	combined := turnDiff.Data
	// Both same-file edits appear as separate sequential sections.
	betaOut := strings.Index(combined, "-alpha")
	gammaIn := strings.Index(combined, "+gamma")
	if betaOut == -1 || gammaIn == -1 || betaOut > gammaIn {
		t.Fatalf("expected sequential sections (alpha edit before gamma edit), got:\n%s", combined)
	}
	if strings.Contains(combined, "parser.go") {
		t.Fatalf("turn 2 diff must not include turn 1 content:\n%s", combined)
	}
	if strings.Count(combined, "diff --git") != 2 {
		t.Fatalf("expected 2 patch sections, got:\n%s", combined)
	}

	// A turn with no edits yields an empty patch, not an error.
	empty, err := app.GetTurnEditsDiff(threadID, 7)
	if err != nil {
		t.Fatalf("GetTurnEditsDiff(no edits) error = %v", err)
	}
	if empty.Data != "" {
		t.Fatalf("expected empty diff for edit-less turn, got %q", empty.Data)
	}
}

func TestGetTurnEditsDiffAttachesPersistedSpans(t *testing.T) {
	app := newTestAppWithStore(t)
	threadID := editDiffFixture(t, app)

	// One of turn 2's payloads has persist-time spans; the other never
	// got a blob (dropped burst) — only the stored seeds attach.
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

	turnDiff, err := app.GetTurnEditsDiff(threadID, 2)
	if err != nil {
		t.Fatalf("GetTurnEditsDiff() error = %v", err)
	}
	if len(turnDiff.PatchSpans) != 1 {
		t.Fatalf("PatchSpans = %+v, want the one persisted seed", turnDiff.PatchSpans)
	}
	got := turnDiff.PatchSpans[0]
	if got.Path != "lexer.go" || got.ContentKey != "ck-lexer" || !got.Primed {
		t.Fatalf("seed = %+v", got)
	}
}
