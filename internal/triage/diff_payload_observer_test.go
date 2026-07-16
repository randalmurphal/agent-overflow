package triage

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

type diffPayloadObservation struct {
	threadID  string
	payloadID string
	previews  []string
	patch     string
}

func collectDiffPayloadObservations(t *testing.T, router *Router, wantThread string) (*[]diffPayloadObservation, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	calls := &[]diffPayloadObservation{}
	router.SetDiffPayloadObserver(func(threadID, payloadID string, previews []string, patch string) {
		if threadID != wantThread {
			t.Errorf("observer thread id = %q, want %q", threadID, wantThread)
		}
		mu.Lock()
		*calls = append(*calls, diffPayloadObservation{threadID, payloadID, previews, patch})
		mu.Unlock()
	})
	return calls, &mu
}

// TestDiffPayloadObserverToolResults pins the observer contract the
// span persistence + diff seed push build on: whenever a tool result
// with per-file preview patches is persisted — the direct exact-patch
// path AND the later summary_only→exact upgrade — the observer receives
// the payload row id, exactly the PreviewPatch strings the frontend's
// diff cards will parse, and the full unified patch that landed in the
// payload's data blob.
func TestDiffPayloadObserverToolResults(t *testing.T) {
	router, st, _ := newTestRouter(t)
	workspace := t.TempDir()
	createToolResultThread(t, st, "t1", workspace)
	calls, mu := collectDiffPayloadObservations(t, router, "t1")

	// Direct exact-patch persist: the tool result carries its own diff
	// content, so persistToolResult runs with preview patches built.
	exactMeta := json.RawMessage(`{
		"item": {
			"id": "item-exact",
			"type": "file_change",
			"data": {
				"item": {
					"changes": [
						{
							"path": "src/app.py",
							"kind": "added",
							"diff": "def f():\n    pass"
						}
					]
				}
			}
		}
	}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-exact",
		ItemType:  "file_change",
		Meta:      exactMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle exact tool start: %v", err)
	}

	mu.Lock()
	if len(*calls) != 1 || len((*calls)[0].previews) != 1 {
		t.Fatalf("expected one observation with one preview, got %#v", *calls)
	}
	exact := (*calls)[0]
	mu.Unlock()
	if exact.payloadID != toolResultPayloadID("item-exact") {
		t.Fatalf("observer payload id = %q, want %q", exact.payloadID, toolResultPayloadID("item-exact"))
	}
	if !strings.HasPrefix(exact.previews[0], "diff --git a/src/app.py b/src/app.py") ||
		!strings.Contains(exact.previews[0], "+    pass") {
		t.Fatalf("exact preview patch wrong: %q", exact.previews[0])
	}
	meta := readToolResultMeta(t, st, toolResultPayloadID("item-exact"))
	if len(meta.InlineDiff.Files) != 1 || meta.InlineDiff.Files[0].PreviewPatch != exact.previews[0] {
		t.Fatalf("observer preview must equal the persisted meta's PreviewPatch")
	}
	// The full patch is exactly the payload's persisted data blob.
	data, err := st.GetPayloadData(exact.payloadID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if exact.patch != string(data) {
		t.Fatalf("observer patch must equal the persisted payload data:\n%q\nvs\n%q", exact.patch, data)
	}

	// Summary-only persist (no diff content, no previews) must NOT
	// observe...
	summaryMeta := json.RawMessage(`{
		"item": {
			"id": "item-summary",
			"type": "file_change",
			"data": {
				"item": {
					"changes": [
						{
							"path": "src/app.ts",
							"kind": {"type": "update", "move_path": null}
						}
					]
				}
			}
		}
	}`)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "item-summary",
		ItemType:  "file_change",
		Meta:      summaryMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle summary tool start: %v", err)
	}
	mu.Lock()
	if len(*calls) != 1 {
		t.Fatalf("summary_only persist must not observe (no previews, no patch), got %#v", *calls)
	}
	mu.Unlock()

	// ...until the turn diff upgrades it to exact_patch, which persists
	// fresh preview patches + the filtered full patch and must observe
	// them under the upgraded payload's id.
	turnDiff := strings.Join([]string{
		"diff --git a/src/app.ts b/src/app.ts",
		"--- a/src/app.ts",
		"+++ b/src/app.ts",
		"@@ -1 +1,2 @@",
		" export const value = 1;",
		"+export const next = 2;",
	}, "\n")
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		Content:   turnDiff,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle diff: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 2 || len((*calls)[1].previews) != 1 {
		t.Fatalf("expected upgrade observation, got %#v", *calls)
	}
	upgrade := (*calls)[1]
	if upgrade.payloadID != toolResultPayloadID("item-summary") {
		t.Fatalf("upgrade payload id = %q, want %q", upgrade.payloadID, toolResultPayloadID("item-summary"))
	}
	upgraded := readToolResultMeta(t, st, toolResultPayloadID("item-summary"))
	if upgraded.InlineDiff.Availability != "exact_patch" {
		t.Fatalf("upgrade did not land: %+v", upgraded.InlineDiff)
	}
	if upgrade.previews[0] != upgraded.InlineDiff.Files[0].PreviewPatch {
		t.Fatalf("upgrade observation must equal the persisted PreviewPatch")
	}
	if upgrade.patch != turnDiff {
		t.Fatalf("upgrade patch = %q, want the filtered turn diff %q", upgrade.patch, turnDiff)
	}
}

// TestDiffPayloadObserverDiffKind pins the diff-kind half of the
// contract: a full write of a "diff" payload observes the payload id
// and the complete patch (no previews — diff payloads carry none),
// while a follow-up APPEND to the same payload must not observe, since
// its content is a delta whose spans would key to text no reader holds.
func TestDiffPayloadObserverDiffKind(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	calls, mu := collectDiffPayloadObservations(t, router, "t1")

	patch := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -1 +1,2 @@",
		" package main",
		"+// hi",
	}, "\n")
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "diff-item",
		Content:   patch,
		Replace:   true,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle diff: %v", err)
	}

	mu.Lock()
	if len(*calls) != 1 {
		t.Fatalf("expected one diff observation, got %#v", *calls)
	}
	got := (*calls)[0]
	mu.Unlock()
	if got.previews != nil || got.patch != patch {
		t.Fatalf("diff observation wrong: %#v", got)
	}
	item, found, err := st.GetThreadItem("t1", "diff-item")
	if err != nil || !found {
		t.Fatalf("get diff item: %v found=%v", err, found)
	}
	if got.payloadID != item.PayloadID {
		t.Fatalf("observer payload id = %q, want the persisted row's %q", got.payloadID, item.PayloadID)
	}

	// Append path (linked payload, Replace=false): content is a delta —
	// no observation.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventDiff,
		ThreadID:  "t1",
		ItemID:    "diff-item",
		Content:   "\n+// more",
		Replace:   false,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("handle diff append: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("append must not observe, got %#v", *calls)
	}
}
