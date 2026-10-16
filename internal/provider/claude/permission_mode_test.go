package claude

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// permissionModeCLIScript is a fake CLI that reports permission-mode changes
// on system/status the way 2.1.300 does: an echo for every
// set_permission_mode (before or after the ack, per ECHO_ORDER), and, for a
// set_model while in auto mode, the auto kick-out before the ack.
const permissionModeCLIScript = `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
mode="${START_MODE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            ack=$(printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}' "$reqid")
            case "$line" in
                *'"subtype":"set_permission_mode"'*)
                    mode=$(printf '%s' "$line" | sed -n 's/.*"mode":"\([^"]*\)".*/\1/p')
                    status=$(printf '{"type":"system","subtype":"status","status":null,"permissionMode":"%s"}' "$mode")
                    if [ "${ECHO_ORDER:-before}" = before ]; then
                        printf '%s\n%s\n' "$status" "$ack"
                    else
                        printf '%s\n%s\n' "$ack" "$status"
                    fi
                    ;;
                *'"subtype":"set_model"'*)
                    if [ "$mode" = auto ]; then
                        mode=default
                        printf '{"type":"system","subtype":"status","status":null,"permissionMode":"default"}\n'
                    fi
                    printf '%s\n' "$ack"
                    ;;
                *)
                    printf '%s\n' "$ack"
                    ;;
            esac
            ;;
    esac
done
`

type permissionModeSession struct {
	s       *Session
	capture string
	events  chan provider.ProviderEvent
}

func newPermissionModeSession(t *testing.T, base, echoOrder string) permissionModeSession {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	mockexec.Write(t, scriptPath, permissionModeCLIScript)
	events := make(chan provider.ProviderEvent, 64)
	cfg := Config{
		Binary:             scriptPath,
		BasePermissionMode: base,
		Env: map[string]string{
			"CAPTURE_FILE": capturePath,
			"START_MODE":   base,
			"ECHO_ORDER":   echoOrder,
		},
	}
	s, err := NewSession(context.Background(), testThread, cfg, func(evt provider.ProviderEvent) {
		if evt.Kind == provider.EventPermissionModeChanged {
			events <- evt
		}
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.controlRequestTimeout = 2 * time.Second
	return permissionModeSession{s: s, capture: capturePath, events: events}
}

// noModeEvent proves no permission-mode event was forwarded for everything
// the fake CLI wrote before the sentinel's round trip.
func (p permissionModeSession) noModeEvent(t *testing.T) {
	t.Helper()
	capturedLinesBeforeSentinel(t, p.s, p.capture)
	// The sentinel is not a control request, so the CLI answers nothing;
	// a control round trip orders the read loop past every earlier frame.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.s.sendControlRequest(ctx, "order probe", map[string]any{"subtype": "mcp_status"}); err != nil {
		t.Fatalf("order probe: %v", err)
	}
	select {
	case evt := <-p.events:
		t.Fatalf("forwarded %s %s, want the echo of AO's own request dropped", evt.Kind, evt.Meta)
	default:
	}
}

func TestPermissionModeEchoOfAOsRequestIsNotForwarded(t *testing.T) {
	for _, order := range []string{"before", "after"} {
		t.Run("echo "+order+" the ack", func(t *testing.T) {
			p := newPermissionModeSession(t, "auto", order)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := p.s.SetInteractionMode(ctx, provider.ModePlan); err != nil {
				t.Fatalf("SetInteractionMode(plan): %v", err)
			}
			if err := p.s.SetInteractionMode(ctx, provider.ModeChat); err != nil {
				t.Fatalf("SetInteractionMode(chat): %v", err)
			}
			if _, err := p.s.ApplyLiveUpdate(ctx, LiveUpdate{BasePermissionMode: "acceptEdits"}, nil); err != nil {
				t.Fatalf("ApplyLiveUpdate: %v", err)
			}
			p.noModeEvent(t)
			if got := p.s.BasePermissionMode(); got != "acceptEdits" {
				t.Fatalf("BasePermissionMode = %q, want acceptEdits", got)
			}
		})
	}
}

// The CLI leaves auto mode when set_model lands on a model without it. The
// session adopts the mode it reports as current and base, forwards the
// change, and its next turn asks for nothing the CLI would refuse.
func TestPermissionModeTheCLIChangesOnItsOwnIsAdopted(t *testing.T) {
	p := newPermissionModeSession(t, "auto", "before")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-sonnet-4-6"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}
	select {
	case evt := <-p.events:
		var meta provider.PermissionModeChangeMeta
		if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.Mode != "default" {
			t.Fatalf("forwarded meta %s (%v), want mode default", evt.Meta, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the CLI's own mode change was never forwarded")
	}
	if got := p.s.BasePermissionMode(); got != "default" {
		t.Fatalf("BasePermissionMode = %q, want default", got)
	}
	if got := p.s.getCurrentPermissionMode(); got != "default" {
		t.Fatalf("current permission mode = %q, want default", got)
	}

	before := len(capturedLinesBeforeSentinel(t, p.s, p.capture))
	if err := p.s.Send(ctx, "next turn", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, line := range capturedLinesBeforeSentinel(t, p.s, p.capture)[before:] {
		if strings.Contains(line, `"set_permission_mode"`) {
			t.Fatalf("the next turn re-sent a permission mode: %s", line)
		}
	}
}

// "plan" is AO's per-turn interaction mode, and an unknown name is not a
// mode the session can run; neither is adopted or forwarded.
func TestPermissionModeReportsThatAreNotAdopted(t *testing.T) {
	s, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "auto"})
	for _, reported := range []string{"", "plan", "yolo", "auto"} {
		if s.adoptReportedPermissionMode(reported) {
			t.Fatalf("adoptReportedPermissionMode(%q) = true, want false", reported)
		}
	}
	if got := s.BasePermissionMode(); got != "auto" {
		t.Fatalf("BasePermissionMode = %q, want auto", got)
	}
}

func TestRuntimeModeForPermissionModeInvertsTheMapping(t *testing.T) {
	for _, runtime := range provider.AllRuntimeModes {
		got, ok := RuntimeModeForPermissionMode(claudeBasePermissionMode(runtime))
		if !ok || got != runtime {
			t.Fatalf("RuntimeModeForPermissionMode(%q) = (%q, %v), want %q", claudeBasePermissionMode(runtime), got, ok, runtime)
		}
	}
	for _, mode := range []string{"plan", "", "yolo"} {
		if got, ok := RuntimeModeForPermissionMode(mode); ok {
			t.Fatalf("RuntimeModeForPermissionMode(%q) = %q, want none", mode, got)
		}
	}
}

// system/status carries the permission mode beside, or instead of, the
// compaction report; both reach the session.
func TestParseStatusPermissionMode(t *testing.T) {
	events, err := NewParser().ParseLine(testThread, []byte(`{"type":"system","subtype":"status","status":null,"permissionMode":"default","uuid":"u","session_id":"s"}`))
	if err != nil || len(events) != 1 || events[0].Kind != provider.EventPermissionModeChanged {
		t.Fatalf("ParseLine = %+v, %v; want one permission mode event", events, err)
	}
	var meta provider.PermissionModeChangeMeta
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil || meta.Mode != "default" {
		t.Fatalf("meta = %s (%v), want mode default", events[0].Meta, err)
	}

	events, err = NewParser().ParseLine(testThread, []byte(`{"type":"system","subtype":"status","status":"compacting"}`))
	if err != nil || len(events) != 1 || events[0].Kind != provider.EventCompactionStatus {
		t.Fatalf("compacting = %+v, %v; want only the compaction event", events, err)
	}

	// init restates the startup resolution, which includes the user's own
	// defaultMode; it is not read as a change.
	events, err = NewParser().ParseLine(testThread, []byte(`{"type":"system","subtype":"init","session_id":"s","model":"m","permissionMode":"acceptEdits"}`))
	if err != nil {
		t.Fatalf("ParseLine(init): %v", err)
	}
	for _, evt := range events {
		if evt.Kind == provider.EventPermissionModeChanged {
			t.Fatalf("init produced a permission mode event: %s", evt.Meta)
		}
	}
}
