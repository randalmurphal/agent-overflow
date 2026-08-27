package rollout

import (
	"context"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func TestParseRealtimeItemsPreservesTranscriptWithoutDuplicatingPromotions(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine,
		`{"timestamp":"2026-08-07T19:07:45.000Z","ordinal":1,"type":"realtime_item","payload":{"id":"rt-start","realtime_session_id":"voice-1","type":"realtime_session_started"}}`,
		`{"timestamp":"2026-08-07T19:07:46.000Z","ordinal":2,"type":"realtime_item","payload":{"id":"rt-user","realtime_session_id":"voice-1","type":"transcript_segment","role":"user","text":"inspect the parser"}}`,
		taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:47.000Z","ordinal":4,"type":"realtime_item","payload":{"id":"rt-assistant","realtime_session_id":"voice-1","type":"transcript_segment","role":"assistant","text":"I am checking it."}}`,
		pagAgentMirrorLine,
		`{"timestamp":"2026-08-07T19:07:48.000Z","ordinal":6,"type":"realtime_item","payload":{"id":"rt-promote","realtime_session_id":"voice-1","type":"bem_item_promoted","turn_id":"turn-1","item_id":"m1","presentation":{"type":"whole_item"}}}`,
		`{"timestamp":"2026-08-07T19:07:49.000Z","ordinal":7,"type":"realtime_item","payload":{"id":"rt-close","realtime_session_id":"voice-1","type":"realtime_session_closed","outcome":"ended"}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)
	if len(res.UnknownTypes) != 0 || res.CorruptLines != 0 {
		t.Fatalf("parse diagnostics: unknown=%v corrupt=%d", res.UnknownTypes, res.CorruptLines)
	}
	users := eventsOfKind(res.Events, provider.EventUserText)
	if len(users) != 1 || users[0].Content != "inspect the parser" {
		t.Fatalf("voice user transcript = %+v", users)
	}
	assistants := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(assistants) != 2 || assistants[0].Content != "I am checking it." || assistants[1].Content != "done" {
		t.Fatalf("assistant transcript = %v", contents(assistants))
	}
	for _, event := range res.Events {
		if event.Kind == provider.EventNotification && strings.Contains(event.Content, "realtime") {
			t.Fatalf("realtime bookkeeping leaked into timeline: %+v", event)
		}
	}
}

func TestParseRealtimeTranscriptFromTailCursor(t *testing.T) {
	segment := `{"timestamp":"2026-08-07T19:07:47.000Z","ordinal":3,"type":"realtime_item","payload":{"id":"rt-user","realtime_session_id":"voice-1","type":"transcript_segment","role":"user","text":"tail speech"}}`
	path := writeRollout(t, testSessionID, paginatedMetaLine, taskStartedLine, segment, taskCompleteLn)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	cursor := int64(strings.Index(string(contents), segment))
	if cursor <= 0 {
		t.Fatalf("segment cursor = %d", cursor)
	}
	res, err := Parse(context.Background(), ParseOptions{Path: path, SessionID: testSessionID, FromOffset: cursor})
	if err != nil {
		t.Fatalf("tail parse: %v", err)
	}
	users := eventsOfKind(res.Events, provider.EventUserText)
	if len(users) != 1 || users[0].Content != "tail speech" {
		t.Fatalf("tail transcript = %+v", users)
	}
}

func TestParseRealtimeItemReportsFutureSubtypeAndChangedDroppedShape(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine,
		`{"timestamp":"2026-08-07T19:07:45.000Z","ordinal":1,"type":"realtime_item","payload":{"id":"rt-new","realtime_session_id":"voice-1","type":"future_audio_marker"}}`,
		`{"timestamp":"2026-08-07T19:07:46.000Z","ordinal":2,"type":"realtime_item","payload":{"id":"rt-promote","realtime_session_id":"voice-1","type":"bem_item_promoted","turn_id":"turn-1","item_id":"m1","presentation":{"type":"inline_visualization"}}}`,
	)
	res := parseFixture(t, path)
	if res.UnknownTypes["realtime_item/future_audio_marker"] != 1 {
		t.Fatalf("future subtype not reported: %v", res.UnknownTypes)
	}
	if res.UnknownTypes["realtime_item/bem_item_promoted (unexpected shape)"] != 1 {
		t.Fatalf("changed promotion shape not reported: %v", res.UnknownTypes)
	}
}

func TestParsePaginatedCompletedSubagentActivitySettlesSpawnWithoutNotificationRow(t *testing.T) {
	started := pagLine(t, 3, "2026-08-07T19:07:47.000Z", map[string]any{
		"type": "SubAgentActivity", "id": "spawn-1", "kind": "started",
		"agent_thread_id": "child-1", "agent_path": "/root/reviewer",
	}, 1786133867000, 1786133867100)
	spawn := `{"timestamp":"2026-08-07T19:07:47.100Z","ordinal":4,"type":"response_item","payload":{"type":"function_call","id":"fc1","name":"spawn_agent","arguments":"{\"task\":\"review\"}","call_id":"spawn-1"}}`
	completed := pagLine(t, 5, "2026-08-07T19:07:48.000Z", map[string]any{
		"type": "SubAgentActivity", "id": "subagent-completed-child-1", "kind": "completed",
		"agent_thread_id": "child-1", "agent_path": "/root/reviewer",
	}, 1786133868000, 1786133868100)
	res := parseFixture(t, writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine, started, spawn, completed, taskCompleteLn))

	statuses := eventsOfKind(res.Events, provider.EventSubagentStatus)
	if len(statuses) != 1 || statuses[0].ItemID != "spawn-1" || statuses[0].ParentToolUseID != "spawn-1" {
		t.Fatalf("completion statuses = %+v", statuses)
	}
	if !strings.Contains(string(statuses[0].Meta), `"status":"completed"`) {
		t.Fatalf("completion meta = %s", statuses[0].Meta)
	}
	starts := eventsOfKind(res.Events, provider.EventToolStart)
	if len(starts) != 1 || !strings.Contains(string(starts[0].Meta), `"receiverThreadIds":["child-1"]`) ||
		!strings.Contains(string(starts[0].Meta), `"is_background":true`) {
		t.Fatalf("spawn start is missing child ownership: %+v", starts)
	}
	for _, event := range eventsOfKind(res.Events, provider.EventNotification) {
		if strings.Contains(event.Content, "completed") {
			t.Fatalf("completion produced duplicate notification row: %+v", event)
		}
	}
}
