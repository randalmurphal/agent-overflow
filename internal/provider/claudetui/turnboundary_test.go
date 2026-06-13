package claudetui

import (
	"encoding/json"
	"fmt"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// newTurnReqBody is a classAgent /v1/messages body whose last message is the
// user's text — the shape of a request that begins a new user turn.
func newTurnReqBody(userText string) string {
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}],`+
			`"messages":[{"role":"user","content":%q}]}`, userText)
}

// continuationReqBody is a classAgent body whose last message carries a
// tool_result — the shape of a request that continues the current turn after a
// tool_use, not a new turn.
func continuationReqBody(toolUseID string) string {
	return fmt.Sprintf(
		`{"model":"claude-haiku","max_tokens":32000,"tools":[{"name":"Bash"},{"name":"Read"}],`+
			`"messages":[{"role":"user","content":"first task"},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}]}`,
		toolUseID, toolUseID)
}

// toolUseSSE is a response that stops at tool_use (model not done — a
// continuation follows, so the turn stays open).
func toolUseSSE(toolUseID string) []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_a","model":"claude-haiku","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":"Bash","input":{}}}`, toolUseID),
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":8}}`,
		`{"type":"message_stop"}`,
	}
}

// endTurnSSE is a response that stops at end_turn (model done — settles the turn).
func endTurnSSE() []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_b","model":"claude-haiku","role":"assistant","usage":{"input_tokens":12,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
}

// reconParser wires a reconstructor to a real claude.Parser and records every
// event it emits, so a test can assert on the normalized stream the TUI path
// produces end to end.
type reconParser struct {
	t   *testing.T
	rec *reconstructor
	out []provider.ProviderEvent
}

func newReconParser(t *testing.T) *reconParser {
	t.Helper()
	rp := &reconParser{t: t}
	parser := claude.NewParser()
	rp.rec = newReconstructor(func(line json.RawMessage) {
		evs, err := parser.ParseLine(testThread, line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		rp.out = append(rp.out, evs...)
	})
	return rp
}

// drive runs one classAgent request through the reconstructor. When echoUUID is
// non-empty it first records an AO send carrying that uuid (what session.Send
// does), so the request confirms it with a replay echo.
func (rp *reconParser) drive(echoUUID, body string, sse []string) {
	rp.t.Helper()
	if echoUUID != "" {
		rp.rec.pushUserEcho("user text", echoUUID)
	}
	class, req := classifyRequest([]byte(body))
	if class != classAgent || req == nil {
		rp.t.Fatalf("classifyRequest(%s) = %v, want classAgent", body, class)
	}
	ar := rp.rec.beginAgentRequest(req)
	for _, s := range sse {
		ar.onSSE(json.RawMessage(s))
	}
	ar.end()
}

func (rp *reconParser) initCount() int { return len(findKind(rp.out, provider.EventInit)) }

func (rp *reconParser) userTexts() []provider.ProviderEvent {
	return findKind(rp.out, provider.EventUserText)
}

// providerItemIDOf returns the provider_item_id stamped on an EventUserText's
// meta — the uuid the reconstructor's replay echo carried.
func providerItemIDOf(t *testing.T, ev provider.ProviderEvent) string {
	t.Helper()
	if len(ev.Meta) == 0 {
		return ""
	}
	var m struct {
		ProviderItemID string `json:"provider_item_id"`
	}
	if err := json.Unmarshal(ev.Meta, &m); err != nil {
		t.Fatalf("unmarshal user_text meta %s: %v", ev.Meta, err)
	}
	return m.ProviderItemID
}

// TestReconstructInitOnMainLoopRestart pins the init signal: a fresh system:init
// fires on every main-loop (re)start from a settled state — a new user turn AND a
// backgrounded-task resume — but NOT on a tool_result continuation. This mirrors
// the headless CLI (local_agent_outlives.ndjson re-inits on resume).
//
// Without the fix (the prior once-only / shape-gated init plus no replay echo)
// turn 2 strands: its content mis-resolves to the prior turn's index, the working
// indicator bound to the new turn's pending send never clears, and no user_text
// echo confirms the send. The user_text assertions are red without the echo
// mechanism; the init #3 assertion is red without turnSettled-driven re-init.
func TestReconstructInitOnMainLoopRestart(t *testing.T) {
	rp := newReconParser(t)

	// Turn 1, req 1: idle send → new turn. init + echo. Stops at tool_use, so the
	// loop stays open.
	rp.drive("uuid-1", newTurnReqBody("first task"), toolUseSSE("toolu_1"))
	if got := rp.initCount(); got != 1 {
		t.Fatalf("after turn-1 req1: init=%d want 1 (kinds %v)", got, kindsOf(rp.out))
	}
	if uts := rp.userTexts(); len(uts) != 1 || providerItemIDOf(t, uts[0]) != "uuid-1" {
		t.Fatalf("after turn-1 req1: want 1 user_text with provider_item_id uuid-1, got %d (%v)", len(uts), kindsOf(rp.out))
	}

	// Turn 1, req 2: the tool_result continuation. No init (loop is open), no echo
	// (no send). Stops at end_turn → settles.
	rp.drive("", continuationReqBody("toolu_1"), endTurnSSE())
	if got := rp.initCount(); got != 1 {
		t.Fatalf("after turn-1 continuation: init=%d want 1 — a continuation must not re-init (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(rp.userTexts()); got != 1 {
		t.Fatalf("after turn-1 continuation: user_text=%d want 1 — no echo on a continuation", got)
	}

	// Turn 2: a fresh idle send. init #2 (the stuck-indicator fix) + echo #2.
	rp.drive("uuid-2", newTurnReqBody("second task"), endTurnSSE())
	if got := rp.initCount(); got != 2 {
		t.Fatalf("after turn-2: init=%d want 2 — each new user turn re-inits (kinds %v)", got, kindsOf(rp.out))
	}
	if uts := rp.userTexts(); len(uts) != 2 || providerItemIDOf(t, uts[1]) != "uuid-2" {
		t.Fatalf("after turn-2: want 2 user_text with provider_item_id uuid-2, got %d", len(uts))
	}

	// Backgrounded resume: the loop is settled (turn 2 ended) and a main request
	// arrives with NO AO send. init #3 re-arms via handleInit; crucially NO
	// phantom echo is synthesized for a restart the user did not initiate.
	rp.drive("", continuationReqBody("toolu_bg"), endTurnSSE())
	if got := rp.initCount(); got != 3 {
		t.Fatalf("after bg resume: init=%d want 3 — a settled-loop restart re-inits (kinds %v)", got, kindsOf(rp.out))
	}
	if got := len(rp.userTexts()); got != 2 {
		t.Fatalf("after bg resume: user_text=%d want 2 — a resume with no Send must not synthesize a user echo", got)
	}
}

// TestReconstructEchoMidTurnSteerNoInit pins the steer case: a queued send that
// flushes while the loop is still open is confirmed by a replay echo but must NOT
// re-init, so it folds into the open turn (matching headless) rather than opening
// a spurious new turn.
func TestReconstructEchoMidTurnSteerNoInit(t *testing.T) {
	rp := newReconParser(t)

	// Turn 1, req 1: idle send → new turn, stops at tool_use (loop stays open).
	rp.drive("uuid-1", newTurnReqBody("first task"), toolUseSSE("toolu_1"))
	if got := rp.initCount(); got != 1 {
		t.Fatalf("after req1: init=%d want 1", got)
	}

	// Mid-turn steer: the next main request carries the queued send. Echo fires
	// (the send is confirmed) but NO init (the loop is open).
	rp.drive("uuid-steer", continuationReqBody("toolu_1"), endTurnSSE())
	if got := rp.initCount(); got != 1 {
		t.Fatalf("after mid-turn steer: init=%d want 1 — a steer into an open loop must not re-init (kinds %v)", got, kindsOf(rp.out))
	}
	if uts := rp.userTexts(); len(uts) != 2 || providerItemIDOf(t, uts[1]) != "uuid-steer" {
		t.Fatalf("after mid-turn steer: want 2 user_text with provider_item_id uuid-steer, got %d", len(uts))
	}
}

// TestReconstructEchoFIFOOrder proves queued sends confirm in registration order:
// two sends recorded before two main requests echo back in FIFO order, matching
// the pending-send FIFO triage consumes from.
func TestReconstructEchoFIFOOrder(t *testing.T) {
	rp := newReconParser(t)

	// Two idle sends queued back to back (e.g. the user fires two messages); each
	// opens its own turn and echoes its own uuid in order.
	rp.drive("uuid-A", newTurnReqBody("task A"), endTurnSSE())
	rp.drive("uuid-B", newTurnReqBody("task B"), endTurnSSE())

	uts := rp.userTexts()
	if len(uts) != 2 {
		t.Fatalf("user_text=%d want 2 (kinds %v)", len(uts), kindsOf(rp.out))
	}
	if got := providerItemIDOf(t, uts[0]); got != "uuid-A" {
		t.Fatalf("first echo provider_item_id=%q want uuid-A", got)
	}
	if got := providerItemIDOf(t, uts[1]); got != "uuid-B" {
		t.Fatalf("second echo provider_item_id=%q want uuid-B", got)
	}
}
