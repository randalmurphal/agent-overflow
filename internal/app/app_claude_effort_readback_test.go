package app

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// stubClaudeAppliedSettings installs the test seam over the `get_settings`
// round trip and returns a channel that receives each read.
func stubClaudeAppliedSettings(
	app *App,
	read func(threadID, sessionToken string) (*claude.AppliedSettings, error),
) {
	app.sessionManager().runtime.SetReadClaudeAppliedSettingsStep(read)
}

// pendingEffortApply registers one effort apply and returns it as the settle
// path would see it, already consumed out of the registry.
func pendingEffortApply(t *testing.T, app *App, id, token, requested, prev string) claudeLiveConfigApply {
	t.Helper()
	app.registerClaudeLiveConfigApplies(id, token,
		provider.SessionOptions{ReasoningEffort: provider.ReasoningEffort(prev)},
		claude.LiveUpdate{Effort: requested},
		claude.LiveApplyReceipt{EffortCommandUUID: "cmd-" + requested})
	pending, ok := app.takeClaudeLiveConfigApply("cmd-" + requested)
	if !ok {
		t.Fatalf("no pending apply registered for %q", requested)
	}
	return pending
}

// A high to xhigh apply the CLI accepted keeps launchOpts at xhigh, leaves
// the effort axis trusted and restarts nothing, whatever form the read-back
// took.
func TestEffortReadBackConfirmsAnAcceptedApply(t *testing.T) {
	t.Parallel()
	const accepted = "Set effort level to xhigh (this session only)"
	for _, tc := range []struct {
		name    string
		applied *claude.AppliedSettings
		err     error
	}{
		// The CLI writes `applied.effort` in whatever spelling its display
		// layer uses. AO stores the lowercase slug, so a raw string compare
		// read "X-High" as a REJECTION of the xhigh the session had in fact
		// accepted, and the decline path reverted launchOpts and restarted.
		{name: "xhigh", applied: &claude.AppliedSettings{Effort: "xhigh"}},
		{name: "X-High", applied: &claude.AppliedSettings{Effort: "X-High"}},
		{name: "x high", applied: &claude.AppliedSettings{Effort: "x high"}},
		{name: "  XHIGH  ", applied: &claude.AppliedSettings{Effort: "  XHIGH  "}},
		{name: "x_high", applied: &claude.AppliedSettings{Effort: "x_high"}},
		// An empty `applied.effort` is the wire's explicit null: an answer
		// about the MODEL ("this one declares no tiers"), not about this
		// request. Reading it as "the session runs a different tier" would
		// restart on every apply against such a model, so the settle falls
		// through to the CLI's reply text.
		{name: "empty tier, confirming reply", applied: &claude.AppliedSettings{Effort: ""}},
		// A read-back that FAILS is not a verdict either: the fallback is the
		// reply text, and an unsupported subtype is expected rather than
		// logged as failure.
		{name: "unsupported read, confirming reply", err: claude.ErrGetSettingsUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			id, token := "thread-effort-confirmed", "tok-confirmed"
			optimistic := provider.SessionOptions{ReasoningEffort: provider.EffortXHigh}
			started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
			pending := pendingEffortApply(t, app, id, token, "xhigh", string(provider.EffortHigh))

			stubClaudeAppliedSettings(app, func(string, string) (*claude.AppliedSettings, error) {
				return tc.applied, tc.err
			})
			app.settleClaudeEffortApplyFromSettings(pending, accepted)

			if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
				t.Fatalf("launchOpts effort = %q, want the confirmed xhigh", got)
			}
			if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
				t.Fatal("a confirmed apply marked the effort axis degraded")
			}
			assertNoRestartWithin(t, started, 50*time.Millisecond, "the apply was confirmed")
		})
	}
}

// A high to xhigh apply the CLI did not take reverts launchOpts to high,
// marks the effort axis degraded and restarts the session.
func TestEffortReadBackDeclinesARejectedApply(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		applied *claude.AppliedSettings
		reply   string
	}{
		// An empty tier with a REJECTING reply text still declines, because
		// the text is then the only evidence there is.
		{name: "no tier, rejecting reply", reply: "Invalid argument: xhigh"},
		// A tier the CLI genuinely did not adopt is the authoritative
		// decline, even when the reply text looked like success: a settings
		// layer AO does not control can outrank the request, and launchOpts
		// must never claim a config the process is not running.
		{name: "different tier, confirming reply", applied: &claude.AppliedSettings{Effort: "medium"}, reply: "Set effort level to xhigh (this session only)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			id, token := "thread-effort-declined", "tok-declined"
			optimistic := provider.SessionOptions{ReasoningEffort: provider.EffortXHigh}
			started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
			pending := pendingEffortApply(t, app, id, token, "xhigh", string(provider.EffortHigh))

			stubClaudeAppliedSettings(app, func(string, string) (*claude.AppliedSettings, error) {
				return tc.applied, nil
			})
			app.settleClaudeEffortApplyFromSettings(pending, tc.reply)

			if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
				t.Fatalf("launchOpts effort = %q, want the reverted high", got)
			}
			if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
				t.Fatal("a declined apply left the effort axis trusted")
			}
			waitRestart(t, started, id)
		})
	}
}

// B15: rapid effort changes interleave. The read-back for change #1 returns
// AFTER change #2 has landed, sees #2's tier in applied.effort, and would
// otherwise compare it against #1's request, decline, restore #1's prevEffort
// and restart the session to undo a change the user just made.
func TestSupersededEffortReadBackDecidesNothing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	id, token := "thread-effort-interleave", "tok-interleave"
	// The session is running what change #2 asked for.
	optimistic := provider.SessionOptions{ReasoningEffort: provider.EffortMax}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	// Change #1: high → xhigh. Consumed as the settle path would.
	first := pendingEffortApply(t, app, id, token, "xhigh", string(provider.EffortHigh))

	// Change #2 lands WHILE #1's read is out: registered from inside the
	// stub, which is exactly the ordering the guard exists for.
	stubClaudeAppliedSettings(app, func(string, string) (*claude.AppliedSettings, error) {
		app.registerClaudeLiveConfigApplies(id, token,
			provider.SessionOptions{ReasoningEffort: provider.EffortXHigh},
			claude.LiveUpdate{Effort: "max"},
			claude.LiveApplyReceipt{EffortCommandUUID: "cmd-max"})
		return &claude.AppliedSettings{Effort: "max"}, nil
	})

	app.settleClaudeEffortApplyFromSettings(first, "Set effort level to xhigh (this session only)")

	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortMax {
		t.Fatalf("launchOpts effort = %q — a superseded read-back reverted the newer change", got)
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "max"}) {
		t.Fatal("a superseded read-back marked the axis degraded")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "a superseded read-back answered a question nobody asked")

	// Change #2 is still pending and still owns the verdict.
	if _, ok := app.takeClaudeLiveConfigApply("cmd-max"); !ok {
		t.Fatal("the newer apply was consumed by the superseded one's settle")
	}
}

// The generation stamp must be per (session, axis): an unrelated axis or a
// different session advancing its own counter cannot supersede this entry.
func TestClaudeLiveApplySupersededIsScopedToSessionAndAxis(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	pending := claudeLiveConfigApply{
		SessionToken: "tok-a",
		Axis:         claudeLiveApplyAxisEffort,
		Generation:   1,
	}
	registerGeneration := func(id, token, axis string) {
		app.sessionManager().runtime.RegisterClaudeLiveApplies(map[string]claudeLiveConfigApply{
			id: {SessionToken: token, Axis: axis, SentAt: time.Now()},
		}, time.Now(), claudeLiveApplyStaleAfter)
	}
	registerGeneration("fast-1", "tok-a", claudeLiveApplyAxisFast)
	registerGeneration("fast-2", "tok-a", claudeLiveApplyAxisFast)
	registerGeneration("other-1", "tok-b", claudeLiveApplyAxisEffort)
	registerGeneration("other-2", "tok-b", claudeLiveApplyAxisEffort)
	if app.claudeLiveApplySuperseded(pending) {
		t.Fatal("another session's or another axis's applies superseded this entry")
	}

	// Two bumps: the first IS this entry's own generation (1), which must not
	// read as superseding itself.
	registerGeneration("effort-1", "tok-a", claudeLiveApplyAxisEffort)
	if app.claudeLiveApplySuperseded(pending) {
		t.Fatal("an entry superseded itself")
	}

	registerGeneration("effort-2", "tok-a", claudeLiveApplyAxisEffort)
	if !app.claudeLiveApplySuperseded(pending) {
		t.Fatal("a newer apply on the same (session, axis) did not supersede this entry")
	}
}

// An entry minted before generations existed (or by a path that never
// stamps one) must not read as superseded — zero is "no stamp", not
// "generation 0".
func TestUnstampedApplyIsNeverSuperseded(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.sessionManager().runtime.RegisterClaudeLiveApplies(map[string]claudeLiveConfigApply{
		"z": {SessionToken: "tok-z", Axis: claudeLiveApplyAxisEffort, SentAt: time.Now()},
	}, time.Now(), claudeLiveApplyStaleAfter)
	if app.claudeLiveApplySuperseded(claudeLiveConfigApply{
		SessionToken: "tok-z",
		Axis:         claudeLiveApplyAxisEffort,
	}) {
		t.Fatal("an unstamped entry read as superseded")
	}
}
