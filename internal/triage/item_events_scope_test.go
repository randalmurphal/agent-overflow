package triage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventscope"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A mid-stream meta carries no row either, so it names the row's parent
// like a delta: the transport reads it to withhold a subagent's rows from a
// connection not viewing that agent, and a meta that read as root scope
// would reach every parent pane.
func TestStreamingMetaNamesTheRowParent(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "src", "foo.ts"), nil, 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	router, st, emissions := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID: "t1", ProjectID: triageTestProjectID, Title: "scoped-meta", Provider: "claude",
		WorkspacePath: wsRoot, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	padding := strings.Repeat("x", streamPersistByteThreshold+1)
	for _, text := range []string{"see src/foo.ts and ", padding} {
		if err := router.Handle(provider.ProviderEvent{
			Kind:            provider.EventTextDelta,
			ThreadID:        "t1",
			ParentToolUseID: "toolu_parent",
			Content:         text,
			Timestamp:       time.Now(),
		}); err != nil {
			t.Fatalf("handle child delta: %v", err)
		}
	}
	metas := filterItemEventMetas(emissions.snapshot())
	if len(metas) != 1 {
		t.Fatalf("metas = %d, want the one mid-stream meta: %+v", len(metas), metas)
	}
	if metas[0].ParentID != "toolu_parent" {
		t.Fatalf("meta for %s names parent %q, want toolu_parent", metas[0].ItemID, metas[0].ParentID)
	}
}

// Every item_event frame triage emits is attributed to the scope of the row
// it describes by the transport's extractor. One row per frame is what lets
// the attribution be a single scope; this pins that each action's shape
// carries it where the extractor looks.
func TestItemStreamEventsAttributeTheirScope(t *testing.T) {
	child := store.Item{ID: "child", ThreadID: "t1", ParentID: "toolu_parent", Kind: "assistant_text"}
	top := store.Item{ID: "top", ThreadID: "t1", Kind: "assistant_text"}
	for _, tc := range []struct {
		name  string
		event ItemStreamEvent
		want  string
	}{
		{"childUpsert", NewItemStreamUpsert(child), "toolu_parent"},
		{"topUpsert", NewItemStreamUpsert(top), ""},
		{"childDelta", newItemStreamDelta(ItemDeltaEvent{ThreadID: "t1", ItemID: "child", ParentID: "toolu_parent", Kind: "assistant_text", Delta: "x"}), "toolu_parent"},
		{"topDelta", newItemStreamDelta(ItemDeltaEvent{ThreadID: "t1", ItemID: "top", Kind: "assistant_text", Delta: "x"}), ""},
		{"childMeta", newItemStreamMeta("t1", "child", "toolu_parent", "assistant_text", "{}", 1), "toolu_parent"},
		{"topMeta", newItemStreamMeta("t1", "top", "", "assistant_text", "{}", 1), ""},
		{"childPatch", newItemStreamPatch("t1", "child", "toolu_parent", "assistant_text", 1, ItemPatchFields{}), "toolu_parent"},
		{"topPatch", newItemStreamPatch("t1", "top", "", "assistant_text", 1, ItemPatchFields{}), ""},
		// The one remove producer retires merged user messages, which are
		// root-scope rows (claude_merge_fold.go).
		{"remove", newItemStreamRemove("t1", "user", "user_text"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventscope.ScopeRootIDFromEvent(tc.event); got != tc.want {
				t.Fatalf("scope = %q, want %q", got, tc.want)
			}
			if got := eventscope.ThreadIDFromEvent(tc.event); got != "t1" {
				t.Fatalf("thread = %q, want t1", got)
			}
		})
	}
}
