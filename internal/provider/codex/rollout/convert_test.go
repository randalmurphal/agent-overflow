package rollout

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

func metaField(t *testing.T, raw json.RawMessage, key string) any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode meta %s: %v", raw, err)
	}
	return obj[key]
}

func eventsOfKind(events []importir.Event, kind provider.EventKind) []importir.Event {
	var out []importir.Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------- message dedup

// A modern rollout carries BOTH the event_msg record and its response_item
// mirror for the same message. Only the event_msg record may become a row, or
// every message would double — and the mirror also holds developer/system
// injections the user never typed.
func TestConvertPrefersEventMsgMessagesOverTheResponseItemMirror(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, taskStartedLine,
		userMsgLine,
		`{"timestamp":"2026-08-07T19:07:47.100Z","type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}`,
		`{"timestamp":"2026-08-07T19:07:47.200Z","type":"response_item","payload":{"type":"message","id":"m2","role":"developer","content":[{"type":"input_text","text":"harness instructions"}]}}`,
		agentMsgLine,
		`{"timestamp":"2026-08-07T19:07:59.100Z","type":"response_item","payload":{"type":"message","id":"m3","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if got := countKind(res.Events, provider.EventUserText); got != 1 {
		t.Fatalf("user rows = %d, want 1 (the event_msg record only)", got)
	}
	if got := countKind(res.Events, provider.EventContentBlockStop); got != 1 {
		t.Fatalf("assistant rows = %d, want 1", got)
	}
}

// A file old enough to carry no event_msg messages at all falls back to the
// response_item mirror — otherwise the whole conversation would import empty.
func TestConvertFallsBackToResponseItemMessagesForOldFiles(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:47.100Z","type":"response_item","payload":{"type":"message","id":"m1","role":"user","content":[{"type":"input_text","text":"legacy prompt"}]}}`,
		`{"timestamp":"2026-08-07T19:07:47.150Z","type":"response_item","payload":{"type":"message","id":"m2","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\nA\nB\n</recommended_plugins>"}]}}`,
		`{"timestamp":"2026-08-07T19:07:47.200Z","type":"response_item","payload":{"type":"message","id":"m3","role":"assistant","content":[{"type":"output_text","text":"legacy answer"}]}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	users := eventsOfKind(res.Events, provider.EventUserText)
	if len(users) != 1 || users[0].Content != "legacy prompt" {
		t.Fatalf("user rows = %+v, want just the real prompt", users)
	}
	blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(blocks) != 1 || blocks[0].Content != "legacy answer" {
		t.Fatalf("assistant rows = %+v", blocks)
	}
}

// --------------------------------------------------------------------- reasoning

func TestConvertSkipsEncryptedOnlyReasoningAndKeepsReadableSummaries(t *testing.T) {
	encrypted := `{"timestamp":"2026-08-07T19:07:50.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs1","summary":[],"encrypted_content":"gAAAAA…"}}`
	readable := `{"timestamp":"2026-08-07T19:07:51.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs2","summary":[{"type":"summary_text","text":"**Planning the change**"}],"encrypted_content":"gAAAAA…"}}`

	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, encrypted, readable, taskCompleteLn))
	blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(blocks) != 1 {
		t.Fatalf("want exactly the readable summary, got %d blocks", len(blocks))
	}
	if blocks[0].Content != "**Planning the change**" {
		t.Fatalf("thinking content = %q", blocks[0].Content)
	}
	if metaField(t, blocks[0].Meta, "blockType") != "thinking" {
		t.Fatalf("readable reasoning must be a thinking block: %s", blocks[0].Meta)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("a skipped encrypted reasoning item is not worth a warning: %+v", res.Warnings)
	}
}

// event_msg/agent_reasoning repeats the response_item summary verbatim; only
// one of the two may become a row.
func TestConvertPrefersEventMsgReasoning(t *testing.T) {
	path := writeRollout(t, testSessionID,
		metaLine, taskStartedLine,
		`{"timestamp":"2026-08-07T19:07:50.000Z","type":"response_item","payload":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"**Same text**"}],"encrypted_content":"x"}}`,
		`{"timestamp":"2026-08-07T19:07:50.100Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"**Same text**"}}`,
		taskCompleteLn,
	)
	res := parseFixture(t, path)
	blocks := eventsOfKind(res.Events, provider.EventContentBlockStop)
	if len(blocks) != 1 || blocks[0].Content != "**Same text**" {
		t.Fatalf("reasoning rows = %+v, want one", blocks)
	}
}

// --------------------------------------------------------------------- usage

// token_count carries no turn id and repeats within a turn; the LAST snapshot
// of a turn wins, and because the wire totals are thread-cumulative the turn's
// own usage is the delta against the previous turn's close.
func TestConvertTokenCountLastPerTurnWinsAsADelta(t *testing.T) {
	count := func(ts string, in, cached, write, out, reasoning, total int) string {
		return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":` +
			strconv.Itoa(in) + `,"cached_input_tokens":` + strconv.Itoa(cached) + `,"cache_write_input_tokens":` + strconv.Itoa(write) +
			`,"output_tokens":` + strconv.Itoa(out) + `,"reasoning_output_tokens":` + strconv.Itoa(reasoning) + `,"total_tokens":` + strconv.Itoa(total) + `}}}}`
	}
	turn2Start := `{"timestamp":"2026-08-07T19:09:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2","started_at":1786133900}}`
	turn2Done := `{"timestamp":"2026-08-07T19:09:30.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2","started_at":1786133900,"completed_at":1786133930}}`

	res := parseFixture(t, writeRollout(t, testSessionID,
		metaLine, turnContextLine, taskStartedLine,
		count("2026-08-07T19:07:50.000Z", 50, 10, 1, 5, 2, 55),
		count("2026-08-07T19:07:58.000Z", 100, 40, 3, 20, 8, 120), // last of turn 1 wins
		taskCompleteLn,
		turn2Start,
		count("2026-08-07T19:09:20.000Z", 180, 60, 5, 35, 12, 215),
		turn2Done,
	))

	completes := eventsOfKind(res.Events, provider.EventTurnComplete)
	if len(completes) != 2 {
		t.Fatalf("turn completions = %d, want 2", len(completes))
	}
	first := completes[0].TurnComplete.(*provider.WireTurnCompleteMeta)
	if first.Usage == nil {
		t.Fatal("turn 1 usage missing")
	}
	// input_tokens INCLUDES cached, so the normalized split is 100-40=60.
	if first.Usage.InputTokens != 60 || first.Usage.CacheReadInputTokens != 40 ||
		first.Usage.CacheCreationInputTokens != 3 || first.Usage.OutputTokens != 20 ||
		first.Usage.ReasoningOutputTokens != 8 {
		t.Fatalf("turn 1 usage = %+v", *first.Usage)
	}
	if len(first.ModelUsage) != 1 || first.ModelUsage[0].Model != "gpt-5.6-sol" {
		t.Fatalf("turn 1 model usage = %+v", first.ModelUsage)
	}
	second := completes[1].TurnComplete.(*provider.WireTurnCompleteMeta)
	// Cumulative → the second turn's own usage is the difference.
	if second.Usage.InputTokens != 60 || second.Usage.CacheReadInputTokens != 20 ||
		second.Usage.OutputTokens != 15 || second.Usage.ReasoningOutputTokens != 4 {
		t.Fatalf("turn 2 usage = %+v", *second.Usage)
	}
}

// --------------------------------------------------------------- compaction

func TestConvertCompactedEmitsOnlyADivider(t *testing.T) {
	compacted := `{"timestamp":"2026-08-07T19:07:56.000Z","type":"compacted","payload":{"message":"Summary of the work so far","window_id":"w1","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"replayed history that must NOT import"}]}]}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, compacted, taskCompleteLn))

	boundaries := eventsOfKind(res.Events, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("want one compaction divider, got %d", len(boundaries))
	}
	if boundaries[0].Content != "Summary of the work so far" {
		t.Fatalf("divider summary = %q", boundaries[0].Content)
	}
	if metaField(t, boundaries[0].Meta, "windowId") != "w1" {
		t.Fatalf("window id not carried: %s", boundaries[0].Meta)
	}
	for _, e := range res.Events {
		if strings.Contains(e.Content, "must NOT import") {
			t.Fatal("replacement_history was written into the transcript")
		}
	}
}

// context_compacted is the lightweight twin of `compacted`; it only becomes a
// divider in files that carry no `compacted` record at all.
//
// Codex writes the durable record FIRST — every compaction path awaits
// Session::replace_compacted_history (which persists RolloutItem::Compacted)
// before emitting the ContextCompaction item whose legacy event is the twin —
// which is what lets the converter dedupe on a running flag instead of a
// whole-file pre-scan. The second case pins that with a file the pre-scan
// short-circuits out of (it carries both event_msg messages and reasoning), so
// the running flag is the only thing doing the work.
func TestConvertContextCompactedIsOnlyAFallbackDivider(t *testing.T) {
	compacted := `{"timestamp":"2026-08-07T19:07:56.000Z","type":"compacted","payload":{"message":"Summary","window_id":"w1"}}`
	ctxCompacted := `{"timestamp":"2026-08-07T19:07:56.500Z","type":"event_msg","payload":{"type":"context_compacted"}}`
	reasoning := `{"timestamp":"2026-08-07T19:07:48.000Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"thinking"}}`

	withBoth := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, compacted, ctxCompacted, taskCompleteLn))
	if got := countKind(withBoth.Events, provider.EventCompactBoundary); got != 1 {
		t.Fatalf("dividers = %d, want 1 when both records are present", got)
	}

	modern := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine,
		userMsgLine, reasoning, agentMsgLine, compacted, ctxCompacted, taskCompleteLn))
	if got := countKind(modern.Events, provider.EventCompactBoundary); got != 1 {
		t.Fatalf("dividers = %d, want 1 on a file the pre-scan short-circuits out of", got)
	}

	alone := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, ctxCompacted, taskCompleteLn))
	if got := countKind(alone.Events, provider.EventCompactBoundary); got != 1 {
		t.Fatalf("dividers = %d, want the fallback divider", got)
	}
}

// The one window where the running flag is not enough on its own: a tail
// refresh whose cursor lands BETWEEN a `compacted` record and its
// `context_compacted` twin. The converter starts past the record, so its own
// flag is clear, and the pre-scan — which reads from 0 — short-circuits
// before the record on any file carrying messages and reasoning. Left alone,
// the twin writes a second divider for a compaction the first import already
// recorded. The pre-scan therefore keeps reading up to FromOffset until it
// has the seed.
func TestParseTailRefreshDoesNotDuplicateADividerAcrossTheCursor(t *testing.T) {
	reasoning := `{"timestamp":"2026-08-07T19:07:48.000Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"thinking"}}`
	compacted := `{"timestamp":"2026-08-07T19:07:56.000Z","type":"compacted","payload":{"message":"Summary","window_id":"w1"}}`
	ctxCompacted := `{"timestamp":"2026-08-07T19:07:56.500Z","type":"event_msg","payload":{"type":"context_compacted"}}`

	// Everything a first import consumed. The pre-scan settles on the
	// reasoning line, four lines before the compaction record.
	head := []string{metaLine, taskStartedLine, userMsgLine, reasoning, agentMsgLine, compacted}
	cursor := int64(0)
	for _, line := range head {
		cursor += int64(len(line)) + 1
	}

	path := writeRollout(t, testSessionID, append(append([]string(nil), head...), ctxCompacted, taskCompleteLn)...)
	if got := countKind(parseFixture(t, path).Events, provider.EventCompactBoundary); got != 1 {
		t.Fatalf("whole-file parse = %d dividers, want 1", got)
	}

	tail, err := Parse(context.Background(), ParseOptions{Path: path, FromOffset: cursor})
	if err != nil {
		t.Fatalf("tail Parse: %v", err)
	}
	if got := countKind(tail.Events, provider.EventCompactBoundary); got != 0 {
		t.Fatalf("tail refresh = %d dividers, want 0 — the twin's compaction is already in the thread", got)
	}

	// …and the seed is not over-eager: a file with no `compacted` record at
	// all still gets its fallback divider from the twin, cursor or no cursor.
	headOnly := []string{metaLine, taskStartedLine, userMsgLine, reasoning, agentMsgLine}
	fallbackCursor := int64(0)
	for _, line := range headOnly {
		fallbackCursor += int64(len(line)) + 1
	}
	fallbackPath := writeRollout(t, testSessionID,
		append(append([]string(nil), headOnly...), ctxCompacted, taskCompleteLn)...)
	fallback, err := Parse(context.Background(), ParseOptions{Path: fallbackPath, FromOffset: fallbackCursor})
	if err != nil {
		t.Fatalf("fallback tail Parse: %v", err)
	}
	if got := countKind(fallback.Events, provider.EventCompactBoundary); got != 1 {
		t.Fatalf("fallback tail = %d dividers, want the twin's own divider", got)
	}
}

// The pre-scan must not read a whole file to prove a negative: a rollout that
// never compacted settles as soon as it has the header, an event_msg message
// and an event_msg reasoning record.
func TestPreScanShortCircuitsOnANeverCompactedFile(t *testing.T) {
	reasoning := `{"timestamp":"2026-08-07T19:07:48.000Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"thinking"}}`
	pre := preScanResult{metaFound: true, hasEventMsgMessage: true, hasEventMsgReasoning: true}
	if !pre.settled() {
		t.Fatal("a file with a header, a message and reasoning must settle without a compaction record")
	}
	// And end to end: the parse still produces the same rows.
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine,
		userMsgLine, reasoning, agentMsgLine, taskCompleteLn))
	if got := countKind(res.Events, provider.EventCompactBoundary); got != 0 {
		t.Fatalf("dividers = %d, want none", got)
	}
	if got := countKind(res.Events, provider.EventUserText); got != 1 {
		t.Fatalf("user rows = %d, want 1", got)
	}
}

// --------------------------------------------------------------- turn safety

// Content outside any task_started still has to belong to a turn, or the
// writer has nothing to key items on.
func TestConvertOpensASyntheticTurnForContentBeforeTaskStarted(t *testing.T) {
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, userMsgLine, taskStartedLine, agentMsgLine, taskCompleteLn))

	starts := eventsOfKind(res.Events, provider.EventTurnStart)
	if len(starts) != 2 {
		t.Fatalf("turn starts = %d, want 2 (synthetic + real)", len(starts))
	}
	if metaField(t, starts[0].Meta, "import_synthetic_turn") != true {
		t.Fatalf("first turn should be marked synthetic: %s", starts[0].Meta)
	}
	if starts[0].TurnIndex != 1 || starts[1].TurnIndex != 2 {
		t.Fatalf("turn indexes = %d/%d, want 1/2", starts[0].TurnIndex, starts[1].TurnIndex)
	}
	if countKind(res.Events, provider.EventTurnComplete) != 2 {
		t.Fatalf("every opened turn must close: %v", kinds(res.Events))
	}
}

// A file that ends mid-turn (the process died) still settles its turn.
func TestConvertClosesAnOpenTurnAtEndOfFile(t *testing.T) {
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, userMsgLine))
	completes := eventsOfKind(res.Events, provider.EventTurnComplete)
	if len(completes) != 1 {
		t.Fatalf("want one synthesised completion, got %d", len(completes))
	}
	if _, ok := completes[0].TurnComplete.(*provider.TruncatedTurnCompleteMeta); !ok {
		t.Fatalf("completion meta = %T, want truncated", completes[0].TurnComplete)
	}
}

func TestConvertTurnAbortedSettlesWithAnError(t *testing.T) {
	aborted := `{"timestamp":"2026-08-07T19:08:00.000Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-1","reason":"interrupted","completed_at":1786133880}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, userMsgLine, aborted))

	complete := firstOfKind(t, res.Events, provider.EventTurnComplete)
	wire, ok := complete.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok {
		t.Fatalf("completion meta = %T", complete.TurnComplete)
	}
	if !wire.Aborted || !strings.Contains(wire.ErrorMessage, "interrupted") {
		t.Fatalf("aborted turn meta = %+v", wire)
	}
}

// A line larger than the reader window but under the cap must still parse —
// that is the ErrBufferFull assembly path, which a naive bufio.Scanner would
// turn into a terminal error for the whole file.
func TestScannerAssemblesLinesLargerThanTheReadBuffer(t *testing.T) {
	big := `{"timestamp":"2026-08-07T19:07:47.000Z","type":"event_msg","payload":{"type":"user_message","message":"` +
		strings.Repeat("y", scanBufferSize*2) + `"}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, big, taskCompleteLn))
	if res.CorruptLines != 0 {
		t.Fatalf("a large-but-legal line is not corruption: %d", res.CorruptLines)
	}
	user := firstOfKind(t, res.Events, provider.EventUserText)
	if len(user.Content) != scanBufferSize*2 {
		t.Fatalf("content length = %d, want %d", len(user.Content), scanBufferSize*2)
	}
}

// ------------------------------------------------------------------- errors

// Errors are user-facing state, not log entries: an imported session must show
// the rate limit that ended the turn.
func TestConvertErrorBecomesAnErrorEvent(t *testing.T) {
	errLine := `{"timestamp":"2026-05-04T04:59:37.693Z","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}`
	res := parseFixture(t, writeRollout(t, testSessionID, metaLine, taskStartedLine, errLine, taskCompleteLn))

	e := firstOfKind(t, res.Events, provider.EventError)
	if e.Content != "You've hit your usage limit." {
		t.Fatalf("error content = %q", e.Content)
	}
	if metaField(t, e.Meta, "codexErrorInfo") != "usage_limit_exceeded" {
		t.Fatalf("error info lost: %s", e.Meta)
	}
}
