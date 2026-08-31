package itemwire

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// bigString builds a leaf big enough to be an elision candidate.
func bigString(n int) string {
	return strings.Repeat("x", n)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(encoded)
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("projected value is not valid JSON (%v): %s", err, raw)
	}
	return out
}

func inputOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	input, ok := decode(t, raw)["input"].(map[string]any)
	if !ok {
		t.Fatalf("projected meta has no input object: %s", raw)
	}
	return input
}

func elidedPaths(t *testing.T, raw string) []string {
	t.Helper()
	marker, ok := decode(t, raw)[MarkerKey]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("re-marshal marker: %v", err)
	}
	var elision Elision
	if err := json.Unmarshal(encoded, &elision); err != nil {
		t.Fatalf("marker is not an Elision: %v", err)
	}
	return elision.Input
}

func TestProjectMeta_UnderBudgetIsByteIdentical(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"toolName": "Edit",
		"input":    map[string]any{"file_path": "src/lib/foo.ts", "new_string": bigString(400)},
	})
	if len(meta) > MetaMaxBytes {
		t.Fatalf("fixture is over budget (%d bytes); it must not be", len(meta))
	}
	if got := ProjectMeta(meta, ""); got != meta {
		t.Fatalf("under-budget meta was rewritten:\n got %s\nwant %s", got, meta)
	}
}

func TestProjectMeta_DropsLargestLeavesKeepsSubFields(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"toolName": "Write",
		"mcp":      map[string]any{"server": "docs", "tool": "search"},
		"input": map[string]any{
			"file_path":  "src/lib/foo.ts",
			"skill":      "artifact-design",
			"query":      "select:Read,Edit",
			"tool":       "Explore",
			"questions":  []any{map[string]any{"question": "which one?"}},
			"files":      []any{"a.ts", "b.ts"},
			"content":    bigString(6000),
			"new_string": bigString(3000),
		},
	})
	projected := ProjectMeta(meta, "")
	if projected == meta {
		t.Fatal("over-budget meta was not projected")
	}
	if len(projected) > MetaMaxBytes {
		t.Fatalf("projected meta is %d bytes, over the %d budget", len(projected), MetaMaxBytes)
	}

	input := inputOf(t, projected)
	// Every sub-field consumer named in remote-access.md §14 plus the
	// ones the sweep found: all of them read short leaves, and all of
	// them must still resolve.
	for key, want := range map[string]string{
		"file_path": "src/lib/foo.ts",
		"skill":     "artifact-design",
		"query":     "select:Read,Edit",
		"tool":      "Explore",
	} {
		if got, _ := input[key].(string); got != want {
			t.Errorf("input.%s = %q, want %q", key, got, want)
		}
	}
	if files, ok := input["files"].([]any); !ok || len(files) != 2 {
		t.Errorf("input.files did not survive: %#v", input["files"])
	}
	if questions, ok := input["questions"].([]any); !ok || len(questions) != 1 {
		t.Errorf("input.questions did not survive: %#v", input["questions"])
	}
	if _, present := input["content"]; present {
		t.Error("input.content was not dropped")
	}
	if mcp, ok := decode(t, projected)["mcp"].(map[string]any); !ok || mcp["server"] != "docs" {
		t.Errorf("a meta key outside input was disturbed: %#v", decode(t, projected)["mcp"])
	}

	paths := elidedPaths(t, projected)
	if len(paths) == 0 {
		t.Fatal("projection dropped values without a marker naming them")
	}
	for _, path := range paths {
		if _, present := input[path]; present {
			t.Errorf("marker names %q as elided but it is still present", path)
		}
	}
}

func TestProjectMeta_SkipsNotBreaksAcrossOversizedLeaves(t *testing.T) {
	// Two oversized leaves where dropping only the largest still leaves
	// the row over budget: the walk must continue to the second.
	meta := mustJSON(t, map[string]any{
		"input": map[string]any{
			"first":     bigString(5000),
			"second":    bigString(4000),
			"file_path": "src/lib/foo.ts",
		},
	})
	projected := ProjectMeta(meta, "")
	input := inputOf(t, projected)
	if _, present := input["first"]; present {
		t.Error("largest leaf survived")
	}
	if _, present := input["second"]; present {
		t.Error("second oversized leaf survived: the budget broke instead of skipping on")
	}
	if input["file_path"] != "src/lib/foo.ts" {
		t.Errorf("a leaf under the floor was dropped: %#v", input["file_path"])
	}
	if got, want := elidedPaths(t, projected), 2; len(got) != want {
		t.Errorf("marker names %d paths, want %d: %v", len(got), want, got)
	}
}

func TestProjectMeta_NeverDropsBelowTheLeafFloor(t *testing.T) {
	// Many leaves just under the floor, summing far over the budget.
	// The projection must leave every one of them alone rather than
	// reach below the floor to make the number work.
	input := map[string]any{}
	for i := range 20 {
		input[string(rune('a'+i))] = bigString(LeafFloorBytes - 32)
	}
	meta := mustJSON(t, map[string]any{"input": input})
	projected := ProjectMeta(meta, "")
	if projected != meta {
		t.Fatalf("projection reached below the %d-byte leaf floor", LeafFloorBytes)
	}
}

func TestProjectMeta_ArrayIndicesSurviveAnElidedElement(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"input": map[string]any{
			"edits": []any{"keep-me", bigString(5000), "keep-me-too"},
		},
	})
	projected := ProjectMeta(meta, "")
	edits, ok := inputOf(t, projected)["edits"].([]any)
	if !ok || len(edits) != 3 {
		t.Fatalf("array length changed: %#v", inputOf(t, projected)["edits"])
	}
	if edits[0] != "keep-me" || edits[2] != "keep-me-too" {
		t.Errorf("array indices shifted: %#v", edits)
	}
	if edits[1] != nil {
		t.Errorf("elided element = %#v, want null", edits[1])
	}
	if got := elidedPaths(t, projected); len(got) != 1 || got[0] != "edits/1" {
		t.Errorf("marker paths = %v, want [edits/1]", got)
	}
}

func TestProjectMeta_NumbersStayLiteral(t *testing.T) {
	meta := `{"input":{"startedAt":1755123456789,"ratio":0.125,"drop":"` + bigString(5000) + `"}}`
	projected := ProjectMeta(meta, "")
	if !strings.Contains(projected, "1755123456789") {
		t.Errorf("a large integer was re-encoded through float64: %s", projected)
	}
	if !strings.Contains(projected, "0.125") {
		t.Errorf("a fractional number was re-encoded lossily: %s", projected)
	}
}

func TestProjectMeta_CommandKeptWhenTheItemHasNoSecondCopy(t *testing.T) {
	meta := mustJSON(t, map[string]any{
		"input": map[string]any{"command": bigString(5000), "description": bigString(4000)},
	})

	// No payload copy: the leaf is the row's only source and the
	// collapsed row renders it uncapped, so it stays.
	kept := ProjectMeta(meta, "")
	if _, present := inputOf(t, kept)["command"]; !present {
		t.Error("command was dropped from a row that carries no other copy of it")
	}
	if _, present := inputOf(t, kept)["description"]; present {
		t.Error("the retention rule leaked onto a leaf it does not cover")
	}

	// The payload already ships the same command: the meta copy is a
	// duplicate and goes.
	payloadMeta := mustJSON(t, map[string]any{"command": bigString(5000), "lineCount": 3})
	dropped := ProjectMeta(meta, payloadMeta)
	if _, present := inputOf(t, dropped)["command"]; present {
		t.Error("a duplicated command survived on the wire twice")
	}
}

// Identity survives whatever its size — the Go half of the pairing the
// frontend tripwire holds (metaInputLeafRenderCaps.test.ts). Written as
// behavior over every key rather than as a snapshot of the list, so
// removing a key fails here instead of silently widening the elision.
func TestProjectMeta_IdentityKeysSurviveWhateverTheirSize(t *testing.T) {
	for _, key := range retainedIdentityKeys {
		t.Run(key, func(t *testing.T) {
			value := bigString(4000)
			meta := mustJSON(t, map[string]any{
				"input": map[string]any{key: value, "content": bigString(6000)},
			})

			// `command` is the one identity key with a second copy to
			// fall back on, so it is retained only when the item does
			// not already ship one.
			projected := ProjectMeta(meta, "")
			if inputOf(t, projected)[key] != value {
				t.Errorf("identity key %q was elided: %s", key, projected)
			}
			// The row's content is still governed by size, or the
			// retention would be an amnesty rather than a boundary.
			if _, present := inputOf(t, projected)["content"]; present {
				t.Errorf("retaining %q left the row's content on the wire too", key)
			}
		})
	}
}

// The AskUserQuestion card renders each question string in full and the
// item carries no second copy, so a long question is exactly the leaf
// the size rule must not take — dropping one does not shorten the card,
// it deletes the question. Retention covers the whole subtree, which is
// the part a leaf-by-leaf retain list would get wrong.
func TestProjectMeta_QuestionsSurviveWhateverTheirSize(t *testing.T) {
	longQuestion := bigString(4000)
	meta := mustJSON(t, map[string]any{
		"input": map[string]any{
			"questions": []any{
				map[string]any{
					"header":   "Approach",
					"question": longQuestion,
					"options":  []any{map[string]any{"label": bigString(3000)}},
				},
			},
			"context": bigString(5000),
		},
	})

	projected := ProjectMeta(meta, "")
	questions, ok := inputOf(t, projected)["questions"].([]any)
	if !ok || len(questions) != 1 {
		t.Fatalf("questions did not survive the projection: %s", projected)
	}
	first, ok := questions[0].(map[string]any)
	if !ok {
		t.Fatalf("question entry is not an object: %s", projected)
	}
	if first["question"] != longQuestion {
		t.Error("a question the card renders in full was elided")
	}
	options, ok := first["options"].([]any)
	if !ok || len(options) != 1 {
		t.Errorf("retention stopped at the question and dropped its options: %s", projected)
	}
	// Retention is an inventory of uncapped readers, not an amnesty for
	// the row: everything outside it is still governed by size.
	if _, present := inputOf(t, projected)["context"]; present {
		t.Error("retaining questions leaked onto a leaf no reader renders whole")
	}
}

func TestProjectMeta_LeavesMetaWithoutInputAlone(t *testing.T) {
	meta := mustJSON(t, map[string]any{"tool_use_result": map[string]any{"stdout": bigString(6000)}})
	if got := ProjectMeta(meta, ""); got != meta {
		t.Error("projection rewrote a meta with no input object")
	}
}

func TestProjectMeta_MalformedMetaPassesThrough(t *testing.T) {
	meta := `{"input": {"content": "` + bigString(5000) + `"` // truncated on purpose
	if got := ProjectMeta(meta, ""); got != meta {
		t.Error("undecodable meta was rewritten rather than passed through")
	}
}

// --- inline diff previews ---

func inlineDiffMeta(t *testing.T, patches ...string) string {
	t.Helper()
	files := make([]any, 0, len(patches))
	for i, patch := range patches {
		files = append(files, map[string]any{
			"path":             string(rune('a'+i)) + ".ts",
			"kind":             "modified",
			"insertions":       3,
			"deletions":        1,
			"previewPatch":     patch,
			"previewLineCount": 4,
			"previewTruncated": true,
		})
	}
	return mustJSON(t, map[string]any{
		"itemType": "file_change",
		"title":    "Edited a.ts",
		"inlineDiff": map[string]any{
			"availability": "exact_patch",
			"files":        files,
			"insertions":   3,
			"deletions":    1,
		},
	})
}

func filesOf(t *testing.T, raw string) []any {
	t.Helper()
	diff, ok := decode(t, raw)["inlineDiff"].(map[string]any)
	if !ok {
		t.Fatalf("no inlineDiff in %s", raw)
	}
	files, ok := diff["files"].([]any)
	if !ok {
		t.Fatalf("no files in %s", raw)
	}
	return files
}

func TestProjectPayloadMeta_PreviewsOffDropsPatchKeepsChrome(t *testing.T) {
	payloadMeta := inlineDiffMeta(t, "@@ -1 +1 @@\n-a\n+b\n")
	projected, elided, kept := ProjectPayloadMeta(payloadMeta, false)
	if !elided || kept {
		t.Fatalf("elided=%v kept=%v, want true/false", elided, kept)
	}
	file := filesOf(t, projected)[0].(map[string]any)
	if _, present := file["previewPatch"]; present {
		t.Error("patch text rode along for a client that renders none of it")
	}
	if file["previewElided"] != true {
		t.Error("dropped preview carries no marker")
	}
	for key, want := range map[string]any{
		"path": "a.ts", "kind": "modified", "insertions": float64(3), "deletions": float64(1),
		"previewLineCount": float64(4), "previewTruncated": true,
	} {
		if file[key] != want {
			t.Errorf("file.%s = %#v, want %#v — the collapsed row's chrome must be untouched", key, file[key], want)
		}
	}
	if decode(t, projected)["title"] != "Edited a.ts" {
		t.Error("a payloadMeta key outside inlineDiff was disturbed")
	}
}

func TestProjectPayloadMeta_PreviewsOnKeepsSmallPatches(t *testing.T) {
	payloadMeta := inlineDiffMeta(t, "@@ -1 +1 @@\n-a\n+b\n")
	projected, elided, kept := ProjectPayloadMeta(payloadMeta, true)
	if elided || !kept {
		t.Fatalf("elided=%v kept=%v, want false/true", elided, kept)
	}
	if projected != payloadMeta {
		t.Error("an under-budget payloadMeta was rewritten")
	}
}

func TestProjectPayloadMeta_BudgetSkipsRatherThanBreaks(t *testing.T) {
	// One file eats the whole budget; the small files after it must
	// still be admitted rather than starved.
	payloadMeta := inlineDiffMeta(t, bigString(InlinePreviewMaxBytes+64), "small-a", "small-b")
	projected, elided, kept := ProjectPayloadMeta(payloadMeta, true)
	if !elided || !kept {
		t.Fatalf("elided=%v kept=%v, want true/true", elided, kept)
	}
	files := filesOf(t, projected)
	if files[0].(map[string]any)["previewElided"] != true {
		t.Error("the oversized file kept its patch")
	}
	for _, index := range []int{1, 2} {
		file := files[index].(map[string]any)
		if _, present := file["previewPatch"]; !present {
			t.Errorf("file %d lost its small patch: one giant file starved the rest", index)
		}
	}
}

func TestProjectPayloadMeta_NoInlineDiffPassesThrough(t *testing.T) {
	payloadMeta := mustJSON(t, map[string]any{"itemType": "tool_result", "preview": bigString(4000)})
	projected, elided, kept := ProjectPayloadMeta(payloadMeta, false)
	if projected != payloadMeta || elided || kept {
		t.Errorf("a payloadMeta with no previews was touched: %v %v", elided, kept)
	}
}

// --- whole-item projection ---

func TestProject_ClearsPreviewSpansOnlyWhenEveryPreviewWent(t *testing.T) {
	item := store.Item{
		PayloadMeta:         inlineDiffMeta(t, "@@ -1 +1 @@\n-a\n+b\n"),
		PayloadPreviewSpans: `{"version":1,"files":[{"path":"a.ts"}]}`,
	}

	off := Project(item, false)
	if off.PayloadPreviewSpans != "" {
		t.Error("spans survived with nothing left for them to highlight")
	}

	on := Project(item, true)
	if on.PayloadPreviewSpans != item.PayloadPreviewSpans {
		t.Error("spans were cleared while their patches were still on the wire")
	}
}

func TestProject_DoesNotMutateTheCallersRow(t *testing.T) {
	original := store.Item{
		Meta:                mustJSON(t, map[string]any{"input": map[string]any{"content": bigString(6000)}}),
		PayloadMeta:         inlineDiffMeta(t, "@@ -1 +1 @@\n-a\n+b\n"),
		PayloadPreviewSpans: `{"version":1}`,
	}
	snapshot := original
	projected := Project(original, false)
	if original != snapshot {
		t.Fatal("Project mutated the row it was given; the stored record must stay complete")
	}
	if projected.Meta == original.Meta {
		t.Fatal("fixture did not exercise the projection")
	}
}

func TestEncodedBytes_TracksTheRealEncoding(t *testing.T) {
	item := store.Item{
		ID: "item-1", ThreadID: "thread-1", Kind: "tool_call", Role: "assistant",
		Status: "completed", Summary: bigString(500), Meta: mustJSON(t, map[string]any{"a": bigString(300)}),
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	estimate := EncodedBytes(item)
	// The estimator exists so the window backstop does not marshal every
	// row. It must stay close enough that the ceiling it enforces means
	// something — within 25% of the real encoding on a realistic row.
	if estimate < len(encoded)*3/4 || estimate > len(encoded)*5/4 {
		t.Errorf("EncodedBytes = %d, real encoding = %d: the estimate has drifted", estimate, len(encoded))
	}
}
