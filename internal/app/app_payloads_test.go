package app

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/jstext/jstexttest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func seedPayloadOwner(t *testing.T, app *App, thread store.Thread, payload store.Payload, itemID string) {
	t.Helper()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", thread.ID, err)
	}
	item := store.Item{
		ID:          itemID,
		ThreadID:    thread.ID,
		TurnIndex:   1,
		Kind:        "tool_call",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "Bash: echo hi",
		ToolName:    "Bash",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.CreatedAt,
	}
	if _, err := app.store.UpsertItem(item, &payload); err != nil {
		t.Fatalf("UpsertItem(%s): %v", itemID, err)
	}
}

func TestGetPayloadBindingsRequireOwningThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	now := time.Now().UnixMilli()
	owner := testThread("thread-owner")
	other := testThread("thread-other")
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}

	payload := store.Payload{
		ID:        "payload-owned",
		Kind:      "command_output",
		Meta:      "{}",
		Data:      []byte("hello bytes"),
		CreatedAt: now,
	}
	if _, err := app.store.UpsertItem(store.Item{
		ID:          "owner-item",
		ThreadID:    owner.ID,
		TurnIndex:   1,
		Kind:        "tool_call",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "Bash: echo hi",
		ToolName:    "Bash",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, &payload); err != nil {
		t.Fatalf("UpsertItem(owner-item): %v", err)
	}

	got, err := app.GetPayloadData(owner.ID, payload.ID)
	if err != nil {
		t.Fatalf("GetPayloadData(owner): %v", err)
	}
	if got.Data != "hello bytes" {
		t.Fatalf("GetPayloadData(owner) = %q, want hello bytes", got.Data)
	}

	preview, err := app.GetPayloadPreview(owner.ID, payload.ID, 5)
	if err != nil {
		t.Fatalf("GetPayloadPreview(owner): %v", err)
	}
	if preview.Data != "hello" {
		t.Fatalf("GetPayloadPreview(owner) = %q, want hello", preview.Data)
	}

	chunk, err := app.GetPayloadChunk(owner.ID, payload.ID, 5, 64)
	if err != nil {
		t.Fatalf("GetPayloadChunk(owner): %v", err)
	}
	if chunk.Data != " bytes" {
		t.Fatalf("GetPayloadChunk(owner) = %q, want requested raw slice", chunk.Data)
	}

	if _, err := app.GetPayloadData(other.ID, payload.ID); err == nil {
		t.Fatal("GetPayloadData(other): err = nil, want ownership error")
	}
	if _, err := app.GetPayloadPreview(other.ID, payload.ID, 5); err == nil {
		t.Fatal("GetPayloadPreview(other): err = nil, want ownership error")
	}
	if _, err := app.GetPayloadChunk(other.ID, payload.ID, 0, 5); err == nil {
		t.Fatal("GetPayloadChunk(other): err = nil, want ownership error")
	}
}

func TestPayloadTextReadsStopBetweenCharacters(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	cases := map[string]string{
		"valid":   "aé€\U0001F600 naïve 日本語\U0001F389z",
		"invalid": "ok\xff\xe2\x82 mid \xf0\x9f\x98 \x80\x80 €\xe2",
	}
	for name, text := range cases {
		thread := testThread("thread-" + name)
		payload := store.Payload{ID: "payload-" + name, Kind: "command_output", Meta: "{}", Data: []byte(text), CreatedAt: time.Now().UnixMilli()}
		seedPayloadOwner(t, app, thread, payload, "item-"+name)
		want := jstexttest.JSONDelivered(t, text)
		for window := 1; window <= len(text)+1; window++ {
			preview, err := app.GetPayloadPreview(thread.ID, payload.ID, window)
			if err != nil {
				t.Fatalf("%s window %d: preview: %v", name, window, err)
			}
			if preview.NextOffset != len(preview.Data) || preview.TotalSize != len(text) {
				t.Fatalf("%s window %d: preview next %d of %d for %d bytes", name, window, preview.NextOffset, preview.TotalSize, len(preview.Data))
			}
			pieces := []string{preview.Data}
			next, complete := preview.NextOffset, preview.IsComplete
			for !complete {
				chunk, err := app.GetPayloadChunk(thread.ID, payload.ID, next, window)
				if err != nil {
					t.Fatalf("%s window %d: chunk at %d: %v", name, window, next, err)
				}
				if chunk.Offset != next || chunk.NextOffset != next+len(chunk.Data) || len(chunk.Data) == 0 {
					t.Fatalf("%s window %d: chunk at %d spans [%d, %d) with %d bytes", name, window, next, chunk.Offset, chunk.NextOffset, len(chunk.Data))
				}
				pieces = append(pieces, chunk.Data)
				next, complete = chunk.NextOffset, chunk.IsComplete
			}
			if next != len(text) {
				t.Fatalf("%s window %d: reads ended at %d of %d", name, window, next, len(text))
			}
			var got strings.Builder
			for i, piece := range pieces {
				if len(piece) > max(window, utf8.UTFMax) {
					t.Fatalf("%s window %d: piece %d holds %d bytes", name, window, i, len(piece))
				}
				if name == "valid" && !utf8.ValidString(piece) {
					t.Fatalf("%s window %d: piece %d %q splits a character", name, window, i, piece)
				}
				got.WriteString(jstexttest.JSONDelivered(t, piece))
			}
			if got.String() != want {
				t.Fatalf("%s window %d: a client reads %q, want %q", name, window, got.String(), want)
			}
		}
	}
}

func TestGetPayloadChunkReadsFromZeroForANegativeOffset(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := testThread("thread-negative")
	payload := store.Payload{ID: "payload-negative", Kind: "command_output", Meta: "{}", Data: []byte("hello bytes"), CreatedAt: time.Now().UnixMilli()}
	seedPayloadOwner(t, app, thread, payload, "item-negative")
	chunk, err := app.GetPayloadChunk(thread.ID, payload.ID, -3, 5)
	if err != nil {
		t.Fatalf("GetPayloadChunk: %v", err)
	}
	if chunk.Data != "hello" || chunk.Offset != 0 || chunk.NextOffset != 5 {
		t.Fatalf("chunk = %q spanning [%d, %d), want hello at [0, 5)", chunk.Data, chunk.Offset, chunk.NextOffset)
	}
}

func TestGetPayloadDataFlushesLiveThinkingBuffer(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread := testThread("thread-live-thinking")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   "first",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking first: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   " second",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking second: %v", err)
	}

	got, err := app.GetPayloadData(thread.ID, "thinking:think:0:1")
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got.Data != "first second" {
		t.Fatalf("GetPayloadData = %q, want flushed thinking payload", got.Data)
	}
}

// A read made while the delta is being emitted already holds that delta's
// text, so a live client never places a read before text it received.
func TestGetPayloadDataIncludesTheDeltaBeforeWireEmission(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind      string
		event     provider.EventKind
		payloadID func(itemID string) string
	}{
		{kind: "thinking", event: provider.EventThinking, payloadID: triage.ThinkingPayloadID},
		{kind: "assistant_text", event: provider.EventTextDelta, payloadID: triage.AssistantTextPayloadID},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			thread := testThread("thread-live-wire-order")
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}

			var payloadDuringDelta string
			app.triage = triage.NewRouter(app.store, func(eventName eventchan.Channel, data any) {
				if eventName != "provider:item_event" {
					return
				}
				evt, ok := data.(triage.ItemStreamEvent)
				if !ok || evt.Action != "delta" || evt.Kind != tc.kind {
					return
				}
				got, err := app.GetPayloadData(thread.ID, tc.payloadID(evt.ItemID))
				if err != nil {
					t.Fatalf("GetPayloadData during delta emission: %v", err)
				}
				payloadDuringDelta = got.Data
			})

			for _, content := range []string{"first", " second"} {
				if err := app.triage.Handle(provider.ProviderEvent{
					Kind:      tc.event,
					ThreadID:  thread.ID,
					Content:   content,
					Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("%s %q: %v", tc.kind, content, err)
				}
			}

			if payloadDuringDelta != "first second" {
				t.Fatalf("GetPayloadData during delta emission = %q, want the full visible text", payloadDuringDelta)
			}
		})
	}
}
