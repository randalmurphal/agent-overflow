package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestParseUser_ReplayEnvelopeEmitsEventUserText pins the parser's
// defensive `message.id` preference. The envelope here (`message.id`
// populated, top-level `uuid` absent) does NOT match a wire shape
// current Claude releases emit — `createUserMessage`
// (claude-code-source-code/src/utils/messages.ts:502-507) never sets
// `message.id`, and `SDKUserMessageReplaySchema`
// (coreSchemas.ts:1297-1303) makes top-level `uuid` required. The
// test guards the parser's behaviour for a hypothetical future SDK
// shape that surfaces the API-assigned id; the queued_command and
// initial-ack tests below cover the shapes Claude actually emits.
func TestParseUser_ReplayEnvelopeEmitsEventUserText(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_abc123","content":"hello world"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}

	got := events[0]
	if got.Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", got.Kind, provider.EventUserText)
	}
	if got.ThreadID != testThread {
		t.Fatalf("ThreadID: got %q, want %q", got.ThreadID, testThread)
	}
	if got.Content != "hello world" {
		t.Fatalf("Content: got %q, want %q", got.Content, "hello world")
	}
	if got.ItemID != "" {
		t.Fatalf("ItemID: got %q, want empty (triage owns the AO row id)", got.ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "msg_abc123" {
		t.Fatalf("meta.provider_item_id: got %v, want msg_abc123", meta["provider_item_id"])
	}
}

// TestParseUser_ReplayEnvelopeWithBlockContent covers the defensive
// secondary shape — `content` as a `[{type:"text",text:"..."}]` array.
// Real captures haven't shown this for replay envelopes, but the
// SDK's content union allows it and extractToolResultText already
// handles both shapes; the parser must not treat array-content as a
// drop just because string is documented.
func TestParseUser_ReplayEnvelopeWithBlockContent(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_block","content":[{"type":"text","text":"hello"}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "hello" {
		t.Fatalf("Content: got %q, want %q", events[0].Content, "hello")
	}
}

// TestParseUser_ReplayEnvelopeMissingMessageID covers the
// no-identifier case — neither `message.id` nor top-level `uuid` is
// present. The parser must still emit EventUserText (the wire echo
// carries semantic value) but must NOT emit an empty-string
// `provider_item_id`. Phase E treats absence-of-key and empty-string
// differently; we collapse them here so triage sees one shape. In
// practice the SDK schema makes top-level `uuid` required, so this is
// a defensive-shape case rather than something the CLI emits.
func TestParseUser_ReplayEnvelopeMissingMessageID(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"content":"no uuid"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content != "no uuid" {
		t.Fatalf("Content: got %q, want %q", events[0].Content, "no uuid")
	}

	// Either absent meta or meta without provider_item_id is acceptable;
	// what is NOT acceptable is meta carrying an empty-string value for
	// the key.
	if len(events[0].Meta) == 0 {
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if v, ok := meta["provider_item_id"]; ok {
		if s, isStr := v.(string); !isStr || s == "" {
			t.Fatalf("meta.provider_item_id present but empty/invalid: %v", v)
		}
	}
}

// TestParseUser_ReplayEnvelopeQueuedCommandShape pins the
// `queued_command` SDK path: when Claude echoes back a user message
// that was written to stdin while a turn was running (the path AO's
// flush dispatcher uses), the replay envelope sets ONLY top-level
// `uuid` — `message` carries `{role, content}` with no `id` field.
// See claude-code-source-code/src/QueryEngine.ts:880-892. The parser
// must fall back to top-level `uuid` so triage's pending-send
// correlator gets a stable handle to stamp onto the AO row;
// otherwise the frontend Zone 2 confirm marker stays stuck because
// the meta-stamp path no-ops on empty id.
func TestParseUser_ReplayEnvelopeQueuedCommandShape(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"queue-uuid-123","session_id":"sess-1","parent_tool_use_id":null,"message":{"role":"user","content":"queued message"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}

	got := events[0]
	if got.Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", got.Kind, provider.EventUserText)
	}
	if got.Content != "queued message" {
		t.Fatalf("Content: got %q, want %q", got.Content, "queued message")
	}

	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "queue-uuid-123" {
		t.Fatalf("meta.provider_item_id: got %v, want queue-uuid-123 (top-level uuid fallback)", meta["provider_item_id"])
	}
}

// TestParseUser_ReplayEnvelopePrefersMessageIDOverTopLevelUUID pins
// the preference order in the defensive scenario where both ids are
// populated. Today current Claude releases never set `message.id`
// (see TestParseUser_ReplayEnvelopeEmitsEventUserText), so this
// preference rule is a guard for a hypothetical future SDK shape that
// exposes the API-assigned id alongside the SDK uuid — when both are
// present we want the more specific identifier.
func TestParseUser_ReplayEnvelopePrefersMessageIDOverTopLevelUUID(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"top-level-uuid","message":{"id":"msg_api_id","content":"both ids"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "msg_api_id" {
		t.Fatalf("meta.provider_item_id: got %v, want msg_api_id (message.id must win over top-level uuid)", meta["provider_item_id"])
	}
}

// TestParseUser_NonReplayUserEnvelopeWithStringContent_StillDrops
// pins the byte-for-byte preservation of today's behaviour: a
// string-content user envelope WITHOUT the replay flag still drops
// silently (no events). The replay flag is the gate; without it
// nothing changes.
func TestParseUser_NonReplayUserEnvelopeWithStringContent_StillDrops(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","message":{"content":"some user text"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for non-replay string-content user envelope, got %d: %+v", len(events), events)
	}
}

// TestParseUser_NonReplayUserEnvelopeWithToolResult_RoutesThroughParseUser
// confirms tool_result envelopes still flow through the existing
// parseUser path unchanged. Pre-check must NOT swallow them.
func TestParseUser_NonReplayUserEnvelopeWithToolResult_RoutesThroughParseUser(t *testing.T) {
	parser := NewParser()
	// Prime an inline tool_use so the tool_result correlates.
	if _, err := parser.ParseLine(testThread, []byte(`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-inline","name":"Read","input":{"file_path":"/etc/hosts"}}]}}`)); err != nil {
		t.Fatalf("seed tool_use: %v", err)
	}

	line := []byte(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tool-inline","type":"tool_result","content":"127.0.0.1"}]}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (EventToolComplete), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventToolComplete {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventToolComplete)
	}
	if events[0].ItemID != "tool-inline" {
		t.Fatalf("ItemID: got %q, want tool-inline", events[0].ItemID)
	}
}

// TestParseUser_ReplayWithBothToolResultAndIsReplay defends against
// a future SDK shape that combines both signals. We choose
// `isReplay:true` wins — exactly one EventUserText emits, no
// EventToolComplete. This shouldn't appear in practice; the test
// pins the choice so a future change has to be deliberate.
func TestParseUser_ReplayWithBothToolResultAndIsReplay(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"message":{"id":"msg_replay","content":[{"type":"tool_result","tool_use_id":"tool-x","content":"would-be-completion"}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event (replay wins), got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q (replay must win over tool_result)", events[0].Kind, provider.EventUserText)
	}
	for _, ev := range events {
		if ev.Kind == provider.EventToolComplete {
			t.Fatalf("EventToolComplete must NOT emit when isReplay:true is present, got %+v", ev)
		}
	}
}

// Suppression of Claude-injected user-replay echoes
// ──────────────────────────────────────────────────
// `--replay-user-messages` echoes the queued task-notification XML
// (and any other Claude-injected user-role attachment) back as a
// `user{isReplay:true}` envelope. These are model-context payloads,
// not real user input — persisting them as user_text rows is the
// "background completion shows up as user message" bug. The
// detection is done at the parser layer so these never reach triage
// at all; identity matching in `consumeMatchingPendingSend` is the
// downstream backstop for shapes this list misses.
//
// Wrap forms (upstream
// `claude-code-source-code/src/utils/messages.ts:5496-5512`
// `wrapCommandText`):
//   - origin='task-notification' → "A background agent completed a task:\n${raw}"
//   - origin='coordinator'       → "The coordinator sent a message while you were working:\n${raw}"
//   - origin='channel'           → "A message arrived from <name> while you were working:\n${raw}"
//   - origin='human'             → "The user sent a new message while you were working:\n${raw}\n\nIMPORTANT: ..."
//
// XML wrappers Claude injects into user-role content (suppress when
// open + close are both present — single-tag mention from a real
// user shouldn't trigger):
//   - <task-notification>...</task-notification>     (LocalShellTask.tsx:160-165)
//   - <system-reminder>...</system-reminder>          (pervasive in messages.ts)
//   - <bash-input>...</bash-input>                    (processBashCommand.tsx)
//   - <bash-stdout>...</bash-stdout>                  (processBashCommand.tsx)
//   - <bash-stderr>...</bash-stderr>                  (processBashCommand.tsx)
//   - <local-command-stdout>...</local-command-stdout> (processSlashCommand.tsx)

// TestParseUser_Replay_TaskNotificationTerminal_EmitsBackgroundTaskNotification
// exercises the load-bearing path: a background subagent (or
// backgrounded bash) completes WHILE a concurrent foreground
// tool_result is in flight, so Claude's CLI skips the structured
// `system/task_notification` envelope and only echoes the inline
// `wrapCommandText('task-notification', ...)` body via
// `--replay-user-messages`. The parser MUST NOT produce an
// EventUserText (would render as a user bubble), AND MUST emit
// EventBackgroundTaskNotification carrying the inner XML fields so
// triage can drain the pending-terminal stash and write the
// `tool_completion` sibling. Without this emission the subagent's
// launch row stays `running` forever (the original bug).
func TestParseUser_Replay_TaskNotificationTerminal_EmitsBackgroundTaskNotification(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"task-notif-uuid-1","message":{"role":"user","content":"A background agent completed a task:\n<task-notification>\n<task-id>task-bg-1</task-id>\n<tool-use-id>tool-bg-1</tool-use-id>\n<output-file>/tmp/agent-out</output-file>\n<status>completed</status>\n<summary>Background command \"sleep 5\" completed (exit code 0)</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventBackgroundTaskNotification, got %d: %+v", len(events), events)
	}
	got := events[0]
	if got.Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("Kind: got %q, want EventBackgroundTaskNotification (and NOT EventUserText)", got.Kind)
	}
	if got.ItemID != "tool-bg-1" {
		t.Fatalf("ItemID: got %q, want tool-bg-1", got.ItemID)
	}
	if got.Content != `Background command "sleep 5" completed (exit code 0)` {
		t.Fatalf("Content: got %q, want the summary text", got.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-bg-1" {
		t.Fatalf("meta.task_id: got %v, want task-bg-1", meta["task_id"])
	}
	if meta["tool_use_id"] != "tool-bg-1" {
		t.Fatalf("meta.tool_use_id: got %v, want tool-bg-1", meta["tool_use_id"])
	}
	if meta["status"] != "completed" {
		t.Fatalf("meta.status: got %v, want completed", meta["status"])
	}
	if meta["output_file"] != "/tmp/agent-out" {
		t.Fatalf("meta.output_file: got %v, want /tmp/agent-out", meta["output_file"])
	}
	if meta["source"] != "task_notification" {
		t.Fatalf("meta.source: got %v, want task_notification (parallel to system-envelope path)", meta["source"])
	}
}

// TestParseUser_Replay_TaskNotificationStallPing_EmitsWithoutStatus pins the
// statusless variant emitted by the stall watchdog
// (`LocalShellTask.tsx:80-95`). No <status> tag, otherwise same shape.
// The SDK system event path doesn't fire (`print.ts:2070` gates on
// status), so this is the only surface — surface it as a
// notification row so the user sees that a backgrounded command is
// waiting for input. `meta.status` is absent (not empty-string) so
// the downstream handler can branch on its presence.
func TestParseUser_Replay_TaskNotificationStallPing_EmitsWithoutStatus(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"stall-ping-uuid","message":{"role":"user","content":"A background agent completed a task:\n<task-notification>\n<task-id>task-stall-1</task-id>\n<output-file>/tmp/agent-stall</output-file>\n<summary>Background command \"npm install\" appears to be waiting for interactive input</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventBackgroundTaskNotification, got %d: %+v", len(events), events)
	}
	got := events[0]
	if got.Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("Kind: got %q, want EventBackgroundTaskNotification", got.Kind)
	}
	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-stall-1" {
		t.Fatalf("meta.task_id: got %v, want task-stall-1", meta["task_id"])
	}
	if _, hasStatus := meta["status"]; hasStatus {
		t.Fatalf("meta.status: present but the wire body had no <status> tag — must omit so downstream can branch on absence: %v", meta["status"])
	}
	if meta["output_file"] != "/tmp/agent-stall" {
		t.Fatalf("meta.output_file: got %v, want /tmp/agent-stall", meta["output_file"])
	}
}

// TestParseUser_Replay_BareTaskNotificationXML_Emits covers the
// defensive path where the wrap prefix is missing but the XML body
// is present. Could happen if a future upstream change ships the
// XML directly without the human-facing prefix. Both tags must match
// — open + close — so a single mention in real user text doesn't
// trigger the suppression.
func TestParseUser_Replay_BareTaskNotificationXML_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"bare-xml-uuid","message":{"role":"user","content":"<task-notification>\n<task-id>task-bare</task-id>\n<status>failed</status>\n<summary>oops</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventBackgroundTaskNotification, got %d: %+v", len(events), events)
	}
	got := events[0]
	if got.Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("Kind: got %q, want EventBackgroundTaskNotification", got.Kind)
	}
	var meta map[string]any
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["task_id"] != "task-bare" {
		t.Fatalf("meta.task_id: got %v, want task-bare", meta["task_id"])
	}
	if meta["status"] != "failed" {
		t.Fatalf("meta.status: got %v, want failed", meta["status"])
	}
}

// TestParseUser_Replay_TaskNotificationWithoutTaskID_NoEmission pins
// the idempotency-key safety: a `<task-notification>` body whose
// inner `<task-id>` is missing or empty cannot be routed downstream
// (the triage handler keys the stash drain on task_id). Emitting a
// no-task-id event would fabricate a row that resolves to no launch
// and can't be deduped. Stay silent — drop both the EventUserText
// AND the synthetic notification.
func TestParseUser_Replay_TaskNotificationWithoutTaskID_NoEmission(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"no-task-id","message":{"role":"user","content":"<task-notification>\n<status>completed</status>\n<summary>missing task id</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events when <task-id> is missing, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_TaskNotificationFallsBackToParserMap covers
// the wire variant where the XML omits `<tool-use-id>` but the
// parser's task_id ↔ tool_use_id map already carries the mapping
// from a prior `system/task_started` envelope. The structured
// `parseTaskNotificationEvent` path uses the same fallback
// (parse_system.go); the replay path must mirror it so the
// downstream handler receives the same ItemID regardless of which
// wire shape Claude chose.
func TestParseUser_Replay_TaskNotificationFallsBackToParserMap(t *testing.T) {
	parser := NewParser()
	// Seed the map via a structured task_started envelope. This is
	// the wire ordering the live session exhibits — task_started
	// fires before any task_notification on the same task_id.
	seed := []byte(`{"type":"system","subtype":"task_started","task_id":"task-fallback-1","tool_use_id":"tool-fallback-1","task_type":"local_agent"}`)
	if _, err := parser.ParseLine(testThread, seed); err != nil {
		t.Fatalf("seed task_started: %v", err)
	}

	// Replay XML body deliberately omits <tool-use-id>; expect the
	// parser to fill it from taskToolUses.
	line := []byte(`{"type":"user","isReplay":true,"uuid":"fallback","message":{"role":"user","content":"<task-notification>\n<task-id>task-fallback-1</task-id>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].ItemID != "tool-fallback-1" {
		t.Fatalf("ItemID: got %q, want tool-fallback-1 (parser-map fallback)", events[0].ItemID)
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["tool_use_id"] != "tool-fallback-1" {
		t.Fatalf("meta.tool_use_id: got %v, want tool-fallback-1", meta["tool_use_id"])
	}
}

// TestParseUser_Replay_TaskNotificationBlockArrayShape_Emits pins the
// defensive content shape: `[{type:"text",text:"<task-notification>..."}]`.
// Real captures haven't shown this for the synthetic-XML path, but the
// SDK's content union allows it and `extractToolResultText` already
// covers both shapes — the synthetic-XML extractor must apply to
// either representation so a future SDK change doesn't silently
// reintroduce the stuck-running bug.
func TestParseUser_Replay_TaskNotificationBlockArrayShape_Emits(t *testing.T) {
	parser := NewParser()
	body := "<task-notification>\n<task-id>task-block-1</task-id>\n<tool-use-id>tool-block-1</tool-use-id>\n<status>completed</status>\n<summary>done via block array</summary>\n</task-notification>"
	line := []byte(`{"type":"user","isReplay":true,"uuid":"block-shape","message":{"role":"user","content":[{"type":"text","text":` + jsonString(body) + `}]}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for block-array synthetic-XML content, got %d: %+v", len(events), events)
	}
	got := events[0]
	if got.Kind != provider.EventBackgroundTaskNotification {
		t.Fatalf("Kind: got %q, want EventBackgroundTaskNotification", got.Kind)
	}
	if got.ItemID != "tool-block-1" {
		t.Fatalf("ItemID: got %q, want tool-block-1", got.ItemID)
	}
	if got.Content != "done via block array" {
		t.Fatalf("Content: got %q, want %q", got.Content, "done via block array")
	}
}

// TestParseUser_Replay_TaskNotificationDecodesXMLEntities pins entity
// decoding for fields that ride through to user-visible state. The
// summary text in particular can carry shell-quoted commands or
// snippets that the CLI escapes on the wire as `&amp;` / `&lt;` /
// `&gt;` / `&quot;`. Without `html.UnescapeString` those entities
// would leak into the AO timeline.
func TestParseUser_Replay_TaskNotificationDecodesXMLEntities(t *testing.T) {
	parser := NewParser()
	body := "<task-notification>\n<task-id>task-ent-1</task-id>\n<tool-use-id>tool-ent-1</tool-use-id>\n<status>completed</status>\n<summary>echo &amp;&amp; ls &lt;dir&gt; ran with exit &quot;0&quot;</summary>\n</task-notification>"
	line := []byte(`{"type":"user","isReplay":true,"uuid":"entities","message":{"role":"user","content":` + jsonString(body) + `}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	want := `echo && ls <dir> ran with exit "0"`
	if events[0].Content != want {
		t.Fatalf("Content: got %q, want %q (XML entities must be decoded)", events[0].Content, want)
	}
}

// TestParseUser_Replay_DefensiveXMLWrappers covers the full set of
// Claude-injected XML wrappers we suppress defensively. None are
// currently wire-replayed by Claude in the agent-overflow setup, but
// the cost of detection is trivial and the cost of a future leak
// would be the same user-bubble bug we just fixed.
func TestParseUser_Replay_DefensiveXMLWrappers(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"system_reminder", "<system-reminder>This is an injected reminder.</system-reminder>"},
		{"agent_message", `<agent-message from="general-purpose">report body</agent-message>`},
		{"bash_input", "<bash-input>ls -la</bash-input>"},
		{"bash_stdout", "<bash-stdout>file1\nfile2</bash-stdout>"},
		{"bash_stderr", "<bash-stderr>permission denied</bash-stderr>"},
		{"local_command_stdout", "<local-command-stdout>/help body</local-command-stdout>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()
			line := []byte(`{"type":"user","isReplay":true,"uuid":"xml-` + tc.name + `","message":{"role":"user","content":` + jsonString(tc.content) + `}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("expected 0 events for %s wrapper, got %d: %+v", tc.name, len(events), events)
			}
		})
	}
}

// TestParseUser_Replay_AgentMessage_Suppressed is the incident replica
// (2026-07). Claude 2.1.x injects a completed subagent's final report
// into the PARENT conversation as a `queued_command` attachment, echoed
// on the `isReplay:true` user envelope wrapped in
// `<agent-message from="…">…</agent-message>`. Before this wrapper was
// catalogued it fell through to EventUserText and persisted as a
// top-level user bubble. It must now suppress like every other injected
// wrapper: zero events (it carries no `<task-notification>`, so there is
// nothing to enrich either).
func TestParseUser_Replay_AgentMessage_Suppressed(t *testing.T) {
	parser := NewParser()
	body := "<agent-message from=\"general-purpose\">\n# Research Report\n\nVerified best practices follow.\n\n## Section\n\nDetails.\n</agent-message>"
	line := []byte(`{"type":"user","isReplay":true,"uuid":"6958bdfb-8975-4779-860e-7a48c58b8ab2","message":{"role":"user","content":` + jsonString(body) + `}}`)
	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for an <agent-message> wrapper (must not emit EventUserText), got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_DefensiveOriginPrefixes covers the
// non-`human` origin prefix forms. We don't support coordinator
// mode or channel messages today, so the prefix-only check is
// enough.
func TestParseUser_Replay_DefensiveOriginPrefixes(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"coordinator", "The coordinator sent a message while you were working:\nDelegated work item completed."},
		{"channel_named", "A message arrived from teammate-alice while you were working:\nQuestion: what's the timeline?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := NewParser()
			line := []byte(`{"type":"user","isReplay":true,"uuid":"prefix-` + tc.name + `","message":{"role":"user","content":` + jsonString(tc.content) + `}}`)
			events, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("expected 0 events for %s prefix, got %d: %+v", tc.name, len(events), events)
			}
		})
	}
}

// TestParseUser_Replay_HumanOriginPrefix_Emits is the load-bearing
// preservation case: a real user message queued via stdin while a
// turn was running. Upstream wraps the body with the
// `wrapCommandText('human', ...)` prefix + IMPORTANT trailer. AO's
// pending-send FIFO is the authoritative deduper for these (matched
// by uuid downstream); the parser must continue emitting the event
// so triage's pending-send correlator can stamp `provider_item_id`
// onto the existing optimistic AO row.
func TestParseUser_Replay_HumanOriginPrefix_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"human-uuid-1","message":{"role":"user","content":"The user sent a new message while you were working:\nplease check on the deploy\n\nIMPORTANT: After completing your current task, you MUST address the user's message above. Do not ignore it."}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText for real user echo, got %d: %+v", len(events), events)
	}
	if events[0].Kind != provider.EventUserText {
		t.Fatalf("Kind: got %q, want %q", events[0].Kind, provider.EventUserText)
	}
	if events[0].Content == "" {
		t.Fatalf("Content empty — should preserve wrapped body for triage to stamp provider_item_id")
	}
	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["provider_item_id"] != "human-uuid-1" {
		t.Fatalf("provider_item_id: got %v, want human-uuid-1", meta["provider_item_id"])
	}
}

// TestParseUser_Replay_NoPrefix_StillEmits ensures the existing
// no-prefix replay shape (Codex-style raw echoes; Claude reattach
// edge cases — defensive future shapes covered by the `message.id`
// preference tests above) keeps emitting. The parser-layer fix is
// surgical: only known-injected shapes are dropped; everything else
// flows through unchanged.
func TestParseUser_Replay_NoPrefix_StillEmits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"raw-uuid-1","message":{"role":"user","content":"plain user content with no wrap prefix"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText for unwrapped echo, got %d: %+v", len(events), events)
	}
	if events[0].Content != "plain user content with no wrap prefix" {
		t.Fatalf("Content: got %q, want raw body", events[0].Content)
	}
}

// TestParseUser_Replay_TaskNotificationPrefixWithoutXML_Emits pins
// the false-positive defense: a real user message starting with the
// literal `"A background agent completed a task:\n"` text but
// without any `<task-notification>` XML body must NOT be suppressed.
// Vanishingly unlikely in practice but the test makes the
// suppression criterion precise.
func TestParseUser_Replay_TaskNotificationPrefixWithoutXML_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"prefix-no-xml","message":{"role":"user","content":"A background agent completed a task:\nbut here is just a plain follow-up question, no XML body"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText (prefix without XML is not load-bearing for suppression), got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_TaskNotificationXMLOpenWithoutClose_Emits
// covers the symmetric edge case: open tag without close tag. A
// truncated wire envelope or a real user typing `<task-notification`
// inline must not trigger suppression; both open AND close must be
// present.
func TestParseUser_Replay_TaskNotificationXMLOpenWithoutClose_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"open-no-close","message":{"role":"user","content":"talking about <task-notification> in a code block but no closing tag"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText (open without close not suppressed), got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_EmptyContent_Emits pins the boundary where
// `isClaudeInjectedReplayContent` early-returns false on empty
// trimmed content. The replay envelope still emits an EventUserText
// (with empty Content) so triage's pending-send correlator can stamp
// the provider_item_id onto the existing AO row — an
// initial-user-ack with empty content body is a real Claude wire
// shape (see TestParseUser_ReplayEnvelopeMissingMessageID coverage
// for the no-id variant).
func TestParseUser_Replay_EmptyContent_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"empty-content","message":{"role":"user","content":""}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText for empty content, got %d: %+v", len(events), events)
	}
	if events[0].Content != "" {
		t.Errorf("Content: got %q, want empty", events[0].Content)
	}
}

// TestParseUser_Replay_WhitespaceOnlyContent_Emits covers the next
// boundary: trimmed-empty content. The helper returns false (no
// suppression), so the envelope flows through and emits unchanged.
func TestParseUser_Replay_WhitespaceOnlyContent_Emits(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"ws-only","message":{"role":"user","content":"   \n\t  "}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText for whitespace-only content, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_MultipleWrappers_Suppressed pins the iterator
// behavior: when multiple distinct wrappers appear in the same body
// (rare but possible if a coordinator forwards a notification), the
// helper still suppresses. Guards against a future refactor that
// short-circuits on the first match in a way that breaks compound
// wrappers.
func TestParseUser_Replay_MultipleWrappers_Suppressed(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"multi-wrap","message":{"role":"user","content":"<system-reminder>noted</system-reminder>\n<task-notification>\n<status>completed</status>\n<summary>done</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for multi-wrapper content, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_AttributeFormTaskNotification_Suppressed pins
// the design choice in sessionfork.InjectedUserContentWrappers: the open
// prefix `<task-notification` (no closing `>`) is intentional so
// attribute-bearing variants match. A future refactor that switches to a
// strict `<task-notification>` literal would silently break
// attribute matching; this test fails loudly if that happens.
func TestParseUser_Replay_AttributeFormTaskNotification_Suppressed(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"attr-form","message":{"role":"user","content":"<task-notification severity=\"info\">\n<status>completed</status>\n<summary>attr-form body</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for attribute-form task-notification, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_PluralTypoNotSuppressedAlone pins the close-tag
// boundary: a hypothetical typo or unrelated tag like
// `<task-notifications>...</task-notifications>` (note the trailing
// `s`) must NOT suppress on its own — the open prefix matches but
// the close `</task-notification>` exact match doesn't (because
// `</task-notifications>` has `s>` after `notification`, not just
// `>`). Guards the close-tag exactness against a refactor that
// loosens it.
func TestParseUser_Replay_PluralTypoNotSuppressedAlone(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"plural-typo","message":{"role":"user","content":"discussing <task-notifications> elements (plural) — what are they?"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 EventUserText (plural form alone must not suppress), got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_RealUserPastingBalancedWrapper_DeliberateTradeoff
// documents the intentional false-positive: a real user pasting a
// code block that contains a balanced wrapper IS suppressed. This is
// the conservative choice (silent drop on rare paste vs. silent
// false-positive on every backgrounded completion). The test pins
// the tradeoff so a future "fix" attempt has to be deliberate, and
// the docstring documents the user-visible consequence.
//
// Practical impact: a developer asking "why does my message get
// dropped?" who pastes `<system-reminder>...</system-reminder>` will
// see their message vanish from the AO transcript. Claude still sees
// the message; only the AO-side user bubble is missing. Mitigation:
// users can wrap the discussion text without including a full
// balanced pair (e.g., quote one tag at a time).
func TestParseUser_Replay_RealUserPastingBalancedWrapper_DeliberateTradeoff(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"user-paste","message":{"role":"user","content":"why is my message dropped when I paste <system-reminder>foo</system-reminder> in a question?"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events (deliberate tradeoff: balanced wrapper inside real user text IS suppressed), got %d: %+v", len(events), events)
	}
}

// systemNotificationBundle reproduces the EXACT coalesced body
// probe_taskoutput_siblings.py captured on 2.1.170: when a sibling
// backgrounded command finishes while the agent is blocked on
// TaskOutput(block=true), the CLI flushes one
// "[SYSTEM NOTIFICATION - NOT USER INPUT]" + <task-notification> per
// completed task into a SINGLE message. Two preambled blocks here — the
// 5s sibling and the 10s TaskOutput-waited command.
const systemNotificationBundle = "[SYSTEM NOTIFICATION - NOT USER INPUT]\n" +
	"This is an automated background-task event, NOT a message from the user.\n" +
	"Do NOT interpret this as user acknowledgement, confirmation, or response to any pending question.\n\n" +
	"<task-notification>\n<task-id>bg5s</task-id>\n<tool-use-id>toolu_5s</tool-use-id>\n" +
	"<output-file>/tmp/5s.out</output-file>\n<status>completed</status>\n" +
	"<summary>Background command \"5s ticks\" completed (exit code 0)</summary>\n</task-notification>\n\n" +
	"[SYSTEM NOTIFICATION - NOT USER INPUT]\n" +
	"This is an automated background-task event, NOT a message from the user.\n" +
	"Do NOT interpret this as user acknowledgement, confirmation, or response to any pending question.\n\n" +
	"<task-notification>\n<task-id>bg10s</task-id>\n<tool-use-id>toolu_10s</tool-use-id>\n" +
	"<output-file>/tmp/10s.out</output-file>\n<status>completed</status>\n" +
	"<summary>Background command \"10s ticks\" completed (exit code 0)</summary>\n</task-notification>"

// TestExtractAllTaskNotificationFields pins the coalesced-body extractor: a
// single message bundling a <task-notification> per just-finished task yields
// EVERY block in wire order. Extracting only the first (the old behaviour)
// stranded every task after the first as "running" forever — the reported bug.
func TestExtractAllTaskNotificationFields(t *testing.T) {
	all := ExtractAllTaskNotificationFields(systemNotificationBundle)
	if len(all) != 2 {
		t.Fatalf("ExtractAll=%d want 2 (both bundled tasks): %+v", len(all), all)
	}
	if all[0].TaskID != "bg5s" || all[0].ToolUseID != "toolu_5s" || all[0].Status != "completed" {
		t.Fatalf("first notification fields wrong: %+v", all[0])
	}
	if all[1].TaskID != "bg10s" || all[1].ToolUseID != "toolu_10s" || all[1].OutputFile != "/tmp/10s.out" {
		t.Fatalf("second notification fields wrong: %+v", all[1])
	}
}

// TestExtractAllTaskNotificationFields_EdgeCases pins loop termination and the
// unroutable-element contract the callers rely on.
func TestExtractAllTaskNotificationFields_EdgeCases(t *testing.T) {
	if got := ExtractAllTaskNotificationFields("no notifications here"); len(got) != 0 {
		t.Fatalf("no-tag content want 0, got %d", len(got))
	}
	// A balanced block with no <task-id> is still RETURNED (TaskID==""), so the
	// caller skips it as unroutable rather than the extractor swallowing it.
	noID := "<task-notification>\n<status>completed</status>\n</task-notification>"
	got := ExtractAllTaskNotificationFields(noID)
	if len(got) != 1 || got[0].TaskID != "" || got[0].Routable() {
		t.Fatalf("no-task-id block want one non-routable element with empty TaskID, got %+v", got)
	}
	// An open tag with no close terminates the scan (no partial element).
	if got := ExtractAllTaskNotificationFields("<task-notification>\n<task-id>x</task-id>"); len(got) != 0 {
		t.Fatalf("unclosed block want 0, got %d: %+v", len(got), got)
	}
	// A stray close tag before the first open must NOT truncate the scan: the
	// valid block that follows still extracts. scanTaskNotification seeks the
	// close AFTER the open, so the leading orphan close is ignored. (Before that
	// fix this returned 0 — one malformed prefix stranded every later block,
	// which for the coalesced wire would resurrect the stuck-running bug.)
	strayClose := "</task-notification>\n<task-notification>\n<task-id>after-stray</task-id>\n" +
		"<status>completed</status>\n</task-notification>"
	if got := ExtractAllTaskNotificationFields(strayClose); len(got) != 1 || got[0].TaskID != "after-stray" {
		t.Fatalf("stray-leading-close want one element after-stray, got %+v", got)
	}
}

// TestParseUser_Replay_BundledTaskNotifications_EmitsEach pins the headless
// half of the coalesced-completion fix: one isReplay envelope can carry a
// <task-notification> per just-finished task (the same enqueuePendingNotification
// flush that bundles them on the /v1/messages wire). Each routable block emits
// its own EventBackgroundTaskNotification; the old first-only extract dropped
// every task after the first.
func TestParseUser_Replay_BundledTaskNotifications_EmitsEach(t *testing.T) {
	parser := NewParser()
	content := "<task-notification>\n<task-id>bgA</task-id>\n<tool-use-id>toolu_a</tool-use-id>\n<status>completed</status>\n<summary>A done</summary>\n</task-notification>\n\n" +
		"<task-notification>\n<task-id>bgB</task-id>\n<tool-use-id>toolu_b</tool-use-id>\n<status>completed</status>\n<summary>B done</summary>\n</task-notification>"
	line := []byte(`{"type":"user","isReplay":true,"uuid":"bundle","message":{"role":"user","content":` + jsonString(content) + `}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seen := map[string]string{} // tool_use_id (ItemID) -> task_id
	for _, ev := range events {
		if ev.Kind != provider.EventBackgroundTaskNotification {
			t.Fatalf("unexpected event kind %q: %+v", ev.Kind, ev)
		}
		var meta map[string]any
		if err := json.Unmarshal(ev.Meta, &meta); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		seen[ev.ItemID], _ = meta["task_id"].(string)
	}
	if len(seen) != 2 || seen["toolu_a"] != "bgA" || seen["toolu_b"] != "bgB" {
		t.Fatalf("want both bundled notifications {toolu_a:bgA, toolu_b:bgB}, got %v (events=%+v)", seen, events)
	}
}

// jsonString quotes s as a JSON string literal (with surrounding
// double quotes) so test fixtures can embed arbitrary content
// without escaping every character. The result drops directly into
// a JSON object value position, e.g. `"content":` + jsonString(body).
func jsonString(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(out)
}
