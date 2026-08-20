package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// codexApprovalIDBase starts mock-originated server-request ids far
// from the app's own request counter (which starts at 1) so wire logs
// stay unambiguous. Correlation doesn't need it — requests and
// responses travel in opposite directions — but readable captures do.
const codexApprovalIDBase = 1_000_000

// codexAdapter speaks the Codex app-server's line-delimited JSON-RPC
// 2.0: app requests are answered from scenario response templates
// (turn/start additionally triggers the matching scenario turn), app
// responses resolve the approval requests this mock originated, and
// app notifications are ignored.
type codexAdapter struct {
	e         *engine
	w         *lineWriter
	responses map[string]string // method → full response template

	mu          sync.Mutex
	waiters     map[int64]chan bool // approval rpc id → decision
	approvalSeq atomic.Int64
	// forkSeq numbers `thread/fork` answers so two forks of the same
	// mock thread never claim the same id.
	forkSeq atomic.Int64
}

func newCodexAdapter(e *engine, w *lineWriter, opts *scenario.CodexOptions) *codexAdapter {
	// Built-in defaults cover the standard handshake; scenarios override
	// only what they need. Templates are full response lines with
	// ${REQUEST_ID} substituted verbatim (number or string id).
	responses := map[string]string{
		"initialize":     `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}`,
		"thread/start":   `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"thread":{"id":"${THREAD_ID}"}}}`,
		"thread/resume":  `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"thread":{"id":"${THREAD_ID}"}}}`,
		"turn/start":     `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"turn":{"id":"${TURN_ID}"}}}`,
		"turn/interrupt": `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}`,
	}
	if opts != nil {
		for method, tmpl := range opts.Responses {
			responses[method] = tmpl
		}
	}
	a := &codexAdapter{e: e, w: w, responses: responses, waiters: make(map[int64]chan bool)}
	a.approvalSeq.Store(codexApprovalIDBase)
	return a
}

func (a *codexAdapter) readStdin() {
	forEachStdinLine(a.handleLine)
	log.Printf("stdin closed; exiting")
	a.e.terminate(0)
}

func (a *codexAdapter) handleLine(line []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("codex: malformed stdin line ignored: %v (line: %.200s)", err, line)
		return
	}
	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case hasID && msg.Method != "":
		a.handleRequest(msg.ID, msg.Method, msg.Params)
	case hasID:
		a.handleResponse(msg.ID, msg.Result, msg.Error)
	case msg.Method != "":
		// Notifications ("initialized", ...) need no reply.
	default:
		log.Printf("codex: stdin line with neither id nor method (ignored): %.200s", line)
	}
}

func (a *codexAdapter) handleRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "thread/start":
		// Codex carries its permission configuration in the thread/start
		// params rather than argv, so this is the earliest observable point.
		// Reported before the response so a test that waits on the config
		// cannot see the session make progress first.
		a.e.rep.report(control.Report{
			Kind: control.ReportSessionConfig,
			SessionConfig: &control.SessionConfig{
				Sandbox:        readParamString(params, "sandbox"),
				ApprovalPolicy: readParamString(params, "approvalPolicy"),
			},
		})
		a.respond(id, method, a.e.currentVars())
	case "thread/resume":
		// Echo the requested thread id and rebind ${THREAD_ID} to it.
		if tid := readParamString(params, "threadId"); tid != "" {
			a.e.setThreadID(tid)
		}
		a.respond(id, method, a.e.currentVars())
	case "turn/start":
		// The app-server validates outputSchema per turn rather than at spawn,
		// so the check lands here; see rejectInvalidOutputSchema.
		if schema := readParamRaw(params, "outputSchema"); len(schema) > 0 {
			rejectInvalidOutputSchema("outputSchema", schema)
		}
		// Response first (real server order), then the turn's steps
		// stream as notifications from the engine goroutine.
		n, vars := a.e.beginTurn()
		a.e.rep.report(control.Report{
			Kind: control.ReportUserInput, Turn: n,
			Input: codexInputText(params), SessionRef: vars["THREAD_ID"],
		})
		a.respond(id, method, vars)
		a.e.enqueueTurn(n)
	case "turn/steer":
		// A steer is user text reaching the provider mid-turn. It reports on
		// the same surface as a turn/start's input — a test asserting what the
		// provider received must not have to know which path the app took —
		// and carries the already-running turn's number, because a steer
		// starts no scenario turn of its own. The response template stays the
		// generic empty result the default branch would have produced.
		turn, vars := a.e.currentTurn(), a.e.currentVars()
		a.e.rep.report(control.Report{
			Kind: control.ReportUserInput, Turn: turn,
			Input: codexInputText(params), SessionRef: vars["THREAD_ID"],
		})
		a.respond(id, method, vars)
	case "thread/fork":
		a.forkThread(id, params)
	case "turn/interrupt":
		// The real app-server replies when TurnAborted arrives, immediately
		// before publishing turn/completed{status:interrupted}. Preserve that
		// observable order while the engine owns the shared abort behavior.
		a.respond(id, method, a.e.currentVars())
		a.e.interruptTurn(readParamString(params, "turnId"))
	default:
		if _, known := a.responses[method]; !known {
			log.Printf("codex: request method %q has no scenario response; answering with empty result", method)
		}
		a.respond(id, method, a.e.currentVars())
	}
}

// forkThread answers `thread/fork`: a NEW thread id plus the turn tail
// that survived the cut, which is the shape `parseThreadForkResponse`
// decodes and `ForkAt` validates its anchor against. `lastTurnId` is
// inclusive; ABSENT copies every turn, which is exactly what a mid-turn
// tail fork sends. A `lastTurnId` naming the IN-PROGRESS turn is refused
// with a JSON-RPC error, matching codex 0.147.0 — that refusal is the
// whole reason AO normalises such an anchor to a tail fork, so the mock
// has to be able to produce it rather than quietly full-forking.
//
// A scenario that declares its own `thread/fork` response template still
// wins; this synthesis only fills the default.
func (a *codexAdapter) forkThread(id json.RawMessage, params json.RawMessage) {
	if _, scripted := a.responses["thread/fork"]; scripted {
		a.respond(id, "thread/fork", a.e.currentVars())
		return
	}
	anchor := readParamString(params, "lastTurnId")
	turnIDs, ok := a.e.forkedTurnIDs(anchor)
	if !ok {
		a.writeRPCError(id, -32602, fmt.Sprintf(
			"thread/fork: lastTurnId %q is unknown or names the in-progress turn", anchor))
		return
	}
	turns := make([]map[string]any, 0, len(turnIDs))
	for _, turnID := range turnIDs {
		turns = append(turns, map[string]any{"id": turnID})
	}
	forked := fmt.Sprintf("%s-fork-%d", a.e.currentVars()["THREAD_ID"], a.forkSeq.Add(1))
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"thread": map[string]any{"id": forked, "turns": turns}},
	}), 0, 0)
}

func (a *codexAdapter) writeRPCError(id json.RawMessage, code int, message string) {
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}), 0, 0)
}

func (a *codexAdapter) sendInterruptedTurn(vars scenario.Vars) {
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "turn/completed",
		"params": map[string]any{
			"threadId": vars["THREAD_ID"],
			"turn": map[string]any{
				"id": vars["TURN_ID"], "items": []any{}, "status": "interrupted", "error": nil,
			},
		},
	}), 0, 0)
}

func (a *codexAdapter) respond(id json.RawMessage, method string, vars scenario.Vars) {
	tmpl, ok := a.responses[method]
	if !ok {
		tmpl = `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}`
	}
	sub := make(scenario.Vars, len(vars)+1)
	for k, v := range vars {
		sub[k] = v
	}
	sub["REQUEST_ID"] = string(id)
	a.w.writeLine(sub.Substitute(tmpl), 0, 0)
}

// handleResponse routes an app response (to one of OUR approval
// requests) to the waiting approval step.
func (a *codexAdapter) handleResponse(id json.RawMessage, result, errRaw json.RawMessage) {
	var rpcID int64
	if err := json.Unmarshal(id, &rpcID); err != nil {
		log.Printf("codex: response with non-integer id %s (ignored)", id)
		return
	}
	a.mu.Lock()
	ch, ok := a.waiters[rpcID]
	if ok {
		delete(a.waiters, rpcID)
	}
	a.mu.Unlock()
	if !ok {
		log.Printf("codex: response for unknown id %d (ignored)", rpcID)
		return
	}
	ch <- codexDecisionAllows(result, errRaw)
}

// codexDecisionAllows interprets the app's approval answer. The normal
// shape is {"decision":"accept"|"acceptForSession"|"decline"|"cancel"}
// (internal/provider/codex/approval.go); permission grants use
// {scope, permissions}; MCP elicitation uses {action}; drains arrive
// as a JSON-RPC error with data.reason "turnTransition" — a deny.
func codexDecisionAllows(result, errRaw json.RawMessage) bool {
	if len(errRaw) > 0 && string(errRaw) != "null" {
		return false
	}
	var body struct {
		Decision string `json:"decision"`
		Action   string `json:"action"`
		Scope    string `json:"scope"`
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &body)
	}
	switch {
	case strings.HasPrefix(body.Decision, "accept"):
		return true
	case body.Decision != "":
		return false
	case body.Action == "accept":
		return true
	case body.Scope != "":
		return true
	}
	return false
}

// sendApproval emits a command-approval server request (Codex approvals
// are requests FROM the server carrying a JSON-RPC id; the app answers
// with a response we correlate by that id).
func (a *codexAdapter) sendApproval(step *scenario.ApprovalStep, vars scenario.Vars) (<-chan bool, func(), error) {
	rpcID := a.approvalSeq.Add(1)
	ch := make(chan bool, 1)
	a.mu.Lock()
	a.waiters[rpcID] = ch
	a.mu.Unlock()

	cancel := func() {
		a.mu.Lock()
		delete(a.waiters, rpcID)
		a.mu.Unlock()
	}

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      rpcID,
		"method":  "item/commandExecution/requestApproval",
		"params": map[string]any{
			"threadId": vars["THREAD_ID"],
			"turnId":   vars["TURN_ID"],
			"itemId":   fmt.Sprintf("approval-%d", rpcID),
			"command":  approvalCommandString(step, vars),
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("marshal approval request: %w", err)
	}
	a.w.writeLine(string(data), 0, 0)
	return ch, cancel, nil
}

// approvalCommandString derives the `command` param the app surfaces:
// Input's "command" field, a bare JSON string Input, or the tool name.
func approvalCommandString(step *scenario.ApprovalStep, vars scenario.Vars) string {
	if len(step.Input) > 0 {
		raw := []byte(vars.Substitute(string(step.Input)))
		var obj struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Command != "" {
			return obj.Command
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return step.ToolName
}

// codexInputText joins the text of a turn/start or turn/steer `input` vec in
// wire order (localImage entries contribute nothing), which is the text the
// app actually sent. An unrecognised shape reports empty rather than guessing.
func codexInputText(params json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(readParamRaw(params, "input"), &items) != nil {
		return ""
	}
	var parts []string
	for _, item := range items {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "")
}

func readParamString(params json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(readParamRaw(params, key), &s) != nil {
		return ""
	}
	return s
}

// readParamRaw returns one request parameter undecoded, for values whose shape
// is not a scalar.
func readParamRaw(params json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return nil
	}
	return m[key]
}
