package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
)

func claudeTurn(items ...recordedItem) recordedTurn {
	return recordedTurn{TurnIndex: 3, UserText: "do the thing", Items: items}
}

// mkPieces builds unstamped pieces — the shape an old row with no
// created_at reads as.
func mkPieces(texts ...string) []recordedPiece {
	out := make([]recordedPiece, len(texts))
	for i, text := range texts {
		out[i] = recordedPiece{Text: text}
	}
	return out
}

func synthOne(t *testing.T, provider string, turn recordedTurn) (*scenario.Scenario, synthStats) {
	t.Helper()
	doc, stats, err := synthesizeScenario(synthOptions{
		Name: "from-thread-abcd1234", Provider: provider, ThreadID: "abcd1234-thread", Turns: []recordedTurn{turn}, DelayMs: 15,
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	return doc, stats
}

// linesOf flattens a turn's emit steps, which is what the mock actually
// writes.
func linesOf(turn scenario.Turn) []string {
	var out []string
	for _, step := range turn.Steps {
		if step.Emit != nil {
			out = append(out, step.Emit.Lines...)
		}
	}
	return out
}

func frameKinds(lines []string) []string {
	kinds := make([]string, 0, len(lines))
	for _, line := range lines {
		var frame struct {
			Type   string `json:"type"`
			Event  string `json:"event"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			kinds = append(kinds, "UNPARSEABLE:"+line)
			continue
		}
		switch {
		case frame.Method != "":
			kinds = append(kinds, frame.Method)
		case frame.Event != "":
			kinds = append(kinds, frame.Event)
		default:
			kinds = append(kinds, frame.Type)
		}
	}
	return kinds
}

// The chunk boundaries ARE the delta boundaries: appendPayloadDataTx
// writes one chunk per flushed delta, so a three-chunk payload replays as
// three deltas, not as one paste of the joined text.
func TestClaudeTextItemEmitsOneDeltaPerRecordedChunk(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "blk-1", Kind: kindAssistantText, Role: "assistant",
		Pieces: mkPieces("Hello ", "from the ", "recording."),
	}))

	lines := linesOf(doc.Turns[0])
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_stop",
		"assistant",
		"result",
	}
	if got := frameKinds(lines); !equalStrings(got, want) {
		t.Fatalf("frames = %v\nwant   %v", got, want)
	}
	if stats.Deltas != 3 {
		t.Errorf("deltas = %d, want 3", stats.Deltas)
	}

	// The coalesced echo carries the WHOLE text: it is what the app dedupes
	// the streamed block against, so a partial one renders the tail twice.
	if !strings.Contains(lines[7], "Hello from the recording.") {
		t.Errorf("assistant echo does not carry the joined text: %s", lines[7])
	}
	// message_start must register the same id the echo carries, or the app
	// renders the block twice (the library's own framing invariant).
	if idOf(t, lines[0]) != idOf(t, lines[7]) {
		t.Errorf("message_start id %q != echo id %q", idOf(t, lines[0]), idOf(t, lines[7]))
	}
}

// A payload the GC already took is not an error: the item existed and its
// position is part of the repro. It replays empty and is COUNTED, so the
// operator knows the fidelity.
func TestClaudeTextItemWithNoPayloadReplaysEmptyAndIsCounted(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "blk-1", Kind: kindAssistantText, Role: "assistant",
	}))
	if stats.Empty != 1 {
		t.Fatalf("empty = %d, want 1", stats.Empty)
	}
	// message_start, content_block_start, ONE empty delta, content_block_stop,
	// message_stop, the coalesced echo, and the turn's result.
	if got, want := len(linesOf(doc.Turns[0])), 7; got != want {
		t.Fatalf("lines = %d, want %d (one empty delta plus the result)", got, want)
	}
}

func TestClaudeThinkingItemUsesTheThinkingBlockVocabulary(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "th-1", Kind: kindThinking, Role: "assistant", Pieces: mkPieces("weighing ", "options"),
	}))
	lines := linesOf(doc.Turns[0])
	for _, want := range []string{`"type":"thinking"`, `"thinking":""`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("content_block_start = %s, want %s", lines[1], want)
		}
	}
	if !strings.Contains(lines[2], `"type":"thinking_delta"`) {
		t.Errorf("delta = %s, want a thinking_delta", lines[2])
	}
	if !strings.Contains(lines[6], `"thinking":"weighing options"`) {
		t.Errorf("echo = %s, want the joined thinking text", lines[6])
	}
}

// The recorded order and the completion_of pairing are the two things a
// tool replay is FOR. A tool_result aimed at the wrong tool_use produces
// a plausible-looking turn describing a different app.
func TestClaudeToolPairKeepsRecordedOrderAndPairing(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderClaude, claudeTurn(
		recordedItem{ID: "tu-a", Kind: kindToolCall, ToolName: "Read", Role: "assistant",
			Input: `{"file_path":"/x/y.go"}`},
		recordedItem{ID: "tc-a", Kind: kindToolCompletion, CompletionOf: "tu-a", Role: "user",
			Pieces: mkPieces("package y")},
		recordedItem{ID: "tu-b", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
			Input: `{"command":"ls"}`},
		recordedItem{ID: "tc-b", Kind: kindToolCompletion, CompletionOf: "tu-b", Role: "user",
			Pieces: mkPieces("a\nb")},
	))
	lines := linesOf(doc.Turns[0])
	if got, want := frameKinds(lines), []string{"assistant", "user", "assistant", "user", "result"}; !equalStrings(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	if stats.Items != 4 {
		t.Errorf("items = %d, want 4", stats.Items)
	}
	for i, pair := range []struct{ use, result int }{{0, 1}, {2, 3}} {
		useID := toolUseID(t, lines[pair.use])
		resultID := toolResultID(t, lines[pair.result])
		if useID == "" || useID != resultID {
			t.Errorf("pair %d: tool_use %q vs tool_result %q", i, useID, resultID)
		}
	}
	// The two pairs must not share an id, or one completion lands on both.
	if toolUseID(t, lines[0]) == toolUseID(t, lines[2]) {
		t.Error("both tool_use frames carry the same id")
	}
	if !strings.Contains(lines[0], `"file_path":"/x/y.go"`) {
		t.Errorf("tool_use dropped its recorded input: %s", lines[0])
	}
}

// An inline tool completion folds into the tool_call row — only
// background tools get a sibling tool_completion row. The synthesizer
// must put the folded result back on the wire, or every inline tool
// replays unresolved and the app's turn-end sweep marks it failed
// (observed live 2026-08-27: 565 of 572 replayed tools "turn ended with
// tool unresolved / command failed").
func TestClaudeFoldedInlineCompletionEmitsAPairedToolResult(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "tu-a", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
		Status: "completed", Input: `{"command":"ls"}`, Pieces: mkPieces("a\nb\n"),
	}))
	lines := linesOf(doc.Turns[0])
	if got, want := frameKinds(lines), []string{"assistant", "user", "result"}; !equalStrings(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	if toolUseID(t, lines[0]) != toolResultID(t, lines[1]) {
		t.Fatalf("folded result is not paired: use %q result %q", toolUseID(t, lines[0]), toolResultID(t, lines[1]))
	}
	if !strings.Contains(lines[1], `"content":"a\nb\n"`) {
		t.Errorf("folded result dropped the recorded output: %s", lines[1])
	}
	if strings.Contains(lines[1], "is_error") {
		t.Errorf("a completed tool carries is_error: %s", lines[1])
	}
}

// A tool_call recorded as errored replays as an errored tool, not a
// silently successful one.
func TestClaudeErroredToolCallCarriesIsError(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "tu-a", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
		Status: "errored", Input: `{"command":"boom"}`, Pieces: mkPieces("exit 1"),
	}))
	lines := linesOf(doc.Turns[0])
	if !strings.Contains(lines[1], `"is_error":true`) {
		t.Fatalf("errored tool result has no is_error: %s", lines[1])
	}
}

// A call answered by an explicit sibling tool_completion row must not
// ALSO get a folded result — that would deliver two results for one
// tool_use.
func TestClaudeSiblingCompletionSuppressesTheFoldedResult(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(
		recordedItem{ID: "tu-a", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
			Status: "completed", Input: `{"command":"sleep"}`},
		recordedItem{ID: "tc-a", Kind: kindToolCompletion, CompletionOf: "tu-a", Role: "user",
			Pieces: mkPieces("done")},
	))
	lines := linesOf(doc.Turns[0])
	if got, want := frameKinds(lines), []string{"assistant", "user", "result"}; !equalStrings(got, want) {
		t.Fatalf("frames = %v, want %v (exactly one result for the pair)", got, want)
	}
}

// A `running` row recorded no completion — the tool was still live when
// the thread was recorded — so nothing is synthesized for it.
func TestClaudeRunningToolCallGetsNoFabricatedResult(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "tu-a", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
		Status: "running", Input: `{"command":"tail -f x"}`,
	}))
	lines := linesOf(doc.Turns[0])
	if got, want := frameKinds(lines), []string{"assistant", "result"}; !equalStrings(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
}

// Real cadence: a recorded gap of gapStepThresholdMs or more becomes a
// delay step (capped at GapCapMs); sub-threshold frames stream in one
// burst. Same-millisecond frames keep recorded order, so pairings hold.
func TestRealCadenceTurnsRecordedGapsIntoDelaySteps(t *testing.T) {
	doc, _, err := synthesizeScenario(synthOptions{
		Name: "x", Provider: scenario.ProviderClaude, ThreadID: "t", DelayMs: 15,
		CadenceReal: true, GapCapMs: 2000,
		Turns: []recordedTurn{{TurnIndex: 1, Items: []recordedItem{
			{ID: "tu-a", Kind: kindToolCall, ToolName: "Bash", Role: "assistant",
				Status: "completed", Input: `{}`, Pieces: mkPieces("out"),
				CreatedAtMs: 1000, UpdatedAtMs: 6000}, // 5s gap → capped to 2000
			{ID: "a-1", Kind: kindAssistantText, Role: "assistant",
				CreatedAtMs: 6010, UpdatedAtMs: 6010,
				Pieces: []recordedPiece{{Text: "done", AtMs: 6010}}}, // 10ms after → same burst
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := doc.Turns[0].Steps
	// tool_use burst, capped delay, then result + text block + turn result.
	var delays []int
	for _, step := range steps {
		if step.DelayMs != 0 {
			delays = append(delays, step.DelayMs)
		}
	}
	if len(delays) != 1 || delays[0] != 2000 {
		t.Fatalf("delay steps = %v, want exactly [2000] (the capped tool gap)", delays)
	}
	// The document must still survive the real parser.
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Parse(encoded); err != nil {
		t.Fatalf("the real parser refused a real-cadence document: %v", err)
	}
}

// Recorded ids are namespaced by turn so a replay into the thread that
// already holds the original does not collide on (thread_id, id).
func TestEmittedIDsAreNamespacedByTurn(t *testing.T) {
	doc, _ := synthOne(t, scenario.ProviderClaude, claudeTurn(recordedItem{
		ID: "tu-a", Kind: kindToolCall, ToolName: "Read", Role: "assistant", Input: `{}`,
	}))
	line := linesOf(doc.Turns[0])[0]
	if !strings.Contains(line, `"id":"r${TURN}-tu-a"`) {
		t.Fatalf("tool_use id is not turn-namespaced: %s", line)
	}
}

// A completion with no completion_of pairs with nothing. The app drops
// such a frame silently, which is a worse answer than a refusal.
func TestClaudeRefusesAToolCompletionWithNoPairing(t *testing.T) {
	_, _, err := synthesizeScenario(synthOptions{
		Name: "x", Provider: scenario.ProviderClaude, Turns: []recordedTurn{claudeTurn(
			recordedItem{ID: "tc-a", Kind: kindToolCompletion, Role: "user", Pieces: mkPieces("out")},
		)},
	})
	if err == nil {
		t.Fatal("an unpaired tool_completion was accepted")
	}
	if !strings.Contains(err.Error(), "completion_of") {
		t.Fatalf("error = %v", err)
	}
}

// App-internal rows are not wire frames. They are dropped — and counted,
// because a scenario that silently lost four rows looks identical to one
// that had four fewer.
func TestAppInternalKindsAreSkippedAndCounted(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderClaude, claudeTurn(
		recordedItem{ID: "n-1", Kind: "notification", Role: "system"},
		recordedItem{ID: "n-2", Kind: "notification", Role: "system"},
		recordedItem{ID: "r-1", Kind: "api_retry", Role: "system"},
		recordedItem{ID: "c-1", Kind: "compaction", Role: "system"},
		recordedItem{ID: "blk", Kind: kindAssistantText, Role: "assistant", Pieces: mkPieces("done")},
	))
	if got, want := stats.SkippedSummary(), "api_retry 1, compaction 1, notification 2"; got != want {
		t.Fatalf("skipped = %q, want %q", got, want)
	}
	if stats.Items != 1 {
		t.Errorf("items = %d, want 1", stats.Items)
	}
	if !strings.Contains(doc.Description, "notification 2") {
		t.Errorf("the document does not report the skips: %s", doc.Description)
	}
}

// The user turn is the TRIGGER, not content: a real `send` opens each
// Turn and the mock's own adapter echoes the envelope back.
func TestUserTextIsNeverReplayedAsAFrame(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderClaude, claudeTurn(
		recordedItem{ID: "u-1", Kind: kindUserText, Role: "user", Pieces: mkPieces("do the thing")},
		recordedItem{ID: "blk", Kind: kindAssistantText, Role: "assistant", Pieces: mkPieces("ok")},
	))
	for _, line := range linesOf(doc.Turns[0]) {
		if strings.Contains(line, "do the thing") {
			t.Fatalf("the user text was replayed as a frame: %s", line)
		}
	}
	if summary := stats.SkippedSummary(); summary != "" {
		t.Errorf("user_text was counted as a skip: %q", summary)
	}
}

// A synthesized document has to survive the same parser the harness runs
// at set time, or the verb produces something no instance will accept.
func TestSynthesizedDocumentSurvivesTheRealParser(t *testing.T) {
	doc, _, err := synthesizeScenario(synthOptions{
		Name: "from-thread-abcd1234", Provider: scenario.ProviderClaude, ThreadID: "abcd1234",
		DelayMs: 15,
		Turns: []recordedTurn{
			{TurnIndex: 1, UserText: "one", Items: []recordedItem{
				{ID: "t-1", Kind: kindThinking, Role: "assistant", Pieces: mkPieces("hm")},
				{ID: "a-1", Kind: kindAssistantText, Role: "assistant", Pieces: mkPieces("first")},
			}},
			{TurnIndex: 2, UserText: "two", Items: []recordedItem{
				{ID: "tu-1", Kind: kindToolCall, ToolName: "Bash", Role: "assistant", Input: `{"command":"ls"}`},
				{ID: "tc-1", Kind: kindToolCompletion, CompletionOf: "tu-1", Role: "user", Pieces: mkPieces("a")},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := scenario.Parse(encoded)
	if err != nil {
		t.Fatalf("the real parser refused the document: %v", err)
	}
	if parsed.AfterTurns != "silent" {
		t.Errorf("afterTurns = %q, want silent (a repeatLast would re-run the last recorded turn forever)", parsed.AfterTurns)
	}
	if len(parsed.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(parsed.Turns))
	}
	// Every emitted line has to be JSON the provider parsers can read.
	for i, turn := range parsed.Turns {
		for _, line := range linesOf(turn) {
			if !json.Valid([]byte(line)) {
				t.Errorf("turn %d emits a line that is not JSON: %s", i+1, line)
			}
		}
	}
}

// -- Codex ----------------------------------------------------------------

// The one Codex content shape internal/harness/scenario/library
// demonstrates, framed exactly as codex-basic.json frames it.
func TestCodexAgentMessageStreamsEveryRecordedChunk(t *testing.T) {
	doc, stats := synthOne(t, scenario.ProviderCodex, claudeTurn(recordedItem{
		ID: "msg-1", Kind: kindAssistantText, Role: "assistant",
		Pieces: mkPieces("Hello! ", "This is codex."),
	}))
	want := []string{
		"turn/started",
		"item/started",
		"item/agentMessage/delta",
		"item/agentMessage/delta",
		"item/completed",
		"turn/completed",
	}
	if got := frameKinds(linesOf(doc.Turns[0])); !equalStrings(got, want) {
		t.Fatalf("frames = %v\nwant   %v", got, want)
	}
	if stats.Deltas != 2 {
		t.Errorf("deltas = %d, want 2", stats.Deltas)
	}
	if !strings.Contains(linesOf(doc.Turns[0])[4], `"text":"Hello! This is codex."`) {
		t.Errorf("item/completed does not carry the joined text: %s", linesOf(doc.Turns[0])[4])
	}
}

// The library demonstrates no Codex reasoning or tool item, and a store
// row cannot supply what those frames need. A guessed dialect would
// install cleanly, stream plausibly, and reproduce a DIFFERENT app — so
// the synthesizer refuses and names the gap instead.
func TestCodexRefusesTheKindsTheLibraryDoesNotDemonstrate(t *testing.T) {
	cases := map[string]recordedItem{
		"reasoning":       {ID: "r-1", Kind: kindThinking, Role: "assistant", Pieces: mkPieces("hm")},
		"tool call":       {ID: "t-1", Kind: kindToolCall, ToolName: "shell", Role: "assistant", Input: `{}`},
		"tool completion": {ID: "c-1", Kind: kindToolCompletion, CompletionOf: "t-1", Role: "user"},
	}
	for label, item := range cases {
		_, _, err := synthesizeScenario(synthOptions{
			Name: "x", Provider: scenario.ProviderCodex, Turns: []recordedTurn{claudeTurn(item)},
		})
		if err == nil {
			t.Fatalf("%s: a Codex scenario was synthesized for an undemonstrated item kind", label)
		}
		if !strings.Contains(err.Error(), item.Kind) || !strings.Contains(err.Error(), "store") {
			t.Errorf("%s: the refusal does not say what is missing: %v", label, err)
		}
	}
}

func TestUnknownProviderIsRefused(t *testing.T) {
	_, _, err := synthesizeScenario(synthOptions{
		Name: "x", Provider: "gemini", Turns: []recordedTurn{claudeTurn()},
	})
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("error = %v", err)
	}
}

func TestShellQuoteSurvivesAnApostrophe(t *testing.T) {
	if got, want := shellQuote(`it's fine`), `'it'\''s fine'`; got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
}

// -- helpers --------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func idOf(t *testing.T, line string) string {
	t.Helper()
	var frame struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Data struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	if frame.Message.ID != "" {
		return frame.Message.ID
	}
	return frame.Data.Message.ID
}

func toolUseID(t *testing.T, line string) string {
	t.Helper()
	var frame struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	for _, block := range frame.Message.Content {
		if block.Type == "tool_use" {
			return block.ID
		}
	}
	return ""
}

func toolResultID(t *testing.T, line string) string {
	t.Helper()
	var frame struct {
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	for _, block := range frame.Message.Content {
		if block.Type == "tool_result" {
			return block.ToolUseID
		}
	}
	return ""
}
