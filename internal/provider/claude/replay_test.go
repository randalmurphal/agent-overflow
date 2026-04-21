package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// These replay tests load captured Claude CLI NDJSON from /tmp spike
// directories and feed each line through (*Parser).ParseLine, then
// assert the resulting event sequence matches the new
// turn/tool/task-lifecycle contract. The fixtures refresh via
// `AGENT_OVERFLOW_DEBUG=provider` re-runs — we never copy them into
// the repo so real-wire evolution stays visible.
//
// See docs/references/claude-wire.md §Captured samples and
// docs/architecture/turn-lifecycle.md §Captured wire samples for the
// mapping from scenario to file.

// loadNDJSONFixture reads a captured NDJSON log and returns its lines
// with blank entries dropped. Missing files skip the test rather than
// fail so CI environments without the spike captures don't break.
func loadNDJSONFixture(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("fixture %s not present — run the spike capture to regenerate", path)
		}
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
// /tmp/claude-bg-spike/ndjson_bash.log.
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
//     (task_notification for fg bash DROPPED — invariant 21)
//  8. EventToolComplete (foreground bash result)
//  9. EventToolStart  (Read)
//  10. EventToolComplete (Read result)
//  11. EventTurnComplete (with lastAssistantMessageID populated)
func TestReplay_NDJsonBash(t *testing.T) {
	events := replayFixture(t, "/tmp/claude-bg-spike/ndjson_bash.log")

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

		// task_notification for fg bash dropped

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

	// Fg task_notification must NOT have left any event behind.
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

	// EventTurnComplete must carry the last assistant.message.id.
	var turnMeta map[string]any
	for _, evt := range events {
		if evt.Kind == provider.EventTurnComplete {
			_ = json.Unmarshal(evt.Meta, &turnMeta)
		}
	}
	if turnMeta == nil {
		t.Fatalf("no EventTurnComplete observed")
	}
	amid, _ := turnMeta["assistant_message_id"].(string)
	if amid == "" {
		t.Fatalf("EventTurnComplete missing assistant_message_id; meta=%v", turnMeta)
	}
	if !strings.HasPrefix(amid, "msg_") {
		t.Fatalf("assistant_message_id should look like an Anthropic msg id, got %q", amid)
	}
}

// TestReplay_NDJsonTask validates the Task subagent + TaskOutput scenario
// in /tmp/claude-bg-spike/ndjson_task.log.
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
	events := replayFixture(t, "/tmp/claude-bg-spike/ndjson_task.log")

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
// /tmp/claude-bg-spike/ndjson_outlives.log: a backgrounded bash whose
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
//     (task_notification dropped)
//  7. EventInit (second turn starts; sample has a turn 2)
//  8. EventTurnComplete (end of second turn)
func TestReplay_NDJsonOutlives(t *testing.T) {
	events := replayFixture(t, "/tmp/claude-bg-spike/ndjson_outlives.log")

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
// /tmp/taskoutput-multi-spike/ndjson.log: two parallel
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
	events := replayFixture(t, "/tmp/taskoutput-multi-spike/ndjson.log")

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
// separately in /tmp/claude-bg-spike/ndjson_outlives_turn2.log.
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
//   - /tmp/claude-bg-spike/ndjson_outlives.log (turn 1 + task_updated
//     after result; also contains the first re-init for turn 2 and
//     its "Background task finished" result).
//   - /tmp/claude-bg-spike/ndjson_outlives_turn2.log (a second fresh
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
		outlivesPath = "/tmp/claude-bg-spike/ndjson_outlives.log"
		turn2Path    = "/tmp/claude-bg-spike/ndjson_outlives_turn2.log"
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
