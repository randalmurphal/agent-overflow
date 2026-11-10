package claude

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionimport"
)

// thinkingSignature builds a signature carrying tag, by hand: field 2
// holds a message whose field 1 holds the tag in field 8. A signature from
// a real session is never committed.
func thinkingSignature(tag string) string {
	header := append([]byte{0x42, byte(len(tag))}, tag...)
	body := append([]byte{0x0a, byte(len(header))}, header...)
	return base64.StdEncoding.EncodeToString(append([]byte{0x12, byte(len(body))}, body...))
}

func streamLine(t *testing.T, scope string, event map[string]any) []byte {
	t.Helper()
	line := map[string]any{"type": "stream_event", "event": event}
	if scope != "" {
		line["parent_tool_use_id"] = scope
	}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal stream line: %v", err)
	}
	return raw
}

func parseOrFatal(t *testing.T, parser *Parser, line []byte) []provider.ProviderEvent {
	t.Helper()
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse %s: %v", line, err)
	}
	return events
}

// streamThinkingBlock streams one thinking block: its start, one thinking
// delta, the signature in the given chunks, and its stop. It returns the
// stop event and every event the signature chunks produced.
func streamThinkingBlock(t *testing.T, parser *Parser, scope string, index int, signatureChunks ...string) (stop provider.ProviderEvent, fromSignature []provider.ProviderEvent) {
	t.Helper()
	parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
	}))
	deltas := parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": "Reading the file."},
	}))
	if len(deltas) != 1 || deltas[0].Kind != provider.EventThinking || len(deltas[0].Meta) != 0 {
		t.Fatalf("thinking delta events = %+v, want one EventThinking with no meta", deltas)
	}
	for _, chunk := range signatureChunks {
		fromSignature = append(fromSignature, parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "signature_delta", "signature": chunk},
		}))...)
	}
	stops := parseOrFatal(t, parser, streamLine(t, scope, map[string]any{"type": "content_block_stop", "index": index}))
	if len(stops) != 1 || stops[0].Kind != provider.EventContentBlockStop {
		t.Fatalf("stop events = %+v, want one EventContentBlockStop", stops)
	}
	return stops[0], fromSignature
}

func stopIsNarration(t *testing.T, stop provider.ProviderEvent) bool {
	t.Helper()
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(stop.Meta, &meta); err != nil {
		t.Fatalf("stop meta %q: %v", stop.Meta, err)
	}
	raw, ok := meta[provider.MetaNarrationKey]
	if !ok {
		return false
	}
	var narration bool
	if err := json.Unmarshal(raw, &narration); err != nil || !narration {
		t.Fatalf("stop meta carries %s=%s, want true or no key", provider.MetaNarrationKey, raw)
	}
	return true
}

// The signature rides signature_delta after the thinking deltas. A block
// whose signature carries the narration tag stops with the marker; the
// signature is never an event of its own and never content.
func TestStreamedThinkingStopCarriesTheNarrationMarker(t *testing.T) {
	narration := thinkingSignature("narration")
	for _, scope := range []string{"", "toolu_agent"} {
		t.Run("scope "+scope, func(t *testing.T) {
			parser := NewParser()
			stop, fromSignature := streamThinkingBlock(t, parser, scope, 0, narration[:7], narration[7:])
			if len(fromSignature) != 0 {
				t.Fatalf("signature deltas emitted events: %+v", fromSignature)
			}
			if !stopIsNarration(t, stop) || contentBlockStopType(t, stop.Meta) != "thinking" {
				t.Fatalf("stop meta = %s, want a thinking stop carrying the marker", stop.Meta)
			}
			if stop.Content != "" || stop.ContentPresent || stop.ParentToolUseID != scope {
				t.Fatalf("stop = %+v, want no content in scope %q", stop, scope)
			}
			if len(parser.streamBlockSignatures) != 0 {
				t.Fatalf("the stop left %d signatures held", len(parser.streamBlockSignatures))
			}
		})
	}
}

// Every other signature leaves the stop exactly as it was before the
// signature was read.
func TestUntaggedThinkingStopIsUnchanged(t *testing.T) {
	want := string(blockMeta(3, "thinking", ""))
	for name, chunks := range map[string][]string{
		"no signature":        nil,
		"thinking tag":        {thinkingSignature("thinking")},
		"unknown tag":         {thinkingSignature("summary")},
		"not base64":          {"!!!"},
		"not a message":       {base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff})},
		"truncated":           {thinkingSignature("narration")[:12]},
		"narration then junk": {thinkingSignature("narration"), "AAAA!"},
	} {
		t.Run(name, func(t *testing.T) {
			stop, _ := streamThinkingBlock(t, NewParser(), "", 3, chunks...)
			if string(stop.Meta) != want {
				t.Fatalf("stop meta = %s, want %s", stop.Meta, want)
			}
		})
	}
}

// Only the thinking block the signature belongs to is marked.
func TestNarrationSignatureMarksOnlyItsOwnBlock(t *testing.T) {
	parser := NewParser()
	narration := thinkingSignature("narration")
	start := func(scope string, index int, blockType string) {
		parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
			"type": "content_block_start", "index": index, "content_block": map[string]any{"type": blockType},
		}))
	}
	sign := func(scope string, index int) {
		parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "signature_delta", "signature": narration},
		}))
	}
	stop := func(scope string, index int) provider.ProviderEvent {
		events := parseOrFatal(t, parser, streamLine(t, scope, map[string]any{"type": "content_block_stop", "index": index}))
		if len(events) != 1 {
			t.Fatalf("stop events = %+v", events)
		}
		return events[0]
	}

	// A text block is never narration, whatever arrives for its index.
	start("", 0, "text")
	sign("", 0)
	if stopIsNarration(t, stop("", 0)) {
		t.Fatal("a text block stopped with the narration marker")
	}
	// A signature with no open block is dropped.
	sign("", 5)
	if len(parser.streamBlockSignatures) != 0 {
		t.Fatalf("a signature with no open block is held: %d", len(parser.streamBlockSignatures))
	}
	// Two scopes stream the same index; only the signed one is marked.
	start("", 1, "thinking")
	start("toolu_agent", 1, "thinking")
	sign("toolu_agent", 1)
	if stopIsNarration(t, stop("", 1)) {
		t.Fatal("the top-level block took another scope's signature")
	}
	if !stopIsNarration(t, stop("toolu_agent", 1)) {
		t.Fatal("the signed block stopped without the marker")
	}
}

// A block that never stops releases its signature, so a later block at
// the same index is not marked by it and nothing accumulates.
func TestUnstoppedThinkingBlockReleasesItsSignature(t *testing.T) {
	narration := thinkingSignature("narration")
	open := func(t *testing.T, parser *Parser, scope string) {
		t.Helper()
		parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"},
		}))
		parseOrFatal(t, parser, streamLine(t, scope, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": narration},
		}))
		if len(parser.streamBlockSignatures) == 0 {
			t.Fatal("the open block's signature is not held")
		}
	}
	t.Run("message_stop", func(t *testing.T) {
		parser := NewParser()
		open(t, parser, "")
		open(t, parser, "toolu_agent")
		parseOrFatal(t, parser, streamLine(t, "", map[string]any{"type": "message_stop"}))
		if len(parser.streamBlockSignatures) != 1 {
			t.Fatalf("a top-level message_stop left %d signatures, want the other scope's one", len(parser.streamBlockSignatures))
		}
		stops := parseOrFatal(t, parser, streamLine(t, "", map[string]any{"type": "content_block_stop", "index": 0}))
		if stopIsNarration(t, stops[0]) {
			t.Fatal("a stop after message_stop carried the released signature")
		}
	})
	t.Run("next block start", func(t *testing.T) {
		parser := NewParser()
		open(t, parser, "")
		stop, _ := streamThinkingBlock(t, parser, "", 0)
		if stopIsNarration(t, stop) {
			t.Fatal("a new block at the index took the old block's signature")
		}
	})
	t.Run("turn boundary", func(t *testing.T) {
		parser := NewParser()
		open(t, parser, "")
		parseOrFatal(t, parser, []byte(`{"type":"result","subtype":"success","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		if parser.streamBlockSignatures != nil {
			t.Fatalf("the turn boundary left %d signatures", len(parser.streamBlockSignatures))
		}
	})
	t.Run("close", func(t *testing.T) {
		parser := NewParser()
		open(t, parser, "")
		parser.Close()
		if parser.streamBlockSignatures != nil {
			t.Fatalf("Close left %d signatures", len(parser.streamBlockSignatures))
		}
	})
	t.Run("block cap", func(t *testing.T) {
		parser := NewParser()
		open(t, parser, "")
		for i := 1; i <= parserStreamBlockCap; i++ {
			parseOrFatal(t, parser, streamLine(t, fmt.Sprintf("toolu_%d", i), map[string]any{
				"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"},
			}))
		}
		if len(parser.streamBlockSignatures) != 0 {
			t.Fatalf("the block cap's reset left %d signatures", len(parser.streamBlockSignatures))
		}
	})
}

// A signature past the cap is dropped whole: the block reads as untagged
// and holds no bytes, whatever arrives after.
func TestOversizedThinkingSignatureIsDropped(t *testing.T) {
	parser := NewParser()
	narration := thinkingSignature("narration")
	filler := strings.Repeat("A", sessionimport.MaxThinkingSignatureBytes)
	stop, _ := streamThinkingBlock(t, parser, "", 0, filler, narration)
	if stopIsNarration(t, stop) {
		t.Fatal("an oversized signature marked its block")
	}

	parseOrFatal(t, parser, streamLine(t, "", map[string]any{
		"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"},
	}))
	for _, chunk := range []string{filler, narration, narration} {
		parseOrFatal(t, parser, streamLine(t, "", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": chunk},
		}))
	}
	held := parser.streamBlockSignatures[streamBlockRef{"", 0}]
	if held == nil || !held.dropped || held.data.Len() != 0 {
		t.Fatalf("the oversized signature is still held: %+v", held)
	}
}

// A block start that already carries its signature seeds it.
func TestThinkingBlockStartSeedsItsSignature(t *testing.T) {
	parser := NewParser()
	parseOrFatal(t, parser, streamLine(t, "", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": thinkingSignature("narration")},
	}))
	stops := parseOrFatal(t, parser, streamLine(t, "", map[string]any{"type": "content_block_stop", "index": 0}))
	if len(stops) != 1 || !stopIsNarration(t, stops[0]) {
		t.Fatalf("stop = %+v, want the marker", stops)
	}
}

// A thinking block recovered from a never-streamed assistant message
// carries its signature whole. Subagent blocks always arrive this way.
func TestRecoveredThinkingBlockCarriesTheNarrationMarker(t *testing.T) {
	for _, scope := range []string{"", "toolu_agent"} {
		for tag, want := range map[string]bool{"narration": true, "thinking": false, "": false} {
			t.Run(fmt.Sprintf("scope %q tag %q", scope, tag), func(t *testing.T) {
				block := map[string]any{"type": "thinking", "thinking": "Reading the file."}
				if tag != "" {
					block["signature"] = thinkingSignature(tag)
				}
				line := map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"id": "msg_retry", "role": "assistant",
						"content": []any{block, map[string]any{"type": "text", "text": "Done."}},
					},
				}
				if scope != "" {
					line["parent_tool_use_id"] = scope
				}
				raw, err := json.Marshal(line)
				if err != nil {
					t.Fatal(err)
				}
				var thinking, text *provider.ProviderEvent
				for _, evt := range parseOrFatal(t, NewParser(), raw) {
					if evt.Kind != provider.EventContentBlockStop {
						continue
					}
					switch contentBlockStopType(t, evt.Meta) {
					case "thinking":
						thinking = &evt
					case "text":
						text = &evt
					}
				}
				if thinking == nil || text == nil {
					t.Fatal("the message's blocks were not recovered")
				}
				if !thinking.ContentPresent || thinking.Content != "Reading the file." || thinking.ItemID == "" {
					t.Fatalf("recovered thinking = %+v", *thinking)
				}
				if got := stopIsNarration(t, *thinking); got != want {
					t.Fatalf("recovered thinking narration = %v, want %v (meta %s)", got, want, thinking.Meta)
				}
				if !want && string(thinking.Meta) != string(blockMeta(0, "thinking", "msg_retry")) {
					t.Fatalf("an untagged recovered block's meta changed: %s", thinking.Meta)
				}
				if stopIsNarration(t, *text) {
					t.Fatal("the text block was recovered with the marker")
				}
			})
		}
	}
}

// The 2.1.132 capture predates the tag: replaying it marks no block.
func TestPreTagThinkingFixtureMarksNoBlock(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/references/fixtures/claude/opus47_thinking_summarized.ndjson")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parser := NewParser()
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	thinkingStops := 0
	for scanner.Scan() {
		for _, evt := range parseOrFatal(t, parser, append([]byte(nil), scanner.Bytes()...)) {
			if evt.Kind != provider.EventContentBlockStop {
				continue
			}
			if stopIsNarration(t, evt) {
				t.Fatalf("the fixture marked a block: %s", evt.Meta)
			}
			if contentBlockStopType(t, evt.Meta) == "thinking" {
				thinkingStops++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if thinkingStops == 0 {
		t.Fatal("the fixture stopped no thinking block")
	}
}

// A mirrored transcript thinking block keeps its narration marker beside
// the snapshot marker the mirror adds.
func TestTranscriptMirrorKeepsTheNarrationMarker(t *testing.T) {
	parser := NewParser()
	parse := func(line string) []provider.ProviderEvent {
		t.Helper()
		return parseOrFatal(t, parser, []byte(line))
	}
	parse(`{"type":"assistant","message":{"id":"m-parent","role":"assistant","content":[{"type":"tool_use","id":"toolu-agent","name":"Agent","input":{"description":"review","subagent_type":"Explore","prompt":"inspect"}}]}}`)
	parse(`{"type":"system","subtype":"task_started","task_id":"agent-bg","task_type":"local_agent","tool_use_id":"toolu-agent"}`)
	parse(`{"type":"system","subtype":"task_updated","task_id":"agent-bg","patch":{"is_backgrounded":true}}`)

	block := func(text, tag string) string {
		return `{"type":"thinking","thinking":"` + text + `","signature":"` + thinkingSignature(tag) + `"}`
	}
	events := parse(`{"type":"transcript_mirror","filePath":"/tmp/agent-agent-bg.jsonl","entries":[{"type":"assistant","uuid":"a-bg","agentId":"agent-bg","isSidechain":true,"timestamp":"2026-08-24T12:00:01Z","message":{"id":"msg-bg","role":"assistant","model":"claude-opus-4-1","content":[` +
		block("Read the file; one line changes.", "narration") + `,` + block("hmm", "thinking") + `]}}]}`)
	var marked []bool
	for _, evt := range events {
		if evt.Kind != provider.EventThinking {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("thinking meta %s: %v", evt.Meta, err)
		}
		if meta[provider.MetaTranscriptSnapshotKey] != true || evt.ParentToolUseID != "toolu-agent" || evt.ItemID == "" {
			t.Fatalf("mirrored thinking = %+v", evt)
		}
		narration, present := meta[provider.MetaNarrationKey]
		if present && narration != true {
			t.Fatalf("mirrored thinking carries %s=%v", provider.MetaNarrationKey, narration)
		}
		marked = append(marked, present)
	}
	if len(marked) != 2 || !marked[0] || marked[1] {
		t.Fatalf("mirrored thinking markers = %v, want the first block only", marked)
	}
}
