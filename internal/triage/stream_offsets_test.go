package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// streamedRow is the stored row a stream's deltas built, read after the
// thread's buffers are written.
func streamedRow(t *testing.T, router *Router, st *store.Store, threadID, itemID string) store.Item {
	t.Helper()
	if err := router.FlushThread(threadID); err != nil {
		t.Fatalf("flush thread: %v", err)
	}
	item, found, err := st.GetThreadItem(threadID, itemID)
	if err != nil || !found {
		t.Fatalf("get %s: found=%v err=%v", itemID, found, err)
	}
	return item
}

// TestStreamDeltaOffsetsCountStreamedBytes: each stream kind numbers its
// deltas by the UTF-8 bytes streamed before them, the row goes out with a
// stream end of 0, and a read of the stored row ends where the next delta
// starts.
func TestStreamDeltaOffsetsCountStreamedBytes(t *testing.T) {
	chunks := []string{"héllo", " wörld", " 🙂", " done"}
	cases := []struct {
		name  string
		kind  string
		event func(content string) provider.ProviderEvent
	}{
		{"text", itemKindAssistantText, func(content string) provider.ProviderEvent {
			return provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: content, Timestamp: time.Now()}
		}},
		{"thinking", itemKindThinking, func(content string) provider.ProviderEvent {
			return provider.ProviderEvent{Kind: provider.EventThinking, ThreadID: "t1", Content: content, Timestamp: time.Now()}
		}},
		{"compaction reasoning", itemKindCompactionReasoning, func(content string) provider.ProviderEvent {
			return scopedThinking("t1", content)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			var streamed string
			for _, chunk := range chunks {
				if err := router.Handle(tc.event(chunk)); err != nil {
					t.Fatalf("handle %q: %v", chunk, err)
				}
				streamed += chunk
			}

			deltas := filterItemEventDeltas(emissions.snapshot())
			if len(deltas) != len(chunks) {
				t.Fatalf("deltas = %d, want %d", len(deltas), len(chunks))
			}
			var want int64
			for i, delta := range deltas {
				if delta.Kind != tc.kind || delta.Delta != chunks[i] {
					t.Fatalf("delta %d = %+v, want %s %q", i, delta, tc.kind, chunks[i])
				}
				if delta.Offset != want {
					t.Fatalf("delta %d offset = %d, want %d", i, delta.Offset, want)
				}
				want += int64(len(delta.Delta))
			}

			upserts := filterItemEventUpserts(emissions.snapshot())
			if len(upserts) != 1 || upserts[0].StreamEnd == nil || *upserts[0].StreamEnd != 0 || upserts[0].Summary != "" {
				t.Fatalf("creation upserts = %+v, want one blank row at stream end 0", upserts)
			}

			row := streamedRow(t, router, st, "t1", deltas[0].ItemID)
			if row.StreamEnd == nil || *row.StreamEnd != want {
				t.Fatalf("stored stream end = %v, want %d", row.StreamEnd, want)
			}
			data, err := st.GetPayloadData("t1", row.PayloadID)
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			if string(data) != streamed {
				t.Fatalf("payload = %q, want %q", data, streamed)
			}
			// The summary is the stream's text or its tail, and ends at the
			// stream end.
			if len(row.Summary) > len(streamed) || streamed[len(streamed)-len(row.Summary):] != row.Summary {
				t.Fatalf("summary %q is not a tail of %q", row.Summary, streamed)
			}
		})
	}
}

// TestStreamDeltaOffsetsSurviveInvalidUTF8: the wire and the store carry
// the same bytes for a chunk that is not valid UTF-8, so the offsets that
// count one also count the other.
func TestStreamDeltaOffsetsSurviveInvalidUTF8(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	for _, chunk := range []string{"a\xffb", "c"} {
		if err := router.Handle(provider.ProviderEvent{Kind: provider.EventThinking, ThreadID: "t1", Content: chunk, Timestamp: time.Now()}); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	deltas := filterItemEventDeltas(emissions.snapshot())
	if len(deltas) != 2 || deltas[0].Delta != "a�b" || deltas[1].Offset != int64(len("a�b")) {
		t.Fatalf("deltas = %+v, want the sanitized chunk and an offset counting it", deltas)
	}
	wire, err := json.Marshal(newItemStreamDelta(deltas[0]))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ItemStreamEvent
	if err := json.Unmarshal(wire, &decoded); err != nil || decoded.Delta != deltas[0].Delta {
		t.Fatalf("wire delta = %q (err %v), want %q", decoded.Delta, err, deltas[0].Delta)
	}
	row := streamedRow(t, router, st, "t1", deltas[0].ItemID)
	if row.StreamEnd == nil || *row.StreamEnd != int64(len("a�bc")) {
		t.Fatalf("stored stream end = %v, want %d", row.StreamEnd, len("a�bc"))
	}
}

// TestSettlePatchNamesTheStreamEnd: a settle that keeps the streamed
// summary names the stream end it settled at; one that replaces the
// summary carries the summary instead.
func TestSettlePatchNamesTheStreamEnd(t *testing.T) {
	stop := func(blockType string, content *string) provider.ProviderEvent {
		evt := provider.ProviderEvent{
			Kind:      provider.EventContentBlockStop,
			ThreadID:  "t1",
			Meta:      json.RawMessage(`{"blockType":"` + blockType + `"}`),
			Timestamp: time.Now(),
		}
		if content != nil {
			evt.Content = *content
			evt.ContentPresent = true
		}
		return evt
	}
	final := "the final text"
	cases := []struct {
		name      string
		event     provider.EventKind
		blockType string
		final     *string
	}{
		{"text", provider.EventTextDelta, "text", nil},
		{"thinking", provider.EventThinking, "thinking", nil},
		{"text with final content", provider.EventTextDelta, "text", &final},
		{"thinking with final content", provider.EventThinking, "thinking", &final},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			for _, chunk := range []string{"one ", "twö ", "three"} {
				if err := router.Handle(provider.ProviderEvent{Kind: tc.event, ThreadID: "t1", Content: chunk, Timestamp: time.Now()}); err != nil {
					t.Fatalf("handle: %v", err)
				}
			}
			if err := router.Handle(stop(tc.blockType, tc.final)); err != nil {
				t.Fatalf("stop: %v", err)
			}
			router.WaitForPendingSettles()

			patches := filterItemEventPatches(emissions.snapshot())
			if len(patches) != 1 {
				t.Fatalf("patches = %d, want 1", len(patches))
			}
			patch := patches[0].Patch
			if tc.final != nil {
				if patch.Summary == nil || patch.StreamEnd != nil {
					t.Fatalf("patch = %+v, want a summary and no stream end", patch.ItemPatchFields)
				}
				return
			}
			if patch.Summary != nil {
				t.Fatalf("patch summary = %q, want the streamed summary kept", *patch.Summary)
			}
			want := int64(len("one twö three"))
			if patch.StreamEnd == nil || *patch.StreamEnd != want {
				t.Fatalf("patch stream end = %v, want %d", patch.StreamEnd, want)
			}
			settled, _, err := st.GetThreadItem("t1", patches[0].ItemID)
			if err != nil {
				t.Fatalf("get settled: %v", err)
			}
			if settled.StreamEnd != nil || settled.Rev != patch.Rev {
				t.Fatalf("settled row stream end = %v rev = %d, want none at the patch's rev %d", settled.StreamEnd, settled.Rev, patch.Rev)
			}
		})
	}
}
