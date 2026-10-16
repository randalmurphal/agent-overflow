package claude

import (
	"context"
	"encoding/json"
	"os"
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
// set_model while in auto mode, the auto kick-out before the ack. After any
// set_model the model has no auto mode: set_permission_mode auto is refused
// with error_code auto_mode_model, and a set_model in plan changes nothing.
// With UNSOLICITED set, each user message is answered with a report of that
// mode, as leaving plan onto the user's own defaultMode does.
const permissionModeCLIScript = `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
mode="${START_MODE:?}"
noauto=0
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"user"'*)
            if [ -n "${UNSOLICITED:-}" ]; then
                mode="$UNSOLICITED"
                printf '{"type":"system","subtype":"status","status":null,"permissionMode":"%s"}\n' "$mode"
            fi
            ;;
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            ack=$(printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}' "$reqid")
            case "$line" in
                *'"mode":"auto"'*'"subtype":"set_permission_mode"'*)
                    if [ "$noauto" = 1 ]; then
                        printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"Cannot set permission mode to auto: auto mode unavailable for this model","error_code":"auto_mode_model"}}\n' "$reqid"
                        continue
                    fi
                    mode=auto
                    status='{"type":"system","subtype":"status","status":null,"permissionMode":"auto"}'
                    if [ "${ECHO_ORDER:-before}" = before ]; then
                        printf '%s\n%s\n' "$status" "$ack"
                    else
                        printf '%s\n%s\n' "$ack" "$status"
                    fi
                    ;;
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
                    noauto=1
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
	return newPermissionModeSessionReporting(t, base, echoOrder, "")
}

func newPermissionModeSessionReporting(t *testing.T, base, echoOrder, unsolicited string) permissionModeSession {
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
			"UNSOLICITED":  unsolicited,
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

// stdinSoFar is every line the session wrote to the fake CLI, through a
// sentinel written now. Unlike capturedLinesBeforeSentinel it can be called
// again: each call reads up to its own sentinel.
func (p permissionModeSession) stdinSoFar(t *testing.T) []string {
	t.Helper()
	read := func() []string {
		data, err := os.ReadFile(p.capture)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read capture file: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	sentinels := func(lines []string) int {
		n := 0
		for _, line := range lines {
			if line == testStdinSentinel {
				n++
			}
		}
		return n
	}
	want := sentinels(read()) + 1
	if err := p.s.proc.WriteLine([]byte(testStdinSentinel)); err != nil {
		t.Fatalf("write stdin sentinel: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		lines := read()
		if sentinels(lines) >= want {
			var out []string
			seen := 0
			for _, line := range lines {
				if line == testStdinSentinel {
					if seen++; seen == want {
						return out
					}
					continue
				}
				out = append(out, line)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdin sentinel %d never captured in %s", want, p.capture)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// noModeEvent proves no permission-mode event was forwarded for everything
// the fake CLI wrote before the sentinel's round trip.
func (p permissionModeSession) noModeEvent(t *testing.T) {
	t.Helper()
	p.stdinSoFar(t)
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

	before := len(p.stdinSoFar(t))
	if err := p.s.Send(ctx, "next turn", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, line := range p.stdinSoFar(t)[before:] {
		if strings.Contains(line, `"set_permission_mode"`) {
			t.Fatalf("the next turn re-sent a permission mode: %s", line)
		}
	}
}

// A supervised thread leaving plan can land the CLI on the user's own
// permissions.defaultMode. The CLI reports it, but the user never picked it
// for this thread: the session forwards nothing, keeps its base, and the
// next turn restores supervised.
func TestPermissionModeAModeTheUserDidNotPickIsNotAdopted(t *testing.T) {
	p := newPermissionModeSessionReporting(t, "default", "before", "acceptEdits")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.s.Send(ctx, "first turn", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p.noModeEvent(t)
	if got := p.s.BasePermissionMode(); got != "default" {
		t.Fatalf("BasePermissionMode = %q, want default", got)
	}
	if got := p.s.getCurrentPermissionMode(); got != "acceptEdits" {
		t.Fatalf("current permission mode = %q, want the reported acceptEdits", got)
	}
	before := len(p.stdinSoFar(t))
	if err := p.s.Send(ctx, "next turn", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	restored := false
	for _, line := range p.stdinSoFar(t)[before:] {
		restored = restored || (strings.Contains(line, `"set_permission_mode"`) && strings.Contains(line, `"mode":"default"`))
	}
	if !restored {
		t.Fatal("the next turn did not restore the supervised mode")
	}
}

// In plan the CLI keeps auto as the mode to return to and reports nothing
// when the model switch takes auto away. Leaving plan asks for auto, the CLI
// refuses it with error_code auto_mode_model, and the session takes the drop
// the CLI makes outside plan: base default, the change forwarded, the
// session on default.
func TestPermissionModeAutoRefusedForTheModelLeavingPlan(t *testing.T) {
	p := newPermissionModeSession(t, "auto", "before")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.s.SetInteractionMode(ctx, provider.ModePlan); err != nil {
		t.Fatalf("SetInteractionMode(plan): %v", err)
	}
	if _, err := p.s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-sonnet-4-6"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}
	p.noModeEvent(t)
	if got := p.s.BasePermissionMode(); got != "auto" {
		t.Fatalf("BasePermissionMode in plan = %q, want auto (the CLI said nothing)", got)
	}
	if err := p.s.SetInteractionMode(ctx, provider.ModeChat); err != nil {
		t.Fatalf("SetInteractionMode(chat): %v", err)
	}
	select {
	case evt := <-p.events:
		var meta provider.PermissionModeChangeMeta
		if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.Mode != "default" {
			t.Fatalf("forwarded meta %s (%v), want mode default", evt.Meta, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the refused auto was never forwarded as a drop")
	}
	if got := p.s.BasePermissionMode(); got != "default" {
		t.Fatalf("BasePermissionMode = %q, want default", got)
	}
	if got := p.s.getCurrentPermissionMode(); got != "default" {
		t.Fatalf("current permission mode = %q, want default", got)
	}
	if err := p.s.Send(ctx, "next turn", provider.SendOptions{InteractionMode: provider.ModeChat}); err != nil {
		t.Fatalf("Send after leaving plan: %v", err)
	}
}

// A refusal of auto while the base is not auto is not a drop: it is the
// error it says.
func TestPermissionModeAutoRefusalWithoutAnAutoBaseIsAnError(t *testing.T) {
	p := newPermissionModeSession(t, "default", "before")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-sonnet-4-6"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}
	if err := p.s.setPermissionMode(ctx, "auto"); err == nil || !strings.Contains(err.Error(), "auto mode unavailable") {
		t.Fatalf("setPermissionMode(auto) = %v, want the CLI's refusal", err)
	}
	p.noModeEvent(t)
}

// "plan" is AO's per-turn interaction mode, and an unknown name is not a
// mode the session can run; neither is adopted or forwarded.
func TestPermissionModeReportsThatAreNotAdopted(t *testing.T) {
	s, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "auto"})
	for _, reported := range []string{"", "plan", "yolo", "auto", "acceptEdits", "bypassPermissions"} {
		if s.adoptReportedPermissionMode(reported) {
			t.Fatalf("adoptReportedPermissionMode(%q) = true, want false", reported)
		}
	}
	if got := s.BasePermissionMode(); got != "auto" {
		t.Fatalf("BasePermissionMode = %q, want auto", got)
	}
}

// A session spawned on auto for a model without it can start on default,
// and only system/init says so. That one change is taken from init; every
// other init mode is the startup resolution (for a supervised spawn, the
// user's own defaultMode) and changes nothing.
func TestPermissionModeInitReportsOnlyTheAutoDrop(t *testing.T) {
	initLine := func(mode string) provider.ProviderEvent {
		return provider.ProviderEvent{Kind: provider.EventInit, Raw: []byte(`{"type":"system","subtype":"init","permissionMode":"` + mode + `"}`)}
	}
	s, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "auto"})
	for _, mode := range []string{"auto", "plan", "acceptEdits", ""} {
		if drop := s.initPermissionModeDrop(initLine(mode)); drop != nil {
			t.Fatalf("init %q on an auto session produced %s", mode, drop.Meta)
		}
	}
	s.notePermissionModeRequested("auto")
	if drop := s.initPermissionModeDrop(initLine("default")); drop != nil {
		t.Fatalf("init default with a request in flight produced %s", drop.Meta)
	}
	s.notePermissionModeSettled("auto")
	drop := s.initPermissionModeDrop(initLine("default"))
	if drop == nil || drop.Kind != provider.EventPermissionModeChanged {
		t.Fatalf("init default on an auto session = %+v, want the drop", drop)
	}
	var meta provider.PermissionModeChangeMeta
	if err := json.Unmarshal(drop.Meta, &meta); err != nil || meta.Mode != "default" {
		t.Fatalf("drop meta %s (%v), want mode default", drop.Meta, err)
	}
	if got := s.BasePermissionMode(); got != "default" {
		t.Fatalf("BasePermissionMode = %q, want default", got)
	}
	if again := s.initPermissionModeDrop(initLine("default")); again != nil {
		t.Fatalf("a second init default produced %s", again.Meta)
	}

	supervised, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	if drop := supervised.initPermissionModeDrop(initLine("acceptEdits")); drop != nil {
		t.Fatalf("init acceptEdits on a supervised session produced %s", drop.Meta)
	}
	if got := supervised.BasePermissionMode(); got != "default" {
		t.Fatalf("supervised BasePermissionMode = %q, want default", got)
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
