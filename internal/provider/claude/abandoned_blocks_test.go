package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

const (
	fixtureStreamAbandonedMidblock   = "../../../docs/references/fixtures/claude/stream_abandoned_midblock_20261016.ndjson"
	fixtureStreamAbandonedTwoblock   = "../../../docs/references/fixtures/claude/stream_abandoned_twoblock_20261016.ndjson"
	fixtureStreamAbandonedAfterblock = "../../../docs/references/fixtures/claude/stream_abandoned_afterblock_20261016.ndjson"
)

func decodeBlocksAbandoned(t *testing.T, evt provider.ProviderEvent) provider.BlocksAbandonedMeta {
	t.Helper()
	var meta provider.BlocksAbandonedMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("blocks abandoned meta %s: %v", evt.Meta, err)
	}
	return meta
}

func metaStringField(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("meta %s: %v", raw, err)
	}
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func eventsOfKind(events []provider.ProviderEvent, kind provider.EventKind) []provider.ProviderEvent {
	var out []provider.ProviderEvent
	for _, evt := range events {
		if evt.Kind == kind {
			out = append(out, evt)
		}
	}
	return out
}

// A message_stop the CLI produced itself carries `abandoned_blocks` on the
// envelope, beside `api_message_id`; it becomes one EventBlocksAbandoned.
// The API's own message_stop carries neither and emits nothing (pinned by
// TestParseStreamEventMessageBoundariesDoNotEmitLifecycleEvents).
func TestParseStreamEventMessageStopAbandonedBlocks(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"message_stop"},"session_id":"s","parent_tool_use_id":null,"uuid":"u","api_message_id":"msg_01","abandoned_blocks":{"api_message_id":"msg_01","from_block_index":1}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventBlocksAbandoned {
		t.Fatalf("events: %+v", events)
	}
	meta := decodeBlocksAbandoned(t, events[0])
	if meta.APIMessageID != "msg_01" || meta.FromBlockIndex != 1 {
		t.Errorf("meta %+v", meta)
	}
	if events[0].ParentToolUseID != "" {
		t.Errorf("parent %q", events[0].ParentToolUseID)
	}
}

// The scope passes through: a scoped signal marks that scope's rows.
func TestParseStreamEventAbandonedBlocksKeepsScope(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"message_stop"},"parent_tool_use_id":"toolu_1","api_message_id":"msg_sub","abandoned_blocks":{"api_message_id":"msg_sub","from_block_index":0}}`)
	events, err := ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].ParentToolUseID != "toolu_1" {
		t.Fatalf("events: %+v", events)
	}
}

// Block lifecycle events name their response. The envelope's
// `api_message_id` wins; without it (older CLIs, the mock provider) the id
// comes from the scope's message_start, and a message_stop releases it.
func TestParseStreamEventBlockMetaCarriesAPIMessageID(t *testing.T) {
	p := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		return events
	}
	parse(`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_start","role":"assistant"}}}`)
	starts := parse(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
	if len(starts) != 1 || metaStringField(t, starts[0].Meta, provider.MetaAPIMessageIDKey) != "msg_start" {
		t.Fatalf("start without envelope id: %+v", starts)
	}
	stamped := parse(`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"api_message_id":"msg_envelope"}`)
	if len(stamped) != 1 || metaStringField(t, stamped[0].Meta, provider.MetaAPIMessageIDKey) != "msg_envelope" {
		t.Fatalf("stop with envelope id: %+v", stamped)
	}
	parse(`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`)
	after := parse(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
	if len(after) != 1 || metaStringField(t, after[0].Meta, provider.MetaAPIMessageIDKey) != "" {
		t.Fatalf("start after message_stop still names a response: %+v", after)
	}
}

// A subagent scope keeps its own response id.
func TestParseStreamEventBlockMetaAPIMessageIDIsPerScope(t *testing.T) {
	p := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		return events
	}
	parse(`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg_main","role":"assistant"}}}`)
	parse(`{"type":"stream_event","event":"message_start","parent_tool_use_id":"toolu_1","data":{"type":"message_start","message":{"id":"msg_sub","role":"assistant"}}}`)
	main := parse(`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
	sub := parse(`{"type":"stream_event","event":"content_block_start","parent_tool_use_id":"toolu_1","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
	if got := metaStringField(t, main[0].Meta, provider.MetaAPIMessageIDKey); got != "msg_main" {
		t.Errorf("main scope id %q", got)
	}
	if got := metaStringField(t, sub[0].Meta, provider.MetaAPIMessageIDKey); got != "msg_sub" {
		t.Errorf("sub scope id %q", got)
	}
}

// Replay of the captured stalls (Claude Code 2.1.300, fake API on
// loopback, CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS at its 10 s floor).
//
// midblock: the stall fell inside block 0, so the whole response is
// abandoned (from 0) and the retry arrives as a bare assistant snapshot
// under a new id, recovered as a completed block.
func TestReplayStreamAbandonedMidblock(t *testing.T) {
	events := replayFixture(t, fixtureStreamAbandonedMidblock)
	abandoned := eventsOfKind(events, provider.EventBlocksAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("blocks abandoned events: %d", len(abandoned))
	}
	if meta := decodeBlocksAbandoned(t, abandoned[0]); meta.APIMessageID != "msg_fake_01" || meta.FromBlockIndex != 0 {
		t.Errorf("meta %+v", meta)
	}
	starts := eventsOfKind(events, provider.EventContentBlockStart)
	if len(starts) != 1 || metaStringField(t, starts[0].Meta, provider.MetaAPIMessageIDKey) != "msg_fake_01" {
		t.Fatalf("block starts: %+v", starts)
	}
	// The abandonment precedes the retry's recovered block, which carries
	// the new response id and the full text.
	var abandonedAt, recoveredAt = -1, -1
	for i, evt := range events {
		switch {
		case evt.Kind == provider.EventBlocksAbandoned:
			abandonedAt = i
		case evt.Kind == provider.EventContentBlockStop && evt.ContentPresent:
			recoveredAt = i
			if metaStringField(t, evt.Meta, provider.MetaAPIMessageIDKey) != "msg_fake_02" {
				t.Errorf("recovered block meta %s", evt.Meta)
			}
			if evt.Content == "" || evt.Content[:8] != "Retried " {
				t.Errorf("recovered content %q", evt.Content)
			}
		}
	}
	if abandonedAt < 0 || recoveredAt < abandonedAt {
		t.Errorf("order: abandoned at %d, recovered retry at %d", abandonedAt, recoveredAt)
	}
}

// twoblock: block 0 finished and the stall cut block 1, so block 1 alone
// is abandoned (from 1) and the continuation streams under msg_fake_02.
func TestReplayStreamAbandonedTwoblock(t *testing.T) {
	events := replayFixture(t, fixtureStreamAbandonedTwoblock)
	abandoned := eventsOfKind(events, provider.EventBlocksAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("blocks abandoned events: %d", len(abandoned))
	}
	if meta := decodeBlocksAbandoned(t, abandoned[0]); meta.APIMessageID != "msg_fake_01" || meta.FromBlockIndex != 1 {
		t.Errorf("meta %+v", meta)
	}
	var ids []string
	for _, evt := range eventsOfKind(events, provider.EventContentBlockStart) {
		ids = append(ids, metaStringField(t, evt.Meta, provider.MetaAPIMessageIDKey))
	}
	if want := []string{"msg_fake_01", "msg_fake_01", "msg_fake_02"}; len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Errorf("block start response ids %v, want %v", ids, want)
	}
	// The second response's snapshot was streamed and is not recovered a
	// second time.
	for _, evt := range eventsOfKind(events, provider.EventContentBlockStop) {
		if evt.ContentPresent {
			t.Errorf("streamed response recovered as a snapshot: %q", evt.Content)
		}
	}
}

// afterblock: block 0 finished and the stall fell before block 1 started.
// The CLI still reports from 1; nothing streamed at that index, which is
// triage's call (TestBlocksAbandonedBeyondSeenBlocksWritesNothing).
func TestReplayStreamAbandonedAfterblock(t *testing.T) {
	events := replayFixture(t, fixtureStreamAbandonedAfterblock)
	abandoned := eventsOfKind(events, provider.EventBlocksAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("blocks abandoned events: %d", len(abandoned))
	}
	if meta := decodeBlocksAbandoned(t, abandoned[0]); meta.APIMessageID != "msg_fake_01" || meta.FromBlockIndex != 1 {
		t.Errorf("meta %+v", meta)
	}
	if starts := eventsOfKind(events, provider.EventContentBlockStart); len(starts) != 2 {
		t.Errorf("block starts: %d", len(starts))
	}
}
