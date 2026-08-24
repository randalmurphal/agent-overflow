package rollout

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// Synthetic paginated fixtures. Every line here is HAND-WRITTEN against the
// shapes declared at codex tag **rust-v0.149.0** (`codex-rs/protocol/src/
// items.rs` for TurnItem, `codex-rs/ext/items/src/lib.rs` for the flattened
// ExtensionItem, `codex-rs/history/src/rollout_payload.rs` for the envelope);
// nothing is copied out of a provider home. The variant tags are PascalCase
// because TurnItem carries `#[serde(tag = "type")]` with no `rename_all` —
// which is NOT the camelCase app-server v2 `ThreadItem` mirror of the same
// data.
const (
	paginatedMetaLine = `{"timestamp":"2026-08-07T19:07:44.339Z","ordinal":0,"type":"session_meta","payload":{"id":"` + testSessionID +
		`","cwd":"/repo","originator":"codex_cli","cli_version":"0.149.0","history_mode":"paginated","git":{"branch":"main"}}}`

	// The `response_item` mirror a paginated rollout still writes for every
	// message and reasoning item (policy.rs `should_persist_response_item`
	// is unconditional), and which AO prefers over the item — see items.go.
	pagUserMirrorLine = `{"timestamp":"2026-08-07T19:07:47.100Z","ordinal":3,"type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}`
	pagAgentItemLine  = `{"timestamp":"2026-08-07T19:07:59.000Z","ordinal":8,"type":"event_msg","payload":{"type":"item_completed","thread_id":"` + testSessionID +
		`","turn_id":"turn-1","started_at_ms":1786133878000,"completed_at_ms":1786133879000,"item":{"type":"AgentMessage","id":"msg-1","content":[{"type":"Text","text":"done"}],"phase":"final_answer","delivery":"async"}}}`
	pagAgentMirrorLine = `{"timestamp":"2026-08-07T19:07:59.100Z","ordinal":9,"type":"response_item","payload":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`

	pagExecItemLine = `{"timestamp":"2026-08-07T19:07:50.000Z","ordinal":5,"type":"event_msg","payload":{"type":"item_completed","thread_id":"` + testSessionID +
		`","turn_id":"turn-1","started_at_ms":1786133870000,"completed_at_ms":1786133871500,"item":{"type":"CommandExecution","id":"exec-1","process_id":"4242",` +
		`"command":["/bin/zsh","-lc","ls -la"],"cwd":"file:///repo/sub%20dir","parsed_cmd":[{"type":"read","cmd":"ls -la"}],` +
		`"source":"unified_exec_startup","status":"completed","stdout":"total 0\n","aggregated_output":"total 0\n","exit_code":0}}}`

	pagFileChangeItemLine = `{"timestamp":"2026-08-07T19:07:52.000Z","ordinal":6,"type":"event_msg","payload":{"type":"item_completed","thread_id":"` + testSessionID +
		`","turn_id":"turn-1","started_at_ms":1786133872000,"completed_at_ms":1786133872400,"item":{"type":"FileChange","id":"exec-2","status":"completed","auto_approved":true,` +
		`"changes":{"/repo/a.go":{"type":"update","unified_diff":"@@ -1,2 +1,3 @@\n line\n+added\n","move_path":null},` +
		`"/repo/new.txt":{"type":"add","content":"hello\nworld\n"}}}}}`
)

func pagLine(t *testing.T, ordinal int, timestamp string, item map[string]any, startedAtMS, completedAtMS any) string {
	t.Helper()
	payload := map[string]any{
		"type":            "item_completed",
		"thread_id":       testSessionID,
		"turn_id":         "turn-1",
		"item":            item,
		"started_at_ms":   startedAtMS,
		"completed_at_ms": completedAtMS,
	}
	encoded, err := json.Marshal(map[string]any{
		"timestamp": timestamp,
		"ordinal":   ordinal,
		"type":      "event_msg",
		"payload":   payload,
	})
	if err != nil {
		t.Fatalf("encode fixture line: %v", err)
	}
	return string(encoded)
}

func toolsByName(events []importir.Event, name string) []importir.Event {
	var out []importir.Event
	for _, e := range events {
		if e.Kind != provider.EventToolComplete || len(e.Meta) == 0 {
			continue
		}
		var meta map[string]any
		if json.Unmarshal(e.Meta, &meta) != nil {
			continue
		}
		if meta["toolName"] == name {
			out = append(out, e)
		}
	}
	return out
}

// ------------------------------------------------------- the paginated set

// The regression the whole file exists for: before item handling, a
// paginated rollout imported with none of its tool detail — every command,
// diff, MCP result and web search was invisible, because those legacy
// `*_end` records are simply not written in that mode.
func TestParsePaginatedRolloutImportsToolItems(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, turnContextLine, taskStartedLine,
		pagUserMirrorLine,
		pagExecItemLine,
		pagFileChangeItemLine,
		pagAgentItemLine, pagAgentMirrorLine,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if res.Meta.HistoryMode != HistoryModePaginated {
		t.Fatalf("history mode = %q, want paginated", res.Meta.HistoryMode)
	}
	if len(res.UnknownTypes) != 0 {
		t.Fatalf("unknown types = %v, want none", res.UnknownTypes)
	}

	execs := toolsByName(res.Events, "Bash")
	if len(execs) != 1 {
		t.Fatalf("command rows = %d, want 1: %v", len(execs), kinds(res.Events))
	}
	if got := metaField(t, execs[0].Meta, "command"); got != "/bin/zsh -lc ls -la" {
		t.Errorf("command = %v", got)
	}
	// PathUri, percent-decoded back to the path a human reads.
	if got := metaField(t, execs[0].Meta, "cwd"); got != "/repo/sub dir" {
		t.Errorf("cwd = %v, want the decoded path", got)
	}
	if got := metaField(t, execs[0].Meta, "exit_code"); got != float64(0) {
		t.Errorf("exit_code = %v", got)
	}
	if got := metaField(t, execs[0].Meta, "item_status"); got != "completed" {
		t.Errorf("item_status = %v", got)
	}
	if execs[0].Content != "total 0\n" {
		t.Errorf("output = %q", execs[0].Content)
	}
	// The item's own clock, not the line's timestamp.
	if got := execs[0].Timestamp.UnixMilli(); got != 1786133871500 {
		t.Errorf("completion clock = %d, want the item's completed_at_ms", got)
	}

	patches := toolsByName(res.Events, "file_change")
	if len(patches) != 1 {
		t.Fatalf("file-change rows = %d, want 1", len(patches))
	}
	diffs := eventsOfKind(res.Events, provider.EventDiff)
	if len(diffs) != 1 {
		t.Fatalf("diff rows = %d, want 1", len(diffs))
	}
	if !strings.Contains(diffs[0].Content, "--- a/repo/a.go") &&
		!strings.Contains(diffs[0].Content, "--- a//repo/a.go") {
		t.Errorf("diff missing the update hunk:\n%s", diffs[0].Content)
	}
	// An `add` carries the whole file as `content` and no hunks at all; it
	// used to render as nothing.
	if !strings.Contains(diffs[0].Content, "+hello") || !strings.Contains(diffs[0].Content, "new file") {
		t.Errorf("diff missing the added file:\n%s", diffs[0].Content)
	}
}

// The precedence rule: the `response_item` mirror owns message and reasoning
// content, the item is dropped. Emitting both would double every message in
// a paginated thread.
func TestParsePaginatedPrefersTheMirrorOverContentItems(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		pagUserMirrorLine,
		pagAgentItemLine, pagAgentMirrorLine,
		pagLine(t, 10, "2026-08-07T19:07:59.500Z", map[string]any{
			"type": "Reasoning", "id": "rs-1",
			"summary_text": []any{"first thought"}, "raw_content": []any{},
		}, 1786133879500, 1786133879600),
		`{"timestamp":"2026-08-07T19:07:59.600Z","ordinal":11,"type":"response_item","payload":{"type":"reasoning","id":"rs-1","summary":[{"type":"summary_text","text":"first thought"}]}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if got := countKind(res.Events, provider.EventUserText); got != 1 {
		t.Fatalf("user rows = %d, want 1", got)
	}
	blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want one assistant + one thinking", len(blocks))
	}
	if blocks[0].Content != "done" || blocks[1].Content != "first thought" {
		t.Fatalf("blocks = %q / %q", blocks[0].Content, blocks[1].Content)
	}
	if len(res.UnknownTypes) != 0 {
		t.Fatalf("content items must be recognised-and-dropped, not unknown: %v", res.UnknownTypes)
	}
}

// A file carrying BOTH the paginated items and legacy event_msg content
// records (the shape a partially-migrated or hand-edited rollout has). Each
// message must appear exactly once, and the mirror is what decides it.
func TestParsePaginatedMixedWithLegacyEventMsgDoesNotDoubleEmit(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		// Legacy region: the event_msg records AND their mirrors.
		userMsgLine,
		pagUserMirrorLine,
		agentMsgLine,
		pagAgentMirrorLine,
		// Paginated region: the item AND its mirror, same content.
		pagLine(t, 12, "2026-08-07T19:08:01.000Z", map[string]any{
			"type": "AgentMessage", "id": "msg-2",
			"content": []any{map[string]any{"type": "Text", "text": "second answer"}},
		}, 1786133881000, 1786133881100),
		`{"timestamp":"2026-08-07T19:08:01.100Z","ordinal":13,"type":"response_item","payload":{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if got := countKind(res.Events, provider.EventUserText); got != 1 {
		t.Fatalf("user rows = %d, want exactly 1", got)
	}
	blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(blocks) != 2 {
		t.Fatalf("assistant rows = %d, want exactly 2: %q", len(blocks), contents(blocks))
	}
	if blocks[0].Content != "done" || blocks[1].Content != "second answer" {
		t.Fatalf("assistant rows = %q", contents(blocks))
	}
}

func contents(events []importir.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Content)
	}
	return out
}

// The item is written one line BEFORE the `response_item` carrying the same
// call id. Whichever arrives first, exactly one row must exist.
func TestParsePaginatedItemAndResponseCallProduceOneRow(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		pagLine(t, 5, "2026-08-07T19:07:50.000Z", map[string]any{
			"type": "WebSearch", "id": "ws-1", "query": "postgres returning",
			"action": map[string]any{"type": "search", "query": "postgres returning"},
		}, 1786133870000, 1786133871000),
		`{"timestamp":"2026-08-07T19:07:50.100Z","ordinal":6,"type":"response_item","payload":{"type":"web_search_call","id":"ws-1","status":"completed","action":{"type":"search","query":"postgres returning"}}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	searches := toolsByName(res.Events, "web_search")
	if len(searches) != 1 {
		t.Fatalf("web-search rows = %d, want 1", len(searches))
	}
	if got := countKind(res.Events, provider.EventToolStart); got != 1 {
		t.Fatalf("tool starts = %d, want 1", got)
	}
	for _, e := range res.Events {
		if len(e.Meta) > 0 && strings.Contains(string(e.Meta), MetaImportUnavailableKey) {
			t.Fatalf("a settled call must not also produce an unavailable marker: %s", e.Meta)
		}
	}
}

// The `compacted` record is the one that carries the summary and always
// precedes its item, exactly as it precedes the legacy `context_compacted`
// twin. One divider, not two.
func TestParsePaginatedContextCompactionItemDoesNotDoubleTheDivider(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:55.000Z","ordinal":6,"type":"compacted","payload":{"message":"summary of the work","window_id":"w-1","replacement_history":[]}}`,
		pagLine(t, 6, "2026-08-07T19:07:55.100Z", map[string]any{
			"type": "ContextCompaction", "id": "cc-1",
		}, 1786133875000, 1786133875100),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	dividers := eventsOfKind(res.Events, provider.EventCompactBoundary)
	if len(dividers) != 1 {
		t.Fatalf("compaction dividers = %d, want 1", len(dividers))
	}
	if dividers[0].Content != "summary of the work" {
		t.Errorf("divider content = %q, want the durable record's summary", dividers[0].Content)
	}
}

// The remaining variants, each mapped onto the same row shape its legacy
// counterpart produces.
func TestParsePaginatedMapsTheRemainingItemVariants(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		pagLine(t, 5, "2026-08-07T19:07:50.000Z", map[string]any{
			"type": "McpToolCall", "id": "mcp-1", "server": "linear", "tool": "issues",
			"arguments": map[string]any{"team": "core"}, "status": "completed",
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "3 issues"}}},
		}, 1786133870000, 1786133870500),
		pagLine(t, 6, "2026-08-07T19:07:51.000Z", map[string]any{
			"type": "DynamicToolCall", "id": "dyn-1", "namespace": "fs", "tool": "read",
			"arguments": map[string]any{"path": "/repo/a.go"}, "status": "failed", "success": false,
			"error": "permission denied",
		}, 1786133871000, 1786133871200),
		pagLine(t, 7, "2026-08-07T19:07:52.000Z", map[string]any{
			"type": "ImageView", "id": "img-1", "path": "file:///repo/shot.png",
		}, 1786133872000, 1786133872100),
		pagLine(t, 8, "2026-08-07T19:07:53.000Z", map[string]any{
			"type": "Extension", "kind": "clock.sleep", "id": "sleep-1", "durationMs": 1500,
		}, 1786133873000, 1786133874500),
		pagLine(t, 9, "2026-08-07T19:07:55.000Z", map[string]any{
			"type": "SubAgentActivity", "id": "spawn-1", "kind": "started",
			"agent_thread_id": "child-1", "agent_path": "root/reviewer",
		}, 1786133875000, 1786133875100),
		pagLine(t, 10, "2026-08-07T19:07:56.000Z", map[string]any{
			"type": "EnteredReviewMode", "id": "rev-1",
			"target": map[string]any{"type": "uncommitted_changes"}, "user_facing_hint": "the working tree",
		}, 1786133876000, 1786133876100),
		pagLine(t, 11, "2026-08-07T19:07:57.000Z", map[string]any{
			"type": "ExitedReviewMode", "id": "rev-2",
			"review_output": map[string]any{
				"findings": []any{}, "overall_correctness": "patch is correct",
				"overall_explanation": "no blocking issues", "overall_confidence_score": 0.9,
			},
		}, 1786133877000, 1786133877100),
		pagLine(t, 12, "2026-08-07T19:07:58.000Z", map[string]any{
			"type": "Plan", "id": "plan-1", "text": "1. do it",
		}, 1786133878000, 1786133878100),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if len(res.UnknownTypes) != 0 {
		t.Fatalf("unknown types = %v, want none", res.UnknownTypes)
	}
	mcp := toolsByName(res.Events, "linear__issues")
	if len(mcp) != 1 || mcp[0].Content != "3 issues" {
		t.Fatalf("mcp rows = %+v", mcp)
	}
	dyn := toolsByName(res.Events, "fs__read")
	if len(dyn) != 1 {
		t.Fatalf("dynamic tool rows = %d, want 1", len(dyn))
	}
	if got := metaField(t, dyn[0].Meta, "is_error"); got != true {
		t.Errorf("failed dynamic call is_error = %v", got)
	}
	if len(toolsByName(res.Events, "view_image")) != 1 {
		t.Errorf("no image-view row: %v", kinds(res.Events))
	}

	notes := eventsOfKind(res.Events, provider.EventNotification)
	var summaries []string
	for _, n := range notes {
		summaries = append(summaries, n.Content)
	}
	joined := strings.Join(summaries, "|")
	for _, want := range []string{"Agent paused for 1.5s", "root/reviewer started"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notifications %q missing %q", joined, want)
		}
	}
	if len(toolsByName(res.Events, importedCodexReviewToolName)) != 1 {
		t.Errorf("no projected review launch: %v", kinds(res.Events))
	}
	if got := countKind(res.Events, provider.EventCommandResult); got != 1 {
		t.Errorf("review result rows = %d, want 1", got)
	}
	if got := countKind(res.Events, provider.EventProposedPlan); got != 1 {
		t.Errorf("plan rows = %d, want 1", got)
	}
}

// A migrated rollout writes `started_at_ms: null` for every item it
// synthesises, because the legacy record it came from carried no start time.
// The gap must degrade to the line's own clock, never to a zero timestamp —
// the import writer refuses an event with no clock at all.
func TestParsePaginatedItemWithNoStartedAtStillCarriesAClock(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		pagLine(t, 5, "2026-08-07T19:07:50.000Z", map[string]any{
			"type": "CommandExecution", "id": "exec-9", "command": []any{"ls"},
			"cwd": "/repo", "parsed_cmd": []any{}, "source": "agent",
			"status": "completed", "exit_code": 0, "stdout": "ok\n",
		}, nil, nil),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	for _, e := range res.Events {
		if e.Timestamp.IsZero() {
			t.Fatalf("event %s has no timestamp", e.Kind)
		}
	}
	if len(toolsByName(res.Events, "Bash")) != 1 {
		t.Fatalf("command rows = %v", kinds(res.Events))
	}
}

// A variant this build has never seen must be counted and skipped, never
// fatal: the enum is open and the installed CLI is routinely ahead of any
// checkout.
func TestParseUnknownItemVariantIsCountedNotFatal(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		pagLine(t, 5, "2026-08-07T19:07:50.000Z", map[string]any{
			"type": "SomethingNewIn0150", "id": "x-1",
		}, 1786133870000, 1786133870100),
		pagLine(t, 6, "2026-08-07T19:07:51.000Z", map[string]any{
			"type": "Extension", "kind": "future.thing", "id": "x-2",
		}, 1786133871000, 1786133871100),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if res.UnknownTypes["event_msg/item_completed/SomethingNewIn0150"] != 1 {
		t.Errorf("unknown variant not counted: %v", res.UnknownTypes)
	}
	if res.UnknownTypes["event_msg/item_completed/Extension/future.thing"] != 1 {
		t.Errorf("unknown extension kind not counted: %v", res.UnknownTypes)
	}
}

// ------------------------------------------------------ additive tolerance

// Three additive changes Codex shipped without a wire break. None may cost a
// row, and none may fail a parse.
func TestParseToleratesAdditiveWireChanges(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		// A `response_item` line with the sibling `metadata` key
		// (RolloutItemWire::ResponseItem's second field). Ignored; the
		// payload still lands.
		`{"timestamp":"2026-08-07T19:07:47.100Z","ordinal":3,"type":"response_item","metadata":{"turn_id":"turn-1","source":"harness"},"payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}`,
		// A `security_risk_score` envelope (SecurityRiskScore @
		// rust-v0.149.0). Recognised and dropped: upstream forbids these
		// scores from reaching a user-visible thread item projection.
		`{"timestamp":"2026-08-07T19:07:48.000Z","ordinal":4,"type":"security_risk_score","payload":{"scores":{"prompt_injection":0.02,"data_exfiltration":0.11},"sampled_at":"2026-08-07T19:07:47.900Z"}}`,
		// A `world_state` envelope (WorldStateItem @ rust-v0.149.0), the
		// engine's resume baseline for model-visible context diffing. Also
		// recognised and dropped — Codex writes one per turn.
		`{"timestamp":"2026-08-07T19:07:48.500Z","ordinal":5,"type":"world_state","payload":{"full":false,"state":{"open_files":["a.go"]}}}`,
		// A `compacted` record that grew `mcp_resource_origins`.
		`{"timestamp":"2026-08-07T19:07:55.000Z","ordinal":6,"type":"compacted","payload":{"message":"summary","window_id":"w-1","replacement_history":[],"mcp_resource_origins":[{"server":"linear","uri":"linear://issue/1"}]}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if res.CorruptLines != 0 {
		t.Fatalf("corrupt lines = %d, want 0", res.CorruptLines)
	}
	users := eventsOfKind(res.Events, provider.EventUserText)
	if len(users) != 1 || users[0].Content != "do the thing" {
		t.Fatalf("user rows = %+v, want the message beside the metadata sibling", users)
	}
	// Neither engine-bookkeeping record may reach the unknown counter: a
	// warning that fires on every modern import is noise that hides the
	// drift the warning exists to report.
	if len(res.UnknownTypes) != 0 {
		t.Errorf("unknown types = %v, want none", res.UnknownTypes)
	}
	dividers := eventsOfKind(res.Events, provider.EventCompactBoundary)
	if len(dividers) != 1 || dividers[0].Content != "summary" {
		t.Fatalf("compaction dividers = %+v, want the one summary", dividers)
	}
}

// ------------------------------------------------------------ history_base

// A rollout whose history begins in ANOTHER file must say so. AO does not
// follow the chain, and presenting a truncated thread as complete is the one
// answer that is worse than a warning.
func TestParseWarnsWhenTheThreadInheritsHistoryFromAnotherRollout(t *testing.T) {
	path := writeRollout(t, testSessionID,
		`{"timestamp":"2026-08-07T19:07:44.339Z","ordinal":0,"type":"session_meta","payload":{"id":"`+testSessionID+
			`","cwd":"/repo","history_mode":"paginated","history_base":{"thread_id":"01a00741-0000-0000-0000-000000000001","end_ordinal_exclusive":412,"end_byte_offset":98765}}}`,
		taskStartedLine, agentMsgLine, taskCompleteLn,
	)
	res := parseFixture(t, path)

	if res.Meta.HistoryBase == nil {
		t.Fatalf("history_base not parsed: %+v", res.Meta)
	}
	if res.Meta.HistoryBase.EndOrdinalExclusive != 412 || res.Meta.HistoryBase.EndByteOffset != 98765 {
		t.Errorf("history_base = %+v", *res.Meta.HistoryBase)
	}
	var found *importir.Warning
	for i := range res.Warnings {
		if res.Warnings[i].Code == WarnHistoryBase {
			found = &res.Warnings[i]
		}
	}
	if found == nil {
		t.Fatalf("no %s warning: %+v", WarnHistoryBase, res.Warnings)
	}
	if !strings.Contains(found.Message, "01a00741-0000-0000-0000-000000000001") {
		t.Errorf("warning should name the prefix rollout: %q", found.Message)
	}
}

// A legacy file must keep every legacy behaviour: no history_base, no
// paginated mode, and the event_msg records still win over the mirror.
func TestParseLegacyFileIsUnchangedByItemHandling(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, taskStartedLine, userMsgLine, pagUserMirrorLine, agentMsgLine, taskCompleteLn)
	res := parseFixture(t, path)

	if res.Meta.HistoryMode != "" || res.Meta.HistoryBase != nil {
		t.Fatalf("legacy meta = %+v", res.Meta)
	}
	if got := countKind(res.Events, provider.EventUserText); got != 1 {
		t.Fatalf("user rows = %d, want 1 (the event_msg record only)", got)
	}
}

// Recognising a record type must not turn into blanket silence: the two
// dropped engine records are dropped BECAUSE their 0.149.0 shape says they
// carry nothing importable. A payload that no longer matches that shape is
// drift, and drift is exactly what the unknown-types warning is for.
func TestParseWarnsWhenADroppedRecordChangesShape(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		// `full` gone, `state` renamed: no longer the WorldStateItem we read.
		`{"timestamp":"2026-08-07T19:07:48.000Z","ordinal":4,"type":"world_state","payload":{"snapshot":{"open_files":[]}}}`,
		// `scores` gone: no longer the SecurityRiskScore we read.
		`{"timestamp":"2026-08-07T19:07:48.500Z","ordinal":5,"type":"security_risk_score","payload":{"verdict":"low"}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if res.UnknownTypes["world_state (unexpected shape)"] != 1 {
		t.Errorf("changed world_state shape not reported: %v", res.UnknownTypes)
	}
	if res.UnknownTypes["security_risk_score (unexpected shape)"] != 1 {
		t.Errorf("changed security_risk_score shape not reported: %v", res.UnknownTypes)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Code == WarnUnknownTypes {
			found = true
		}
	}
	if !found {
		t.Errorf("drift should reach the user as a warning: %+v", res.Warnings)
	}
}

// SleepItem is the ONE extension item that opts into
// `#[serde(rename_all = "camelCase")]` (codex-rs/ext/items/src/sleep.rs,
// unchanged rust-v0.146 → rust-v0.149), so `durationMs` is what every observed
// file carries and is the spelling that must keep working. `duration_ms` is
// what its Rust field name and every sibling core item would produce, and is
// accepted so losing that one attribute upstream would be inert here rather
// than silently importing every pause as "Agent paused" with no duration.
func TestParsePaginatedReadsBothSleepDurationSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"camelCase (the observed wire spelling)", "durationMs"},
		{"snake_case (the sibling-item spelling)", "duration_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRollout(t, testSessionID,
				paginatedMetaLine, taskStartedLine,
				pagLine(t, 5, "2026-08-07T19:07:53.000Z", map[string]any{
					"type": "Extension", "kind": "clock.sleep", "id": "sleep-1", tc.key: 5000,
				}, 1786133873000, 1786133878000),
				taskCompleteLn,
			)
			res := parseFixture(t, path)
			if len(res.UnknownTypes) != 0 {
				t.Fatalf("unknown types = %v, want none", res.UnknownTypes)
			}
			var found bool
			for _, ev := range eventsOfKind(res.Events, provider.EventNotification) {
				if metaField(t, ev.Meta, "kind") != "sleep" {
					continue
				}
				found = true
				if ev.Content != "Agent paused for 5s" {
					t.Fatalf("sleep summary = %q, want the 5000ms duration", ev.Content)
				}
			}
			if !found {
				t.Fatalf("no sleep notification: %v", kinds(res.Events))
			}
		})
	}
}
