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
// detection is done at the parser layer so triage's destructive
// FIFO pop in `consumePendingSendHead` is never corrupted.
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

// TestParseUser_Replay_TaskNotificationTerminal_Suppressed exercises
// the load-bearing path: a backgrounded bash completes, the queue
// drains the XML attachment, Claude echoes it back via
// `--replay-user-messages`. The wire body is the
// `wrapCommandText('task-notification', ...)` output. Must NOT
// produce an EventUserText (that would render as a user-bubble row).
func TestParseUser_Replay_TaskNotificationTerminal_Suppressed(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"task-notif-uuid-1","message":{"role":"user","content":"A background agent completed a task:\n<task-notification>\n<task-id>task-bg-1</task-id>\n<tool-use-id>tool-bg-1</tool-use-id>\n<output-file>/tmp/agent-out</output-file>\n<status>completed</status>\n<summary>Background command \"sleep 5\" completed (exit code 0)</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for Claude-injected task-notification echo, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_TaskNotificationStallPing_Suppressed pins the
// statusless variant emitted by the stall watchdog
// (`LocalShellTask.tsx:80-95`). No <status> tag, otherwise same shape.
// SDK system event path doesn't fire (`print.ts:2070` gates on
// status), so the wire-replay echo is the only surface — and we
// suppress it. v1 simplification; can re-add as a notification-kind
// row later if user demand surfaces.
func TestParseUser_Replay_TaskNotificationStallPing_Suppressed(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"stall-ping-uuid","message":{"role":"user","content":"A background agent completed a task:\n<task-notification>\n<task-id>task-stall-1</task-id>\n<output-file>/tmp/agent-stall</output-file>\n<summary>Background command \"npm install\" appears to be waiting for interactive input</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for statusless stall-ping echo, got %d: %+v", len(events), events)
	}
}

// TestParseUser_Replay_BareTaskNotificationXML_Suppressed covers the
// defensive path where the wrap prefix is missing but the XML body
// is present. Could happen if a future upstream change ships the
// XML directly without the human-facing prefix. Both tags must match
// — open + close — so a single mention in real user text doesn't
// trigger the suppression.
func TestParseUser_Replay_BareTaskNotificationXML_Suppressed(t *testing.T) {
	parser := NewParser()
	line := []byte(`{"type":"user","isReplay":true,"uuid":"bare-xml-uuid","message":{"role":"user","content":"<task-notification>\n<task-id>task-bare</task-id>\n<status>failed</status>\n<summary>oops</summary>\n</task-notification>"}}`)

	events, err := parser.ParseLine(testThread, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for bare task-notification XML, got %d: %+v", len(events), events)
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
// the design choice in claudeInjectedXMLWrappers: the open prefix
// `<task-notification` (no closing `>`) is intentional so attribute-
// bearing variants match. A future refactor that switches to a
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
