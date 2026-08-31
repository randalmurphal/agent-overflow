package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// The structured half of Claude's live-config confirmation
// (docs/references/claude-wire.md §"Live config commands"): the verdict on an
// /effort apply comes from `get_settings.applied.effort` where the CLI offers
// it, and from the reply text only where it does not.
//
// These tests drive a REAL claude.Session over a shell-script fake CLI. The
// script answers control_requests only — it never spawns anything, reads no
// provider home, and makes no API call (root AGENTS.md §Permanent
// invariants).

// writeGetSettingsFakeCLI writes a fake `claude` that answers every
// control_request with success, except `get_settings`, which it answers with
// settingsPayload — or, when errMsg is non-empty, with a control_response
// error carrying it.
func writeGetSettingsFakeCLI(t *testing.T, settingsPayload, errMsg string) string {
	t.Helper()
	answer := `printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":` + settingsPayload + `}}\n' "$reqid"`
	if errMsg != "" {
		answer = `printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"` + errMsg + `"}}\n' "$reqid"`
	}
	script := `#!/bin/sh
set -eu
while IFS= read -r line; do
    case "$line" in
        *'"subtype":"get_settings"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            ` + answer + `
            ;;
        *'"type":"control_request"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// seedClaudeGetSettingsThread is seedClaudeLiveConfigThread with a live
// claude.Session attached, since the structured confirmation path is skipped
// entirely for a session that has none.
func seedClaudeGetSettingsThread(
	t *testing.T, app *App, id, token, binary string, launchOpts provider.SessionOptions,
) (*claude.Session, chan string) {
	t.Helper()
	started := seedClaudeLiveConfigThread(t, app, id, token, launchOpts)

	sess, err := claude.NewSession(context.Background(), id, claude.Config{
		Binary:          binary,
		Model:           "claude-opus-5",
		ReasoningEffort: "high",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	current, _ := app.sessionManager().get(id)
	current.Claude = sess
	app.sessionManager().put(id, current)
	return sess, started
}

func waitForCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestEffortApplyConfirmedByGetSettings pins that the STRUCTURED read-back is
// primary. The reply text here is deliberately wording the parser does not
// recognise — the exact drift that costs a user an unnecessary restart today
// — and the apply must still confirm, because the CLI's own `applied.effort`
// says the session is running the requested tier.
func TestEffortApplyConfirmedByGetSettings(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-getsettings-ok", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[],"applied":{"model":"claude-opus-5","effort":"xhigh"}}`, "")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	sess, started := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Effort level is now xhigh for this session."))

	waitForCondition(t, "the structured read-back to land", func() bool {
		return sess.AppliedSettingsSnapshot() != nil
	})
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("a structurally confirmed apply marked the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the confirmed xhigh", got)
	}
	assertNoRestartWithin(t, started, 100*time.Millisecond, "get_settings confirmed the effort apply")
}

// TestEffortApplyDeclinedByGetSettings is the other direction, and the reason
// the structured path is AUTHORITATIVE rather than merely additive: the reply
// text reads as a clean success while the CLI reports a different resolved
// tier. launchOpts must never claim a config the process is not running, so
// the axis reverts, degrades, and the restart watcher converges.
func TestEffortApplyDeclinedByGetSettings(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-getsettings-mismatch", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[],"applied":{"model":"claude-opus-5","effort":"low"}}`, "")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	_, started := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Set effort level to xhigh (this session only): deep reasoning"))

	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("a structurally declined apply did not mark the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
}

// TestEffortApplyFallsBackToTextWhenGetSettingsUnsupported covers the older
// CLI: the subtype errors out, and the reply text is the verdict exactly as
// it was before the structured path existed.
func TestEffortApplyFallsBackToTextWhenGetSettingsUnsupported(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-getsettings-old", "tok-1"
	binary := writeGetSettingsFakeCLI(t, "", "Unsupported control request subtype: get_settings")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	sess, started := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	// The CLI's success text is the only evidence available; it confirms.
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Set effort level to xhigh (this session only): deep reasoning"))

	waitForCondition(t, "the session to record that get_settings is unsupported", func() bool {
		return sess.GetSettingsUnsupported()
	})
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the text fallback declined an apply the CLI confirmed")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the text-confirmed xhigh", got)
	}
	assertNoRestartWithin(t, started, 100*time.Millisecond, "the text fallback confirmed the effort apply")

	// Second apply: the session already knows the subtype is unsupported, so
	// nothing may reach the wire again — the fallback must not re-probe.
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-2"})
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-2", "Invalid argument: low"))
	waitRestart(t, started, id)
}

// TestGetSettingsRecordsProjectOverride pins the observability field: a
// repository `.claude/settings.json` naming a different effortLevel than AO
// requested is recorded on the session so it can be surfaced later. It is
// NOT acted on — the CLI's merge already decided who wins.
func TestGetSettingsRecordsProjectOverride(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-getsettings-project", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[{"source":"projectSettings","settings":{"effortLevel":"low"}}],`+
			`"applied":{"model":"claude-opus-5","effort":"high"}}`, "")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortHigh}
	sess, _ := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)

	applied, err := app.readClaudeAppliedSettings(id, token)
	if err != nil {
		t.Fatalf("readClaudeAppliedSettings: %v", err)
	}
	if applied == nil || applied.Effort != "high" {
		t.Fatalf("applied = %+v, want effort high", applied)
	}
	overrides := sess.SettingsOverrides()
	if len(overrides) != 1 {
		t.Fatalf("SettingsOverrides = %+v, want the one project effortLevel notice", overrides)
	}
	if overrides[0].Source != "projectSettings" || overrides[0].Configured != "low" || overrides[0].Requested != "high" {
		t.Fatalf("notice = %+v, want projectSettings effortLevel low vs the requested high", overrides[0])
	}
}

// TestReadClaudeAppliedSettingsSkipsReplacedSession pins the token guard: a
// read-back aimed at a session that has since been replaced must not run
// against its successor, whose config is a different question entirely.
func TestReadClaudeAppliedSettingsSkipsReplacedSession(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-getsettings-stale", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[],"applied":{"model":"claude-opus-5","effort":"high"}}`, "")
	seedClaudeGetSettingsThread(t, app, id, token, binary, provider.SessionOptions{Model: "claude-opus-5"})

	applied, err := app.readClaudeAppliedSettings(id, "a-newer-token")
	if err != nil || applied != nil {
		t.Fatalf("readClaudeAppliedSettings(stale token) = (%+v, %v), want (nil, nil)", applied, err)
	}
}

// TestModelReadBackIsSkippedWithoutAModel pins the gate on the model
// read-back. A prompt-only live update rides the same set_model with an
// empty model field, and claude.AppliedSettings carries model, effort,
// advisor and ultracode and nothing about the system prompt — so asking
// get_settings after one puts a control_request on stdin to verify a fact
// the answer cannot contain, and then compares the model the session was
// already running against the empty string it never requested.
func TestModelReadBackIsSkippedWithoutAModel(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-readback-gate", "tok-1"
	reads := make(chan string, 4)
	stubClaudeAppliedSettings(app, func(threadID, sessionToken string) (*claude.AppliedSettings, error) {
		reads <- threadID
		return &claude.AppliedSettings{Model: "claude-opus-5"}, nil
	})

	app.readBackClaudeAppliedModel(id, token, "")
	select {
	case <-reads:
		t.Fatal("a prompt-only update asked get_settings to verify a model it never requested")
	default:
	}

	app.readBackClaudeAppliedModel(id, token, "claude-opus-5")
	select {
	case <-reads:
	default:
		t.Fatal("a real model change made no read-back; the family-alias step-down would go unrecorded")
	}
}
