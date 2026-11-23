package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

const acpApprovalIDBase = 2_000_000

// acpAdapter speaks ACP v1 JSON-RPC 2.0 over stdio for Antigravity (`agy acp`).
type acpAdapter struct {
	e *engine
	w *lineWriter

	mu             sync.Mutex
	waiters        map[int64]chan approvalAnswer
	approvalSeq    atomic.Int64
	sessionSeq     atomic.Int64
	configReported atomic.Bool
	mcpServers     map[string]mcpTarget
	currentModel   string
	currentThought string
	currentMode    string
	activePromptID json.RawMessage
	promptReplied  bool
}

func newACPAdapter(e *engine, w *lineWriter) *acpAdapter {
	a := &acpAdapter{
		e:              e,
		w:              w,
		waiters:        make(map[int64]chan approvalAnswer),
		mcpServers:     make(map[string]mcpTarget),
		currentModel:   "gemini-3.1-pro-high",
		currentThought: "high",
		currentMode:    "auto",
	}
	a.approvalSeq.Store(acpApprovalIDBase)
	return a
}

func (a *acpAdapter) readStdin() {
	forEachStdinLine(a.handleLine)
	log.Printf("acp: stdin closed; exiting")
	a.e.awaitExitDelay()
	a.e.terminate(0)
}

func (a *acpAdapter) handleLine(line []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("acp: malformed stdin line ignored: %v (line: %.200s)", err, line)
		return
	}
	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case hasID && msg.Method != "":
		a.handleRequest(msg.ID, msg.Method, msg.Params)
	case hasID:
		a.handleResponse(msg.ID, msg.Result, msg.Error)
	case msg.Method != "":
		a.handleNotification(msg.Method, msg.Params)
	}
}

func (a *acpAdapter) writeResult(id json.RawMessage, result any) {
	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
	if err != nil {
		log.Printf("acp: marshal result: %v", err)
		return
	}
	a.w.writeLine(string(payload), 0, 0)
}

func (a *acpAdapter) configOptionsLocked() []map[string]any {
	return []map[string]any{
		{
			"id":           "model",
			"name":         "Model",
			"category":     "model",
			"type":         "select",
			"currentValue": a.currentModel,
			"options": []map[string]any{
				{"value": "gemini-3.1-pro-high", "name": "Gemini 3.1 Pro (High)"},
				{"value": "gemini-3-flash", "name": "Gemini 3 Flash"},
			},
		},
		{
			"id":           "thought_level",
			"name":         "Thought Level",
			"category":     "thought_level",
			"type":         "select",
			"currentValue": a.currentThought,
			"options": []map[string]any{
				{"value": "low", "name": "Low"},
				{"value": "medium", "name": "Medium"},
				{"value": "high", "name": "High"},
				{"value": "xhigh", "name": "Extra High"},
			},
		},
	}
}

func (a *acpAdapter) modesBlockLocked() map[string]any {
	return map[string]any{
		"currentModeId": a.currentMode,
		"availableModes": []map[string]any{
			{"id": "ask", "name": "Ask"},
			{"id": "auto", "name": "Auto"},
			{"id": "plan", "name": "Plan"},
		},
	}
}

func (a *acpAdapter) handleRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "initialize":
		a.e.awaitStartupDelay()
		a.writeResult(id, map[string]any{
			"protocolVersion": 1,
			"agentInfo": map[string]any{
				"name":    "antigravity-cli-mock",
				"version": a.e.providerVersion(),
			},
			"agentCapabilities": map[string]any{
				"loadSession": true,
				"sessionCapabilities": map[string]any{
					"list":   map[string]any{},
					"resume": map[string]any{},
					"close":  map[string]any{},
				},
			},
		})
	case "session/new":
		a.noteSessionOpen(params, false)
		vars := a.e.currentVars()
		sessionID := vars["SESSION_ID"]
		if seq := a.sessionSeq.Add(1); seq > 1 {
			sessionID = fmt.Sprintf("%s-%d", sessionID, seq)
		}
		a.mu.Lock()
		res := map[string]any{
			"sessionId":     sessionID,
			"configOptions": a.configOptionsLocked(),
			"modes":         a.modesBlockLocked(),
		}
		a.mu.Unlock()
		a.writeResult(id, res)
	case "session/resume", "session/load":
		var req struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &req)
		a.noteSessionOpen(params, true)
		if req.SessionID != "" {
			a.e.setThreadID(req.SessionID)
		}
		a.mu.Lock()
		res := map[string]any{
			"sessionId":     req.SessionID,
			"configOptions": a.configOptionsLocked(),
			"modes":         a.modesBlockLocked(),
		}
		a.mu.Unlock()
		a.writeResult(id, res)
	case "session/list":
		a.writeResult(id, map[string]any{"sessions": []any{}})
	case "session/close":
		a.writeResult(id, map[string]any{})
	case "session/set_config_option":
		var req struct {
			ConfigID string `json:"configId"`
			Value    string `json:"value"`
		}
		_ = json.Unmarshal(params, &req)
		a.mu.Lock()
		switch req.ConfigID {
		case "model":
			a.currentModel = req.Value
		case "thought_level":
			a.currentThought = req.Value
		}
		opts := a.configOptionsLocked()
		a.mu.Unlock()
		a.writeResult(id, map[string]any{"configOptions": opts})
	case "session/set_mode":
		var req struct {
			ModeID string `json:"modeId"`
		}
		_ = json.Unmarshal(params, &req)
		a.mu.Lock()
		if req.ModeID != "" {
			a.currentMode = req.ModeID
		}
		a.mu.Unlock()
		a.writeResult(id, map[string]any{})
	case "session/prompt":
		var req struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(params, &req)
		var textParts []string
		for _, block := range req.Prompt {
			if block.Type == "text" && block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		}
		n, _ := a.e.beginTurn()
		doneCh := a.e.turnDoneChan(n)
		extra := scenario.Vars{"USER_INPUT": strings.Join(textParts, "\n")}
		if req.SessionID != "" {
			extra["SESSION_ID"] = req.SessionID
		}
		a.e.setTurnVars(n, extra)
		a.mu.Lock()
		a.activePromptID = append(json.RawMessage(nil), id...)
		a.promptReplied = false
		a.mu.Unlock()
		a.e.enqueueTurn(n)
		go func(promptID json.RawMessage) {
			<-doneCh
			a.mu.Lock()
			already := a.promptReplied
			a.promptReplied = true
			a.mu.Unlock()
			if !already {
				a.writeResult(promptID, map[string]any{"stopReason": "end_turn"})
			}
		}(append(json.RawMessage(nil), id...))
	default:
		a.writeResult(id, map[string]any{})
	}
}

func (a *acpAdapter) handleNotification(method string, _ json.RawMessage) {
	if method == "session/cancel" {
		a.e.interruptTurn("")
	}
}

func (a *acpAdapter) handleResponse(id json.RawMessage, result json.RawMessage, errRaw json.RawMessage) {
	numID, err := strconv.ParseInt(strings.TrimSpace(string(id)), 10, 64)
	if err != nil {
		return
	}
	a.mu.Lock()
	ch := a.waiters[numID]
	delete(a.waiters, numID)
	a.mu.Unlock()
	if ch == nil {
		return
	}
	ans := approvalAnswer{}
	if len(errRaw) == 0 || string(errRaw) == "null" {
		var res struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if json.Unmarshal(result, &res) == nil {
			opt := strings.ToLower(res.Outcome.OptionID)
			ans.allow = res.Outcome.Outcome == "selected" && !strings.Contains(opt, "reject") && !strings.Contains(opt, "deny")
		}
	}
	ch <- ans
}

func (a *acpAdapter) noteSessionOpen(params json.RawMessage, _ bool) {
	var req struct {
		MCPServers []struct {
			Name    string `json:"name"`
			URL     string `json:"url"`
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"mcpServers"`
		Meta struct {
			Antigravity struct {
				SystemPrompt struct {
					Append string `json:"append"`
				} `json:"systemPrompt"`
			} `json:"antigravity"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &req) != nil {
		return
	}
	names := make([]string, 0, len(req.MCPServers))
	a.mu.Lock()
	for _, s := range req.MCPServers {
		names = append(names, s.Name)
		if s.URL != "" {
			headers := make(map[string]string, len(s.Headers))
			for _, h := range s.Headers {
				headers[h.Name] = h.Value
			}
			a.mcpServers[s.Name] = mcpTarget{url: s.URL, headers: headers}
		}
	}
	a.mu.Unlock()
	sort.Strings(names)
	if a.configReported.CompareAndSwap(false, true) {
		a.e.rep.report(control.Report{
			Kind: control.ReportSessionConfig,
			SessionConfig: &control.SessionConfig{
				MCPServers:         names,
				AppendSystemPrompt: req.Meta.Antigravity.SystemPrompt.Append,
			},
		})
	}
}

func (a *acpAdapter) sendApproval(step *scenario.ApprovalStep, vars scenario.Vars) (<-chan approvalAnswer, func(bool), error) {
	rpcID := a.approvalSeq.Add(1)
	ch := make(chan approvalAnswer, 1)
	a.mu.Lock()
	a.waiters[rpcID] = ch
	a.mu.Unlock()
	sessionID := vars["SESSION_ID"]
	toolName := step.ToolName
	if toolName == "" {
		toolName = "run_command"
	}
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      rpcID,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": sessionID,
			"toolCall": map[string]any{
				"toolCallId": fmt.Sprintf("perm-%d", rpcID),
				"title":      toolName,
				"rawInput":   step.Input,
			},
			"options": []map[string]any{
				{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
			},
		},
	})
	a.w.writeLine(string(req), 0, 0)
	cancel := func(_ bool) {
		a.mu.Lock()
		delete(a.waiters, rpcID)
		a.mu.Unlock()
	}
	return ch, cancel, nil
}

func (a *acpAdapter) sendInterruptedTurn(_ scenario.Vars) {
	a.mu.Lock()
	promptID := a.activePromptID
	already := a.promptReplied
	a.promptReplied = true
	a.mu.Unlock()
	if !already && len(promptID) > 0 {
		a.writeResult(promptID, map[string]any{"stopReason": "cancelled"})
	}
}

func (a *acpAdapter) mcpTarget(name string) (mcpTarget, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.mcpServers[name]
	return t, ok
}

func (a *acpAdapter) writeMcpToolUse(vars scenario.Vars, call mcpCall) {
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": vars["SESSION_ID"],
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    call.toolUseID,
				"title":         call.server + "__" + call.tool,
				"kind":          "other",
				"status":        "in_progress",
				"rawInput":      json.RawMessage(call.args),
			},
		},
	})
	a.w.writeLine(string(line), 0, 0)
}

func (a *acpAdapter) writeMcpToolResult(vars scenario.Vars, call mcpCall) {
	status := "completed"
	if call.isError {
		status = "failed"
	}
	line, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": vars["SESSION_ID"],
			"update": map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    call.toolUseID,
				"status":        status,
				"rawOutput":     map[string]any{"text": call.text},
			},
		},
	})
	a.w.writeLine(string(line), 0, 0)
}
