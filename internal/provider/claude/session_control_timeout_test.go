package claude

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// set_model can take up to 15 s to confirm (Claude 2.1.307), so it gets a
// longer silence budget than every other control request.
func TestControlRequestTimeoutFor(t *testing.T) {
	production := &Session{}
	if got := production.controlRequestTimeoutFor("set_model"); got != SetModelControlRequestTimeout {
		t.Fatalf("set_model budget = %s, want %s", got, SetModelControlRequestTimeout)
	}
	for _, subtype := range []string{"interrupt", "stop_task", "set_permission_mode", "set_max_thinking_tokens", "mcp_status", "get_settings", ""} {
		if got := production.controlRequestTimeoutFor(subtype); got != DefaultControlRequestTimeout {
			t.Fatalf("%q budget = %s, want the default %s", subtype, got, DefaultControlRequestTimeout)
		}
	}
	overridden := &Session{controlRequestTimeout: 300 * time.Millisecond}
	for _, subtype := range []string{"set_model", "interrupt"} {
		if got := overridden.controlRequestTimeoutFor(subtype); got != 300*time.Millisecond {
			t.Fatalf("%q budget with an override = %s, want the override", subtype, got)
		}
	}
}

// slowAckScript answers every control request after 11 s of
// silence. It announces itself before it reads.
const slowAckScript = `#!/bin/sh
set -u
printf '{"type":"tool_progress"}\n'
while IFS= read -r line; do
    reqid=${line#*'"request_id":"'}
    reqid=${reqid%%'"'*}
    sleep 11
    printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
done
`

func newSlowAckSession(t *testing.T) *Session {
	t.Helper()
	path := t.TempDir() + "/fake-claude"
	mockexec.Write(t, path, slowAckScript)
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, provider.SpawnConfig{Binary: path})
	if err != nil {
		cancel()
		t.Fatalf("spawn: %v", err)
	}
	ready := make(chan struct{})
	s := &Session{
		proc:     proc,
		threadID: testThread,
		parser:   NewParser(),
		onEvent: func(evt provider.ProviderEvent) {
			if evt.Kind == provider.EventSessionStatus && evt.Content == "tool_progress" {
				close(ready)
			}
		},
		cancel:   cancel,
		readDone: make(chan struct{}),
	}
	go s.readLoop()
	t.Cleanup(func() { _ = s.Close() })
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the fake CLI did not start")
	}
	return s
}

// With production budgets, a set_model acked after 11 s of silence lands,
// and any other request acked after the same silence times out at the
// default. Both run in parallel to keep the suite's wall time near 11 s.
func TestSetModelOutlastsTheDefaultControlRequestTimeout(t *testing.T) {
	t.Parallel()
	s := newSlowAckSession(t)
	if err := s.setModel(context.Background(), "claude-opus-4-7"); err != nil {
		t.Fatalf("setModel after 11 s of silence: %v", err)
	}
}

func TestOtherControlRequestsKeepTheDefaultTimeout(t *testing.T) {
	t.Parallel()
	s := newSlowAckSession(t)
	start := time.Now()
	err := s.setPermissionMode(context.Background(), "plan")
	if err == nil || !strings.Contains(err.Error(), "no progress from the Claude CLI for "+DefaultControlRequestTimeout.String()) {
		t.Fatalf("setPermissionMode = %v, want a timeout after %s", err, DefaultControlRequestTimeout)
	}
	if elapsed := time.Since(start); elapsed > DefaultControlRequestTimeout+3*time.Second {
		t.Fatalf("timed out after %s, want near %s", elapsed, DefaultControlRequestTimeout)
	}
}
