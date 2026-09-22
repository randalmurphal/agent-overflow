package rollout

import (
	"testing"

	"agent-overflow/internal/provider"
)

func TestAsyncQuestionsImportNativeAndLegacy(t *testing.T) {
	call := `{"timestamp":"2026-08-07T19:07:50Z","type":"response_item","payload":{"type":"function_call","call_id":"question-call","name":"request_user_input_async","arguments":"{\"questions\":[{\"title\":\"Which scope?\",\"options\":[\"One\",\"Two\"]},{\"title\":\"When?\"}]}"}}`
	accepted := `{"timestamp":"2026-08-07T19:07:51Z","type":"response_item","payload":{"type":"function_call_output","call_id":"question-call","output":"{\"accepted\":true}"}}`
	typed := `{"timestamp":"2026-08-07T19:07:51Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"AgentMessage","id":"question-call","delivery":"async","questions":[{"title":"Which scope?","options":["One","Two"]},{"title":"When?"}]}}}`
	legacyMirror := `{"timestamp":"2026-08-07T19:07:51Z","type":"event_msg","payload":{"type":"agent_message","message":"Which scope?\n- One\n- Two\n\nWhen?","phase":"final_answer","delivery":"async","questions":[{"title":"Which scope?","options":["One","Two"]},{"title":"When?"}]}}`
	for name, lines := range map[string][]string{
		"legacy": {call, legacyMirror, accepted}, "native": {call, typed, accepted}, "item-before-call": {typed, call, accepted}, "mirrors-before-item": {call, accepted, typed},
	} {
		t.Run(name, func(t *testing.T) {
			metadata := paginatedMetaLine
			if name == "legacy" {
				metadata = metaLine
			}
			fixture := append([]string{metadata, taskStartedLine}, lines...)
			fixture = append(fixture, taskCompleteLn)
			res := parseFixture(t, writeRollout(t, testSessionID, fixture...))
			blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
			if len(blocks) != 1 || blocks[0].ItemID != "question-call" || metaField(t, blocks[0].Meta, "delivery") != "async" {
				t.Fatalf("question projection: %+v", blocks)
			}
			if countKind(res.Events, provider.EventToolStart) != 0 || countKind(res.Events, provider.EventToolComplete) != 0 || len(res.Warnings) != 0 {
				t.Fatalf("duplicate tools or warnings: %+v", res)
			}
			if countKind(res.Events, provider.EventTextDelta) != 0 {
				t.Fatal("question mirror also rendered as prose")
			}
		})
	}
	failed := `{"timestamp":"2026-08-07T19:07:51Z","type":"response_item","payload":{"type":"function_call_output","call_id":"question-call","output":"invalid question","success":false}}`
	for _, tail := range []string{failed, ""} {
		lines := []string{paginatedMetaLine, taskStartedLine, call}
		if tail != "" {
			lines = append(lines, tail)
		}
		lines = append(lines, taskCompleteLn)
		res := parseFixture(t, writeRollout(t, testSessionID, lines...))
		if countKind(res.Events, provider.EventContentBlockStop) != 0 || countKind(res.Events, provider.EventToolStart) != 1 || countKind(res.Events, provider.EventToolComplete) != 1 {
			t.Fatalf("failed call was hidden or became a prompt: %+v", res)
		}
	}
}
