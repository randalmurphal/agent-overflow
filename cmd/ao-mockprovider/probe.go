package main

import (
	"encoding/json"
	"log"
	"os"
)

// runClaudeProbe serves the app's zero-token account probe
// (internal/provider/claude/probe.go): emit a plausible system/init
// line, success-ack every control_request (the probe's initialize ack
// carries the account object), and exit 0 when stdin closes. Probes
// are side invocations — no scenario, no control-channel registration.
func runClaudeProbe(cwd string) {
	log.Printf("claude account-probe invocation detected (--max-turns 0)")
	w := newLineWriter(os.Stdout)

	w.writeLine(mustJSON(struct {
		Type              string        `json:"type"`
		Subtype           string        `json:"subtype"`
		SessionID         string        `json:"session_id"`
		Model             string        `json:"model"`
		Cwd               string        `json:"cwd"`
		Tools             []string      `json:"tools"`
		ClaudeCodeVersion string        `json:"claude_code_version"`
		Account           claudeAccount `json:"account"`
	}{
		Type: "system", Subtype: "init", SessionID: "mock-probe", Model: "claude-opus-4-7",
		Cwd: cwd, Tools: []string{}, ClaudeCodeVersion: "2.99.0", Account: claudeAccountForHome(),
	}), 0, 0)

	forEachStdinLine(func(line []byte) {
		var env struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			log.Printf("probe: malformed stdin line ignored: %v", err)
			return
		}
		if env.Type == "control_request" {
			writeClaudeControlAck(w, env.RequestID, claudeControlRequestSubtype(env.Request), env.Request)
		}
	})
	// EOF: the probe closes stdin to tear us down.
}
