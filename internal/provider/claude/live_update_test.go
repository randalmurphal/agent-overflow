package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func liveUpdateBaseOptions() provider.SessionOptions {
	return provider.SessionOptions{
		Provider:        string(provider.Claude),
		Model:           "claude-sonnet-5",
		WorkDir:         "/tmp/work",
		ReasoningEffort: provider.EffortHigh,
		ContextWindow:   200000,
		Mode:            provider.ModeChat,
		RuntimeMode:     provider.RuntimeApprovalRequired,
		SystemPrompt:    "base prompt",
		Resume:          "session-ref-1",
	}
}

func TestPlanLiveUpdate(t *testing.T) {
	tests := []struct {
		name string
		// mutatePrev shapes the session's launch config; mutate shapes the
		// thread row's target config. Both start from liveUpdateBaseOptions.
		mutatePrev func(*provider.SessionOptions)
		mutate     func(*provider.SessionOptions)
		wantOK     bool
		wantUpdate LiveUpdate
	}{
		{
			name:   "identical options need nothing",
			mutate: func(*provider.SessionOptions) {},
			wantOK: true,
		},
		{
			name:       "model change is live",
			mutate:     func(o *provider.SessionOptions) { o.Model = "claude-fable-5" },
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-fable-5"},
		},
		{
			name:       "runtime mode change is live",
			mutate:     func(o *provider.SessionOptions) { o.RuntimeMode = provider.RuntimeAutoAcceptEdits },
			wantOK:     true,
			wantUpdate: LiveUpdate{BasePermissionMode: "acceptEdits"},
		},
		{
			name: "model and runtime mode together are live",
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-fable-5"
				o.RuntimeMode = provider.RuntimeFullAccess
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-fable-5", BasePermissionMode: "bypassPermissions"},
		},
		{
			name:   "resume cursor drift is lifecycle, not config",
			mutate: func(o *provider.SessionOptions) { o.Resume = "session-ref-2" },
			wantOK: true,
		},
		{
			name:       "effort change is live",
			mutate:     func(o *provider.SessionOptions) { o.ReasoningEffort = provider.EffortLow },
			wantOK:     true,
			wantUpdate: LiveUpdate{Effort: "low"},
		},
		{
			name: "effort change round-trips back",
			mutatePrev: func(o *provider.SessionOptions) {
				o.ReasoningEffort = provider.EffortLow
			},
			mutate:     func(o *provider.SessionOptions) { o.ReasoningEffort = provider.EffortHigh },
			wantOK:     true,
			wantUpdate: LiveUpdate{Effort: "high"},
		},
		{
			name: "model and effort together are live",
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-fable-5"
				o.ReasoningEffort = provider.EffortXHigh
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-fable-5", Effort: "xhigh"},
		},
		{
			name: "switch to an effortless model needs restart",
			mutate: func(o *provider.SessionOptions) {
				// Haiku declares no reasoning effort, so the target config
				// carries no effort flag at all — and there is no /effort
				// argument that restores "send no effort".
				o.Model = "claude-haiku-4-5"
			},
			wantOK: false,
		},
		{
			name: "fast mode enable is live with the spawn opt-in question deferred to apply",
			mutatePrev: func(o *provider.SessionOptions) {
				// claude-opus-4-8 supports fast mode; a non-capable model
				// would coerce to off on both sides and read as no change.
				o.Model = "claude-opus-4-8"
			},
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-opus-4-8"
				o.FastMode = true
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{FastMode: FastModeOn},
		},
		{
			name: "fast mode disable is live",
			mutatePrev: func(o *provider.SessionOptions) {
				o.Model = "claude-opus-4-8"
				o.FastMode = true
			},
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-opus-4-8"
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{FastMode: FastModeOff},
		},
		{
			name: "model and fast mode together plan both axes",
			// The apply-order contract (set_model before /fast) exists for
			// exactly this bundle — the plan must carry both.
			mutatePrev: func(o *provider.SessionOptions) {
				o.Model = "claude-opus-4-8"
			},
			mutate: func(o *provider.SessionOptions) {
				o.Model = "claude-opus-5"
				o.FastMode = true
			},
			wantOK:     true,
			wantUpdate: LiveUpdate{Model: "claude-opus-5", FastMode: FastModeOn},
		},
		{
			name: "context window change is live absent an autocompact override",
			mutate: func(o *provider.SessionOptions) {
				o.ContextWindow = provider.ClaudeExtendedContextWindow
			},
			wantOK: true,
			// The tier rides the model string: set_model accepts
			// marker-carrying ids (spike-verified 2.1.219 — the
			// context-1m beta follows on the next request).
			wantUpdate: LiveUpdate{Model: "claude-sonnet-5[1m]"},
		},
		{
			name: "context window change with an autocompact override needs restart",
			mutatePrev: func(o *provider.SessionOptions) {
				o.AutoCompactPercent = 50
			},
			mutate: func(o *provider.SessionOptions) {
				o.AutoCompactPercent = 50
				o.ContextWindow = provider.ClaudeExtendedContextWindow
			},
			// CLAUDE_CODE_AUTO_COMPACT_WINDOW is rendered into the
			// spawn-only --settings env block when a percent is set, and it
			// must match the live window.
			wantOK: false,
		},
		{
			name: "autocompact override change needs restart",
			mutate: func(o *provider.SessionOptions) {
				o.AutoCompactPercent = 50
			},
			wantOK: false,
		},
		{
			// A prompt landing on a non-empty value rides
			// set_model.system_prompt. Only the empty NEXT side (an
			// override turned off) still needs a restart — see
			// TestPlanLiveUpdateSystemPromptTransitions, which owns that
			// whole axis.
			name:       "system prompt swap is live",
			mutate:     func(o *provider.SessionOptions) { o.SystemPrompt = "different prompt" },
			wantOK:     true,
			wantUpdate: LiveUpdate{SystemPrompt: "different prompt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			next := liveUpdateBaseOptions()
			if tt.mutatePrev != nil {
				tt.mutatePrev(&prev)
			}
			tt.mutate(&next)

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tt.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v", ok, tt.wantOK)
			}
			if update != tt.wantUpdate {
				t.Fatalf("PlanLiveUpdate update = %+v, want %+v", update, tt.wantUpdate)
			}
		})
	}
}

// liveUpdateTestSession spawns a fake-CLI session that acks every
// control_request with success and captures stdin lines to a file.
func liveUpdateTestSession(t *testing.T, cfg Config) (*Session, string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	capturePath := filepath.Join(t.TempDir(), "stdin.ndjson")
	script := `#!/bin/sh
set -eu
capture="${CAPTURE_FILE:?}"
while IFS= read -r line; do
    printf '%s\n' "$line" >> "$capture"
    case "$line" in
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude script: %v", err)
	}

	cfg.Binary = scriptPath
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["CAPTURE_FILE"] = capturePath

	s, err := NewSession(context.Background(), testThread, cfg, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.controlRequestTimeout = 2 * time.Second
	return s, capturePath
}

func TestApplyLiveUpdateSendsSetModel(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" || captured.Request.Subtype != "set_model" || captured.Request.Model != "claude-fable-5" {
		t.Fatalf("captured line = %+v, want set_model claude-fable-5", captured)
	}
}

func TestApplyLiveUpdateAppliesBasePermissionMode(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{BasePermissionMode: "acceptEdits"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Type != "control_request" || captured.Request.Subtype != "set_permission_mode" || captured.Request.Mode != "acceptEdits" {
		t.Fatalf("captured line = %+v, want set_permission_mode acceptEdits", captured)
	}
	if got := s.getCurrentPermissionMode(); got != "acceptEdits" {
		t.Fatalf("currentPermissionMode = %q, want acceptEdits", got)
	}
	// The new base survives a later plan-turn restore cycle.
	if got := s.desiredPermissionModeForTurn(provider.ModeChat); got != "acceptEdits" {
		t.Fatalf("desiredPermissionModeForTurn(chat) = %q, want acceptEdits", got)
	}
}

// TestApplyLiveUpdateBypassEscalationRequiresRestart pins the CLI
// constraint verified on 2.1.205: a session spawned without
// --allow-dangerously-skip-permissions cannot escalate to
// bypassPermissions via set_permission_mode. ApplyLiveUpdate must signal
// the restart fallback before any side effect (no wire write, model
// untouched).
func TestApplyLiveUpdateBypassEscalationRequiresRestart(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5", BasePermissionMode: "bypassPermissions"}, nil)
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}

	// No control_request may have hit stdin — the model half of the update
	// must not half-apply when the permission half needs a restart.
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

func TestApplyLiveUpdateBypassAllowedWhenSpawnedWithAllowFlag(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{
		BasePermissionMode: "default",
		PermissionFlags:    []string{"--permission-mode", "default", "--allow-dangerously-skip-permissions"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{BasePermissionMode: "bypassPermissions"}, nil); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	var captured struct {
		Request struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &captured); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if captured.Request.Subtype != "set_permission_mode" || captured.Request.Mode != "bypassPermissions" {
		t.Fatalf("captured line = %+v, want set_permission_mode bypassPermissions", captured)
	}
}

// TestPlanLiveUpdateReadOnlyTransitionsRequireRestart covers the transitions,
// not just the states. `--disallowedTools` is applied once at spawn and no
// control_request can add or drop a tool on a live session, so every move
// into or out of read-only must fall through to a restart.
//
// Without this, a read-only → auto-accept-edits switch would ack a bare
// set_permission_mode while Write/Edit stayed missing from the process: a
// session reporting a mode it cannot honour. The reverse (anything →
// read-only) is worse — the session would report read-only while the write
// tools were still loaded and only the softer dontAsk denial applied.
func TestPlanLiveUpdateReadOnlyTransitionsRequireRestart(t *testing.T) {
	// Derived from the canonical list rather than written out, so a tier added
	// to provider.AllRuntimeModes is held to the restart rule without anyone
	// remembering to extend this test.
	var others []provider.RuntimeMode
	for _, mode := range provider.AllRuntimeModes {
		if mode != provider.RuntimeReadOnly {
			others = append(others, mode)
		}
	}

	for _, other := range others {
		t.Run("into read-only from "+string(other), func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			prev.RuntimeMode = other
			next := prev
			next.RuntimeMode = provider.RuntimeReadOnly
			if _, ok := PlanLiveUpdate(prev, next); ok {
				t.Error("PlanLiveUpdate allowed a live switch into read-only; tool removal is spawn-only")
			}
		})
		t.Run("out of read-only to "+string(other), func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			prev.RuntimeMode = provider.RuntimeReadOnly
			next := prev
			next.RuntimeMode = other
			if _, ok := PlanLiveUpdate(prev, next); ok {
				t.Error("PlanLiveUpdate allowed a live switch out of read-only; removed tools cannot be restored")
			}
		})
	}

	t.Run("read-only to read-only needs nothing", func(t *testing.T) {
		prev := liveUpdateBaseOptions()
		prev.RuntimeMode = provider.RuntimeReadOnly
		update, ok := PlanLiveUpdate(prev, prev)
		if !ok {
			t.Fatal("PlanLiveUpdate rejected an unchanged read-only session")
		}
		if update != (LiveUpdate{}) {
			t.Errorf("update = %+v, want zero for an unchanged session", update)
		}
	})
}

// TestPlanLiveUpdateAutoTransitions covers the auto tier's transitions rather
// than its resting state. Auto differs from its neighbours only in
// BasePermissionMode — it strips no tools — so every move between auto and a
// non-read-only tier must be expressible as one set_permission_mode, and every
// move between auto and read-only must not be (read-only's `--disallowedTools`
// is spawn-only and no control_request can restore a removed tool).
//
// The bypassPermissions asymmetry is deliberate and lives one layer down:
// PlanLiveUpdate happily PLANS auto → full-access, and ApplyLiveUpdate is what
// refuses it on a process spawned without --allow-dangerously-skip-permissions
// (TestApplyLiveUpdateBypassEscalationRequiresRestart). Planning and applying
// answer different questions — "is this expressible on the wire" versus "can
// THIS process accept it" — and collapsing them here would make the plan
// depend on session state it does not have.
func TestPlanLiveUpdateAutoTransitions(t *testing.T) {
	cases := []struct {
		from, to   provider.RuntimeMode
		wantLive   bool
		wantUpdate LiveUpdate
	}{
		{provider.RuntimeApprovalRequired, provider.RuntimeAuto, true, LiveUpdate{BasePermissionMode: "auto"}},
		{provider.RuntimeAuto, provider.RuntimeApprovalRequired, true, LiveUpdate{BasePermissionMode: "default"}},
		{provider.RuntimeAutoAcceptEdits, provider.RuntimeAuto, true, LiveUpdate{BasePermissionMode: "auto"}},
		{provider.RuntimeAuto, provider.RuntimeAutoAcceptEdits, true, LiveUpdate{BasePermissionMode: "acceptEdits"}},
		{provider.RuntimeFullAccess, provider.RuntimeAuto, true, LiveUpdate{BasePermissionMode: "auto"}},
		{provider.RuntimeAuto, provider.RuntimeFullAccess, true, LiveUpdate{BasePermissionMode: "bypassPermissions"}},
		{provider.RuntimeAuto, provider.RuntimeReadOnly, false, LiveUpdate{}},
		{provider.RuntimeReadOnly, provider.RuntimeAuto, false, LiveUpdate{}},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+" to "+string(tc.to), func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			prev.RuntimeMode = tc.from
			next := prev
			next.RuntimeMode = tc.to
			update, ok := PlanLiveUpdate(prev, next)
			if ok != tc.wantLive {
				t.Fatalf("PlanLiveUpdate live = %v, want %v", ok, tc.wantLive)
			}
			if update != tc.wantUpdate {
				t.Errorf("update = %+v, want %+v", update, tc.wantUpdate)
			}
		})
	}

	t.Run("auto to auto needs nothing", func(t *testing.T) {
		prev := liveUpdateBaseOptions()
		prev.RuntimeMode = provider.RuntimeAuto
		update, ok := PlanLiveUpdate(prev, prev)
		if !ok {
			t.Fatal("PlanLiveUpdate rejected an unchanged auto session")
		}
		if update != (LiveUpdate{}) {
			t.Errorf("update = %+v, want zero for an unchanged session", update)
		}
	})
}

// decodeCapturedUserCommand asserts a captured stdin line is a user
// envelope carrying exactly one text block and returns (text, uuid).
func decodeCapturedUserCommand(t *testing.T, line string) (string, string) {
	t.Helper()
	var captured struct {
		Type    string `json:"type"`
		UUID    string `json:"uuid"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &captured); err != nil {
		t.Fatalf("unmarshal captured line %q: %v", line, err)
	}
	if captured.Type != "user" || captured.Message.Role != "user" || len(captured.Message.Content) != 1 {
		t.Fatalf("captured line = %+v, want single-block user envelope", captured)
	}
	return captured.Message.Content[0].Text, captured.UUID
}

// TestApplyLiveUpdateSendsEffortCommand pins the /effort live apply: the
// command goes out as a slash-guard-exempt user message (no leading
// newline — the CLI's command router must see "/" first) whose uuid comes
// back on the receipt for async confirmation.
func TestApplyLiveUpdateSendsEffortCommand(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort", "fast"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var receipt LiveApplyReceipt
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Effort: "xhigh"}, func(r LiveApplyReceipt) {
		receipt = r
	}); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}
	if receipt.EffortCommandUUID == "" {
		t.Fatalf("receipt = %+v, want an effort command uuid", receipt)
	}

	lines := waitCapturedLines(t, capturePath, 1)
	text, id := decodeCapturedUserCommand(t, lines[0])
	if text != "/effort xhigh" {
		t.Fatalf("command text = %q, want %q", text, "/effort xhigh")
	}
	if id != receipt.EffortCommandUUID {
		t.Fatalf("envelope uuid = %q, want receipt uuid %q", id, receipt.EffortCommandUUID)
	}
}

// TestApplyLiveUpdateOrdersModelBeforeCommands pins the full sequence:
// set_model first (a /fast enable may consult the active model), then
// /effort, then /fast.
func TestApplyLiveUpdateOrdersModelBeforeCommands(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{
		BasePermissionMode: "default",
		FastMode:           true,
	})
	s.replaceAdvertisedCommands([]string{"effort", "fast"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var receipt LiveApplyReceipt
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{
		Model:    "claude-opus-5[1m]",
		Effort:   "low",
		FastMode: FastModeOff,
	}, func(r LiveApplyReceipt) {
		receipt = r
	}); err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}

	lines := waitCapturedLines(t, capturePath, 3)
	var first struct {
		Type    string `json:"type"`
		Request struct {
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal captured line: %v", err)
	}
	if first.Request.Subtype != "set_model" || first.Request.Model != "claude-opus-5[1m]" {
		t.Fatalf("first line = %+v, want set_model claude-opus-5[1m]", first)
	}
	effortText, effortID := decodeCapturedUserCommand(t, lines[1])
	if effortText != "/effort low" || effortID != receipt.EffortCommandUUID {
		t.Fatalf("second line = %q/%q, want /effort low with receipt uuid", effortText, effortID)
	}
	fastText, fastID := decodeCapturedUserCommand(t, lines[2])
	if fastText != "/fast off" || fastID != receipt.FastCommandUUID {
		t.Fatalf("third line = %q/%q, want /fast off with receipt uuid", fastText, fastID)
	}
}

// TestApplyLiveUpdateEffortNeedsAdvertisedCommand: a session that has not
// advertised /effort (no init yet, older CLI, or gated account) routes the
// whole update to the restart path before any side effect.
func TestApplyLiveUpdateEffortNeedsAdvertisedCommand(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5", Effort: "low"}, nil)
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

// TestApplyLiveUpdateRejectsUnknownEffortTier: ApplyLiveUpdate is a public
// API; a tier outside the vocabulary AO validated against the CLI must fail
// loudly here, because the CLI would answer it with a non-error "Invalid
// argument" text.
func TestApplyLiveUpdateRejectsUnknownEffortTier(t *testing.T) {
	s, _ := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Effort: "ultracode"}, nil)
	if err == nil || errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want a validation error", err)
	}
}

// TestApplyLiveUpdateFastEnableRequiresSpawnOptIn pins the SDK gate: /fast
// on a session spawned without the fastMode settings opt-in answers
// "not available in the Agent SDK", so the enable must take the restart
// path (the respawn adds the opt-in). Disable has no such gate.
func TestApplyLiveUpdateFastEnableRequiresSpawnOptIn(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort", "fast"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{FastMode: FastModeOn}, nil)
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

// TestApplyLiveUpdatePreSendRunsBeforeAnyWrite pins the confirmation-race
// contract: preSend fires after validation and before any byte reaches the
// wire, so the caller's pending-apply registration always beats the CLI's
// answer.
func TestApplyLiveUpdatePreSendRunsBeforeAnyWrite(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var receipt LiveApplyReceipt
	preSendCalls := 0
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Effort: "low"}, func(r LiveApplyReceipt) {
		preSendCalls++
		receipt = r
		if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
			t.Errorf("preSend ran after wire writes: %s", data)
		}
	})
	if err != nil {
		t.Fatalf("ApplyLiveUpdate: %v", err)
	}
	if preSendCalls != 1 {
		t.Fatalf("preSend calls = %d, want exactly 1", preSendCalls)
	}
	lines := waitCapturedLines(t, capturePath, 1)
	_, id := decodeCapturedUserCommand(t, lines[0])
	if id != receipt.EffortCommandUUID {
		t.Fatalf("wire uuid = %q, want the preSend receipt's %q", id, receipt.EffortCommandUUID)
	}
}

// TestApplyLiveUpdateEffortRefusedOnEffortlessModel — the planner never
// produces this update, but the gate must not live in caller discipline:
// /effort against a model with no reasoning tiers fails loudly before any
// side effect.
func TestApplyLiveUpdateEffortRefusedOnEffortlessModel(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{Model: "claude-haiku-4-5", BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Effort: "low"}, func(LiveApplyReceipt) {
		t.Error("preSend ran for a rejected update")
	})
	if err == nil || errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want a validation error", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

// TestApplyLiveUpdateRejectsGarbageFastArgument — FastModeChange is an open
// string type; anything outside on/off must fail before the wire, because
// the CLI answers `/fast garbage` with a non-error "Usage:" text.
func TestApplyLiveUpdateRejectsGarbageFastArgument(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default", FastMode: true})
	s.replaceAdvertisedCommands([]string{"fast"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{FastMode: FastModeChange("toggle")}, nil)
	if err == nil || errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want a validation error", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}
}

// TestApplyLiveUpdateCommandAxesRefusedDuringResumeRepair — /effort and
// /fast are user messages on the wire; a transcript pending the
// --resume-session-at repair must not receive one. The restart fallback
// performs the repair.
func TestApplyLiveUpdateCommandAxesRefusedDuringResumeRepair(t *testing.T) {
	s, capturePath := liveUpdateTestSession(t, Config{BasePermissionMode: "default"})
	s.replaceAdvertisedCommands([]string{"effort"})
	s.leafTracker.ingestLine([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"go"}}`))
	s.leafTracker.ingestLine([]byte(`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`))
	s.leafTracker.markTurnComplete()
	if !s.RequiresResumeAtBeforeUserSend() {
		t.Fatal("test setup: session not in the resume-repair state")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Effort: "low"}, nil)
	if !errors.Is(err, ErrLiveUpdateRequiresRestart) {
		t.Fatalf("ApplyLiveUpdate error = %v, want ErrLiveUpdateRequiresRestart", err)
	}
	time.Sleep(100 * time.Millisecond)
	if data, err := os.ReadFile(capturePath); err == nil && len(data) > 0 {
		t.Fatalf("expected no stdin writes, captured: %s", data)
	}

	// A model-only update carries no user message and stays live even in
	// the repair state.
	if _, err := s.ApplyLiveUpdate(ctx, LiveUpdate{Model: "claude-fable-5"}, nil); err != nil {
		t.Fatalf("model-only ApplyLiveUpdate during repair state: %v", err)
	}
}

// TestLiveEffortTiersMatchOptionCoercion pins the /effort vocabulary to the
// option layer: every claude-legal reasoning tier is live-appliable and
// nothing else is, so a tier added to one layer cannot silently miss the
// other.
func TestLiveEffortTiersMatchOptionCoercion(t *testing.T) {
	for _, tier := range provider.AllReasoningEfforts {
		coerced := claudeEffortFromOption(tier)
		if got := IsLiveEffortTier(string(tier)); got != (coerced == string(tier)) {
			t.Fatalf("tier %q: IsLiveEffortTier = %v but claudeEffortFromOption maps it to %q", tier, got, coerced)
		}
	}
	if IsLiveEffortTier("ultracode") || IsLiveEffortTier("auto") || IsLiveEffortTier("") {
		t.Fatal("CLI-only tiers must not be live-appliable as thread config")
	}
}
