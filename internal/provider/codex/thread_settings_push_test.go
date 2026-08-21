package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestPlanThreadSettingsPushNamesOnlyChangedPushableAxes(t *testing.T) {
	base := codexLiveUpdateBaseOptions()

	cases := []struct {
		name        string
		queueNative bool
		next        func(provider.SessionOptions) provider.SessionOptions
		want        ThreadSettingsPush
	}{
		{
			name: "no change",
			next: func(o provider.SessionOptions) provider.SessionOptions { return o },
			want: ThreadSettingsPush{},
		},
		{
			name: "model",
			next: func(o provider.SessionOptions) provider.SessionOptions { o.Model = "gpt-5.6-codex"; return o },
			want: ThreadSettingsPush{Model: true},
		},
		{
			name: "effort",
			next: func(o provider.SessionOptions) provider.SessionOptions {
				o.ReasoningEffort = provider.EffortLow
				return o
			},
			want: ThreadSettingsPush{Effort: true},
		},
		{
			name: "fast mode on",
			next: func(o provider.SessionOptions) provider.SessionOptions {
				o.FastMode = true
				o.FastModeTierID = "turbo"
				return o
			},
			want: ThreadSettingsPush{ServiceTier: true},
		},
		{
			// Non-native: the three ride turn/start, which AO sends for every
			// turn, so pushing them would add a second writer for no gain.
			name: "runtime mode is not pushed on a non-native session",
			next: func(o provider.SessionOptions) provider.SessionOptions {
				o.RuntimeMode = provider.RuntimeFullAccess
				return o
			},
			want: ThreadSettingsPush{},
		},
		{
			// Native: the next turn may be one the app-server starts out of
			// its own queue, and that turn carries no overrides at all.
			name:        "runtime mode is pushed on a queue-native session",
			queueNative: true,
			next: func(o provider.SessionOptions) provider.SessionOptions {
				o.RuntimeMode = provider.RuntimeFullAccess
				return o
			},
			want: ThreadSettingsPush{ApprovalPolicy: true, Sandbox: true},
		},
		{
			// `auto` deliberately keeps approval-required's policy PAIR and
			// changes only who adjudicates (parent guide, RuntimeMode), so
			// this transition has exactly one changed axis — and a plan that
			// named the other two would push values Codex is already running.
			name:        "reviewer change is pushed on a queue-native session",
			queueNative: true,
			next: func(o provider.SessionOptions) provider.SessionOptions {
				o.RuntimeMode = provider.RuntimeAuto
				return o
			},
			want: ThreadSettingsPush{ApprovalsReviewer: true},
		},
		{
			name:        "an unchanged queue-native session pushes nothing",
			queueNative: true,
			next:        func(o provider.SessionOptions) provider.SessionOptions { return o },
			want:        ThreadSettingsPush{},
		},
		{
			name: "empty model is not a clear",
			next: func(o provider.SessionOptions) provider.SessionOptions { o.Model = ""; return o },
			want: ThreadSettingsPush{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanThreadSettingsPush(base, tc.next(base), tc.queueNative)
			if got != tc.want {
				t.Errorf("PlanThreadSettingsPush = %+v, want %+v", got, tc.want)
			}
			if got.Empty() != (got == ThreadSettingsPush{}) {
				t.Errorf("Empty() disagrees with the zero value for %+v", got)
			}
		})
	}
}

// TestPushThreadSettingsWireShape pins what actually goes out: one request
// carrying only the named axes, with the fast-mode clear as an explicit null.
func TestPushThreadSettingsWireShape(t *testing.T) {
	s, capturePath := newSettingsPushSession(t, Config{
		Binary:         "",
		Model:          "gpt-5.5",
		WorkDir:        "/tmp",
		ApprovalPolicy: "untrusted",
		Sandbox:        "read-only",
	})

	base := codexLiveUpdateBaseOptions()
	fastOn := base
	fastOn.FastMode = true
	fastOn.FastModeTierID = "turbo"
	fastOn.Model = "gpt-5.6-codex"

	apply := func(prev, next provider.SessionOptions) {
		t.Helper()
		update, ok := PlanLiveUpdate(prev, next)
		if !ok {
			t.Fatalf("PlanLiveUpdate needs a restart")
		}
		s.ApplyLiveUpdate(update)
		if err := s.PushThreadSettings(context.Background(), PlanThreadSettingsPush(prev, next, false)); err != nil {
			t.Fatalf("PushThreadSettings: %v", err)
		}
	}
	apply(base, fastOn)
	apply(fastOn, base)
	_ = s.Close()

	pushes := capturedRequestParams(t, capturePath, threadSettingsUpdateMethod)
	if len(pushes) != 2 {
		t.Fatalf("captured %d %s requests, want 2", len(pushes), threadSettingsUpdateMethod)
	}

	if pushes[0]["threadId"] != "mock-thread" {
		t.Errorf("first push threadId = %v, want mock-thread", pushes[0]["threadId"])
	}
	if pushes[0]["model"] != "gpt-5.6-codex" {
		t.Errorf("first push model = %v, want gpt-5.6-codex", pushes[0]["model"])
	}
	if pushes[0]["serviceTier"] != "turbo" {
		t.Errorf("first push serviceTier = %v, want turbo", pushes[0]["serviceTier"])
	}
	for _, forbidden := range []string{"approvalPolicy", "sandboxPolicy", "approvalsReviewer"} {
		if _, present := pushes[0][forbidden]; present {
			t.Errorf("push carried %s; runtime-mode axes ride turn/start only", forbidden)
		}
	}

	tier, present := pushes[1]["serviceTier"]
	if !present || tier != nil {
		t.Errorf("second push serviceTier = %v (present=%v), want an explicit null", tier, present)
	}
}

// TestPushThreadSettingsSkipsWhenNothingToSay proves an empty plan never
// reaches the wire. Upstream drops an all-None update without emitting an
// echo, so sending one is a round trip that also leaves a dangling
// expectation.
func TestPushThreadSettingsSkipsWhenNothingToSay(t *testing.T) {
	s, capturePath := newSettingsPushSession(t, Config{Model: "gpt-5.5", WorkDir: "/tmp"})
	if err := s.PushThreadSettings(context.Background(), ThreadSettingsPush{}); err != nil {
		t.Fatalf("PushThreadSettings: %v", err)
	}
	// A push naming an axis whose value is empty is equally silent.
	if err := s.PushThreadSettings(context.Background(), ThreadSettingsPush{Effort: true}); err != nil {
		t.Fatalf("PushThreadSettings: %v", err)
	}
	_ = s.Close()

	if pushes := capturedRequestParams(t, capturePath, threadSettingsUpdateMethod); len(pushes) != 0 {
		t.Fatalf("captured %d %s requests, want none", len(pushes), threadSettingsUpdateMethod)
	}
}

// TestPushThreadSettingsUnsupportedTransitions covers the version gate as
// transitions rather than states: a session that meets the unsupported error
// must stop calling and must not report a failure, and a fresh session
// against a newer binary must start calling again.
func TestPushThreadSettingsUnsupportedTransitions(t *testing.T) {
	push := ThreadSettingsPush{Model: true}

	t.Run("supported to unsupported latches once", func(t *testing.T) {
		s, capturePath := newSessionWithScript(t, codexSettingsUpdateScript(
			filepath.Join(t.TempDir(), "codex-stdin.log"), "unknown-variant"))
		if err := s.PushThreadSettings(context.Background(), push); err != nil {
			t.Fatalf("an unsupported method must not surface as an error: %v", err)
		}
		if err := s.PushThreadSettings(context.Background(), push); err != nil {
			t.Fatalf("second push: %v", err)
		}
		_ = s.Close()
		if pushes := capturedRequestParams(t, capturePath, threadSettingsUpdateMethod); len(pushes) != 1 {
			t.Fatalf("captured %d %s requests, want exactly 1 before the latch", len(pushes), threadSettingsUpdateMethod)
		}
	})

	t.Run("method not found is also a downgrade", func(t *testing.T) {
		s, _ := newSessionWithScript(t, codexSettingsUpdateScript(
			filepath.Join(t.TempDir(), "codex-stdin.log"), "method-not-found"))
		if err := s.PushThreadSettings(context.Background(), push); err != nil {
			t.Fatalf("-32601 must not surface as an error: %v", err)
		}
		if s.pendingSettingsEcho != nil {
			t.Error("a downgraded push must not leave an armed expectation behind")
		}
		_ = s.Close()
	})

	t.Run("other rpc errors still surface", func(t *testing.T) {
		s, _ := newSessionWithScript(t, codexSettingsUpdateScript(
			filepath.Join(t.TempDir(), "codex-stdin.log"), "invalid-request"))
		err := s.PushThreadSettings(context.Background(), push)
		if err == nil {
			t.Fatal("a real rejection must surface, not be swallowed as a downgrade")
		}
		if s.settingsUpdateUnsupported {
			t.Error("a real rejection must not latch the unsupported flag")
		}
		// The expectation is armed before the request goes out (the echo is
		// not ordered against the response), so a rejected push must take it
		// back down — otherwise the next unrelated settings echo within the
		// window would be reported as a rejection of a request Codex never
		// accepted.
		if s.pendingSettingsEcho != nil {
			t.Error("a rejected push must not leave an armed expectation behind")
		}
		_ = s.Close()
	})

	t.Run("a fresh session against a newer binary calls again", func(t *testing.T) {
		// The latch is per session by construction — a live session cannot
		// swap binaries. Prove the upgrade direction: a new session on a
		// binary that answers normally pushes.
		s, capturePath := newSettingsPushSession(t, Config{Model: "gpt-5.5", WorkDir: "/tmp"})
		if err := s.PushThreadSettings(context.Background(), push); err != nil {
			t.Fatalf("PushThreadSettings: %v", err)
		}
		_ = s.Close()
		if pushes := capturedRequestParams(t, capturePath, threadSettingsUpdateMethod); len(pushes) != 1 {
			t.Fatalf("captured %d %s requests, want 1", len(pushes), threadSettingsUpdateMethod)
		}
	})
}

func TestVerifyThreadSettingsEcho(t *testing.T) {
	future := time.Now().Add(time.Minute)
	cases := []struct {
		name        string
		expectation *settingsEchoExpectation
		settings    ThreadSettings
		wantMatch   string
	}{
		{
			name:        "no expectation is silent",
			expectation: nil,
			settings:    ThreadSettings{Model: "anything"},
		},
		{
			name:        "agreement is silent",
			expectation: &settingsEchoExpectation{model: "gpt-5.6", effort: "high", tierAsserted: "turbo", expires: future},
			settings:    ThreadSettings{Model: "gpt-5.6", ReasoningEffort: "high", ServiceTier: "turbo"},
		},
		{
			name:        "model divergence surfaces",
			expectation: &settingsEchoExpectation{model: "gpt-5.6", expires: future},
			settings:    ThreadSettings{Model: "gpt-5.5"},
			wantMatch:   `model "gpt-5.5"`,
		},
		{
			name:        "cleared tier still running is a divergence",
			expectation: &settingsEchoExpectation{tierCleared: "turbo", expires: future},
			settings:    ThreadSettings{ServiceTier: "turbo"},
			wantMatch:   "standard routing",
		},
		{
			name:        "cleared tier echoing the standard sentinel is agreement",
			expectation: &settingsEchoExpectation{tierCleared: "turbo", expires: future},
			settings:    ThreadSettings{ServiceTier: "default"},
		},
		{
			name:        "an unreported axis is not a divergence",
			expectation: &settingsEchoExpectation{model: "gpt-5.6", effort: "high", expires: future},
			settings:    ThreadSettings{},
		},
		{
			name:        "an expired expectation is dropped unverified",
			expectation: &settingsEchoExpectation{model: "gpt-5.6", expires: time.Now().Add(-time.Second)},
			settings:    ThreadSettings{Model: "gpt-5.5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{pendingSettingsEcho: tc.expectation}
			got := s.verifyThreadSettingsEcho(tc.settings)
			switch {
			case tc.wantMatch == "" && got != "":
				t.Errorf("verifyThreadSettingsEcho = %q, want silence", got)
			case tc.wantMatch != "" && !strings.Contains(got, tc.wantMatch):
				t.Errorf("verifyThreadSettingsEcho = %q, want it to mention %q", got, tc.wantMatch)
			}
			if s.pendingSettingsEcho != nil {
				t.Error("the expectation must be single-shot; a second echo cannot re-raise it")
			}
		})
	}
}

// TestReconcileThreadSettingsEmitsEchoDivergence proves the check is wired to
// the notification path and reaches the user as thread error state.
func TestReconcileThreadSettingsEmitsEchoDivergence(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: testThread,
		onEvent:  func(e provider.ProviderEvent) { events = append(events, e) },
		pendingSettingsEcho: &settingsEchoExpectation{
			model:   "gpt-5.6-codex",
			expires: time.Now().Add(time.Minute),
		},
	}
	s.setRootThreadID("codex-thread")
	s.reconcileThreadSettings(json.RawMessage(`{
		"threadId": "codex-thread",
		"threadSettings": {"model": "gpt-5.5", "approvalPolicy": "untrusted"}
	}`))

	if len(events) != 1 || events[0].Kind != provider.EventError {
		t.Fatalf("events = %+v, want one EventError", events)
	}
	if !strings.Contains(events[0].Content, "gpt-5.5") {
		t.Errorf("error content = %q, want it to name the model Codex is running", events[0].Content)
	}
	if observed, known := s.ObservedThreadSettings(); !known || observed.Model != "gpt-5.5" {
		t.Errorf("the divergence must still be recorded as observed settings, got %+v (known=%v)", observed, known)
	}
}

// newSettingsPushSession spins a session on the shared stdin-logging stub.
func newSettingsPushSession(t *testing.T, cfg Config) (*Session, string) {
	t.Helper()
	capturePath := filepath.Join(t.TempDir(), "codex-stdin.log")
	return newSessionWithScriptCfg(t, codexTurnCaptureScript(capturePath), cfg), capturePath
}

func newSessionWithScript(t *testing.T, script string) (*Session, string) {
	t.Helper()
	capturePath := codexScriptCapturePath(t, script)
	return newSessionWithScriptCfg(t, script, Config{Model: "gpt-5.5", WorkDir: "/tmp"}), capturePath
}

func newSessionWithScriptCfg(t *testing.T, script string, cfg Config) *Session {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}
	cfg.Binary = scriptPath
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/tmp"
	}
	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// codexSettingsUpdateScript answers thread/settings/update with the named
// failure and everything else with the minimal success shape.
//
// The "unknown-variant" mode reproduces what a codex predating the method
// actually returns: -32600 InvalidRequest from serde, not -32601. Captured
// verbatim from codex-cli 0.146.0 by calling a method name it does not know.
func codexSettingsUpdateScript(capturePath, mode string) string {
	var code int
	var message string
	switch mode {
	case "unknown-variant":
		code = -32600
		message = "Invalid request: unknown variant `thread/settings/update`, expected one of `initialize`, ..."
	case "method-not-found":
		code = -32601
		message = "Method not found"
	default:
		code = -32600
		message = "Invalid request: bad model id"
	}
	// The error frame is emitted through printf with the message inside
	// SINGLE quotes so the backticks upstream's serde message carries stay
	// literal instead of being command-substituted by the shell.
	return fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
    printf '%%s\n' "$line" >> %q
    id=$(echo "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if echo "$line" | grep -q '"method":"thread/settings/update"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"error":{"code":%d,"message":"%s"}}\n' "$id"
    else
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"mock-thread"}}}\n' "$id"
    fi
done
`, capturePath, code, message)
}

// codexScriptCapturePath recovers the log path baked into a generated script
// so the helpers can hand it back without a second parameter.
func codexScriptCapturePath(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, `>> "`)
	if start < 0 {
		t.Fatalf("script has no capture redirect")
	}
	rest := script[start+4:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("script capture redirect is unterminated")
	}
	return rest[:end]
}

// capturedRequestParams extracts the params object of every captured request
// for the given method.
func capturedRequestParams(t *testing.T, path, method string) []map[string]any {
	t.Helper()
	captured, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture: %v", err)
	}
	var params []map[string]any
	for _, line := range strings.Split(string(captured), "\n") {
		if !strings.Contains(line, `"method":"`+method+`"`) {
			continue
		}
		var request map[string]any
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatalf("unmarshal %s: %v", method, err)
		}
		bag, ok := request["params"].(map[string]any)
		if !ok {
			t.Fatalf("%s carried no params object: %s", method, line)
		}
		params = append(params, bag)
	}
	return params
}
