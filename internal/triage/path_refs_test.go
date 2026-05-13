package triage

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/pathlinks"
)

// TestMergePathRefsIntoMeta_PreservesTaskID is the regression guard
// on the v17 partial JSON index `idx_items_task_id` over
// `items.meta -> $.task_id`. Path-link enrichment writes the
// `pathRefs` key alongside existing meta keys; if the merge ever
// replaced the whole object, the partial index would lose its
// content and background-task pairing would silently break.
func TestMergePathRefsIntoMeta_PreservesTaskID(t *testing.T) {
	existing := `{"task_id":"abc-123","other":"value"}`
	refs := []pathlinks.PathRef{{Path: "src/lib/foo.ts", Line: 42}}
	merged, err := mergePathRefsIntoMeta(existing, refs)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	if string(got["task_id"]) != `"abc-123"` {
		t.Fatalf("task_id lost in merge; got %q in %q", got["task_id"], merged)
	}
	if string(got["other"]) != `"value"` {
		t.Fatalf("other key lost in merge; got %q in %q", got["other"], merged)
	}
	if string(got["pathRefs"]) == "" {
		t.Fatalf("pathRefs not added; merged=%q", merged)
	}
}

func TestMergePathRefsIntoMeta_HandlesEmptyMeta(t *testing.T) {
	for _, empty := range []string{"", "  ", "{}"} {
		refs := []pathlinks.PathRef{{Path: "a.ts"}}
		merged, err := mergePathRefsIntoMeta(empty, refs)
		if err != nil {
			t.Fatalf("empty=%q merge: %v", empty, err)
		}
		if !strings.Contains(merged, `"pathRefs"`) {
			t.Fatalf("empty=%q: pathRefs not in merged %q", empty, merged)
		}
	}
}

func TestMergePathRefsIntoMeta_OverwritesCorruptMeta(t *testing.T) {
	// A meta string that's syntactically not a JSON object (e.g. an
	// array, a primitive). The store shouldn't produce these, but the
	// helper must degrade cleanly rather than refusing to persist.
	refs := []pathlinks.PathRef{{Path: "x.ts"}}
	merged, err := mergePathRefsIntoMeta(`["not an object"]`, refs)
	if err != nil {
		t.Fatalf("merge corrupt: %v", err)
	}
	if !strings.Contains(merged, `"pathRefs"`) {
		t.Fatalf("pathRefs missing from %q", merged)
	}
	if strings.Contains(merged, "not an object") {
		t.Fatalf("corrupt payload leaked into merged meta: %q", merged)
	}
}

func TestMergePathRefsIntoMeta_RoundTripPathRef(t *testing.T) {
	refs := []pathlinks.PathRef{
		{Path: "src/a.ts"},
		{Path: "src/b.ts", Line: 12},
		{Path: "src/c.ts", Line: 1, Col: 4},
	}
	merged, err := mergePathRefsIntoMeta(`{}`, refs)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got struct {
		PathRefs []pathlinks.PathRef `json:"pathRefs"`
	}
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.PathRefs) != 3 {
		t.Fatalf("expected 3 refs, got %#v", got.PathRefs)
	}
	if got.PathRefs[1].Line != 12 || got.PathRefs[2].Col != 4 {
		t.Fatalf("ref fields lost: %#v", got.PathRefs)
	}
}
