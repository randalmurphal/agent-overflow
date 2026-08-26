package main

// The pure half of `scenario from-thread`: recorded timeline rows in, a
// mock-provider scenario document out. Split from the command for the
// same reason bench_report.go is split from cmd_bench.go — the frame
// vocabulary is the part a wrong answer is silently believed about, and
// it must be testable with no backend and no database.
//
// Every frame shape here is copied from a scenario in
// internal/harness/scenario/library, which is the only vocabulary this
// repo can demonstrate rather than guess (see the root guide's "When
// Behavior Is Unclear"). Where the library demonstrates nothing, this
// file REFUSES instead of inventing a dialect — see codexTurnSteps.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/harness/scenario"
)

// Item kinds this synthesizer knows how to put back on the wire. Every
// other kind in the `items` CHECK is an app-internal artifact — a
// notification row, an api_retry marker, a compaction boundary — rather
// than something a provider wrote, so it is skipped and COUNTED.
const (
	kindUserText       = "user_text"
	kindAssistantText  = "assistant_text"
	kindThinking       = "thinking"
	kindToolCall       = "tool_call"
	kindToolCompletion = "tool_completion"
)

// recordedItem is one timeline row as the synthesizer sees it: identity,
// position, and the streaming boundaries its payload was written at.
type recordedItem struct {
	ID           string
	TurnIndex    int
	ItemIndex    int
	Kind         string
	Role         string
	ToolName     string
	CompletionOf string
	Summary      string

	// Pieces are the content boundaries, in order: the payload head
	// (payloads.data, when non-empty) followed by one entry per
	// payload_chunks row in chunk_index order. That IS the delta
	// boundary set — the app wrote one chunk per streamed delta — so
	// replaying one emit line per piece reproduces the original cadence
	// rather than a re-chunking of the final text.
	Pieces []string

	// Input is a tool_call's input payload, raw. Empty when the row had
	// no input_payload_id or the payload was gone.
	Input string
}

// Content is the item's whole payload: the head plus every chunk.
func (r recordedItem) Content() string { return strings.Join(r.Pieces, "") }

// recordedTurn is one user turn's rows, in recorded order, plus the user
// text that opened it (which the scenario does NOT replay — a real
// `send` is what triggers each Turn).
type recordedTurn struct {
	TurnIndex int
	UserText  string
	Items     []recordedItem
}

// synthStats is the fidelity report. An operator driving a repro needs to
// know what did NOT survive the round trip, and a count is the only
// honest way to say it: a scenario that silently dropped four rows looks
// identical to one that had four fewer.
type synthStats struct {
	Turns     int            `json:"turns"`
	Items     int            `json:"items"`
	Deltas    int            `json:"deltas"`
	Empty     int            `json:"emptyPayloads"`
	Skipped   map[string]int `json:"skipped,omitempty"`
	skipOrder []string
}

func (s *synthStats) skip(kind string) {
	if s.Skipped == nil {
		s.Skipped = map[string]int{}
	}
	if _, seen := s.Skipped[kind]; !seen {
		s.skipOrder = append(s.skipOrder, kind)
	}
	s.Skipped[kind]++
}

// SkippedSummary renders the skip tally in a stable order, so two runs
// over the same thread print the same sentence.
func (s synthStats) SkippedSummary() string {
	if len(s.Skipped) == 0 {
		return ""
	}
	kinds := append([]string(nil), s.skipOrder...)
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", kind, s.Skipped[kind]))
	}
	return strings.Join(parts, ", ")
}

// synthOptions is everything the pure half needs.
type synthOptions struct {
	Name     string
	Provider string
	ThreadID string
	Title    string
	Turns    []recordedTurn
	DelayMs  int
}

// synthesizeScenario rebuilds recorded turns as a mock scenario.
//
// afterTurns is "silent" on purpose: the document describes exactly the
// turns that were recorded, and a repeatLast would re-run the last one
// forever for an operator who sent one message too many — which reads as
// the app duplicating work rather than as the script running out.
func synthesizeScenario(opts synthOptions) (*scenario.Scenario, synthStats, error) {
	stats := synthStats{Turns: len(opts.Turns)}
	if len(opts.Turns) == 0 {
		return nil, stats, fmt.Errorf("no turns to rebuild")
	}

	var build func(recordedTurn, int, *synthStats) (scenario.Turn, error)
	switch opts.Provider {
	case scenario.ProviderClaude:
		build = claudeTurnSteps
	case scenario.ProviderCodex:
		build = codexTurnSteps
	default:
		return nil, stats, fmt.Errorf("thread provider %q is neither %q nor %q", opts.Provider, scenario.ProviderClaude, scenario.ProviderCodex)
	}

	turns := make([]scenario.Turn, 0, len(opts.Turns))
	for _, recorded := range opts.Turns {
		turn, err := build(recorded, opts.DelayMs, &stats)
		if err != nil {
			return nil, stats, err
		}
		turns = append(turns, turn)
	}

	doc := &scenario.Scenario{
		Version:     scenario.CurrentVersion,
		Name:        opts.Name,
		Description: synthDescription(opts, stats),
		Provider:    opts.Provider,
		Turns:       turns,
		AfterTurns:  "silent",
	}
	if err := doc.Validate(); err != nil {
		return nil, stats, err
	}
	return doc, stats, nil
}

func synthDescription(opts synthOptions, stats synthStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rebuilt from the last %d recorded turn(s) of thread %s", stats.Turns, opts.ThreadID)
	if opts.Title != "" {
		fmt.Fprintf(&b, " (%q)", opts.Title)
	}
	fmt.Fprintf(&b, " by `ao-harness scenario from-thread`. %d item(s) replayed, %d delta(s).",
		stats.Items, stats.Deltas)
	if summary := stats.SkippedSummary(); summary != "" {
		fmt.Fprintf(&b, " Skipped app-internal item kinds that are not provider wire frames: %s.", summary)
	}
	if stats.Empty > 0 {
		fmt.Fprintf(&b, " %d item(s) had no stored payload and replay with empty content.", stats.Empty)
	}
	b.WriteString(" It carries real recorded session content — do not commit it.")
	return b.String()
}

// emitID namespaces a recorded id so a replay into a thread that already
// holds the original does not collide on (thread_id, id). ${TURN} is
// constant within a turn, so every pairing the recording carried —
// tool_use to tool_result, message_start to its assistant echo — survives
// the rename.
func emitID(id string) string { return "r${TURN}-" + id }

// jsonString renders a Go string as a JSON string literal, which is how
// every recorded value reaches a wire line. Marshalling a string cannot
// fail, so the error is dropped deliberately rather than threaded through
// every frame builder.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil { // unreachable: encoding/json always encodes a string
		return `""`
	}
	return string(encoded)
}

// -- Claude ---------------------------------------------------------------
//
// Frame shapes copied from library/bench-giant-turn.json (tool pairs plus
// a streamed text block), library/thinking-then-text.json (the thinking
// block vocabulary) and library/tool-call.json. The mock's Claude adapter
// owns system/init and the user echo; a scenario owns content framing,
// and a text or thinking envelope needs a prior message_start registering
// the same message id or the app renders the block twice.

func claudeTurnSteps(turn recordedTurn, delayMs int, stats *synthStats) (scenario.Turn, error) {
	steps := make([]scenario.Step, 0, len(turn.Items)+1)
	for _, item := range turn.Items {
		lines, err := claudeItemLines(item, stats)
		if err != nil {
			return scenario.Turn{}, err
		}
		if len(lines) == 0 {
			continue
		}
		stats.Items++
		steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{Lines: lines, DelayBetweenMs: delayMs}})
	}
	steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{
		Lines: []string{`{"type":"result","subtype":"success","is_error":false}`},
	}})
	return scenario.Turn{Label: fmt.Sprintf("recorded-turn-%d", turn.TurnIndex), Steps: steps}, nil
}

func claudeItemLines(item recordedItem, stats *synthStats) ([]string, error) {
	switch item.Kind {
	case kindAssistantText:
		return claudeBlockLines(item, "text", "text_delta", "text", stats), nil
	case kindThinking:
		return claudeBlockLines(item, "thinking", "thinking_delta", "thinking", stats), nil
	case kindToolCall:
		input := strings.TrimSpace(item.Input)
		if input == "" || !json.Valid([]byte(input)) {
			// A tool_use with no readable input still belongs in the
			// replay: its position and its pairing with the completion are
			// what the repro is about. An empty object is the shape the
			// app parses; a missing key is not.
			input = "{}"
			stats.Empty++
		}
		return []string{fmt.Sprintf(
			`{"type":"assistant","message":{"id":%s,"role":"assistant","content":[{"type":"tool_use","id":%s,"name":%s,"input":%s}]}}`,
			jsonString("msg-"+emitID(item.ID)), jsonString(emitID(item.ID)), jsonString(item.ToolName), input)}, nil
	case kindToolCompletion:
		if item.CompletionOf == "" {
			// completion_of is the pairing. Without it the frame would be
			// a tool_result for nothing, which the app drops silently —
			// worse than saying so.
			return nil, fmt.Errorf("tool_completion %s carries no completion_of, so it pairs with no tool_use", item.ID)
		}
		content := item.Content()
		if content == "" {
			stats.Empty++
		}
		return []string{fmt.Sprintf(
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%s,"content":%s}]}}`,
			jsonString(emitID(item.CompletionOf)), jsonString(content))}, nil
	case kindUserText:
		// Never replayed: a real `send` drives each Turn, and the mock's
		// own adapter echoes the user envelope back.
		return nil, nil
	default:
		stats.skip(item.Kind)
		return nil, nil
	}
}

// claudeBlockLines frames one streamed content block. blockType is what
// content_block_start declares, deltaType/deltaField are the delta's own
// pair, and the same field carries the whole text on the coalesced
// assistant echo the app dedupes against.
func claudeBlockLines(item recordedItem, blockType, deltaType, deltaField string, stats *synthStats) []string {
	messageID := emitID(item.ID)
	pieces := item.Pieces
	content := item.Content()
	if len(pieces) == 0 {
		// No payload at all. Emit the block anyway — the item existed and
		// the timeline shape is what a repro reproduces — with one empty
		// delta so the block has a body.
		pieces = []string{""}
		stats.Empty++
	}
	lines := make([]string, 0, len(pieces)+5)
	lines = append(lines, fmt.Sprintf(
		`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":%s,"role":"assistant"}}}`,
		jsonString(messageID)))
	lines = append(lines, fmt.Sprintf(
		`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":%s,%s:""}}}`,
		jsonString(blockType), jsonString(deltaField)))
	for _, piece := range pieces {
		lines = append(lines, fmt.Sprintf(
			`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":%s,%s:%s}}}`,
			jsonString(deltaType), jsonString(deltaField), jsonString(piece)))
		stats.Deltas++
	}
	lines = append(lines,
		`{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
		fmt.Sprintf(
			`{"type":"assistant","message":{"id":%s,"role":"assistant","content":[{"type":%s,%s:%s}]}}`,
			jsonString(messageID), jsonString(blockType), jsonString(deltaField), jsonString(content)))
	return lines
}

// -- Codex ----------------------------------------------------------------

// codexUnsupportedKinds names the item kinds this synthesizer refuses to
// put back on the Codex wire, and says what is missing for each.
//
// The reason is the repo's own rule about guessing provider behaviour.
// The scenario library demonstrates exactly one Codex content shape —
// `agentMessage`, streamed through `item/agentMessage/delta`
// (library/codex-basic.json) — plus the turn envelope. Everything else
// would have to be INVENTED from a normalized store row:
//
//   - a `thinking` row is a Codex `reasoning` item, whose deltas arrive
//     on one of two mutually-exclusive methods depending on the model
//     class (`item/reasoning/textDelta` vs `summaryTextDelta`) and whose
//     completed shape is a summary structure, not a `text` field. The
//     store keeps neither the method nor the structure.
//   - a `tool_call` row is one of several Codex item types
//     (`commandExecution`, `fileChange`, `mcpToolCall`, `webSearch`, …),
//     each with its own required item payload (command / changes /
//     server+tool / query) and its own completion fields
//     (`aggregatedOutput`, `exitCode`, …). The store keeps a normalized
//     tool name and a RENDERED result, from which the wire item cannot be
//     recovered.
//
// A guessed dialect here would produce a scenario that installs cleanly,
// streams plausibly, and reproduces a different app than the one being
// debugged — the worst outcome available. So it refuses and names the
// gap.
var codexUnsupportedKinds = map[string]string{
	kindThinking: "a Codex reasoning item needs its delta method (item/reasoning/textDelta vs summaryTextDelta, model-class dependent) " +
		"and its summary structure, neither of which the store records",
	kindToolCall: "a Codex tool item needs its wire item type (commandExecution / fileChange / mcpToolCall / webSearch / …) " +
		"and that type's own payload, which the store's normalized tool_name and rendered result cannot reconstruct",
	kindToolCompletion: "a Codex tool completion needs the same wire item type plus its completion fields (aggregatedOutput, exitCode, changes, …), " +
		"which the store's rendered result cannot reconstruct",
}

func codexTurnSteps(turn recordedTurn, delayMs int, stats *synthStats) (scenario.Turn, error) {
	steps := []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
		`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
	}}}}
	for _, item := range turn.Items {
		if reason, unsupported := codexUnsupportedKinds[item.Kind]; unsupported {
			return scenario.Turn{}, fmt.Errorf(
				"cannot rebuild a Codex scenario from turn %d: item %s is %q, and %s\n"+
					"the Claude half of this verb is complete; the Codex half covers assistant text only "+
					"(the one content shape internal/harness/scenario/library demonstrates). "+
					"Rebuild a text-only Codex turn, or script the tool frames by hand against a captured fixture",
				turn.TurnIndex, item.ID, item.Kind, reason)
		}
		if item.Kind != kindAssistantText {
			if item.Kind != kindUserText {
				stats.skip(item.Kind)
			}
			continue
		}
		stats.Items++
		steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{
			Lines:          codexAgentMessageLines(item, stats),
			DelayBetweenMs: delayMs,
		}})
	}
	steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{Lines: []string{
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
	}}})
	return scenario.Turn{Label: fmt.Sprintf("recorded-turn-%d", turn.TurnIndex), Steps: steps}, nil
}

func codexAgentMessageLines(item recordedItem, stats *synthStats) []string {
	itemID := emitID(item.ID)
	pieces := item.Pieces
	if len(pieces) == 0 {
		pieces = []string{""}
		stats.Empty++
	}
	lines := make([]string, 0, len(pieces)+2)
	lines = append(lines, fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"agentMessage","id":%s,"status":"inProgress","text":""}}}`,
		jsonString(itemID)))
	for _, piece := range pieces {
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","itemId":%s,"delta":%s}}`,
			jsonString(itemID), jsonString(piece)))
		stats.Deltas++
	}
	lines = append(lines, fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"agentMessage","id":%s,"status":"completed","text":%s}}}`,
		jsonString(itemID), jsonString(item.Content())))
	return lines
}

// shellQuote renders text for the single-quoted argument of a printed
// `ao-harness send` line. The full text is printed rather than a
// truncation: the recipe is meant to be pasted, and a cut prompt drives a
// different turn than the one that was recorded.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
