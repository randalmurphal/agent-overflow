package rollout

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// ------------------------------------------------------------------- tool calls

const (
	execCallLine = `{"timestamp":"2026-08-07T19:07:52.000Z","type":"response_item","payload":{"type":"function_call","id":"fc1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\",\"workdir\":\"/repo\"}","call_id":"call_A","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}`
	execOutLine  = `{"timestamp":"2026-08-07T19:07:55.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_A","output":[{"type":"input_text","text":"ok\n"}]}}`
)

func TestConvertMatchesToolCallsByCallID(t *testing.T) {
	other := `{"timestamp":"2026-08-07T19:07:52.500Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc1","call_id":"call_B","name":"exec","input":"await tools.exec_command({cmd:\"ls\"})"}}`
	otherOut := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_B","output":[{"type":"input_text","text":"listing"}]}}`

	// Interleaved calls: the outputs arrive out of order, which is exactly
	// what parallel tool use looks like on the wire.
	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, taskStartedLine, execCallLine, other, otherOut, execOutLine, taskCompleteLn))

	starts := eventsOfKind(res.Events, provider.EventToolStart)
	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(starts) != 2 || len(completes) != 2 {
		t.Fatalf("starts=%d completes=%d, want 2/2", len(starts), len(completes))
	}
	byID := map[string]importir.Event{}
	for _, e := range completes {
		byID[e.ItemID] = e
	}
	if byID["call_A"].Content != "ok\n" {
		t.Fatalf("call_A output = %q", byID["call_A"].Content)
	}
	if byID["call_B"].Content != "listing" {
		t.Fatalf("call_B output = %q", byID["call_B"].Content)
	}
	// Both unified-exec shapes present as the same command tool a live
	// Codex session produces, with the wire name preserved.
	for _, e := range starts {
		if metaField(t, e.Meta, "toolName") != "Bash" {
			t.Fatalf("toolName = %v, want Bash: %s", metaField(t, e.Meta, "toolName"), e.Meta)
		}
	}
	if metaField(t, byID["call_B"].Meta, "codexToolName") != "exec" {
		t.Fatalf("wire tool name not preserved: %s", byID["call_B"].Meta)
	}
	// function_call arguments arrive as a JSON *string*; cmd/workdir are
	// folded onto the keys AO's shared preview helpers read.
	input := metaField(t, byID["call_A"].Meta, "input").(map[string]any)
	if input["command"] != "go test ./..." || input["cwd"] != "/repo" {
		t.Fatalf("input not normalized: %+v", input)
	}
	// The custom tool's opaque script becomes the command so it previews.
	inputB := metaField(t, byID["call_B"].Meta, "input").(map[string]any)
	if !strings.Contains(inputB["command"].(string), "tools.exec_command") {
		t.Fatalf("script input not carried: %+v", inputB)
	}
}

// exec_command_end is only present in older rollouts (Codex no longer
// persists it) and arrives BEFORE the call's output line. It is matched by
// call_id like every other end event — never by command text or arrival
// order — and is the only source of an exit code on the import path.
func TestConvertFoldsExecCommandEndIntoTheCompletion(t *testing.T) {
	end := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_A","turn_id":"turn-1","command":["/bin/zsh","-lc","go test ./..."],"cwd":"/repo","exit_code":2,"stdout":"","stderr":"boom","aggregated_output":"FAIL","source":"unified_exec_startup"}}`
	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, taskStartedLine, execCallLine, end, execOutLine, taskCompleteLn))

	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("completes = %d, want 1 (the end event must enrich, not duplicate)", len(completes))
	}
	meta := completes[0].Meta
	if metaField(t, meta, "exit_code") != float64(2) {
		t.Fatalf("exit_code not folded in: %s", meta)
	}
	if metaField(t, meta, "is_error") != true {
		t.Fatalf("non-zero exit should mark the row errored: %s", meta)
	}
	if metaField(t, meta, "cwd") != "/repo" {
		t.Fatalf("cwd not folded in: %s", meta)
	}
	// The output line still wins for content; the end event only fills gaps.
	if completes[0].Content != "ok\n" {
		t.Fatalf("content = %q", completes[0].Content)
	}
}

// An end record that names no known call but carries the whole result stands
// on its own. This is the dominant case for patches: `apply_patch` run from
// inside an `exec` script is stamped with a synthetic `exec-<uuid>` call id
// that appears nowhere else in the file, and turning those into "unavailable"
// placeholders would discard most of a session's file changes.
func TestConvertEmitsSelfContainedEndEventsAsTheirOwnRow(t *testing.T) {
	patch := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"exec-11e3397e-5a36-4765-b72d-b41ef87b4109","turn_id":"turn-1","stdout":"Success","success":true,"changes":{"/repo/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-a\n+b\n"}}}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, patch, taskCompleteLn))

	starts := eventsOfKind(res.Events, provider.EventToolStart)
	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	diffs := eventsOfKind(res.Events, provider.EventDiff)
	if len(starts) != 1 || len(completes) != 1 || len(diffs) != 1 {
		t.Fatalf("starts=%d completes=%d diffs=%d, want 1/1/1", len(starts), len(completes), len(diffs))
	}
	if metaField(t, starts[0].Meta, "toolName") != "file_change" {
		t.Fatalf("standalone patch row tool = %s", starts[0].Meta)
	}
	if !strings.Contains(diffs[0].Content, "+b") {
		t.Fatalf("diff lost: %q", diffs[0].Content)
	}
	if metaField(t, completes[0].Meta, "import_unavailable") != nil {
		t.Fatalf("a complete record must not be labelled unavailable: %s", completes[0].Meta)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnUnmatchedEnd {
			t.Fatalf("a self-contained record is not a degraded import: %+v", res.Warnings)
		}
	}
}

// Only an end record with nothing left to show degrades to the marker.
func TestConvertDegradesContentlessUnmatchedEndEvents(t *testing.T) {
	orphan := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"ws_GONE","query":"","action":{"type":"other"}}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, orphan, taskCompleteLn))

	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("want one degraded row, got %d", len(completes))
	}
	if metaField(t, completes[0].Meta, "import_unavailable") != "exec-detail" {
		t.Fatalf("want the import_unavailable marker: %s", completes[0].Meta)
	}
	var warned bool
	for _, w := range res.Warnings {
		if w.Code == WarnUnmatchedEnd && strings.Contains(w.Message, "ws_GONE") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("an unmatched end event must warn: %+v", res.Warnings)
	}
}

func TestConvertPatchApplyEndBecomesADiff(t *testing.T) {
	call := `{"timestamp":"2026-08-07T19:07:52.000Z","type":"response_item","payload":{"type":"custom_tool_call","id":"ctc1","call_id":"call_P","name":"apply_patch","input":"*** Begin Patch"}}`
	end := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_P","turn_id":"turn-1","stdout":"Success","stderr":"","success":true,"changes":{"/repo/a.go":{"type":"update","unified_diff":"@@ -1,2 +1,2 @@\n-old\n+new\n"}}}}`
	out := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_P","output":[{"type":"input_text","text":"Success"}]}}`

	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, call, end, out, taskCompleteLn))

	diffs := eventsOfKind(res.Events, provider.EventDiff)
	if len(diffs) != 1 {
		t.Fatalf("want one diff event, got %d: %v", len(diffs), kinds(res.Events))
	}
	patch := diffs[0].Content
	// The headers Codex omits have to be synthesised, or every downstream
	// diff reader (and triage.ExtractDiffMeta) sees an unnamed patch.
	for _, want := range []string{"--- a//repo/a.go", "+++ b//repo/a.go", "@@ -1,2 +1,2 @@", "+new"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("patch missing %q:\n%s", want, patch)
		}
	}
	if diffs[0].ItemID != "call_P" {
		t.Fatalf("diff must hang off the tool row: %q", diffs[0].ItemID)
	}
	starts := eventsOfKind(res.Events, provider.EventToolStart)
	if metaField(t, starts[0].Meta, "toolName") != "file_change" {
		t.Fatalf("apply_patch should map to the file_change tool: %s", starts[0].Meta)
	}
}

func TestConvertReportsToolCallsThatNeverResolved(t *testing.T) {
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, execCallLine, taskCompleteLn))

	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("an unresolved call still needs a completion, got %d", len(completes))
	}
	if metaField(t, completes[0].Meta, "import_unresolved") != true {
		t.Fatalf("want the import_unresolved marker: %s", completes[0].Meta)
	}
	// It settles before the turn does, so the row belongs to that turn.
	if idxOfKind(res.Events, provider.EventToolComplete) > idxOfKind(res.Events, provider.EventTurnComplete) {
		t.Fatal("unresolved completion must be emitted before the turn closes")
	}
	var warned bool
	for _, w := range res.Warnings {
		if w.Code == WarnUnresolvedTool {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("want an unresolved-tool warning: %+v", res.Warnings)
	}
}

func idxOfKind(events []importir.Event, kind provider.EventKind) int {
	for i, e := range events {
		if e.Kind == kind {
			return i
		}
	}
	return -1
}

// ------------------------------------------------------------------- collab

// A spawn creates a child THREAD, so what lands in the parent's file is the
// child's activity and its delivered result. Both are parented under the
// spawning tool call using the wire's own linkage (`event_id` is the spawn's
// call_id; the delivery names the child by agent path).
func TestConvertParentsSubagentRecordsUnderTheSpawningCall(t *testing.T) {
	spawn := `{"timestamp":"2026-08-07T19:07:52.000Z","type":"response_item","payload":{"type":"function_call","id":"fc1","name":"spawn_agent","arguments":"{\"task\":\"review\"}","call_id":"call_S"}}`
	spawnOut := `{"timestamp":"2026-08-07T19:07:52.500Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_S","output":[{"type":"input_text","text":"spawned"}]}}`
	activity := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"call_S","agent_thread_id":"child-1","agent_path":"/root/review_perf","kind":"started"}}`
	delivery := `{"timestamp":"2026-08-07T19:07:58.000Z","type":"response_item","payload":{"type":"agent_message","author":"/root/review_perf","recipient":"/root","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nPayload:\nfound one bug"},{"type":"encrypted_content","encrypted_content":"gAAA"}]}}`

	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, taskStartedLine, spawn, spawnOut, activity, delivery, taskCompleteLn))

	notes := eventsOfKind(res.Events, provider.EventNotification)
	if len(notes) != 2 {
		t.Fatalf("want the activity row and the delivery row, got %d", len(notes))
	}
	for _, n := range notes {
		if n.ParentToolUseID != "call_S" {
			t.Fatalf("notification %q not parented under the spawn: %q", n.Content, n.ParentToolUseID)
		}
	}
	if !strings.Contains(notes[0].Content, "/root/review_perf") {
		t.Fatalf("activity summary = %q", notes[0].Content)
	}
	if !strings.Contains(notes[1].Content, "found one bug") {
		t.Fatalf("delivery content = %q", notes[1].Content)
	}
	// The encrypted half of the delivery contributes nothing.
	if strings.Contains(notes[1].Content, "gAAA") {
		t.Fatalf("encrypted payload leaked into the row: %q", notes[1].Content)
	}
}

func TestConvertSubagentInteractionIsStandaloneToolActivity(t *testing.T) {
	followup := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"response_item","payload":{"type":"function_call","name":"followup_task","arguments":"{\"target\":\"/root/review_perf\",\"message\":\"encrypted\"}","call_id":"call_F"}}`
	activity := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"call_F","agent_thread_id":"child-1","agent_path":"/root/review_perf","kind":"interacted"}}`

	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, taskStartedLine, followup, activity, taskCompleteLn))
	starts := eventsOfKind(res.Events, provider.EventToolStart)
	if len(starts) != 1 || starts[0].ItemType != "send_input" {
		t.Fatalf("tool starts = %+v, want one normalized interaction", starts)
	}
	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("tool completions = %+v, want one interaction", completes)
	}
	event := completes[0]
	if event.ItemType != "send_input" || event.ParentToolUseID != "" {
		t.Fatalf("interaction event = %+v", event)
	}
	var meta struct {
		Input struct {
			Tool         string `json:"tool"`
			ActivityKind string `json:"activityKind"`
			ActivityTool string `json:"activityTool"`
			Target       string `json:"target"`
		} `json:"input"`
	}
	if err := json.Unmarshal(event.Meta, &meta); err != nil {
		t.Fatalf("decode interaction meta: %v", err)
	}
	if meta.Input.Tool != "send_input" || meta.Input.ActivityKind != "interacted" ||
		meta.Input.ActivityTool != "followup_task" || meta.Input.Target != "/root/review_perf" {
		t.Fatalf("interaction meta = %+v", meta.Input)
	}
	for _, note := range eventsOfKind(res.Events, provider.EventNotification) {
		if strings.Contains(note.Content, "received a message") {
			t.Fatalf("interaction also emitted a nested notification: %+v", note)
		}
	}
}

func TestConvertSubagentProgressIsStandaloneToolActivity(t *testing.T) {
	activity := `{"timestamp":"2026-08-07T19:07:52.000Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"call_S","agent_thread_id":"child-1","agent_path":"/root/review_perf","kind":"started"}}`
	delivery := `{"timestamp":"2026-08-07T19:07:58.000Z","type":"response_item","payload":{"type":"agent_message","author":"/root/review_perf","recipient":"/root","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/review_perf\nPayload:\nrunning focused tests"}]}}`

	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, taskStartedLine, activity, delivery, taskCompleteLn))
	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("tool completions = %+v, want one progress activity", completes)
	}
	event := completes[0]
	if event.ItemType != "send_input" || event.ParentToolUseID != "" {
		t.Fatalf("progress event = %+v", event)
	}
	var meta struct {
		Input struct {
			ActivityKind string `json:"activityKind"`
			Message      string `json:"message"`
			Target       string `json:"target"`
		} `json:"input"`
	}
	if err := json.Unmarshal(event.Meta, &meta); err != nil {
		t.Fatalf("decode progress meta: %v", err)
	}
	if meta.Input.ActivityKind != "progress" || meta.Input.Message != "running focused tests" ||
		meta.Input.Target != "/root/review_perf" {
		t.Fatalf("progress meta = %+v", meta.Input)
	}
	for _, note := range eventsOfKind(res.Events, provider.EventNotification) {
		if strings.Contains(note.Content, "Message Type: MESSAGE") {
			t.Fatalf("progress also emitted a nested notification: %+v", note)
		}
	}
}

func TestConvertRejectsForeignOrMalformedProgressEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name    string
		payload string
	}{
		{
			name:    "wrong recipient",
			payload: `{"type":"agent_message","author":"/root/review_perf","recipient":"/root/other","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root/other\nSender: /root/review_perf\nPayload:\nnot root progress"}]}`,
		},
		{
			name:    "sender mismatch",
			payload: `{"type":"agent_message","author":"/root/review_perf","recipient":"/root","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/forged\nPayload:\nforged"}]}`,
		},
		{
			name:    "trigger turn",
			payload: `{"type":"agent_message","author":"/root/review_perf","recipient":"/root","trigger_turn":true,"content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/review_perf\nPayload:\nnot a mailbox drain"}]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			delivery := `{"timestamp":"2026-08-07T19:07:58.000Z","type":"response_item","payload":` + tt.payload + `}`
			res := parseFixture(t, writeRollout(t, testSessionID,
				metaLine, taskStartedLine, delivery, taskCompleteLn))
			for _, event := range eventsOfKind(res.Events, provider.EventToolComplete) {
				if event.ItemType == "send_input" {
					t.Fatalf("foreign envelope became progress: %+v", event)
				}
			}
		})
	}
}

func TestConvertSubagentInteractionCannotRelabelCollidingTool(t *testing.T) {
	execCall := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}","call_id":"call_X"}}`
	activity := `{"timestamp":"2026-08-07T19:07:54.000Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":"call_X","agent_thread_id":"child-1","agent_path":"/root/review_perf","kind":"interacted"}}`

	for _, tt := range []struct {
		name  string
		lines []string
	}{
		{name: "raw call first", lines: []string{execCall, activity}},
		{name: "typed activity first", lines: []string{activity, execCall}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := append([]string{metaLine, taskStartedLine}, tt.lines...)
			lines = append(lines, taskCompleteLn)
			res := parseFixture(t, writeRollout(t, testSessionID, lines...))
			if res.CorruptLines == 0 {
				t.Fatal("forged activity/tool id collision was not reported as corrupt")
			}
			var bashStarts, collabStarts int
			for _, event := range eventsOfKind(res.Events, provider.EventToolStart) {
				switch event.ItemType {
				case "commandExecution":
					bashStarts++
				case "send_input":
					collabStarts++
				}
			}
			if bashStarts != 1 || collabStarts != 1 {
				t.Fatalf("tool starts = %+v, want preserved Bash plus standalone interaction", eventsOfKind(res.Events, provider.EventToolStart))
			}
		})
	}
}

// The 0.146 spelling carries only `trigger_turn` and no content; it must be
// recognised and dropped, not counted as an unknown type.
func TestConvertIgnoresContentlessInterAgentMetadata(t *testing.T) {
	line := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"inter_agent_communication_metadata","payload":{"trigger_turn":true}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, line, taskCompleteLn))
	if len(res.UnknownTypes) != 0 {
		t.Fatalf("should be recognised, not unknown: %+v", res.UnknownTypes)
	}
	if countKind(res.Events, provider.EventNotification) != 0 {
		t.Fatal("a contentless metadata record must not create a row")
	}
}

// ------------------------------------------------------- collab v1 end events

// The four MultiAgentV1 collab end records are absent from current rollouts
// (Codex stopped persisting them) but present in older ones, and the waiting /
// close variants carry the CHILD'S ANSWER — the whole reason to read them.
func TestConvertFoldsCollabWaitingEndOntoTheWaitCall(t *testing.T) {
	waitCall := `{"timestamp":"2026-05-03T00:29:00.000Z","type":"response_item","payload":{"type":"function_call","id":"fc1","name":"wait","arguments":"{}","call_id":"call_W"}}`
	waitEnd := `{"timestamp":"2026-05-03T00:29:39.769Z","type":"event_msg","payload":{"type":"collab_waiting_end","call_id":"call_W","agent_statuses":[{"thread_id":"child-1","agent_nickname":"Gauss","agent_role":"explorer","status":{"completed":"Found the leak in cache.go"}}]}}`
	waitOut := `{"timestamp":"2026-05-03T00:29:40.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_W","output":[]}}`

	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, waitCall, waitEnd, waitOut, taskCompleteLn))

	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(completes))
	}
	if !strings.Contains(completes[0].Content, "Found the leak in cache.go") {
		t.Fatalf("child answer lost: %q", completes[0].Content)
	}
	if !strings.Contains(completes[0].Content, "Gauss") {
		t.Fatalf("child identity lost: %q", completes[0].Content)
	}
	if len(res.UnknownTypes) != 0 {
		t.Fatalf("collab end records must be recognised: %+v", res.UnknownTypes)
	}
}

func TestConvertCollabEndMarksAnErroredChild(t *testing.T) {
	call := `{"timestamp":"2026-05-03T00:29:00.000Z","type":"response_item","payload":{"type":"function_call","id":"fc1","name":"close_agent","arguments":"{}","call_id":"call_C"}}`
	end := `{"timestamp":"2026-05-03T00:29:39.769Z","type":"event_msg","payload":{"type":"collab_close_end","call_id":"call_C","receiver_agent_nickname":"Parfit","status":{"errored":"the child crashed"}}}`
	out := `{"timestamp":"2026-05-03T00:29:40.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_C","output":[]}}`

	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, call, end, out, taskCompleteLn))
	complete := firstOfKind(t, res.Events, provider.EventToolComplete)
	if metaField(t, complete.Meta, "is_error") != true {
		t.Fatalf("errored child should mark the row: %s", complete.Meta)
	}
	if metaField(t, complete.Meta, "item_status") != "failed" {
		t.Fatalf("item_status = %v", metaField(t, complete.Meta, "item_status"))
	}
}

// -------------------------------------------------------------- web search

// `web_search_call` has NO `*_output` response item at all; it carries its own
// terminal status. Treating it like an unfinished tool would mark nearly every
// search in the corpus as unresolved.
func TestConvertWebSearchCallSettlesOnItsOwnStatus(t *testing.T) {
	search := `{"timestamp":"2026-07-06T23:24:58.000Z","type":"response_item","payload":{"type":"web_search_call","id":"ws_1","status":"completed"}}`
	end := `{"timestamp":"2026-07-06T23:24:58.085Z","type":"event_msg","payload":{"type":"web_search_end","call_id":"ws_1","query":"go sqlite immutable","action":{"type":"search"}}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, search, end, taskCompleteLn))

	completes := eventsOfKind(res.Events, provider.EventToolComplete)
	if len(completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(completes))
	}
	if metaField(t, completes[0].Meta, "import_unresolved") != nil {
		t.Fatalf("a completed search is not unresolved: %s", completes[0].Meta)
	}
	if metaField(t, completes[0].Meta, "item_status") != "completed" {
		t.Fatalf("item_status = %v, want the wire status", metaField(t, completes[0].Meta, "item_status"))
	}
	if metaField(t, completes[0].Meta, "query") != "go sqlite immutable" {
		t.Fatalf("query not folded in: %s", completes[0].Meta)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnUnresolvedTool {
			t.Fatalf("no unresolved warning expected: %+v", res.Warnings)
		}
	}
}

// `FileChange::Add` / `::Delete` carry no `unified_diff` at all — the whole
// file arrives as `content` — so the whole-file rendering is the only thing
// that keeps a created or deleted file in the patch. Only the `delete` and
// unknown-type branches were untested.
func TestAssembleUnifiedPatchRendersWholeFileAddsAndDeletes(t *testing.T) {
	patch := assembleUnifiedPatch(map[string]json.RawMessage{
		"/repo/new.go":  json.RawMessage(`{"type":"add","content":"one\ntwo\n"}`),
		"/repo/gone.go": json.RawMessage(`{"type":"delete","content":"old\n"}`),
	})
	for _, want := range []string{
		"new file\n--- a//repo/new.go\n+++ b//repo/new.go\n@@ -0,0 +1,2 @@\n+one\n+two",
		"deleted file\n--- a//repo/gone.go\n+++ b//repo/gone.go\n@@ -1,1 +0,0 @@\n-old",
	} {
		if !strings.Contains(patch, want) {
			t.Fatalf("patch missing %q:\n%s", want, patch)
		}
	}
}

// A change type this build does not know how to render whole-file is the ONLY
// thing that drops an entry. Rendering it as an add or a delete would state
// the opposite of what the record says.
func TestAssembleUnifiedPatchSkipsAnUnknownHunklessChangeType(t *testing.T) {
	patch := assembleUnifiedPatch(map[string]json.RawMessage{
		"/repo/a.go": json.RawMessage(`{"type":"chmod","content":"whatever"}`),
		"/repo/b.go": json.RawMessage(`{"type":"add","content":"kept\n"}`),
	})
	if strings.Contains(patch, "/repo/a.go") {
		t.Fatalf("an unrenderable change type must be skipped:\n%s", patch)
	}
	if !strings.Contains(patch, "+kept") {
		t.Fatalf("its neighbour must still render:\n%s", patch)
	}
}

// `touch newfile` is a real edit with an empty `content`. It used to vanish
// from the patch entirely — headers included — because an empty body was read
// as "nothing to show" rather than "the file is empty".
func TestAssembleUnifiedPatchKeepsAnEmptyAddedFile(t *testing.T) {
	patch := assembleUnifiedPatch(map[string]json.RawMessage{
		"/repo/empty.txt": json.RawMessage(`{"type":"add","content":""}`),
	})
	want := "diff --git a//repo/empty.txt b//repo/empty.txt\nnew file\n--- a//repo/empty.txt\n+++ b//repo/empty.txt\n@@ -0,0 +0,0 @@\n"
	if patch != want {
		t.Fatalf("empty added file patch =\n%q\nwant\n%q", patch, want)
	}
}

// The same, end to end: the diff event must exist at all.
func TestConvertKeepsADiffForAnEmptyCreatedFile(t *testing.T) {
	end := `{"timestamp":"2026-08-07T19:07:53.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"exec-empty","turn_id":"turn-1","stdout":"Success","success":true,"changes":{"/repo/empty.txt":{"type":"add","content":""}}}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, end, taskCompleteLn))

	diffs := eventsOfKind(res.Events, provider.EventDiff)
	if len(diffs) != 1 {
		t.Fatalf("want one diff event, got %d: %v", len(diffs), kinds(res.Events))
	}
	if !strings.Contains(diffs[0].Content, "--- a//repo/empty.txt") {
		t.Fatalf("empty added file lost its headers: %q", diffs[0].Content)
	}
}
