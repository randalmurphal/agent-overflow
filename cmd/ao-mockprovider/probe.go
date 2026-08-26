package main

import (
	"encoding/json"
	"fmt"
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

	w.writeLine(fmt.Sprintf(
		`{"type":"system","subtype":"init","session_id":"mock-probe","model":"claude-opus-4-7","cwd":%s,"tools":[],"claude_code_version":"2.99.0","account":%s}`,
		mustJSON(cwd), claudeAccountJSON), 0, 0)

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
