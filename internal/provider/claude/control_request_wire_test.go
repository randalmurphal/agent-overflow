package claude

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestControlRequestWireKeys pins the exact JSON keys of every
// control_request AO sends to the Claude CLI.
//
// This test exists because a mis-spelled key is INVISIBLE at runtime:
// the CLI destructures the fields it wants off `request` and never
// validates the object, so an unknown key is dropped and the field it
// should have filled reads `undefined`. `mcp_authenticate` shipped with
// `server_name` where the CLI reads `serverName` and answered
// `Server not found: undefined` for every server — for months, with the
// round-trip, the error path, and the status projection all working
// correctly around it. `mcp_oauth_callback_url` carried the same defect
// in both of its keys.
//
// The spellings below are read off the 2.1.237 binary's own handlers
// (2026-08-21), e.g.:
//
//	subtype==="mcp_authenticate"){let{serverName:Ct,redirectUri:Ut}=...
//	subtype==="mcp_oauth_callback_url"){let{serverName:Ct,callbackUrl:Ut}=...
//	subtype==="mcp_toggle"){...,{serverName:Ut,enabled:zr}=...
//	subtype==="stop_task"){let{task_id:Ct}=...
//
// The CLI mixes camelCase and snake_case per handler with no rule, so
// "match the neighbouring request" is not a safe way to add a new one.
// Re-derive from the installed binary and add a row here.
func TestControlRequestWireKeys(t *testing.T) {
	cases := []struct {
		name     string
		subtype  string
		wantKeys []string
		invoke   func(ctx context.Context, s *Session) error
	}{
		{
			name:     "mcp_toggle",
			subtype:  "mcp_toggle",
			wantKeys: []string{"enabled", "serverName", "subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				return s.ToggleMCPServer(ctx, "plugin:playwright:playwright", false)
			},
		},
		{
			name:     "mcp_reconnect",
			subtype:  "mcp_reconnect",
			wantKeys: []string{"serverName", "subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				return s.ReconnectMCPServer(ctx, "plugin:playwright:playwright")
			},
		},
		{
			name:     "mcp_authenticate",
			subtype:  "mcp_authenticate",
			wantKeys: []string{"serverName", "subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				_, err := s.AuthenticateMCP(ctx, "plugin:atlassian:atlassian")
				return err
			},
		},
		{
			name:     "mcp_oauth_callback_url",
			subtype:  "mcp_oauth_callback_url",
			wantKeys: []string{"callbackUrl", "serverName", "subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				return s.CompleteMCPOAuth(ctx, "plugin:atlassian:atlassian", "http://localhost:1/cb?code=c&state=s")
			},
		},
		{
			name:     "mcp_status",
			subtype:  "mcp_status",
			wantKeys: []string{"subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				_, err := s.QueryMCPStatus(ctx)
				return err
			},
		},
		{
			name:     "interrupt",
			subtype:  "interrupt",
			wantKeys: []string{"subtype"},
			invoke:   func(ctx context.Context, s *Session) error { return s.Interrupt(ctx) },
		},
		{
			name:     "stop_task",
			subtype:  "stop_task",
			wantKeys: []string{"subtype", "task_id"},
			invoke:   func(ctx context.Context, s *Session) error { return s.StopTask(ctx, "task-1") },
		},
		{
			name:     "set_permission_mode",
			subtype:  "set_permission_mode",
			wantKeys: []string{"mode", "subtype"},
			invoke:   func(ctx context.Context, s *Session) error { return s.setPermissionMode(ctx, "plan") },
		},
		{
			name:     "set_model",
			subtype:  "set_model",
			wantKeys: []string{"model", "subtype"},
			invoke:   func(ctx context.Context, s *Session) error { return s.setModel(ctx, "opus", "") },
		},
		{
			name:     "set_model with system prompt",
			subtype:  "set_model",
			wantKeys: []string{"model", "subtype", "system_prompt"},
			invoke:   func(ctx context.Context, s *Session) error { return s.setModel(ctx, "opus", "be terse") },
		},
		{
			name:     "set_max_thinking_tokens",
			subtype:  "set_max_thinking_tokens",
			wantKeys: []string{"max_thinking_tokens", "subtype", "thinking_display"},
			invoke: func(ctx context.Context, s *Session) error {
				return s.setMaxThinkingTokens(ctx, ThinkingUpdate{
					Apply: true, SendBudget: true, Budget: 2048, Display: "summarized",
				})
			},
		},
		{
			name:     "get_settings",
			subtype:  "get_settings",
			wantKeys: []string{"subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				_, err := s.GetSettings(ctx)
				return err
			},
		},
		{
			name:     "get_context_usage",
			subtype:  "get_context_usage",
			wantKeys: []string{"subtype"},
			invoke: func(ctx context.Context, s *Session) error {
				_, err := s.GetContextUsage(ctx)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, capturePath := newControlWireSession(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// The ack carries no payload, so decoding a typed response
			// may fail; only the OUTBOUND shape is under test here.
			_ = tc.invoke(ctx, s)

			req := captureControlRequest(t, capturePath, tc.subtype)
			got := make([]string, 0, len(req))
			for k := range req {
				got = append(got, k)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.wantKeys...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("control_request %q keys = %v, want %v", tc.subtype, got, want)
			}
		})
	}
}

// TestControlRequestEnvelopeShape pins the envelope every
// control_request rides in. The CLI matches responses by `request_id`
// at the TOP level, not inside `request`.
func TestControlRequestEnvelopeShape(t *testing.T) {
	s, capturePath := newControlWireSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.QueryMCPStatus(ctx); err != nil {
		t.Fatalf("QueryMCPStatus: %v", err)
	}

	line := captureControlRequestLine(t, capturePath, "mcp_status")
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	got := make([]string, 0, len(envelope))
	for k := range envelope {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := "request,request_id,type"; strings.Join(got, ",") != want {
		t.Errorf("envelope keys = %v, want %s", got, want)
	}
	var typ string
	if err := json.Unmarshal(envelope["type"], &typ); err != nil || typ != "control_request" {
		t.Errorf("envelope type = %q (err %v), want control_request", typ, err)
	}
}

// captureControlRequest returns the decoded `request` object of the
// first captured control_request carrying subtype.
func captureControlRequest(t *testing.T, capturePath, subtype string) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Request map[string]json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(captureControlRequestLine(t, capturePath, subtype), &envelope); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	return envelope.Request
}

// captureControlRequestLine finds the raw captured line whose
// request.subtype matches. A missing line fails loudly: a method that
// never reached the wire must not pass this test by default.
func captureControlRequestLine(t *testing.T, capturePath, subtype string) []byte {
	t.Helper()
	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read capture %s: %v", capturePath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			continue
		}
		if probe.Type == "control_request" && probe.Request.Subtype == subtype {
			return []byte(line)
		}
	}
	t.Fatalf("no control_request with subtype %q in capture:\n%s", subtype, data)
	return nil
}

// controlWireResponderScript acks EVERY control_request with a bare
// success and appends every received line to capturePath. Deliberately
// subtype-agnostic so a newly wired control_request is captured without
// touching the script.
func controlWireResponderScript(capturePath string) string {
	return `#!/bin/sh
set -u
while IFS= read -r line; do
    printf '%s\n' "$line" >> '` + capturePath + `'
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s"}}\n' "$reqid"
            ;;
    esac
done
`
}

func newControlWireSession(t *testing.T) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	capturePath := dir + "/capture.ndjson"
	scriptPath := dir + "/fake-claude"
	if err := os.WriteFile(scriptPath, []byte(controlWireResponderScript(capturePath)), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: scriptPath})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s := &Session{
		proc:                  proc,
		threadID:              "thread-control-wire",
		onEvent:               func(evt provider.ProviderEvent) { _ = evt },
		cancel:                cancel,
		readDone:              make(chan struct{}),
		controlRequestTimeout: 3 * time.Second,
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	return s, capturePath
}
