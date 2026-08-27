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

// recordedPiece is one content boundary with the moment the store wrote
// it (epoch ms). The stamp is a WRITE time, not an arrival time — chunk
// rows land at flush boundaries — so it is cadence at flush granularity,
// which is still the thread's real rhythm rather than a uniform tick.
type recordedPiece struct {
	Text string
	AtMs int64
}

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
	Status       string

	// CreatedAtMs / UpdatedAtMs are the row's own stamps. For a
	// tool_call they bracket the tool's real lifetime: the call row is
	// written at tool_use and UPDATED when the folded inline completion
	// lands, so the pair is what the synthesized tool_result's timing
	// comes from.
	CreatedAtMs int64
	UpdatedAtMs int64

	// Pieces are the content boundaries, in order: the payload head
	// (payloads.data, when non-empty) followed by one entry per
	// payload_chunks row in chunk_index order. That IS the delta
	// boundary set — the app wrote one chunk per streamed delta — so
	// replaying one emit line per piece reproduces the original stream
	// shape rather than a re-chunking of the final text.
	Pieces []recordedPiece

	// Input is a tool_call's input payload, raw. Empty when the row had
	// no input_payload_id or the payload was gone.
	Input string
}

// Content is the item's whole payload: the head plus every chunk.
func (r recordedItem) Content() string {
	var b strings.Builder
	for _, p := range r.Pieces {
		b.WriteString(p.Text)
	}
	return b.String()
}

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

	// DelayMs paces lines inside a burst (and is the whole cadence in
	// uniform mode).
	DelayMs int

	// CadenceReal replays the recorded inter-frame gaps: frames are laid
	// on a timeline from the rows' own stamps, gaps of
	// gapStepThresholdMs or more become explicit delay steps (capped at
	// GapCapMs), and sub-threshold runs stream at DelayMs. Off = the old
	// uniform pacing.
	CadenceReal bool
	// GapCapMs bounds a single recorded gap in real-cadence mode, so a
	// tool that ran for ten minutes replays as a visible pause rather
	// than a stalled rig. <= 0 means defaultGapCapMs.
	GapCapMs int
}

// gapStepThresholdMs splits "streaming burst" from "real pause": frame
// gaps below it are streamed at DelayMs inside one emit step, gaps at or
// above it become their own delay step. 40ms sits above the store's
// same-flush jitter and below anything a human perceives as a pause.
const gapStepThresholdMs = 40

// defaultGapCapMs bounds one replayed gap. Long tool waits are real
// cadence too, but a soak driver cannot afford ten wall-minutes of
// silence per Bash call; three seconds reads as "the tool ran" without
// stalling the run.
const defaultGapCapMs = 3000

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

	var build func(recordedTurn, synthOptions, *synthStats) (scenario.Turn, error)
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
		turn, err := build(recorded, opts, &stats)
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

// wireFrame is one emitted line placed on the turn's recorded timeline.
type wireFrame struct {
	line string
	atMs int64
}

func claudeTurnSteps(turn recordedTurn, opts synthOptions, stats *synthStats) (scenario.Turn, error) {
	// An inline tool completion folds into the tool_call row (status +
	// result payload) — only background tools get a sibling
	// tool_completion row. A tool_result is synthesized for every folded
	// call, EXCEPT the ones an explicit sibling row already answers:
	// those calls would otherwise get two results.
	answered := map[string]bool{}
	for _, item := range turn.Items {
		if item.Kind == kindToolCompletion && item.CompletionOf != "" {
			answered[item.CompletionOf] = true
		}
	}

	var frames []wireFrame
	for _, item := range turn.Items {
		itemFrames, err := claudeItemFrames(item, answered, stats)
		if err != nil {
			return scenario.Turn{}, err
		}
		if len(itemFrames) == 0 {
			continue
		}
		stats.Items++
		frames = append(frames, itemFrames...)
	}
	if opts.CadenceReal {
		sortFramesByTime(frames)
	}
	steps := framesToSteps(frames, opts)
	steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{
		Lines: []string{`{"type":"result","subtype":"success","is_error":false}`},
	}})
	return scenario.Turn{Label: fmt.Sprintf("recorded-turn-%d", turn.TurnIndex), Steps: steps}, nil
}

func claudeItemFrames(item recordedItem, answered map[string]bool, stats *synthStats) ([]wireFrame, error) {
	switch item.Kind {
	case kindAssistantText:
		return claudeBlockFrames(item, "text", "text_delta", "text", stats), nil
	case kindThinking:
		return claudeBlockFrames(item, "thinking", "thinking_delta", "thinking", stats), nil
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
		frames := []wireFrame{{
			line: fmt.Sprintf(
				`{"type":"assistant","message":{"id":%s,"role":"assistant","content":[{"type":"tool_use","id":%s,"name":%s,"input":%s}]}}`,
				jsonString("msg-"+emitID(item.ID)), jsonString(emitID(item.ID)), jsonString(item.ToolName), input),
			atMs: item.CreatedAtMs,
		}}
		// The folded inline completion: the call row's own payload is the
		// result and its updated_at is when the tool finished. Without
		// this frame every inline tool replays unresolved and the app's
		// turn-end sweep marks it failed. A `running` row recorded no
		// completion (it was still live when recorded) and an `answered`
		// call's result is the sibling tool_completion row's business.
		if !answered[item.ID] && (item.Status == "completed" || item.Status == "errored") {
			frames = append(frames, wireFrame{
				line: claudeToolResultLine(item.ID, item.Content(), item.Status == "errored"),
				atMs: maxInt64(item.UpdatedAtMs, item.CreatedAtMs),
			})
		}
		return frames, nil
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
		return []wireFrame{{
			line: claudeToolResultLine(item.CompletionOf, content, item.Status == "errored"),
			atMs: item.CreatedAtMs,
		}}, nil
	case kindUserText:
		// Never replayed: a real `send` drives each Turn, and the mock's
		// own adapter echoes the user envelope back.
		return nil, nil
	default:
		stats.skip(item.Kind)
		return nil, nil
	}
}

// claudeToolResultLine is the one tool_result frame shape, shared by the
// folded-inline synthesis and the sibling tool_completion row so the two
// sources cannot drift.
func claudeToolResultLine(toolUseID, content string, isError bool) string {
	errField := ""
	if isError {
		errField = `,"is_error":true`
	}
	return fmt.Sprintf(
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%s,"content":%s%s}]}}`,
		jsonString(emitID(toolUseID)), jsonString(content), errField)
}

// claudeBlockFrames frames one streamed content block. blockType is what
// content_block_start declares, deltaType/deltaField are the delta's own
// pair, and the same field carries the whole text on the coalesced
// assistant echo the app dedupes against.
func claudeBlockFrames(item recordedItem, blockType, deltaType, deltaField string, stats *synthStats) []wireFrame {
	messageID := emitID(item.ID)
	pieces := item.Pieces
	content := item.Content()
	if len(pieces) == 0 {
		// No payload at all. Emit the block anyway — the item existed and
		// the timeline shape is what a repro reproduces — with one empty
		// delta so the block has a body.
		pieces = []recordedPiece{{AtMs: item.CreatedAtMs}}
		stats.Empty++
	}
	frames := make([]wireFrame, 0, len(pieces)+5)
	frames = append(frames, wireFrame{
		line: fmt.Sprintf(
			`{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":%s,"role":"assistant"}}}`,
			jsonString(messageID)),
		atMs: item.CreatedAtMs,
	}, wireFrame{
		line: fmt.Sprintf(
			`{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":%s,%s:""}}}`,
			jsonString(blockType), jsonString(deltaField)),
		atMs: item.CreatedAtMs,
	})
	lastAt := item.CreatedAtMs
	for _, piece := range pieces {
		at := piece.AtMs
		if at == 0 {
			at = lastAt
		}
		frames = append(frames, wireFrame{
			line: fmt.Sprintf(
				`{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":%s,%s:%s}}}`,
				jsonString(deltaType), jsonString(deltaField), jsonString(piece.Text)),
			atMs: at,
		})
		lastAt = maxInt64(lastAt, at)
		stats.Deltas++
	}
	frames = append(frames, wireFrame{
		line: `{"type":"stream_event","event":"content_block_stop","data":{"type":"content_block_stop","index":0}}`,
		atMs: lastAt,
	}, wireFrame{
		line: `{"type":"stream_event","event":"message_stop","data":{"type":"message_stop"}}`,
		atMs: lastAt,
	}, wireFrame{
		line: fmt.Sprintf(
			`{"type":"assistant","message":{"id":%s,"role":"assistant","content":[{"type":%s,%s:%s}]}}`,
			jsonString(messageID), jsonString(blockType), jsonString(deltaField), jsonString(content)),
		atMs: lastAt,
	})
	return frames
}

// sortFramesByTime orders frames by their recorded stamp, stably, so
// same-millisecond frames keep their recorded item order — which is what
// preserves every pairing (message_start before its deltas, tool_use
// before its result at equal stamps).
func sortFramesByTime(frames []wireFrame) {
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].atMs < frames[j].atMs })
}

// framesToSteps lays frames out as scenario steps. Uniform mode is one
// emit step (the old shape). Real cadence splits at every recorded gap of
// gapStepThresholdMs or more: the gap becomes a delay step (capped), and
// the frames between gaps stream as one burst at DelayMs.
func framesToSteps(frames []wireFrame, opts synthOptions) []scenario.Step {
	if len(frames) == 0 {
		return nil
	}
	lines := func(fs []wireFrame) []string {
		out := make([]string, len(fs))
		for i, f := range fs {
			out[i] = f.line
		}
		return out
	}
	if !opts.CadenceReal {
		return []scenario.Step{{Emit: &scenario.EmitStep{Lines: lines(frames), DelayBetweenMs: opts.DelayMs}}}
	}
	cap := opts.GapCapMs
	if cap <= 0 {
		cap = defaultGapCapMs
	}
	var steps []scenario.Step
	burstStart := 0
	prev := frames[0].atMs
	flush := func(end int) {
		if end > burstStart {
			steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{
				Lines:          lines(frames[burstStart:end]),
				DelayBetweenMs: opts.DelayMs,
			}})
		}
		burstStart = end
	}
	for i := 1; i < len(frames); i++ {
		gap := frames[i].atMs - prev
		if gap < 0 {
			gap = 0
		}
		if gap >= gapStepThresholdMs {
			flush(i)
			if gap > int64(cap) {
				gap = int64(cap)
			}
			steps = append(steps, scenario.Step{DelayMs: int(gap)})
		}
		if frames[i].atMs > prev {
			prev = frames[i].atMs
		}
	}
	flush(len(frames))
	return steps
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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

func codexTurnSteps(turn recordedTurn, opts synthOptions, stats *synthStats) (scenario.Turn, error) {
	var frames []wireFrame
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
		frames = append(frames, codexAgentMessageFrames(item, stats)...)
	}
	if opts.CadenceReal {
		sortFramesByTime(frames)
	}
	steps := []scenario.Step{{Emit: &scenario.EmitStep{Lines: []string{
		`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}"}}}`,
	}}}}
	steps = append(steps, framesToSteps(frames, opts)...)
	steps = append(steps, scenario.Step{Emit: &scenario.EmitStep{Lines: []string{
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"${THREAD_ID}","turn":{"id":"${TURN_ID}","status":"completed"}}}`,
	}}})
	return scenario.Turn{Label: fmt.Sprintf("recorded-turn-%d", turn.TurnIndex), Steps: steps}, nil
}

func codexAgentMessageFrames(item recordedItem, stats *synthStats) []wireFrame {
	itemID := emitID(item.ID)
	pieces := item.Pieces
	if len(pieces) == 0 {
		pieces = []recordedPiece{{AtMs: item.CreatedAtMs}}
		stats.Empty++
	}
	frames := make([]wireFrame, 0, len(pieces)+2)
	frames = append(frames, wireFrame{
		line: fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"agentMessage","id":%s,"status":"inProgress","text":""}}}`,
			jsonString(itemID)),
		atMs: item.CreatedAtMs,
	})
	lastAt := item.CreatedAtMs
	for _, piece := range pieces {
		at := piece.AtMs
		if at == 0 {
			at = lastAt
		}
		frames = append(frames, wireFrame{
			line: fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","itemId":%s,"delta":%s}}`,
				jsonString(itemID), jsonString(piece.Text)),
			atMs: at,
		})
		lastAt = maxInt64(lastAt, at)
		stats.Deltas++
	}
	frames = append(frames, wireFrame{
		line: fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"${THREAD_ID}","turnId":"${TURN_ID}","item":{"type":"agentMessage","id":%s,"status":"completed","text":%s}}}`,
			jsonString(itemID), jsonString(item.Content())),
		atMs: lastAt,
	})
	return frames
}

// shellQuote renders text for the single-quoted argument of a printed
// `ao-harness send` line. The full text is printed rather than a
// truncation: the recipe is meant to be pasted, and a cut prompt drives a
// different turn than the one that was recorded.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
