package sessionimport

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestConvertAssistantAPIError(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("Rate limit reached")}, "2026-01-01T00:00:01.000Z",
			with("message", map[string]any{
				"role": "assistant", "id": "msg_1", "model": "claude-test-1",
				"error":   "rate_limit",
				"content": []any{textBlock("Rate limit reached")},
			})),
	)
	errs := eventsOfKind(events, provider.EventError)
	if len(errs) != 1 {
		t.Fatalf("got %d error events, want 1: %s", len(errs), renderEvents(events))
	}
	meta := decodeMeta(t, errs[0].Meta)
	if meta["api_error_enum"] != "rate_limit" {
		t.Errorf("api_error_enum = %v, want rate_limit (drives the api_error row kind)", meta)
	}
	if errs[0].Content != "Rate limit reached" {
		t.Errorf("content = %q", errs[0].Content)
	}
	// The error copy must not ALSO become an assistant bubble.
	if len(eventsOfKind(events, provider.EventTextDelta)) != 0 {
		t.Errorf("error envelope text leaked as assistant content: %s", renderEvents(events))
	}
}

func TestConvertCLIErrorRow(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("API Error: 529")}, "2026-01-01T00:00:01.000Z",
			with("isApiErrorMessage", true), with("apiErrorStatus", 529)),
	)
	errs := eventsOfKind(events, provider.EventError)
	if len(errs) != 1 {
		t.Fatalf("got %d error events, want 1", len(errs))
	}
	if meta := decodeMeta(t, errs[0].Meta); meta["api_error_enum"] != "529" {
		t.Errorf("api_error_enum = %v, want 529", meta["api_error_enum"])
	}
}

func TestConvertSyntheticCommandOutput(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "/usage", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("Usage report")}, "2026-01-01T00:00:01.000Z",
			with("message", map[string]any{
				"role": "assistant", "id": "msg_1", "model": syntheticCLIModel,
				"content": []any{textBlock("Usage report")},
			})),
	)
	results := eventsOfKind(events, provider.EventCommandResult)
	if len(results) != 1 || results[0].Content != "Usage report" {
		t.Fatalf("command results = %s", renderEvents(events))
	}
	if len(eventsOfKind(events, provider.EventTextDelta)) != 0 {
		t.Errorf("CLI output leaked as an assistant bubble: %s", renderEvents(events))
	}
}

func TestConvertSystemRows(t *testing.T) {
	events, warnings := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		map[string]any{"type": "system", "subtype": "local_command", "uuid": "s1", "parentUuid": "u1",
			"isSidechain": false, "content": "command output", "timestamp": "2026-01-01T00:00:01.000Z"},
		map[string]any{"type": "system", "subtype": "api_error", "uuid": "s2", "parentUuid": "s1",
			"isSidechain": false, "content": "API Error: overloaded", "timestamp": "2026-01-01T00:00:02.000Z"},
		map[string]any{"type": "system", "subtype": "model_refusal_fallback", "uuid": "s3", "parentUuid": "s2",
			"isSidechain": false, "content": "Falling back", "timestamp": "2026-01-01T00:00:03.000Z"},
		map[string]any{"type": "system", "subtype": "brand_new_thing", "uuid": "s4", "parentUuid": "s3",
			"isSidechain": false, "content": "?", "timestamp": "2026-01-01T00:00:09.000Z"},
		map[string]any{"type": "system", "subtype": "turn_duration", "uuid": "s5", "parentUuid": "s4",
			"isSidechain": false, "durationMs": 1234, "timestamp": "2026-01-01T00:00:10.000Z"},
	)
	if n := len(eventsOfKind(events, provider.EventCommandResult)); n != 1 {
		t.Errorf("command_result events = %d, want 1", n)
	}
	if n := len(eventsOfKind(events, provider.EventError)); n != 1 {
		t.Errorf("error events = %d, want 1", n)
	}
	if n := len(eventsOfKind(events, provider.EventNotification)); n != 1 {
		t.Errorf("notification events = %d, want 1", n)
	}
	if !hasWarning(warnings, WarnUnknownSystemSubtype) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnUnknownSystemSubtype)
	}
	// turn_duration writes no item of its own, but its timestamp is the
	// turn's real end and must land on the synthesised completion.
	for _, e := range events {
		if e.SourceUUID == "s5" && e.Kind != provider.EventTurnComplete {
			t.Errorf("turn_duration produced an item event: %s", renderEvents(events))
		}
	}
	complete := eventsOfKind(events, provider.EventTurnComplete)[0]
	if want := parseISOMillis("2026-01-01T00:00:10.000Z"); complete.Timestamp.UnixMilli() != want {
		t.Errorf("turn completed at %d, want the turn_duration row time %d", complete.Timestamp.UnixMilli(), want)
	}
}

func TestConvertDropsAttachmentRows(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		map[string]any{
			"type": "attachment", "uuid": "at1", "parentUuid": "u1", "isSidechain": false,
			"timestamp":  "2026-01-01T00:00:01.000Z",
			"attachment": map[string]any{"type": "deferred_tools_delta", "addedNames": []any{"WebFetch"}},
		},
		assistantRow("a1", "at1", "msg_1", []any{textBlock("ok")}, "2026-01-01T00:00:02.000Z"),
	)
	for _, e := range events {
		if e.SourceUUID == "at1" {
			t.Errorf("attachment row produced an event: %s", renderEvents(events))
		}
	}
	// It must still be transparent in the chain — the assistant reply survives.
	if len(eventsOfKind(events, provider.EventTextDelta)) != 1 {
		t.Errorf("assistant reply lost behind the attachment: %s", renderEvents(events))
	}
}

func TestConvertDropsToolResultWithNoToolUseID(t *testing.T) {
	events, warnings := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		userBlocksRow("r1", "u1", []any{
			map[string]any{"type": "tool_result", "content": "orphan"},
		}, "2026-01-01T00:00:01.000Z"),
	)
	if n := len(eventsOfKind(events, provider.EventToolComplete)); n != 0 {
		t.Errorf("tool_complete events = %d, want 0", n)
	}
	if !hasWarning(warnings, WarnUncorrelatedResult) {
		t.Errorf("warnings = %+v, want %s", warnings, WarnUncorrelatedResult)
	}
}

func TestConvertToolResultBlockContentShapes(t *testing.T) {
	cases := []struct {
		name    string
		content any
		want    string
	}{
		{"string", "plain output", "plain output"},
		{"text blocks", []any{textBlock("one "), textBlock("two")}, "one two"},
		{"structured object", map[string]any{"rows": 3}, `{"rows":3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := convertFixture(t, ConvertOptions{},
				userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
				assistantRow("a1", "u1", "msg_1", []any{
					toolUseBlock("toolu_1", "Bash", map[string]any{"command": "x"}),
				}, "2026-01-01T00:00:01.000Z"),
				userBlocksRow("r1", "a1", []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": tc.content},
				}, "2026-01-01T00:00:02.000Z"),
			)
			got := eventsOfKind(events, provider.EventToolComplete)[0].Content
			if got != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertAccumulatesUsagePerModel(t *testing.T) {
	usage := func(in, out int) map[string]any {
		return map[string]any{"input_tokens": in, "output_tokens": out}
	}
	assistantWithUsage := func(uuid, parent, id, model string, u map[string]any, ts string) map[string]any {
		return assistantRow(uuid, parent, id, []any{textBlock("x")}, ts,
			with("message", map[string]any{
				"role": "assistant", "id": id, "model": model,
				"content": []any{textBlock("x")}, "usage": u,
			}))
	}
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		assistantWithUsage("a1", "u1", "msg_1", "model-a", usage(10, 1), "2026-01-01T00:00:01.000Z"),
		assistantWithUsage("a2", "a1", "msg_2", "model-a", usage(20, 2), "2026-01-01T00:00:02.000Z"),
		assistantWithUsage("a3", "a2", "msg_3", "model-b", usage(5, 3), "2026-01-01T00:00:03.000Z"),
	)
	meta := eventsOfKind(events, provider.EventTurnComplete)[0].TurnComplete.(*provider.WireTurnCompleteMeta)
	if len(meta.ModelUsage) != 2 {
		t.Fatalf("modelUsage = %+v, want one row per model", meta.ModelUsage)
	}
	if meta.ModelUsage[0].Model != "model-a" || meta.ModelUsage[0].InputTokens != 30 {
		t.Errorf("model-a usage = %+v, want the summed delta", meta.ModelUsage[0])
	}
	if meta.Usage == nil || meta.Usage.InputTokens != 35 || meta.Usage.OutputTokens != 6 {
		t.Errorf("aggregate usage = %+v", meta.Usage)
	}
}

func TestConvertKeepsSourceUUIDOnEveryEvent(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "go", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("ok")}, "2026-01-01T00:00:01.000Z"),
	)
	for _, e := range events {
		if e.SourceUUID == "" {
			t.Errorf("event %s has no SourceUUID — refresh could not resume from it", e.Kind)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("event %s has no timestamp — import must never restamp now()", e.Kind)
		}
	}
}
