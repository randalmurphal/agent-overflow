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
	// queue backs the provider-owned user-message queue (`thread/queue/*`,
	// codex >= 0.148) that `list` and `delete` answer over. Guarded by mu.
	// Nothing fills it — `add` is a tripwire. See codex_queue.go.
	queue []codexQueueEntry
	// steerSeq numbers the `userMessage` echo each `turn/steer` produces, so
	// several steers inside one turn never share an item id.
	steerSeq atomic.Int64
	// historyMode is the thread's persisted history contract
	// ("legacy" / "paginated"), decided at thread/start and merely echoed
	// on thread/resume. It gates `thread/revert`. See codex_revert.go.
	historyMode string
	// resumedThread records that this connection joined an EXISTING
	// thread, so turns it never ran are history rather than nonsense.
	// See codex_revert.go#anchorIsCuttable.
	resumedThread bool
}

// mockCodexModelList is the default `model/list` answer. It is not optional
// detail: AO treats the app-server's live catalog as REPLACING its shipped one
// and a miss as authoritative (`CodexLiveModelCatalog`), so an empty answer
// means "this account has no codex models at all" — after which every
// explicit reasoning effort on a codex thread is refused and the model picker
// is blank. The list names the harness seed's own model
// (`app_harness_seed.go`) plus one current catalog slug, each with the full
// effort menu and the fast tier, so a harness session looks like an account
// that can actually run.
const mockCodexModelList = `{"data":[` +
	`{"model":"gpt-5.2-codex","displayName":"GPT 5.2 Codex (mock)","defaultReasoningEffort":"medium",` +
	`"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"},` +
	`{"reasoningEffort":"high"},{"reasoningEffort":"xhigh"}],` +
	`"serviceTiers":[{"id":"priority","name":"Fast","description":"mock fast tier"}]},` +
	`{"model":"gpt-5.6-sol","displayName":"GPT 5.6 Sol (mock)","defaultReasoningEffort":"low",` +
	`"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"},` +
	`{"reasoningEffort":"high"},{"reasoningEffort":"xhigh"}],` +
	`"serviceTiers":[{"id":"priority","name":"Fast","description":"mock fast tier"}]}],` +
	`"nextCursor":null}`

func newCodexAdapter(e *engine, w *lineWriter, opts *scenario.CodexOptions) *codexAdapter {
	// Built-in defaults cover the standard handshake; scenarios override
	// only what they need. Templates are full response lines with
	// ${REQUEST_ID} substituted verbatim (number or string id).
	responses := map[string]string{
		// The userAgent is what the app parses its VERSION off
		// (`app_server_version.go`), and several method gates fail closed
		// without it — thread/queue, thread/revert, thread-scoped usage, and
		// the >= 0.149 approval-policy remap. The mock answers `--version`
		// with 99.0.0 and says the same here, so a harness session behaves
		// like the newest supported app-server rather than an unknown one.
		// A scenario's `providerVersion` pins it LOWER, which is the only
		// way a spec can drive the fails-closed side of one of those gates
		// — the mock otherwise answers above every gate there is.
		"initialize": `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"userAgent":"codex_cli_rs/` + e.providerVersion() + ` (mock; ao-mockprovider)"}}`,
		// ${HISTORY_MODE} is bound by respond(), not by a scenario: the
		// thread's history contract is decided by the thread/start params
		// (codex_revert.go), and it is what makes `thread/revert` available
		// or refused.
		"thread/start":   `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"thread":{"id":"${THREAD_ID}","historyMode":"${HISTORY_MODE}"}}}`,
		"thread/resume":  `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"thread":{"id":"${THREAD_ID}","historyMode":"${HISTORY_MODE}"}}}`,
		"turn/start":     `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"turn":{"id":"${TURN_ID}"}}}`,
		"turn/interrupt": `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{}}`,
		"model/list":     `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":` + mockCodexModelList + `}`,
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
	case "initialize":
		// The handshake response is what proves the app-server is up, so
		// it is where a scenario's startupDelayMs is spent.
		a.e.awaitStartupDelay()
		a.respond(id, method, a.e.currentVars())
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
				MCPServers:     codexMCPServerNames(params),
			},
		})
		// The history contract is settled here and nowhere else — a resume
		// can only report what the thread already is.
		a.noteThreadStart(params)
		a.respond(id, method, a.e.currentVars())
	case "thread/resume":
		// Echo the requested thread id and rebind ${THREAD_ID} to it.
		if tid := readParamString(params, "threadId"); tid != "" {
			a.e.setThreadID(tid)
		}
		a.noteThreadResume(a.e.currentVars()["THREAD_ID"])
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
		input := codexInputText(params)
		// Bind the turn's own text AND the caller's correlation handle so a
		// scenario can echo the `userMessage` item a real app-server emits for
		// every turn. Without the echo an app's pending-send correlation never
		// resolves. Without the `clientId` on it, an app that registered the
		// send BY client id (which AO does for every codex turn) cannot
		// consume the entry at all — the echo is provably not that entry's, so
		// it skips it and the message persists as injected provider context.
		a.e.setTurnVars(n, scenario.Vars{
			"USER_INPUT": input,
			"CLIENT_ID":  readParamString(params, "clientUserMessageId"),
		})
		a.e.rep.report(control.Report{
			Kind: control.ReportUserInput, Turn: n,
			Input: input, SessionRef: vars["THREAD_ID"],
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
		a.emitSteeredUserMessage(params, vars)
	case "thread/fork":
		a.forkThread(id, params)
	case "thread/revert":
		a.revertThread(id, params)
	case "turn/interrupt":
		// The real app-server replies when TurnAborted arrives, immediately
		// before publishing turn/completed{status:interrupted}. Preserve that
		// observable order while the engine owns the shared abort behavior.
		a.respond(id, method, a.e.currentVars())
		a.e.interruptTurn(readParamString(params, "turnId"))
	default:
		if a.handleQueueRequest(id, method, params) {
			return
		}
		// A scenario's own response template is the most specific
		// statement there is and wins over everything below.
		if _, scripted := a.responses[method]; scripted {
			a.respond(id, method, a.e.currentVars())
			return
		}
		if a.handleReadRequest(id, method) {
			return
		}
		// -32601, not `{"result":{}}`. An empty success is a LIE that no
		// real app-server tells, and it defeated the app's own fallback
		// machinery: codex.IsMethodUnsupported keys on this code, so under
		// the old default it could never fire and every optional surface
		// (thread compaction, review, config writes, MCP OAuth, terminal
		// termination) reported success against a server that had done
		// nothing. The honest answer exercises those fallback branches.
		log.Printf("codex: request method %q is not implemented by this mock; answering -32601", method)
		a.writeRPCError(id, -32601, fmt.Sprintf("Method not found: %s", method))
	}
}

// handleReadRequest answers the READ-shaped methods the app calls as a
// matter of course — the ones a default harness session needs answered
// for the UI to render at all. Each answer is minimal but REAL: it
// decodes cleanly through the app's own decoder for that method, and an
// empty collection is a truthful answer rather than a placeholder.
//
// What is deliberately NOT here stays -32601: the optional and newer
// surfaces (thread/compact/start, review/start, config/batchWrite,
// config/mcpServer/reload, mcpServer/oauth/login, the background
// terminal terminate/clean pair). "This build does not have it" is a
// real thing for an app-server to say, and those are exactly the
// branches worth exercising in a harness.
//
// Returns false when the method is not one of them.
func (a *codexAdapter) handleReadRequest(id json.RawMessage, method string) bool {
	var result string
	switch method {
	case "account/read":
		// provider.AccountInfo comes off `account.{type,email,planType}`;
		// a response with no `account` decodes to a zero AccountInfo,
		// which is how "signed out" would read.
		result = `{"account":{"type":"chatgpt","email":"mock@agent-overflow.test","planType":"pro"}}`
	case "account/usage/read":
		// `summary` + `dailyUsageBuckets` are the two keys the decoder
		// reads; every summary field is a pointer, so an empty summary is
		// a legal "this build reports no lifetime numbers".
		result = `{"summary":{},"dailyUsageBuckets":[]}`
	case "thread/read":
		// `thread.status.type` is REQUIRED by decodeProbeResponse — it
		// errors on an absent one — so the reconcile probe needs a real
		// status here or every reopen reports a broken session.
		// agentNickname/agentRole stay absent: those name a COLLAB CHILD
		// thread, and claiming them for the root thread would make every
		// harness thread look like someone's subagent.
		result = `{"thread":{"id":"${THREAD_ID}","status":{"type":"idle","activeFlags":[]}}}`
	case "thread/turns/list":
		// Turn shells, and the mock keeps no rollout to list them from.
		// An empty page with a null cursor is the honest answer and the
		// one the revert-boundary probe handles as "no durable turns".
		result = `{"data":[],"nextCursor":null}`
	case "thread/settings/update":
		// Upstream answers a bare result; the app only checks for an error.
		result = `{}`
	case "skills/list":
		// One entry per requested cwd would be more faithful, but the
		// params carry the cwd list and an EMPTY data array is already a
		// complete answer ("no skills visible"), which is what a workspace
		// with no SKILL.md files really returns.
		result = `{"data":[]}`
	case "config/read":
		// `config.review_model` is the only key any caller reads, and an
		// absent one means "fall back to the turn's model" — a real
		// configuration, not a gap.
		result = `{"config":{}}`
	case "mcpServerStatus/list":
		result = `{"data":[],"nextCursor":null}`
	case "thread/backgroundTerminals/list":
		// The mock spawns no PTYs, so nothing is running. A null cursor
		// terminates the app's paging walk on the first page.
		result = `{"data":[],"nextCursor":null}`
	default:
		return false
	}
	vars := a.e.currentVars()
	sub := make(scenario.Vars, len(vars)+1)
	for k, v := range vars {
		sub[k] = v
	}
	sub["REQUEST_ID"] = string(id)
	a.w.writeLine(sub.Substitute(`{"jsonrpc":"2.0","id":${REQUEST_ID},"result":`+result+`}`), 0, 0)
	return true
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
	anchor := readParamString(params, "lastTurnId")
	// Reported before the answer, like thread/revert's, and before the
	// scripted-response branch: the two cuts are alternatives chosen by a
	// version + history-mode gate, so a test asserting the choice must be
	// able to see the one that WAS taken.
	a.e.rep.report(control.Report{
		Kind:       control.ReportHistoryCut,
		Detail:     "thread/fork",
		Input:      anchor,
		SessionRef: a.e.currentVars()["THREAD_ID"],
	})
	if _, scripted := a.responses["thread/fork"]; scripted {
		a.respond(id, "thread/fork", a.e.currentVars())
		return
	}
	turnIDs, ok := a.e.forkedTurnIDs(anchor)
	if !ok {
		if !a.anchorIsCuttable(anchor) {
			a.writeRPCError(id, -32602, fmt.Sprintf(
				"thread/fork: lastTurnId %q is unknown or names the in-progress turn", anchor))
			return
		}
		// An anchor from before this process resumed the thread. A real
		// app-server reads those turns out of the rollout; the mock has
		// none, so it answers with the only turn it can name — the anchor
		// itself, as the last surviving turn, which is the one property
		// ForkAt validates.
		turnIDs = []string{anchor}
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
	sub["HISTORY_MODE"] = a.currentHistoryMode()
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

// emitSteeredUserMessage writes the `userMessage` item a real app-server emits
// when a steer lands in the running turn.
//
// The ADAPTER owns this one, not a scenario step, for the same reason the
// queue's dispatched echo used to be adapter-owned: the text and the
// `clientId` exist only at steer time, so no scenario file could name them. It
// is also the half of the steer contract the app cannot work without — a steer
// registers its pending send BY client id, so an echo that never arrives (or
// arrives without the id) leaves the message rendering as injected provider
// context instead of the user's own.
//
// The item id counts steers rather than reusing the turn's, because several
// steers can land in one turn and two items sharing an id is a shape no
// app-server produces.
func (a *codexAdapter) emitSteeredUserMessage(params json.RawMessage, vars scenario.Vars) {
	item := map[string]any{
		"type":    "userMessage",
		"id":      fmt.Sprintf("umsg-steer-%d", a.steerSeq.Add(1)),
		"status":  "completed",
		"content": []any{map[string]any{"type": "text", "text": codexInputText(params)}},
	}
	if clientID := readParamString(params, "clientUserMessageId"); clientID != "" {
		item["clientId"] = clientID
	}
	a.w.writeLine(mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "item/completed",
		"params": map[string]any{
			"threadId": vars["THREAD_ID"],
			"turnId":   vars["TURN_ID"],
			"item":     item,
		},
	}), 0, 0)
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

func codexMCPServerNames(params json.RawMessage) []string {
	var config map[string]json.RawMessage
	if json.Unmarshal(readParamRaw(params, "config"), &config) != nil {
		return nil
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(config["mcp_servers"], &servers) != nil {
		return nil
	}
	return sortedMCPServerNames(servers)
}
