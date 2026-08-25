package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func TestDispatchLineSubagentNotificationUsesAgentPathMapping(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread:    make(map[string]string),
			childParentByAgentPath: map[string]string{"/root/researcher": "call-collab-1"},
			agentPathByThread:      make(map[string]string),
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"/root/researcher\",\"status\":{\"completed\":\"done\"}}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind != provider.EventSubagentNotification {
			continue
		}
		if evt.ItemID != "call-collab-1" {
			t.Fatalf("ItemID: got %q, want call-collab-1", evt.ItemID)
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("meta unmarshal: %v", err)
		}
		if meta["status"] != "completed" {
			t.Fatalf("meta.status: got %v, want completed", meta["status"])
		}
		if meta["message"] != "done" {
			t.Fatalf("meta.message: got %v, want done", meta["message"])
		}
		return
	}
	t.Fatalf("expected EventSubagentNotification, got %+v", events)
}

// -- subagent notification parser tests --

// TestParseSubagentNotifications_SingleTag pins the canonical wire
// shape emitted by codex-source
// (core/src/session_prefix.rs::format_subagent_notification_message):
// a single
// <subagent_notification>{"agent_path":..,"status":..}</subagent_notification>
// block whose JSON body round-trips to a subagentNotification value.
func TestParseSubagentNotifications_SingleTag(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" {
		t.Errorf("AgentPath: got %q, want %q", got[0].AgentPath, "child-1")
	}
	if got[0].Status != "completed" {
		t.Errorf("Status: got %q, want %q", got[0].Status, "completed")
	}
}

// TestParseSubagentNotifications_LegacyAgentIDFallback pins the
// backward-compat branch: older (pre-rename) Codex builds emit
// `agent_id` instead of `agent_path`. The parser accepts either so a
// fleet straddling the rename doesn't silently drop notifications.
// Production wire is `agent_path` and is the fast path.
func TestParseSubagentNotifications_LegacyAgentIDFallback(t *testing.T) {
	text := `<subagent_notification>{"agent_id":"legacy-child","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "legacy-child" {
		t.Errorf("AgentPath: got %q, want %q (agent_id fallback)", got[0].AgentPath, "legacy-child")
	}
	if got[0].Status != "completed" {
		t.Errorf("Status: got %q, want %q", got[0].Status, "completed")
	}
	// Legacy key must not leak into Extra — it has its own logical slot.
	if _, dup := got[0].Extra["agent_id"]; dup {
		t.Errorf("Extra should not preserve the legacy agent_id key: %+v", got[0].Extra)
	}
}

// TestParseSubagentNotifications_AgentPathWinsOverAgentID locks the
// precedence rule: when both the production key (`agent_path`) and the
// legacy key (`agent_id`) are present (a weird mixed build but cheap
// to define), the production key wins and neither key leaks into Extra.
func TestParseSubagentNotifications_AgentPathWinsOverAgentID(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"new","agent_id":"old","status":"completed"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "new" {
		t.Errorf("AgentPath: got %q, want %q (agent_path takes precedence over agent_id)", got[0].AgentPath, "new")
	}
	if _, dup := got[0].Extra["agent_id"]; dup {
		t.Errorf("Extra should not preserve the legacy agent_id key: %+v", got[0].Extra)
	}
	if _, dup := got[0].Extra["agent_path"]; dup {
		t.Errorf("Extra should not duplicate agent_path: %+v", got[0].Extra)
	}
}

// TestParseSubagentNotifications_MultipleTags verifies that multiple
// notifications in one user message are all extracted and returned in
// source order. In practice this happens when several children finish
// between two parent turns.
func TestParseSubagentNotifications_MultipleTags(t *testing.T) {
	text := `Ordinary prose.

<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>

More prose.

<subagent_notification>{"agent_path":"child-2","status":"errored"}</subagent_notification>
`
	got := parseSubagentNotifications(text)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" || got[0].Status != "completed" {
		t.Errorf("entry 0: got %+v", got[0])
	}
	if got[1].AgentPath != "child-2" || got[1].Status != "errored" {
		t.Errorf("entry 1: got %+v", got[1])
	}
}

// TestParseSubagentNotifications_WhitespaceLenient verifies the regex
// tolerates leading/trailing whitespace around the JSON body. Codex's
// tests pin a tight shape but the refactor plan flagged "be lenient on
// whitespace" as a correctness criterion.
func TestParseSubagentNotifications_WhitespaceLenient(t *testing.T) {
	text := "<subagent_notification>\n  {\"agent_path\":\"child-3\",\"status\":\"interrupted\"}\n</subagent_notification>"
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-3" || got[0].Status != "interrupted" {
		t.Errorf("got %+v", got[0])
	}
}

// TestParseSubagentNotifications_PreservesExtraFields keeps forward
// compatibility: when Codex adds fields inside the notification JSON,
// we preserve them on the Extra map so downstream can opt into richer
// rendering without a parser update. The load-bearing `agent_path` and
// `status` and `message` keys are stripped from Extra (they have their own fields).
func TestParseSubagentNotifications_PreservesExtraFields(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":"completed","message":"ok","duration_ms":1234}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].Message != "ok" {
		t.Errorf("Message: got %v, want %q", got[0].Message, "ok")
	}
	// JSON numbers decode as float64 in map[string]any.
	if got[0].Extra["duration_ms"].(float64) != 1234 {
		t.Errorf("Extra.duration_ms: got %v, want 1234", got[0].Extra["duration_ms"])
	}
	if _, dup := got[0].Extra["agent_path"]; dup {
		t.Errorf("Extra should not duplicate agent_path: %+v", got[0].Extra)
	}
	if _, dup := got[0].Extra["status"]; dup {
		t.Errorf("Extra should not duplicate status: %+v", got[0].Extra)
	}
}

func TestParseSubagentNotifications_ObjectStatus(t *testing.T) {
	text := `<subagent_notification>{"agent_path":"child-1","status":{"completed":"done"}}</subagent_notification>
<subagent_notification>{"agent_path":"child-2","status":{"errored":"boom"}}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2 (got=%+v)", len(got), got)
	}
	if got[0].Status != "completed" || got[0].Message != "done" {
		t.Errorf("entry 0: got %+v, want completed with message", got[0])
	}
	if got[1].Status != "errored" || got[1].Message != "boom" {
		t.Errorf("entry 1: got %+v, want errored with message", got[1])
	}
}

// TestParseSubagentNotifications_SkipsMalformed ensures a single
// broken tag never blocks sibling tags from parsing. A Codex bug (or a
// partial stream) that emits malformed JSON inside one block should
// still let the parent render the remaining user text.
func TestParseSubagentNotifications_SkipsMalformed(t *testing.T) {
	text := `<subagent_notification>not json at all</subagent_notification>
<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>
<subagent_notification>{"agent_path":"","status":"completed"}</subagent_notification>
<subagent_notification>{"agent_path":"child-2"}</subagent_notification>`
	got := parseSubagentNotifications(text)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" {
		t.Errorf("got %+v", got[0])
	}
}

// TestParseSubagentNotifications_NoTag returns nil for plain user text
// — the hot path that runs on every userMessage in a session. A
// positive answer would churn a throwaway slice on every turn.
func TestParseSubagentNotifications_NoTag(t *testing.T) {
	if got := parseSubagentNotifications(""); got != nil {
		t.Errorf("empty: got %+v, want nil", got)
	}
	if got := parseSubagentNotifications("plain user message"); got != nil {
		t.Errorf("plain text: got %+v, want nil", got)
	}
}

// TestExtractSubagentNotificationsFromUserMessage_WireShape feeds the
// exact params shape that comes off the wire on item/completed for a
// userMessage (UserInput array with type=text entries) and asserts the
// notifications are extracted. This is the integration between the
// JSON-shape path and the parser.
func TestExtractSubagentNotificationsFromUserMessage_WireShape(t *testing.T) {
	params := json.RawMessage(`{
		"threadId":"parent-thread",
		"item":{
			"id":"user-msg-1",
			"type":"userMessage",
			"content":[
				{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>","text_elements":[]},
				{"type":"text","text":"follow-up question","text_elements":[]}
			]
		}
	}`)
	got := extractSubagentNotificationsFromUserMessage(params)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
	}
	if got[0].AgentPath != "child-1" || got[0].Status != "completed" {
		t.Errorf("got %+v", got[0])
	}
}

// TestExtractSubagentNotificationsFromUserMessage_NotUserMessage
// guards the filter: assistant and reasoning items must not trigger
// the parser. This matters because the parser walks the full content
// array — running it on every item would waste allocations on every
// turn.
func TestExtractSubagentNotificationsFromUserMessage_NotUserMessage(t *testing.T) {
	params := json.RawMessage(`{
		"item":{
			"id":"asst-1",
			"type":"agentMessage",
			"text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>"
		}
	}`)
	if got := extractSubagentNotificationsFromUserMessage(params); got != nil {
		t.Errorf("non-userMessage: got %+v, want nil", got)
	}
}

// TestExtractSubagentNotificationsFromUserMessage_NonTextContent
// confirms the UserInput tagged union filter: image / mention / skill
// entries must not be text-concatenated (their `text` field, if any,
// carries different semantics).
func TestExtractSubagentNotificationsFromUserMessage_NonTextContent(t *testing.T) {
	params := json.RawMessage(`{
		"item":{
			"id":"user-msg-1",
			"type":"userMessage",
			"content":[
				{"type":"image","url":"https://example.test/image.png"},
				{"type":"mention","name":"file","path":"/tmp/a"}
			]
		}
	}`)
	if got := extractSubagentNotificationsFromUserMessage(params); got != nil {
		t.Errorf("non-text content: got %+v, want nil", got)
	}
}

// TestDispatchLineSubagentNotificationEmitsEvent pins the emission
// contract: when an item/completed userMessage carries a
// <subagent_notification> tag, dispatchLine must fire an
// EventSubagentNotification with ThreadID and a Meta payload carrying
// at least agent_path and status. This is the integration between the
// parser and the event emission path — the triage handler and UI
// renderer downstream assume the event actually fires.
func TestDispatchLineSubagentNotificationEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-done": "call-collab-1"},
		},
	}

	// Shape mirrors the userMessage item/completed frame Codex core
	// emits after a detached child agent reaches a terminal state.
	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var notif *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventSubagentNotification {
			notif = &events[i]
			break
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification among emitted events; got %+v", events)
	}
	if notif.ThreadID != "parent-thread" {
		t.Errorf("ThreadID: got %q, want parent-thread", notif.ThreadID)
	}

	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-done" {
		t.Errorf("meta.agent_path: got %v, want child-done", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status: got %v, want completed", meta["status"])
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByAgentPath: map[string]string{
				"/root/researcher": "call-collab-1",
			},
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "/root/researcher",
		"status": map[string]any{
			"completed": "No findings.",
		},
	})
	s.dispatchLine(line)

	var notif *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventSubagentNotification {
			notif = &events[i]
			break
		}
	}
	if notif == nil {
		t.Fatalf("expected EventSubagentNotification among emitted events; got %+v", events)
	}
	if notif.ThreadID != "parent-thread" {
		t.Errorf("ThreadID: got %q, want parent-thread", notif.ThreadID)
	}
	if notif.ItemID != "call-collab-1" {
		t.Errorf("ItemID: got %q, want call-collab-1", notif.ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(notif.Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "/root/researcher" {
		t.Errorf("meta.agent_path: got %v, want /root/researcher", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status: got %v, want completed", meta["status"])
	}
	if meta["message"] != "No findings." {
		t.Errorf("meta.message: got %v, want final child answer", meta["message"])
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationWithoutPhaseIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByAgentPath: map[string]string{
				"/root/researcher": "call-collab-1",
			},
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThreadAndPhase(t, "parent-provider-thread", "", map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("no-phase raw carrier must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationMixedContentIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByAgentPath: map[string]string{
				"/root/researcher": "call-collab-1",
			},
		},
	}

	line := rawInterAgentMessageLine(t, "ordinary note\n"+subagentNotificationTag(t, map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	}))
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("mixed raw inter-agent content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationAuthorMismatchIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByAgentPath: map[string]string{
				"/root/other": "call-collab-1",
			},
		},
	}

	line := rawInterAgentSubagentNotificationLine(t, map[string]any{
		"agent_path": "/root/other",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("author-mismatched raw inter-agent content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationFromChildThreadIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-provider-thread": "call-collab-1",
			},
			childParentByAgentPath: map[string]string{
				"/root/researcher": "call-collab-1",
			},
		},
	}
	s.setRootThreadID("parent-provider-thread")

	line := rawInterAgentSubagentNotificationLineForThread(t, "child-provider-thread", map[string]any{
		"agent_path": "/root/researcher",
		"status":     "completed",
	})
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("child-thread raw inter-agent content must not emit parent-observed completion: %+v", events)
		}
	}
}

func TestDispatchLineRawUserSubagentNotificationMixedContentIgnored(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-done": "call-collab-1",
			},
		},
	}

	line := rawUserMessageLineForThread(t, "parent-thread", "ordinary note\n"+subagentNotificationTag(t, map[string]any{
		"agent_path": "child-done",
		"status":     "completed",
	}))
	s.dispatchLine(line)

	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("mixed raw user content must not emit control event: %+v", events)
		}
	}
}

func TestDispatchLineRawUserSubagentNotificationWrongBlockTypeIgnored(t *testing.T) {
	for _, blockType := range []string{"output_text", "text"} {
		t.Run(blockType, func(t *testing.T) {
			var events []provider.ProviderEvent
			s := &Session{
				threadID: "parent-thread",
				pending:  make(map[int64]chan json.RawMessage),
				onEvent: func(evt provider.ProviderEvent) {
					events = append(events, evt)
				},
				collab: sessionCollabState{
					childParentByThread: map[string]string{
						"child-done": "call-collab-1",
					},
				},
			}

			line := rawUserMessageLineForThreadAndBlockType(t, "parent-thread", blockType, subagentNotificationTag(t, map[string]any{
				"agent_path": "child-done",
				"status":     "completed",
			}))
			s.dispatchLine(line)

			for _, evt := range events {
				if evt.Kind == provider.EventSubagentNotification {
					t.Fatalf("raw user %s block must not emit control event: %+v", blockType, events)
				}
			}
		})
	}
}

func TestDispatchLineRawInterAgentSubagentNotificationWrongBlockTypeIgnored(t *testing.T) {
	for _, blockType := range []string{"input_text", "text"} {
		t.Run(blockType, func(t *testing.T) {
			var events []provider.ProviderEvent
			s := &Session{
				threadID: "parent-thread",
				pending:  make(map[int64]chan json.RawMessage),
				onEvent: func(evt provider.ProviderEvent) {
					events = append(events, evt)
				},
				collab: sessionCollabState{
					childParentByAgentPath: map[string]string{
						"/root/researcher": "call-collab-1",
					},
				},
			}

			line := rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, "parent-thread", "commentary", subagentNotificationTag(t, map[string]any{
				"agent_path": "/root/researcher",
				"status":     "completed",
			}), blockType)
			s.dispatchLine(line)

			for _, evt := range events {
				if evt.Kind == provider.EventSubagentNotification {
					t.Fatalf("raw assistant %s block must not emit control event: %+v", blockType, events)
				}
			}
		})
	}
}

func TestDispatchLineRawAssistantMessageDoesNotEmitSubagentNotification(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rawMessageLine(t, "plain assistant commentary")
	s.dispatchLine(line)

	if len(events) != 0 {
		t.Fatalf("ordinary raw assistant message should stay non-visual, got %+v", events)
	}
}

func TestDispatchLineSubagentNotificationCarrierDoesNotEmitUserTextWhenMapped(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-done": "call-collab-1"},
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawNotification bool
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventSubagentNotification:
			sawNotification = true
			if evt.ItemID != "call-collab-1" {
				t.Fatalf("notification ItemID = %q, want call-collab-1", evt.ItemID)
			}
		case provider.EventUserText:
			t.Fatalf("carrier userMessage emitted EventUserText: %+v", evt)
		}
	}
	if !sawNotification {
		t.Fatalf("expected EventSubagentNotification, got %+v", events)
	}
}

func TestDispatchLineSubagentNotificationMixedContentKeepsUserText(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{"child-done": "call-collab-1"},
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"keep this text\n<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawNotification bool
	var sawUserText bool
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventSubagentNotification:
			sawNotification = true
		case provider.EventUserText:
			sawUserText = true
		}
	}
	if sawNotification {
		t.Fatalf("mixed user text must not emit forgeable subagent notification: %+v", events)
	}
	if !sawUserText {
		t.Fatalf("mixed content should keep user text, got %+v", events)
	}
}

func TestDispatchLineSubagentNotificationUnmappedCarrierKeepsUserText(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: make(map[string]string),
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var sawUserText bool
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			t.Fatalf("unmapped notification carrier emitted control event: %+v", evt)
		}
		if evt.Kind == provider.EventUserText {
			sawUserText = true
		}
	}
	if !sawUserText {
		t.Fatalf("unmapped carrier should remain literal user text, got %+v", events)
	}
}

// TestDispatchLineSubagentNotificationMultipleTagsEmitOnce pins that a
// userMessage carrying multiple <subagent_notification> tags produces
// one EventSubagentNotification per tag, in source order. The UI
// surfaces each terminal child as its own notification; a single
// combined event would collapse them.
func TestDispatchLineSubagentNotificationMultipleTagsEmitOnce(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
		collab: sessionCollabState{
			childParentByThread: map[string]string{
				"child-1": "call-collab-1",
				"child-2": "call-collab-2",
			},
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>\n<subagent_notification>{\"agent_path\":\"child-2\",\"status\":\"errored\"}</subagent_notification>"}]}}}`)
	s.dispatchLine(line)

	var agents []string
	for _, evt := range events {
		if evt.Kind != provider.EventSubagentNotification {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("meta unmarshal: %v", err)
		}
		agents = append(agents, meta["agent_path"].(string))
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 EventSubagentNotification events, got %d (agents=%v events=%+v)", len(agents), agents, events)
	}
	if agents[0] != "child-1" || agents[1] != "child-2" {
		t.Errorf("order: got %v, want [child-1 child-2]", agents)
	}
}

// TestBuildSubagentNotificationMetaIncludesExtra pins the Extra-field
// forward-compat promise: custom fields Codex core adds to the
// notification JSON must round-trip through buildSubagentNotificationMeta
// onto the frontend-facing meta blob. The load-bearing agent_path /
// status keys always win on collision.
func TestBuildSubagentNotificationMetaIncludesExtra(t *testing.T) {
	n := subagentNotification{
		AgentPath: "child-extra",
		Status:    "completed",
		Message:   "ok",
		Extra: map[string]any{
			"message":     "clobber-attempt",
			"duration_ms": float64(1234),
			// Attempted collision — the canonical fields must win.
			"agent_path": "clobber-attempt",
			"status":     "clobber-attempt",
		},
	}
	raw := buildSubagentNotificationMeta(n)
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-extra" {
		t.Errorf("agent_path: got %v, want child-extra (Extra must not clobber)", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("status: got %v, want completed (Extra must not clobber)", meta["status"])
	}
	if meta["message"] != "ok" {
		t.Errorf("message: got %v, want ok", meta["message"])
	}
	if meta["duration_ms"] != float64(1234) {
		t.Errorf("duration_ms: got %v, want 1234", meta["duration_ms"])
	}
}
