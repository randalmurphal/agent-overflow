package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestScopedNativeBlocksSurviveSessionReplacement(t *testing.T) {
	for _, snapshotOnly := range []bool{false, true} {
		name := "streamed"
		if snapshotOnly {
			name = "snapshot"
		}
		t.Run(name, func(t *testing.T) {
			r, st, _ := newTestRouter(t)
			createCodexBackgroundTestThread(t, st, "t1")
			seedOpenTurn(t, r, st, "t1", 0)
			seedCodexSpawnCard(t, r, st, "t1", "spawn", "child")
			emit := func(id, body string, thinking bool) {
				delta, stop := provider.EventTextDelta, provider.EventContentBlockStop
				if thinking {
					delta, stop = provider.EventThinking, provider.EventContentBlockStop
				}
				e := provider.ProviderEvent{ThreadID: "t1", ParentToolUseID: "spawn", ItemID: id, Content: body, Timestamp: time.Now()}
				if !snapshotOnly {
					e.Kind = delta
					if err := r.Handle(e); err != nil {
						t.Fatal(err)
					}
				}
				e.Meta = json.RawMessage(`{"blockType":"text"}`)
				if thinking {
					e.Meta = json.RawMessage(`{"blockType":"thinking"}`)
				}
				e.Kind = stop
				e.ContentPresent = true
				if err := r.Handle(e); err != nil {
					t.Fatal(err)
				}
				r.WaitForPendingSettles()
			}
			emit("first-answer", "original answer", false)
			emit("first-thought", "original reasoning", true)
			r.CleanupThread("t1")
			r.MarkThreadActive("t1")
			emit("second-answer", "later answer", false)
			emit("second-thought", "later reasoning", true)
			// Snapshot replay must update the matching native item, not add a row.
			for _, e := range []provider.ProviderEvent{
				{Kind: provider.EventContentBlockStop, ItemID: "second-answer", Content: "later answer"},
				{Kind: provider.EventContentBlockStop, ItemID: "second-thought", Content: "later reasoning"},
			} {
				e.Meta = json.RawMessage(`{"blockType":"text"}`)
				if e.ItemID == "second-thought" {
					e.Meta = json.RawMessage(`{"blockType":"thinking"}`)
				}
				e.ThreadID = "t1"
				e.ParentToolUseID = "spawn"
				e.ContentPresent = true
				e.Timestamp = time.Now()
				if err := r.Handle(e); err != nil {
					t.Fatal(err)
				}
			}
			r.WaitForPendingSettles()
			items, err := st.ListItems("t1")
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]int{}
			for _, item := range items {
				if item.ParentID == "spawn" && (item.Kind == itemKindAssistantText || item.Kind == itemKindThinking) {
					found[item.Summary]++
				}
			}
			for _, body := range []string{"original answer", "later answer", "original reasoning", "later reasoning"} {
				if found[body] != 1 {
					t.Fatalf("history lost or duplicated %q: %v", body, found)
				}
			}
		})
	}
}
