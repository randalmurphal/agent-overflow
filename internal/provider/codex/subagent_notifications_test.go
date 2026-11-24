package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func TestDispatchLineSubagentNotificationUsesAgentPathMapping(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})
	s.collab = sessionCollabState{
		childParentByThread:    make(map[string]string),
		childParentByAgentPath: map[string]string{"/root/researcher": "call-collab-1"},
		agentPathByThread:      make(map[string]string),
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"/root/researcher\",\"status\":{\"completed\":\"done\"}}</subagent_notification>"}]}}}`)
	feed(t, s, line)

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

// TestParseSubagentNotifications_SingleTag pins one tag's JSON body
// round-tripping to a subagentNotification, with the identity and
// status keys in their own fields and never left in Extra. The
// canonical wire shape comes from codex-source
// (core/src/session_prefix.rs::format_subagent_notification_message).
func TestParseSubagentNotifications_SingleTag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, wantPath, wantStatus string
	}{
		{"canonical", `<subagent_notification>{"agent_path":"child-1","status":"completed"}</subagent_notification>`, "child-1", "completed"},
		// Older (pre-rename) Codex builds emit `agent_id`; a fleet straddling
		// the rename must not drop notifications.
		{"legacy agent_id fallback", `<subagent_notification>{"agent_id":"legacy-child","status":"completed"}</subagent_notification>`, "legacy-child", "completed"},
		{"agent_path wins over agent_id", `<subagent_notification>{"agent_path":"new","agent_id":"old","status":"completed"}</subagent_notification>`, "new", "completed"},
		{"whitespace around the body", "<subagent_notification>\n  {\"agent_path\":\"child-3\",\"status\":\"interrupted\"}\n</subagent_notification>", "child-3", "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSubagentNotifications(tc.text)
			if len(got) != 1 {
				t.Fatalf("len=%d, want 1 (got=%+v)", len(got), got)
			}
			if got[0].AgentPath != tc.wantPath || got[0].Status != tc.wantStatus {
				t.Errorf("got (%q, %q), want (%q, %q)", got[0].AgentPath, got[0].Status, tc.wantPath, tc.wantStatus)
			}
			if len(got[0].Extra) != 0 {
				t.Errorf("Extra must not keep agent_path, agent_id or status: %+v", got[0].Extra)
			}
		})
	}
}

// TestParseSubagentNotifications_MultipleTags verifies that multiple
// notifications in one user message are all extracted and returned in
// source order. In practice this happens when several children finish
// between two parent turns.
func TestParseSubagentNotifications_MultipleTags(t *testing.T) {
	t.Parallel()
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

// TestParseSubagentNotifications_PreservesExtraFields keeps forward
// compatibility: when Codex adds fields inside the notification JSON,
// we preserve them on the Extra map so downstream can opt into richer
// rendering without a parser update. The load-bearing `agent_path` and
// `status` and `message` keys are stripped from Extra (they have their own fields).
func TestParseSubagentNotifications_PreservesExtraFields(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// guards the item filters: an assistant item must not reach the parser,
// and in a userMessage only the UserInput `text` variant is concatenated
// (an image or mention entry's fields carry different semantics).
func TestExtractSubagentNotificationsFromUserMessage_NotUserMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		params string
	}{
		{"agentMessage item", `{"item":{"id":"asst-1","type":"agentMessage",` +
			`"text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>"}}`},
		{"non-text content", `{"item":{"id":"user-msg-1","type":"userMessage","content":[` +
			`{"type":"image","url":"https://example.test/image.png"},{"type":"mention","name":"file","path":"/tmp/a"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractSubagentNotificationsFromUserMessage(json.RawMessage(tc.params)); got != nil {
				t.Errorf("got %+v, want nil", got)
			}
		})
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
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})
	s.collab = sessionCollabState{
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
	}

	// Shape mirrors the userMessage item/completed frame Codex core
	// emits after a detached child agent reaches a terminal state.
	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	feed(t, s, line)

	var notif *provider.ProviderEvent
	for i := range events {
		if events[i].Kind == provider.EventUserText {
			// The carrier is consumed by the notification it carries.
			t.Fatalf("carrier userMessage emitted EventUserText: %+v", events[i])
		}
		if events[i].Kind != provider.EventSubagentNotification {
			continue
		}
		if events[i].ItemID != "call-collab-1" {
			t.Errorf("ItemID: got %q, want call-collab-1", events[i].ItemID)
		}
		if notif == nil {
			notif = &events[i]
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
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "parent-provider-thread", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})
	s.collab = sessionCollabState{
		childParentByAgentPath: map[string]string{
			"/root/researcher": "call-collab-1",
		},
	}

	line := rawInterAgentSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "/root/researcher",
		"status": map[string]any{
			"completed": "No findings.",
		},
	})
	feed(t, s, line)

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

// TestDispatchLineRawSubagentNotificationCarrierIgnored pins the raw
// carriers that must not emit a control event even though their text holds
// a mapped child's notification tag: the wrong phase, author, thread or
// block type, or a tag mixed with ordinary text.
func TestDispatchLineRawSubagentNotificationCarrierIgnored(t *testing.T) {
	t.Parallel()
	researcher := map[string]any{"agent_path": "/root/researcher", "status": "completed"}
	researcherTag := subagentNotificationTag(t, researcher)
	childDoneTag := subagentNotificationTag(t, map[string]any{"agent_path": "child-done", "status": "completed"})
	byPath := func(path string) sessionCollabState {
		return sessionCollabState{childParentByAgentPath: map[string]string{path: "call-collab-1"}}
	}
	byThread := func(thread string) sessionCollabState {
		return sessionCollabState{childParentByThread: map[string]string{thread: "call-collab-1"}}
	}
	for _, tc := range []struct {
		name       string
		rootThread string
		collab     sessionCollabState
		line       []byte
	}{
		{"inter-agent without phase", "parent-provider-thread", byPath("/root/researcher"),
			rawInterAgentSubagentNotificationLineForThreadAndPhase(t, "parent-provider-thread", "", researcher)},
		{"inter-agent mixed content", "", byPath("/root/researcher"),
			rawInterAgentMessageLine(t, "ordinary note\n"+researcherTag)},
		{"inter-agent author mismatch", "", byPath("/root/other"),
			rawInterAgentSubagentNotificationLine(t, map[string]any{"agent_path": "/root/other", "status": "completed"})},
		{"inter-agent from child thread", "parent-provider-thread", sessionCollabState{
			childParentByThread:    map[string]string{"child-provider-thread": "call-collab-1"},
			childParentByAgentPath: map[string]string{"/root/researcher": "call-collab-1"},
		}, rawInterAgentSubagentNotificationLineForThread(t, "child-provider-thread", researcher)},
		{"inter-agent input_text block", "", byPath("/root/researcher"),
			rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, "parent-thread", "commentary", researcherTag, "input_text")},
		{"inter-agent text block", "", byPath("/root/researcher"),
			rawInterAgentMessageLineForThreadAndPhaseAndBlockType(t, "parent-thread", "commentary", researcherTag, "text")},
		{"user mixed content", "", byThread("child-done"),
			rawUserMessageLineForThread(t, "parent-thread", "ordinary note\n"+childDoneTag)},
		{"user output_text block", "", byThread("child-done"),
			rawUserMessageLineForThreadAndBlockType(t, "parent-thread", "output_text", childDoneTag)},
		{"user text block", "", byThread("child-done"),
			rawUserMessageLineForThreadAndBlockType(t, "parent-thread", "text", childDoneTag)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var events []provider.ProviderEvent
			s := fixtureSession(t, "parent-thread", tc.rootThread, func(evt provider.ProviderEvent) {
				events = append(events, evt)
			})
			s.collab = tc.collab
			feed(t, s, tc.line)

			for _, evt := range events {
				if evt.Kind == provider.EventSubagentNotification {
					t.Fatalf("raw carrier emitted a control event: %+v", events)
				}
			}
		})
	}
}

func TestDispatchLineRawAssistantMessageDoesNotEmitSubagentNotification(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})

	line := rawMessageLine(t, "plain assistant commentary")
	feed(t, s, line)

	if len(events) != 0 {
		t.Fatalf("ordinary raw assistant message should stay non-visual, got %+v", events)
	}
}

func TestDispatchLineSubagentNotificationMixedContentKeepsUserText(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})
	s.collab = sessionCollabState{
		childParentByThread: map[string]string{"child-done": "call-collab-1"},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"keep this text\n<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	feed(t, s, line)

	var sawNotification bool
	var sawUserText bool
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			sawNotification = true
		} else if evt.Kind == provider.EventUserText {
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
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-done\",\"status\":\"completed\"}</subagent_notification>"}]}}}`)
	feed(t, s, line)

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
	t.Parallel()
	var events []provider.ProviderEvent
	s := fixtureSession(t, "parent-thread", "", func(evt provider.ProviderEvent) {
		events = append(events, evt)
	})
	s.collab = sessionCollabState{
		childParentByThread: map[string]string{
			"child-1": "call-collab-1",
			"child-2": "call-collab-2",
		},
	}

	line := []byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"parent-thread","item":{"id":"user-msg-1","type":"userMessage","content":[{"type":"text","text":"<subagent_notification>{\"agent_path\":\"child-1\",\"status\":\"completed\"}</subagent_notification>\n<subagent_notification>{\"agent_path\":\"child-2\",\"status\":\"errored\"}</subagent_notification>"}]}}}`)
	feed(t, s, line)

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
	t.Parallel()
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

// parseSubagentNotifications is parseSubagentNotificationsWithCarrierRemainder
// without the remainder.
func parseSubagentNotifications(text string) []subagentNotification {
	notifications, _ := parseSubagentNotificationsWithCarrierRemainder(text)
	return notifications
}

// extractSubagentNotificationsFromUserMessage is
// extractSubagentNotificationsAndRemainderFromUserMessage without the
// remainder.
func extractSubagentNotificationsFromUserMessage(params json.RawMessage) []subagentNotification {
	notifications, _ := extractSubagentNotificationsAndRemainderFromUserMessage(params)
	return notifications
}
