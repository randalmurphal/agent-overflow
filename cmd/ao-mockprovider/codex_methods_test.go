package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider/codex"
)

// codexRPCFrame is the response envelope the app's JSON-RPC reader
// decodes, with the two members that matter here.
type codexRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// newCodexUnitAdapter builds the adapter over a buffer, with no scenario
// response overrides — the state a default harness Codex session runs in.
func newCodexUnitAdapter(t *testing.T, buf *bytes.Buffer, opts *scenario.CodexOptions) *codexAdapter {
	t.Helper()
	sc := &scenario.Scenario{Version: scenario.CurrentVersion, Name: "unit", Provider: scenario.ProviderCodex, Codex: opts}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(buf), &reporter{}, scenario.Vars{
		"THREAD_ID":  "th-unit",
		"SESSION_ID": "th-unit",
		"CWD":        "/ws",
	})
	e.exitFn = func(code int) { t.Fatalf("unexpected exit(%d)", code) }
	a := newCodexAdapter(e, newLineWriter(buf), opts)
	e.adapter = a
	return a
}

func codexRequest(t *testing.T, a *codexAdapter, buf *bytes.Buffer, method, params string) codexRPCFrame {
	t.Helper()
	buf.Reset()
	a.handleRequest(json.RawMessage("7"), method, json.RawMessage(params))
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatalf("method %q produced no answer at all; the app would hang on it", method)
	}
	var frame codexRPCFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("answer to %q is not decodable: %v (line %s)", method, err, line)
	}
	if frame.JSONRPC != "2.0" || string(frame.ID) != "7" {
		t.Fatalf("answer to %q has the wrong envelope: %s", method, line)
	}
	return frame
}

// TestCodexUnimplementedMethodIsMethodNotFound is the fix for a mock that
// answered `{"result":{}}` to anything it did not recognise.
//
// An empty success is a lie no real app-server tells, and it defeated the
// app's own fallback machinery: codex.IsMethodUnsupported keys on -32601,
// so under the old default it could never fire and every optional surface
// reported success against a server that had done nothing.
func TestCodexUnimplementedMethodIsMethodNotFound(t *testing.T) {
	// Each of these is a genuinely optional or newer surface. -32601 is
	// the HONEST answer for a mock that does not implement them, and it is
	// what exercises the app's fallback branch.
	optional := []string{
		"thread/compact/start",
		"review/start",
		"config/batchWrite",
		"config/mcpServer/reload",
		"mcpServer/oauth/login",
		"thread/backgroundTerminals/terminate",
		"thread/backgroundTerminals/clean",
	}
	var buf bytes.Buffer
	a := newCodexUnitAdapter(t, &buf, nil)
	for _, method := range optional {
		t.Run(method, func(t *testing.T) {
			frame := codexRequest(t, a, &buf, method, `{}`)
			if frame.Error == nil {
				t.Fatalf("%s answered a success (%s); IsMethodUnsupported can never fire", method, frame.Result)
			}
			if frame.Error.Code != -32601 {
				t.Fatalf("%s error code = %d, want -32601", method, frame.Error.Code)
			}
			err := &codex.RPCError{Method: method, Code: frame.Error.Code, Message: frame.Error.Message}
			if !codex.IsMethodUnsupported(err, method) {
				t.Fatalf("the app would not classify %q as unsupported: %v", method, err)
			}
		})
	}
}

// TestCodexReadMethodsAnswerDecodableShapes: the methods the app calls as
// a matter of course must still work, or -32601-by-default would break
// the DEFAULT harness experience rather than only the optional surfaces.
// Each answer is checked against the shape the app's own decoder needs.
func TestCodexReadMethodsAnswerDecodableShapes(t *testing.T) {
	var buf bytes.Buffer
	a := newCodexUnitAdapter(t, &buf, nil)

	t.Run("account/read", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "account/read", `{"refreshToken":false}`)
		var decoded struct {
			Account *struct {
				Type     string `json:"type"`
				Email    string `json:"email"`
				PlanType string `json:"planType"`
			} `json:"account"`
		}
		if frame.Error != nil {
			t.Fatalf("account/read = %+v", frame.Error)
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.Account == nil || decoded.Account.Email == "" || decoded.Account.PlanType == "" {
			t.Fatalf("account/read = %s; the app reads this as signed out", frame.Result)
		}
	})

	t.Run("account/usage/read", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "account/usage/read", `{}`)
		var decoded struct {
			Summary           map[string]json.RawMessage `json:"summary"`
			DailyUsageBuckets []json.RawMessage          `json:"dailyUsageBuckets"`
		}
		if frame.Error != nil {
			t.Fatalf("account/usage/read = %+v", frame.Error)
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.Summary == nil {
			t.Fatalf("account/usage/read = %s, want a summary object", frame.Result)
		}
	})

	t.Run("thread/read", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "thread/read", `{"threadId":"th-unit","includeTurns":false}`)
		if frame.Error != nil {
			t.Fatalf("thread/read = %+v", frame.Error)
		}
		var decoded struct {
			Thread struct {
				ID     string `json:"id"`
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// decodeProbeResponse ERRORS on an absent thread.status.type, so
		// without it every reopen reports a broken session.
		if decoded.Thread.Status.Type == "" {
			t.Fatalf("thread/read = %s, want a thread.status.type", frame.Result)
		}
		if decoded.Thread.ID != "th-unit" {
			t.Fatalf("thread/read echoed id %q, want the session's own thread id", decoded.Thread.ID)
		}
	})

	t.Run("thread/turns/list", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "thread/turns/list", `{"threadId":"th-unit","limit":100}`)
		if frame.Error != nil {
			t.Fatalf("thread/turns/list = %+v", frame.Error)
		}
		var decoded struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// An empty cursor is what terminates the app's paging walk; a
		// repeated or missing one would spin it to the page cap.
		if decoded.NextCursor != "" {
			t.Fatalf("thread/turns/list nextCursor = %q, want empty so the walk terminates", decoded.NextCursor)
		}
	})

	t.Run("thread/settings/update", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "thread/settings/update", `{"threadId":"th-unit"}`)
		if frame.Error != nil {
			t.Fatalf("thread/settings/update = %+v", frame.Error)
		}
	})

	t.Run("skills/list", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "skills/list", `{"cwds":["/ws"]}`)
		if frame.Error != nil {
			t.Fatalf("skills/list = %+v", frame.Error)
		}
		var decoded struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
	})

	t.Run("config/read", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "config/read", `{"cwd":"/ws","includeLayers":false}`)
		if frame.Error != nil {
			t.Fatalf("config/read = %+v", frame.Error)
		}
		var decoded struct {
			Config map[string]json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.Config == nil {
			t.Fatalf("config/read = %s, want a config object", frame.Result)
		}
	})

	t.Run("mcpServerStatus/list", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "mcpServerStatus/list", `{"detail":"toolsAndAuthOnly"}`)
		if frame.Error != nil {
			t.Fatalf("mcpServerStatus/list = %+v", frame.Error)
		}
		var decoded codex.MCPServerStatusList
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode into the app's own shape: %v", err)
		}
	})

	t.Run("thread/backgroundTerminals/list", func(t *testing.T) {
		frame := codexRequest(t, a, &buf, "thread/backgroundTerminals/list", `{"threadId":"th-unit"}`)
		if frame.Error != nil {
			t.Fatalf("thread/backgroundTerminals/list = %+v", frame.Error)
		}
		var decoded struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(frame.Result, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.NextCursor != nil {
			t.Fatalf("nextCursor = %v, want null so the paging walk stops", *decoded.NextCursor)
		}
	})
}

// TestCodexScenarioResponsesStillWin: a scenario's own response template
// is the most specific statement there is, so it outranks both the
// built-in read answers and the -32601 default.
func TestCodexScenarioResponsesStillWin(t *testing.T) {
	var buf bytes.Buffer
	a := newCodexUnitAdapter(t, &buf, &scenario.CodexOptions{Responses: map[string]string{
		"thread/read":  `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"thread":{"id":"scripted","status":{"type":"error"}}}}`,
		"review/start": `{"jsonrpc":"2.0","id":${REQUEST_ID},"result":{"review":{"id":"rev-1"}}}`,
	}})

	if frame := codexRequest(t, a, &buf, "thread/read", `{}`); !strings.Contains(string(frame.Result), "scripted") {
		t.Fatalf("scenario template lost to the built-in read answer: %s", frame.Result)
	}
	frame := codexRequest(t, a, &buf, "review/start", `{}`)
	if frame.Error != nil {
		t.Fatalf("scenario template lost to the -32601 default: %+v", frame.Error)
	}
}

// TestCodexInitializeCarriesTheScenarioProviderVersion: the userAgent is
// where the app parses the connected app-server's build from, and every
// per-method version gate fails CLOSED without it. A scenario pinning a
// version LOWER is the only way a spec can drive that closed side.
func TestCodexInitializeCarriesTheScenarioProviderVersion(t *testing.T) {
	var buf bytes.Buffer
	sc := &scenario.Scenario{
		Version:         scenario.CurrentVersion,
		Name:            "downgrade",
		Provider:        scenario.ProviderCodex,
		ProviderVersion: "0.147.0",
		Turns:           []scenario.Turn{{Steps: []scenario.Step{{DelayMs: 1}}}},
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("a pinned providerVersion must validate: %v", err)
	}
	e := newEngine(sc, t.TempDir(), t.TempDir(), newLineWriter(&buf), &reporter{}, scenario.Vars{"THREAD_ID": "th"})
	a := newCodexAdapter(e, newLineWriter(&buf), nil)
	e.adapter = a

	frame := codexRequest(t, a, &buf, "initialize", `{}`)
	if frame.Error != nil {
		t.Fatalf("initialize = %+v", frame.Error)
	}
	var decoded struct {
		UserAgent string `json:"userAgent"`
	}
	if err := json.Unmarshal(frame.Result, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(decoded.UserAgent, "codex_cli_rs/0.147.0") {
		t.Fatalf("userAgent = %q, want the scenario's pinned 0.147.0", decoded.UserAgent)
	}
}
