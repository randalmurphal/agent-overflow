package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/harness/scenario"
)

// claudeAccountJSON is the account object served on the probe's
// system/init line and on initialize control_responses. Shape matches
// internal/provider/claude/probe.go extractAccountInfoFromInitResponse
// and internal/testutil/app.go WriteMockClaudeInit.
const claudeAccountJSON = `{"email":"mock@agent-overflow.test","subscriptionType":"Claude Max","apiProvider":"firstParty","tokenSource":"claude.ai"}`

// claudeInitializeResponsePayload is the inner response payload for a
// control_request{subtype:"initialize"} ack — the probe reads
// `account` out of it.
const claudeInitializeResponsePayload = `{"commands":[],"agents":[],"account":` + claudeAccountJSON + `}`

// claudeAdapter speaks Claude Code's stream-json NDJSON protocol:
// inbound user envelopes trigger turns, inbound control_requests are
// success-acked out-of-band, and inbound control_responses resolve the
// approval requests this mock originated.
//
// Two protocol behaviours are adapter-owned rather than scenario-
// authored, because AO's turn machinery depends on them for EVERY turn
// and a scenario author forgetting either breaks the turn lifecycle
// silently:
//
//   - system/init per user turn. The real CLI emits one init per user
//     message (verified: fixtures/claude/multiturn_cost_cumulative_
//     20260703.ndjson carries 3 inits for 3 turns). Triage opens the
//     logical turn when init arrives with a pending send registered
//     (triage.handleInit case 1) — without it no `turns` row is
//     written and provider:turn_started/turn_completed never fire.
//   - user-envelope replay echo. AO launches the CLI with
//     --replay-user-messages; the echo (isReplay:true, same uuid)
//     resolves triage's pending-send, stamps provider_item_id onto the
//     user row, and is the persistence trigger for queued sends.
type claudeAdapter struct {
	e *engine
	w *lineWriter

	mu      sync.Mutex
	waiters map[string]chan bool // approval request_id → decision
	seq     int

	// lastEchoUUID chains parentUuid across echoes, matching the CLI's
	// JSONL parent linkage. Stdin-goroutine-only; no lock needed.
	lastEchoUUID string
}

func newClaudeAdapter(e *engine, w *lineWriter) *claudeAdapter {
	return &claudeAdapter{e: e, w: w, waiters: make(map[string]chan bool)}
}

// readStdin drives the session until the app closes stdin, then exits
// 0 — the app owns process lifetime.
func (a *claudeAdapter) readStdin() {
	forEachStdinLine(a.handleLine)
	log.Printf("stdin closed; exiting")
	a.e.terminate(0)
}

// claudeStdinEnvelope covers every stdin shape the app writes: user
// messages, control_requests (interrupt, set_permission_mode,
// set_model, mcp_*, initialize), and control_responses to OUR
// can_use_tool requests.
type claudeStdinEnvelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Behavior string `json:"behavior"`
		} `json:"response"`
	} `json:"response"`
}

func (a *claudeAdapter) handleLine(line []byte) {
	var env claudeStdinEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("claude: malformed stdin line ignored: %v (line: %.200s)", err, line)
		return
	}
	switch env.Type {
	case "control_request":
		// Success-ack every inbound control_request without consuming a
		// turn — matches the real CLI's out-of-band handling of
		// interrupt / set_permission_mode / set_model / mcp_*.
		writeClaudeControlAck(a.w, env.RequestID, env.Request.Subtype)
	case "control_response":
		allow := env.Response.Subtype == "success" && env.Response.Response.Behavior == "allow"
		a.deliverDecision(env.Response.RequestID, allow)
	case "control_cancel_request":
		// The app never abandons its own responses this way; log and drop.
		log.Printf("claude: unexpected control_cancel_request from app (ignored)")
	default:
		// Anything else — normally {"type":"user",...} — is the next
		// user turn. The init + echo protocol frames (see the adapter
		// doc) are written synchronously here so they precede every
		// scenario step of the turn.
		n, vars := a.e.beginTurn()
		a.writeInit(vars)
		a.echoUserEnvelope(line)
		a.e.enqueueTurn(n)
	}
}

// writeInit emits the per-turn system/init line. Built with
// json.Marshal so cwd/session values survive any characters.
func (a *claudeAdapter) writeInit(vars scenario.Vars) {
	a.w.writeLine(mustJSON(map[string]any{
		"type":                "system",
		"subtype":             "init",
		"session_id":          vars["SESSION_ID"],
		"model":               "claude-opus-4-7",
		"cwd":                 vars["CWD"],
		"tools":               []string{"Task", "Bash", "Read", "Write", "Edit"},
		"claude_code_version": mockVersionNumber,
	}), 0, 0)
}

// echoUserEnvelope mirrors the CLI's --replay-user-messages behaviour:
// the inbound envelope goes back out with isReplay:true and a
// parentUuid assigned by the CLI (here: the previous echo's uuid).
func (a *claudeAdapter) echoUserEnvelope(line []byte) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(line, &env); err != nil {
		log.Printf("claude: user envelope unparseable for echo (skipped): %v", err)
		return
	}
	env["isReplay"] = json.RawMessage("true")
	if a.lastEchoUUID != "" {
		env["parentUuid"] = json.RawMessage(mustJSON(a.lastEchoUUID))
	}
	if u, ok := env["uuid"]; ok {
		var s string
		if json.Unmarshal(u, &s) == nil && s != "" {
			a.lastEchoUUID = s
		}
	}
	a.w.writeLine(mustJSON(env), 0, 0)
}

// writeClaudeControlAck answers an inbound control_request with the
// standard success control_response. The initialize subtype carries the
// account payload the app's account probe (and any live initialize)
// expects; everything else gets an empty response object.
func writeClaudeControlAck(w *lineWriter, requestID, subtype string) {
	payload := "{}"
	if subtype == "initialize" {
		payload = claudeInitializeResponsePayload
	}
	w.writeLine(fmt.Sprintf(
		`{"type":"control_response","response":{"subtype":"success","request_id":%s,"response":%s}}`,
		mustJSON(requestID), payload), 0, 0)
}

// sendApproval emits a CanUseTool control_request and registers a
// waiter for the app's control_response (routed by request_id in
// handleLine).
func (a *claudeAdapter) sendApproval(step *scenario.ApprovalStep, vars scenario.Vars) (<-chan bool, func(), error) {
	a.mu.Lock()
	a.seq++
	requestID := fmt.Sprintf("mock-req-%d", a.seq)
	ch := make(chan bool, 1)
	a.waiters[requestID] = ch
	a.mu.Unlock()

	cancel := func() {
		a.mu.Lock()
		delete(a.waiters, requestID)
		a.mu.Unlock()
	}

	input := json.RawMessage(vars.Substitute(string(step.Input)))
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request": map[string]any{
			"subtype":   "can_use_tool",
			"tool_name": step.ToolName,
			"input":     input,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("marshal can_use_tool request: %w", err)
	}
	a.w.writeLine(string(data), 0, 0)
	return ch, cancel, nil
}

func (a *claudeAdapter) deliverDecision(requestID string, allow bool) {
	a.mu.Lock()
	ch, ok := a.waiters[requestID]
	if ok {
		delete(a.waiters, requestID)
	}
	a.mu.Unlock()
	if !ok {
		log.Printf("claude: control_response for unknown request_id %q (ignored)", requestID)
		return
	}
	ch <- allow
}

// mustJSON marshals a plain value that cannot fail (strings, numbers).
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("marshal %v: %v", v, err)
	}
	return string(data)
}
