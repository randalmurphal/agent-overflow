package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

const (
	fixtureNDJSONBash             = "../../../docs/references/fixtures/claude/ndjson_bash.log"
	fixtureNDJSONTask             = "../../../docs/references/fixtures/claude/ndjson_task.log"
	fixtureNDJSONOutlives         = "../../../docs/references/fixtures/claude/ndjson_outlives.log"
	fixtureNDJSONOutlivesTurn2    = "../../../docs/references/fixtures/claude/ndjson_outlives_turn2.log"
	fixtureTaskOutputMulti        = "../../../docs/references/fixtures/claude/taskoutput_multi.ndjson"
	fixtureLocalAgentOutlives     = "../../../docs/references/fixtures/claude/local_agent_outlives.ndjson"
	fixtureLocalAgentUserInputMid = "../../../docs/references/fixtures/claude/local_agent_user_input_during_wait.ndjson"
	fixtureLocalAgentPlusBgBash   = "../../../docs/references/fixtures/claude/local_agent_plus_bg_bash.ndjson"
)

// These replay tests load captured Claude CLI NDJSON from the repo's
// checked-in fixtures and feed each line through (*Parser).ParseLine,
// then assert the resulting event sequence matches the new
// turn/tool/task-lifecycle contract. Refresh fixtures from fresh
// `AGENT_OVERFLOW_DEBUG=provider` captures when the upstream wire
// changes, then update docs/references/claude-wire.md in the same
// commit.

// loadNDJSONFixture reads a captured NDJSON log and returns its lines
// with blank entries dropped. These fixtures are checked into the repo,
// so a missing file is a real test failure.
func loadNDJSONFixture(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out [][]byte
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out
}

// replayFixture parses every line in a fixture through a single
// (*Parser).ParseLine call chain and concatenates the emitted events.
// Parse errors fail the test — our parser is specified to never error
// on well-formed JSON, and malformed lines in a captured log would be
// a sample problem worth surfacing.
func replayFixture(t *testing.T, path string) []provider.ProviderEvent {
	t.Helper()
	lines := loadNDJSONFixture(t, path)
	parser := NewParser()
	var events []provider.ProviderEvent
	for i, line := range lines {
		got, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("line %d (%.80s): parse error: %v", i+1, line, err)
		}
		events = append(events, got...)
	}
	return events
}

// filterKinds drops events whose Kind is not in the target set. Used to
// strip the streaming / usage / rate-limit noise out before asserting
// lifecycle sequences.
func filterKinds(events []provider.ProviderEvent, kinds ...provider.EventKind) []provider.ProviderEvent {
	want := make(map[provider.EventKind]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	out := make([]provider.ProviderEvent, 0, len(events))
	for _, evt := range events {
		if _, ok := want[evt.Kind]; ok {
			out = append(out, evt)
		}
	}
	return out
}

// TestReplay_NDJsonBash validates the backgrounded-Bash + foreground
// Bash + Read + result scenario in
// docs/references/fixtures/claude/ndjson_bash.log.
//
// Expected lifecycle sequence (ignoring text/thinking/usage deltas):
//
//  1. EventInit
//  2. EventToolStart  (backgrounded bash: toolu_015s9XtK1RXLBS1AtHF79Dyy)
//  3. EventToolStart  (task_started for the bg bash, meta-only)
//  4. EventToolComplete (background placeholder; is_background=true)
//  5. EventToolStart  (foreground bash: toolu_0153SNjjDKJC6Y2u6BVP8tyE)
//  6. EventToolStart  (task_started for fg bash, meta-only)
//  7. EventBackgroundTaskTerminal (bg bash task_updated)
//     plus a foreground task_notification row
//  8. EventToolComplete (foreground bash result)
//  9. EventToolStart  (Read)
//  10. EventToolComplete (Read result)
//  11. EventTurnComplete (with lastAssistantMessageID populated)
func TestReplay_NDJsonBash(t *testing.T) {
	events := replayFixture(t, fixtureNDJSONBash)

	lifecycle := filterKinds(events,
		provider.EventInit,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventTurnStart,
		provider.EventTurnComplete,
	)

	const (
		bgBash   = "toolu_015s9XtK1RXLBS1AtHF79Dyy"
		fgBash   = "toolu_0153SNjjDKJC6Y2u6BVP8tyE"
		readTool = "toolu_01LCMPzpxsdsVULRDTztqNn4"
		bgTaskID = "bslbv9989"
		fgTaskID = "bwjfjlgn0"
	)

	// Expected kinds in order (ignoring ItemID duplicates from
	// task_started meta-only events). We assert kind+ItemID.
	type step struct {
		kind provider.EventKind
		id   string
	}
	want := []step{
		{provider.EventInit, ""},

		// backgrounded bash tool_use
		{provider.EventToolStart, bgBash},
		{provider.EventToolStart, bgBash}, // meta-only from task_started
		{provider.EventToolComplete, bgBash},

		// foreground bash tool_use
		{provider.EventToolStart, fgBash},
		{provider.EventToolStart, fgBash}, // meta-only from task_started

		// task_updated for bg bash (terminal)
		{provider.EventBackgroundTaskTerminal, bgBash},

		// foreground/background task_notification events are asserted separately

		// foreground bash result
		{provider.EventToolComplete, fgBash},

		// Read tool
		{provider.EventToolStart, readTool},
		{provider.EventToolComplete, readTool},

		// Turn complete
		{provider.EventTurnComplete, ""},
	}
	if len(lifecycle) != len(want) {
		t.Fatalf("lifecycle event count: got %d, want %d\ngot:\n%s", len(lifecycle), len(want), dumpLifecycle(lifecycle))
	}
	for i, step := range want {
		got := lifecycle[i]
		if got.Kind != step.kind {
			t.Fatalf("[%d] Kind: got %q, want %q", i, got.Kind, step.kind)
		}
		if step.id != "" && got.ItemID != step.id {
			t.Fatalf("[%d] ItemID: got %q, want %q (kind=%q)", i, got.ItemID, step.id, got.Kind)
		}
	}

	// Assert background-aware fields.
	for _, evt := range lifecycle {
		if evt.Kind != provider.EventToolComplete || evt.ItemID != bgBash {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("bg bash complete: unmarshal meta: %v", err)
		}
		if meta["is_background"] != true {
			t.Fatalf("bg bash complete: expected is_background=true, got %v", meta["is_background"])
		}
	}
	for _, evt := range lifecycle {
		if evt.Kind != provider.EventBackgroundTaskTerminal {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			t.Fatalf("bg task terminal: unmarshal meta: %v", err)
		}
		if meta["task_id"] != bgTaskID {
			t.Fatalf("bg task terminal: task_id got %v, want %s", meta["task_id"], bgTaskID)
		}
		if meta["status"] != "completed" {
			t.Fatalf("bg task terminal: status got %v, want completed", meta["status"])
		}
	}

	// Foreground task_notification must not masquerade as a lifecycle terminal.
	for _, evt := range lifecycle {
		if evt.Kind != provider.EventBackgroundTaskTerminal {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal(evt.Meta, &meta)
		if meta["task_id"] == fgTaskID {
			t.Fatalf("unexpected EventBackgroundTaskTerminal for foreground task (invariant 21 violated): %+v", meta)
		}
	}

	notifications := filterKinds(events, provider.EventBackgroundTaskNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 EventBackgroundTaskNotification (foreground bash), got %d", len(notifications))
	}
	var notificationMeta map[string]any
	if err := json.Unmarshal(notifications[0].Meta, &notificationMeta); err != nil {
		t.Fatalf("notification unmarshal: %v", err)
	}
	if notificationMeta["task_id"] != fgTaskID {
		t.Fatalf("notification task_id = %v, want %s", notificationMeta["task_id"], fgTaskID)
	}
	if notificationMeta["tool_use_id"] != fgBash {
		t.Fatalf("notification tool_use_id = %v, want %s", notificationMeta["tool_use_id"], fgBash)
	}

	// EventTurnComplete must carry the last assistant.message.id.
	var amid string
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			amid = requireWireTurnComplete(t, []provider.ProviderEvent{evt}).AssistantMessageID
		}
	}
	if amid == "" {
		t.Fatalf("no EventTurnComplete observed")
	}
	if !strings.HasPrefix(amid, "msg_") {
		t.Fatalf("assistant_message_id should look like an Anthropic msg id, got %q", amid)
	}
}

// TestReplay_NDJsonTask validates the Task subagent + TaskOutput scenario
// in docs/references/fixtures/claude/ndjson_task.log.
//
// Expected lifecycle:
//
//  1. EventInit
//  2. EventToolStart  (ToolSearch)
//  3. EventToolComplete (ToolSearch)
//  4. EventToolStart  (Agent subagent: toolu_01LDZokHwdArDz2tBo3ShApT, with run_in_background)
//  5. EventToolStart  (task_started meta-only)
//  6. EventToolComplete (Agent placeholder; is_background=true)
//  7. EventBackgroundTaskTerminal (Agent task_updated)
//  8. EventToolStart  (TaskOutput: toolu_012npoxf1xW6aEKuqkTidY4y)
//  9. EventToolComplete (TaskOutput own id)
//  10. EventBackgroundTaskTerminal (enrichment for the Agent task)
//  11. EventTurnComplete
func TestReplay_NDJsonTask(t *testing.T) {
	events := replayFixture(t, fixtureNDJSONTask)

	lifecycle := filterKinds(events,
		provider.EventInit,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventTurnComplete,
	)

	const (
		toolSearch  = "toolu_019NsGTXaBvb6XWZeB1oJezo"
		agent       = "toolu_01LDZokHwdArDz2tBo3ShApT"
		taskOutput  = "toolu_012npoxf1xW6aEKuqkTidY4y"
		agentTaskID = "a83b47c3c0d9a2254"
	)

	type step struct {
		kind provider.EventKind
		id   string
	}
	want := []step{
		{provider.EventInit, ""},
		{provider.EventToolStart, toolSearch},
		{provider.EventToolComplete, toolSearch},
		{provider.EventToolStart, agent},
		{provider.EventToolStart, agent}, // meta-only from task_started
		{provider.EventToolComplete, agent},
		{provider.EventBackgroundTaskTerminal, agent},
		{provider.EventToolStart, taskOutput},
		{provider.EventToolComplete, taskOutput},
		{provider.EventBackgroundTaskTerminal, agent},
		{provider.EventTurnComplete, ""},
	}
	if len(lifecycle) != len(want) {
		t.Fatalf("lifecycle count: got %d, want %d\n%s", len(lifecycle), len(want), dumpLifecycle(lifecycle))
	}
	for i, s := range want {
		got := lifecycle[i]
		if got.Kind != s.kind {
			t.Fatalf("[%d] Kind: got %q, want %q", i, got.Kind, s.kind)
		}
		if s.id != "" && got.ItemID != s.id {
			t.Fatalf("[%d] ItemID: got %q, want %q (kind=%q)", i, got.ItemID, s.id, got.Kind)
		}
	}

	// The enrichment terminal (emitted after TaskOutput's own complete)
	// must carry task_id + task_type awareness plus the content
	// enrichment from the `task` subobject.
	var enrichment map[string]any
	for i := len(lifecycle) - 1; i >= 0; i-- {
		if lifecycle[i].Kind == provider.EventBackgroundTaskTerminal && lifecycle[i].ItemID == agent {
			if err := json.Unmarshal(lifecycle[i].Meta, &enrichment); err != nil {
				t.Fatalf("enrichment unmarshal: %v", err)
			}
			break
		}
	}
	if enrichment == nil {
		t.Fatalf("enrichment EventBackgroundTaskTerminal not found")
	}
	if enrichment["task_id"] != agentTaskID {
		t.Fatalf("enrichment task_id: got %v, want %s", enrichment["task_id"], agentTaskID)
	}
	if enrichment["status"] != "completed" {
		t.Fatalf("enrichment status: got %v, want completed", enrichment["status"])
	}
}

// TestReplay_NDJsonOutlives validates the outlives-turn scenario in
// docs/references/fixtures/claude/ndjson_outlives.log: a backgrounded bash whose
// `task_updated` / `task_notification` arrive AFTER the turn's
// `result`. This is the canonical "turn closes while background work
// is still in flight" shape (invariant 24).
//
// Expected lifecycle:
//
//  1. EventInit
//  2. EventToolStart  (backgrounded bash)
//  3. EventToolStart  (task_started meta-only)
//  4. EventToolComplete (bg placeholder, is_background=true)
//  5. EventTurnComplete  <-- turn closes
//  6. EventBackgroundTaskTerminal (late task_updated)
//     plus a separate notification event/row
//  7. EventInit (second turn starts; sample has a turn 2)
//  8. EventTurnComplete (end of second turn)
func TestReplay_NDJsonOutlives(t *testing.T) {
	events := replayFixture(t, fixtureNDJSONOutlives)

	lifecycle := filterKinds(events,
		provider.EventInit,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventTurnComplete,
	)

	const bgBash = "toolu_01NoZSorBGb7jSQMhNrs6qZj"

	// Locate the EventTurnComplete and EventBackgroundTaskTerminal
	// relative positions.
	turnCompleteIdx := -1
	terminalIdx := -1
	for i, evt := range lifecycle {
		if evt.Kind == provider.EventTurnComplete && turnCompleteIdx < 0 {
			turnCompleteIdx = i
		}
		if evt.Kind == provider.EventBackgroundTaskTerminal && evt.ItemID == bgBash && terminalIdx < 0 {
			terminalIdx = i
		}
	}
	if turnCompleteIdx < 0 {
		t.Fatalf("no EventTurnComplete in lifecycle:\n%s", dumpLifecycle(lifecycle))
	}
	if terminalIdx < 0 {
		t.Fatalf("no EventBackgroundTaskTerminal for bg bash in lifecycle:\n%s", dumpLifecycle(lifecycle))
	}
	if terminalIdx <= turnCompleteIdx {
		t.Fatalf("bg task terminal must arrive AFTER first turn_complete: terminalIdx=%d turnCompleteIdx=%d\n%s",
			terminalIdx, turnCompleteIdx, dumpLifecycle(lifecycle))
	}

	// Confirm the sequence: the bg bash's completion fires in the
	// first turn but is_background=true. Scan for the bg placeholder
	// complete event before the first turn-complete.
	var placeholder *provider.ProviderEvent
	for i := 0; i < turnCompleteIdx; i++ {
		if lifecycle[i].Kind == provider.EventToolComplete && lifecycle[i].ItemID == bgBash {
			copy := lifecycle[i]
			placeholder = &copy
			break
		}
	}
	if placeholder == nil {
		t.Fatalf("bg bash placeholder EventToolComplete missing before turn complete\n%s", dumpLifecycle(lifecycle))
	}
	var meta map[string]any
	_ = json.Unmarshal(placeholder.Meta, &meta)
	if meta["is_background"] != true {
		t.Fatalf("bg bash placeholder: expected is_background=true, got %v", meta["is_background"])
	}

	// No double-emission: task_notification for the same task must
	// not have produced a second EventBackgroundTaskTerminal for the
	// same tool_use_id.
	terminalCount := 0
	for _, evt := range lifecycle {
		if evt.Kind == provider.EventBackgroundTaskTerminal && evt.ItemID == bgBash {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("expected exactly 1 EventBackgroundTaskTerminal for bg bash, got %d", terminalCount)
	}
}

// TestReplay_NDJsonTaskOutputMulti validates the scenario in
// docs/references/fixtures/claude/taskoutput_multi.ndjson: two parallel
// `run_in_background:true` Bashes, then a blocking `TaskOutput` on the
// longer one.
//
// Expected lifecycle highlights:
//   - two EventToolStart + two task_started EventToolStart (meta-only)
//     for the two bg bashes
//   - each bg bash's placeholder EventToolComplete (is_background=true)
//   - two EventBackgroundTaskTerminal from task_updated (one per bg task)
//   - TaskOutput start/own-complete + EventBackgroundTaskTerminal
//     enrichment for the task it polled
//   - EventTurnComplete
func TestReplay_NDJsonTaskOutputMulti(t *testing.T) {
	events := replayFixture(t, fixtureTaskOutputMulti)

	lifecycle := filterKinds(events,
		provider.EventInit,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventTurnComplete,
	)

	const (
		toolSearch = "toolu_01FcJ6kPc1D3LRr4oDy36Cs4"
		shortBash  = "toolu_01RLgjSYxuLDPsNky8sduPMz"
		longBash   = "toolu_01MHQNDs87tCQ6CwYwTxPUop"
		taskOutput = "toolu_01M1ggaDigc5hFfgjLS1xzZC"
		shortTask  = "b6ip2hjfl"
		longTask   = "bqv2stloe"
	)

	// Collect terminal events keyed by ItemID to cross-check.
	type terminal struct {
		itemID string
		meta   map[string]any
	}
	var terminals []terminal
	for _, evt := range lifecycle {
		if evt.Kind != provider.EventBackgroundTaskTerminal {
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal(evt.Meta, &meta)
		terminals = append(terminals, terminal{itemID: evt.ItemID, meta: meta})
	}
	// Expect exactly 3 terminals: one per bg bash's task_updated, plus
	// one TaskOutput enrichment.
	if len(terminals) != 3 {
		t.Fatalf("expected 3 EventBackgroundTaskTerminal, got %d\n%s", len(terminals), dumpLifecycle(lifecycle))
	}
	// First two: task_updated terminals for each bg bash.
	if terminals[0].meta["task_id"] != shortTask {
		t.Fatalf("terminal[0] task_id: got %v, want %s", terminals[0].meta["task_id"], shortTask)
	}
	if terminals[0].itemID != shortBash {
		t.Fatalf("terminal[0] ItemID: got %q, want %s", terminals[0].itemID, shortBash)
	}
	if terminals[1].meta["task_id"] != longTask {
		t.Fatalf("terminal[1] task_id: got %v, want %s", terminals[1].meta["task_id"], longTask)
	}
	if terminals[1].itemID != longBash {
		t.Fatalf("terminal[1] ItemID: got %q, want %s", terminals[1].itemID, longBash)
	}
	// Third: TaskOutput enrichment for the long bash task. The
	// terminal's ItemID is the original bg bash tool_use_id.
	if terminals[2].meta["task_id"] != longTask {
		t.Fatalf("terminal[2] task_id: got %v, want %s (TaskOutput enrichment)", terminals[2].meta["task_id"], longTask)
	}
	if terminals[2].itemID != longBash {
		t.Fatalf("terminal[2] ItemID: got %q, want %s", terminals[2].itemID, longBash)
	}

	// TaskOutput must have emitted its own EventToolComplete in addition
	// to the enrichment terminal (invariant 20).
	sawTaskOutputComplete := false
	for _, evt := range lifecycle {
		if evt.Kind == provider.EventToolComplete && evt.ItemID == taskOutput {
			sawTaskOutputComplete = true
			break
		}
	}
	if !sawTaskOutputComplete {
		t.Fatalf("missing EventToolComplete for TaskOutput's own id (%s) — invariant 20", taskOutput)
	}

	// Ensure ToolSearch emitted normally too.
	sawToolSearchComplete := false
	for _, evt := range lifecycle {
		if evt.Kind == provider.EventToolComplete && evt.ItemID == toolSearch {
			sawToolSearchComplete = true
			break
		}
	}
	if !sawToolSearchComplete {
		t.Fatalf("missing EventToolComplete for ToolSearch id %s", toolSearch)
	}
}

// TestReplay_NDJsonOutlivesTurn2 extends the outlives scenario by
// continuing the same parser session into a fresh turn 2 captured
// separately in docs/references/fixtures/claude/ndjson_outlives_turn2.log.
//
// The captured session_id matches ndjson_outlives.log, which means the
// same parser instance would observe both streams in production: turn
// 1 fires the backgrounded Bash, turn 1's `result` closes the turn,
// then the backgrounded bash's `task_updated` arrives — followed by a
// fresh user message that starts a new turn 2 with its own init +
// `result`. The spec invariant (invariant 24) says the bg task's
// terminal must continue to key off the turn-1 tool_use_id even though
// turn 2 is now active, and turn 2's own `result` envelope must emit
// exactly one EventTurnComplete without retroactively touching the
// turn-1 bg launch.
//
// Fixtures:
//   - docs/references/fixtures/claude/ndjson_outlives.log (turn 1 + task_updated
//     after result; also contains the first re-init for turn 2 and
//     its "Background task finished" result).
//   - docs/references/fixtures/claude/ndjson_outlives_turn2.log (a second fresh
//     turn 2 capture for the same session — simulates the user typing
//     "hi" and the agent responding while the parser has already
//     forgotten the bg task in its task map).
//
// We replay both through ONE Parser so the cross-line state behaves
// exactly as it would at runtime. The assertions then pin:
//   - Exactly one EventBackgroundTaskTerminal for the bg Bash, keyed
//     by the turn-1 tool_use_id (never re-attributed to turn 2).
//   - Turn 2's result (the final `result` line in the second fixture)
//     emits a single EventTurnComplete and does NOT produce any
//     additional tool-lifecycle events for the turn-1 bg launch.
func TestReplay_NDJsonOutlivesTurn2(t *testing.T) {
	const (
		outlivesPath = fixtureNDJSONOutlives
		turn2Path    = fixtureNDJSONOutlivesTurn2
		bgBash       = "toolu_01NoZSorBGb7jSQMhNrs6qZj"
		bgTaskID     = "bwh4ptwpo"
	)

	// Drive BOTH fixtures through the same Parser instance so the
	// task_id ↔ tool_use_id correlation map carries across the session
	// boundary just like the real CLI read loop would.
	parser := NewParser()
	var events []provider.ProviderEvent
	for _, path := range []string{outlivesPath, turn2Path} {
		lines := loadNDJSONFixture(t, path)
		for i, line := range lines {
			got, err := parser.ParseLine(testThread, line)
			if err != nil {
				t.Fatalf("%s line %d (%.80s): parse error: %v", path, i+1, line, err)
			}
			events = append(events, got...)
		}
	}

	lifecycle := filterKinds(events,
		provider.EventInit,
		provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventBackgroundTaskTerminal,
		provider.EventTurnComplete,
	)

	// Count turn completes across both fixtures. ndjson_outlives.log
	// already contains two `result` lines (turn 1 + the re-init turn
	// after the bg task notification); ndjson_outlives_turn2.log adds a
	// third. So we expect exactly three EventTurnComplete emissions —
	// anything else means the parser started spuriously force-closing
	// turn 1 on top of turn 2's real completion.
	var turnCompletes []provider.ProviderEvent
	for _, evt := range lifecycle {
		if evt.Kind == provider.EventTurnComplete {
			turnCompletes = append(turnCompletes, evt)
		}
	}
	if len(turnCompletes) != 3 {
		t.Fatalf("expected 3 EventTurnComplete (outlives turn1 + outlives turn2 + standalone turn2), got %d\n%s",
			len(turnCompletes), dumpLifecycle(lifecycle))
	}

	// Exactly one EventBackgroundTaskTerminal for the bg Bash — the
	// task_updated terminal in outlives.log. task_notification must NOT
	// have produced a second terminal (invariant 21).
	bgTerminals := 0
	var bgTerminal provider.ProviderEvent
	for _, evt := range lifecycle {
		if evt.Kind != provider.EventBackgroundTaskTerminal {
			continue
		}
		if evt.ItemID != bgBash {
			t.Fatalf("EventBackgroundTaskTerminal with wrong item id: got %q, want %q", evt.ItemID, bgBash)
		}
		bgTerminal = evt
		bgTerminals++
	}
	if bgTerminals != 1 {
		t.Fatalf("expected exactly 1 EventBackgroundTaskTerminal for bg bash (invariant 24), got %d\n%s",
			bgTerminals, dumpLifecycle(lifecycle))
	}

	// The terminal's Meta must carry the bg task_id (confirming the
	// correlation map kept the turn-1 ↔ tool_use_id pairing rather than
	// re-attributing to a turn-2 tool_use).
	var terminalMeta map[string]any
	if err := json.Unmarshal(bgTerminal.Meta, &terminalMeta); err != nil {
		t.Fatalf("bg terminal meta unmarshal: %v", err)
	}
	if terminalMeta["task_id"] != bgTaskID {
		t.Fatalf("bg terminal task_id = %v, want %s (re-attributed to turn 2?)", terminalMeta["task_id"], bgTaskID)
	}

	// The terminal must land BEFORE the final turn 2 result — it's the
	// authoritative "bg work finished after turn-1 result" signal. The
	// first turn_complete in lifecycle is turn 1's; the terminal
	// follows it; the remaining turn_completes are turn 2 + standalone
	// turn 2 and must come later than the terminal.
	firstTurnComplete := -1
	terminalIdx := -1
	lastTurnComplete := -1
	for i, evt := range lifecycle {
		if evt.Kind == provider.EventTurnComplete {
			if firstTurnComplete < 0 {
				firstTurnComplete = i
			}
			lastTurnComplete = i
		}
		if evt.Kind == provider.EventBackgroundTaskTerminal && evt.ItemID == bgBash && terminalIdx < 0 {
			terminalIdx = i
		}
	}
	if firstTurnComplete < 0 || terminalIdx < 0 || lastTurnComplete < 0 {
		t.Fatalf("missing anchor events in lifecycle:\n%s", dumpLifecycle(lifecycle))
	}
	if terminalIdx <= firstTurnComplete {
		t.Fatalf("bg task terminal landed before turn-1 complete: terminalIdx=%d firstTurnComplete=%d\n%s",
			terminalIdx, firstTurnComplete, dumpLifecycle(lifecycle))
	}
	if lastTurnComplete <= terminalIdx {
		t.Fatalf("expected a later turn_complete after the bg terminal (turn 2 standalone): terminalIdx=%d lastTurnComplete=%d\n%s",
			terminalIdx, lastTurnComplete, dumpLifecycle(lifecycle))
	}

	// Turn 2's result must not have produced any parser-side event for
	// the turn-1 bg tool_use_id. Concretely: no EventToolComplete for
	// bgBash after the first turn_complete — only the in-turn-1
	// placeholder completion is legitimate.
	bgCompletionsBeforeFirstTurn := 0
	bgCompletionsAfterFirstTurn := 0
	for i, evt := range lifecycle {
		if evt.Kind != provider.EventToolComplete || evt.ItemID != bgBash {
			continue
		}
		if i < firstTurnComplete {
			bgCompletionsBeforeFirstTurn++
		} else {
			bgCompletionsAfterFirstTurn++
		}
	}
	if bgCompletionsBeforeFirstTurn != 1 {
		t.Fatalf("expected 1 bg placeholder EventToolComplete before turn-1 complete, got %d", bgCompletionsBeforeFirstTurn)
	}
	if bgCompletionsAfterFirstTurn != 0 {
		t.Fatalf("turn 2's result spuriously emitted %d EventToolComplete for turn-1 bg tool_use_id (%s)\n%s",
			bgCompletionsAfterFirstTurn, bgBash, dumpLifecycle(lifecycle))
	}

	// As a belt-and-braces check: no EventToolStart for bgBash after
	// the first turn_complete either — turn 2 must not rewrite the
	// turn-1 launch.
	for i, evt := range lifecycle {
		if i <= firstTurnComplete {
			continue
		}
		if evt.Kind == provider.EventToolStart && evt.ItemID == bgBash {
			t.Fatalf("turn 2 spuriously re-emitted EventToolStart for turn-1 bg tool_use_id at index %d\n%s",
				i, dumpLifecycle(lifecycle))
		}
	}
}

// dumpLifecycle returns a printable listing of the lifecycle events
// for use in fatal-message context. Keeps test failure output
// self-contained without dumping the entire fixture.
func dumpLifecycle(events []provider.ProviderEvent) string {
	var b strings.Builder
	for i, evt := range events {
		b.WriteString("  [")
		b.WriteString(itoa(i))
		b.WriteString("] ")
		b.WriteString(string(evt.Kind))
		if evt.ItemID != "" {
			b.WriteString(" id=")
			b.WriteString(evt.ItemID)
		}
		if len(evt.Meta) > 0 && len(evt.Meta) < 160 {
			b.WriteString(" meta=")
			b.Write(evt.Meta)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// itoa avoids importing strconv just for a debug dumper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// TestReplay_LocalAgentOutlives validates the bug-fix scenario from
// the 2026-05 spike: parent launches a local_agent (Task) subagent
// in background, narrates, ends its message with stop_reason=end_turn,
// and Claude CLI WITHHOLDS the `result` envelope until the subagent
// completes (~10s gap in the captured fixture).
//
// The contract this test pins:
//
//  1. The parent's `message_delta.stop_reason="end_turn"` MUST emit a
//     soft EventTurnComplete.
//  2. The soft EventTurnComplete MUST appear before the wire
//     `result`-driven EventTurnComplete in the event stream.
//  3. Both EventTurnCompletes carry stop_reason="end_turn".
//
// Without this behavior the working indicator stays on for the entire
// subagent runtime even though the parent is idle. See
// invariants.md §27 for the full rule.
func TestReplay_LocalAgentOutlives(t *testing.T) {
	events := replayFixture(t, fixtureLocalAgentOutlives)

	type observedComplete struct {
		stopReason         string
		assistantMessageID string
		idx                int
	}
	var softs, results []observedComplete

	for i, evt := range events {
		if evt.Kind != provider.EventTurnComplete {
			continue
		}
		// Soft completes only fire on parent messages (parent_tool_use_id == "")
		// — sanity-check that no subagent message_delta produced one.
		if evt.ParentToolUseID != "" {
			t.Errorf("EventTurnComplete from subagent (parent_tool_use_id=%q) at idx %d — should never fire from subagent end_turn",
				evt.ParentToolUseID, i)
		}
		switch meta := evt.TurnComplete.(type) {
		case *provider.SoftRoundCloseMeta:
			if meta == nil {
				t.Fatalf("event %d: nil soft turn-complete meta", i)
			}
			softs = append(softs, observedComplete{stopReason: meta.StopReason, assistantMessageID: meta.AssistantMessageID, idx: i})
		case *provider.WireTurnCompleteMeta:
			if meta == nil {
				t.Fatalf("event %d: nil wire turn-complete meta", i)
			}
			results = append(results, observedComplete{stopReason: meta.StopReason, assistantMessageID: meta.AssistantMessageID, idx: i})
		default:
			t.Fatalf("event %d: turn-complete meta type = %T", i, evt.TurnComplete)
		}
	}

	// The fixture has a two-round cascade: parent end_turn → wait for
	// subagent → re-round init → parent end_turn → result envelope.
	// That's two soft completes + one result-driven complete.
	if len(softs) != 2 {
		t.Errorf("expected exactly 2 soft EventTurnComplete (one per parent end_turn round), got %d", len(softs))
	}
	if len(results) != 1 {
		t.Errorf("expected exactly 1 result-driven EventTurnComplete (trailing wire `result`), got %d", len(results))
	}

	// Every soft complete carries stop_reason=end_turn (we don't
	// emit soft on tool_use / pause_turn / max_tokens).
	for _, s := range softs {
		if s.stopReason != "end_turn" {
			t.Errorf("soft @%d stop_reason: got %q, want %q (only end_turn/stop_sequence/refusal should fire soft)",
				s.idx, s.stopReason, "end_turn")
		}
	}

	// The first soft must appear before the result-driven complete
	// — that's the load-bearing ordering that makes the indicator
	// clear before the wire turn-end signal arrives.
	if len(softs) > 0 && len(results) > 0 && softs[0].idx >= results[0].idx {
		t.Errorf("first soft must precede the first result-driven complete; soft@%d, result@%d",
			softs[0].idx, results[0].idx)
	}

	// Trailing real `result` carries the final assistant_message_id
	// (parser consumed via takeLastAssistantMessageID).
	if len(results) > 0 && results[0].assistantMessageID == "" {
		t.Errorf("result-driven complete should carry the final assistant_message_id; got empty")
	}
}

// TestReplay_LocalAgentUserInputDuringWait validates that the parser
// handles the same scenario when the host injects a new user message
// via stdin during the parent's end_turn → result wait window. The
// captured fixture has THREE parent message_delta stop_reason=end_turn
// signals across three rounds, but only ONE wire `result` envelope at
// the very end. The parser must emit a soft EventTurnComplete for each
// parent end_turn so the frontend's working indicator cycles correctly
// per round, and must NOT fire soft on the intermediate tool_use
// stop_reason that ends the parent's first message of round 1.
func TestReplay_LocalAgentUserInputDuringWait(t *testing.T) {
	events := replayFixture(t, fixtureLocalAgentUserInputMid)

	var softCount, resultCount int
	for _, evt := range filterKinds(events, provider.EventTurnComplete) {
		if evt.ParentToolUseID != "" {
			t.Errorf("EventTurnComplete from subagent — should never fire from subagent end_turn")
		}
		switch meta := evt.TurnComplete.(type) {
		case *provider.SoftRoundCloseMeta:
			if meta == nil {
				t.Fatal("nil soft turn-complete meta")
			}
			if meta.StopReason != "end_turn" && meta.StopReason != "stop_sequence" && meta.StopReason != "refusal" {
				t.Errorf("soft fired with disallowed stop_reason %q", meta.StopReason)
			}
			softCount++
		case *provider.WireTurnCompleteMeta:
			resultCount++
		default:
			t.Fatalf("turn-complete meta type = %T", evt.TurnComplete)
		}
	}
	if softCount != 3 {
		t.Errorf("expected exactly 3 soft EventTurnComplete (one per parent end_turn round), got %d", softCount)
	}
	if resultCount != 1 {
		t.Errorf("expected exactly 1 result-driven EventTurnComplete (the wire `result` at cascade end), got %d", resultCount)
	}
}

// TestReplay_LocalAgentPlusBgBash confirms the result-delay is keyed
// on `local_agent` specifically: a parent that launches both a bg
// Bash AND a bg local_agent has only ONE wire `result` envelope at
// the end of the entire cascade (subagent dominates), while emitting
// a soft EventTurnComplete for each parent end_turn across the
// re-round cascade triggered by the bash's earlier completion.
func TestReplay_LocalAgentPlusBgBash(t *testing.T) {
	events := replayFixture(t, fixtureLocalAgentPlusBgBash)

	var softCount, resultCount int
	for _, evt := range filterKinds(events, provider.EventTurnComplete) {
		if evt.ParentToolUseID != "" {
			t.Errorf("EventTurnComplete from subagent — should never fire from subagent end_turn")
		}
		switch evt.TurnComplete.(type) {
		case *provider.SoftRoundCloseMeta:
			softCount++
		case *provider.WireTurnCompleteMeta:
			resultCount++
		default:
			t.Fatalf("turn-complete meta type = %T", evt.TurnComplete)
		}
	}
	// Three soft completes (one per round across the cascade:
	// initial parent end_turn, post-bash-completion re-round end_turn,
	// post-subagent-completion re-round end_turn) and exactly one
	// result-driven complete at cascade end.
	if softCount != 3 {
		t.Errorf("expected exactly 3 soft EventTurnComplete, got %d", softCount)
	}
	if resultCount != 1 {
		t.Errorf("expected exactly 1 result-driven EventTurnComplete, got %d", resultCount)
	}
}
