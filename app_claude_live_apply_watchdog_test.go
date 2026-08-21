package main

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// The asynchronous confirmation contract has three outcomes, not two: the
// CLI confirms, the CLI answers something else, or nothing ever arrives.
// The third one used to be a rest state — the optimistic launchOpts write
// stood unchallenged for the session's whole life, so the composer showed a
// tier the process was not running and the reconciler saw "already
// converged" and never restarted anything. These tests pin the bounded
// window that turns silence into a verdict.

// armWatchdogWindow shortens the unconfirmed-apply window so tests observe
// the sweep without waiting 45 seconds. Set before any registration.
func armWatchdogWindow(app *App, d time.Duration) {
	app.claudeLiveApplyConfirmAfterOverride = d
}

// TestUnansweredLiveApplyDeclinesAfterTheWindow — no command result, no
// lifecycle frame, nothing: the axis must revert, degrade so the reconciler
// stops re-sending into the silence, and converge through the restart path.
func TestUnansweredLiveApplyDeclinesAfterTheWindow(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-silent", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 20*time.Millisecond)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("an apply the CLI never answered did not mark the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high — the session never confirmed xhigh", got)
	}
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("the swept entry stayed in the registry")
	}
}

// TestUnansweredFastApplyDeclinesAfterTheWindow — the fast axis has no
// structured read-back at all, so silence is its only unhandled outcome.
func TestUnansweredFastApplyDeclinesAfterTheWindow(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-fast", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", FastMode: true}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 20*time.Millisecond)

	prev := optimistic
	prev.FastMode = false
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{FastMode: claude.FastModeOn}, claude.LiveApplyReceipt{FastCommandUUID: "cmd-fast"})

	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("an unanswered fast apply did not mark the axis degraded")
	}
	if launchOptsForThread(t, app, id).FastMode {
		t.Fatal("launchOpts still claims fast mode is on; nothing ever confirmed it")
	}
}

// TestWatchdogWaitsWhileATurnRuns — `/effort` and `/fast` are stdin user
// messages that queue behind a running turn, so a turn in flight is the one
// legitimate reason for silence. The window must be re-armed, not spent:
// declining here would revert an axis the CLI is still going to apply and
// restart the session out from under the running turn.
func TestWatchdogWaitsWhileATurnRuns(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-busy", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 10*time.Millisecond)

	liveness := newSessionLiveness(time.Now())
	liveness.activeTurns.Store(1)
	app.mu.Lock()
	current := app.sessions[id]
	current.liveness = liveness
	app.sessions[id] = current
	app.mu.Unlock()

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	// Many windows' worth: each one must re-arm rather than decide.
	assertNoRestartWithin(t, started, 200*time.Millisecond, "a turn is running and the command is queued behind it")
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); !ok {
		t.Fatal("the watchdog consumed a pending apply that is still legitimately queued")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the still-pending xhigh", got)
	}

	// Turn ends, the answer is still nowhere: now the window applies.
	liveness.activeTurns.Store(0)
	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the post-turn sweep did not mark the axis degraded")
	}
}

// TestWatchdogIgnoresAnsweredAndSupersededApplies — the sweep must be a
// no-op for every entry somebody else already owns. cmd-1 is superseded by
// cmd-2 (a tombstone: its verdict decides nothing), and cmd-2 is confirmed
// by the CLI. Neither may produce a revert or a restart when the windows
// later expire.
func TestWatchdogIgnoresAnsweredAndSupersededApplies(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-settled", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 20*time.Millisecond)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-2"})
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-2", "Set effort level to xhigh (this session only): deep reasoning"))

	assertNoRestartWithin(t, started, 200*time.Millisecond, "one apply was superseded and the other was confirmed")
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the watchdog degraded an axis whose apply was confirmed")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the confirmed xhigh", got)
	}
}

// TestUnansweredEffortApplyConfirmsFromGetSettings — silence is not evidence
// that the command failed. The CLI states the tier it resolved, and when
// that matches the request the command DID run and only its output went
// missing: no revert, no degrade, no restart.
func TestUnansweredEffortApplyConfirmsFromGetSettings(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-readback", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[],"applied":{"model":"claude-opus-5","effort":"xhigh"}}`, "")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	sess, started := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)
	armWatchdogWindow(app, 20*time.Millisecond)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	waitForCondition(t, "the watchdog's structured read-back to land", func() bool {
		return sess.AppliedSettingsSnapshot() != nil
	})
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the watchdog degraded an axis the CLI reports as applied")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the read-back-confirmed xhigh", got)
	}
	assertNoRestartWithin(t, started, 100*time.Millisecond, "get_settings reports the requested tier is applied")
}

// TestUnansweredEffortApplyDeclinesWhenGetSettingsDisagrees — the same read
// back, other direction: the session is running a tier AO did not ask for,
// which is the wrong-state launchOpts must never keep.
func TestUnansweredEffortApplyDeclinesWhenGetSettingsDisagrees(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-readback-bad", "tok-1"
	binary := writeGetSettingsFakeCLI(t,
		`{"effective":{},"sources":[],"applied":{"model":"claude-opus-5","effort":"low"}}`, "")

	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	_, started := seedClaudeGetSettingsThread(t, app, id, token, binary, optimistic)
	armWatchdogWindow(app, 20*time.Millisecond)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the declining sweep did not mark the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
}

// installClaudeLivenessForThread attaches a liveness record to an already
// seeded session so a test can raise and drop the turn count at will. The
// returned record is the one threadTurnInFlight reads.
func installClaudeLivenessForThread(app *App, threadID string) *sessionLiveness {
	liveness := newSessionLiveness(time.Now())
	app.mu.Lock()
	current := app.sessions[threadID]
	current.liveness = liveness
	app.sessions[threadID] = current
	app.mu.Unlock()
	return liveness
}

// TestWatchdogRestartsTheWindowWhenTheTurnDrains pins the interleaving the
// re-arm-while-busy rule alone does not cover: the sweep samples only at its
// own expiry, so a turn that ENDS between two sweeps hands the next one a
// command the CLI has held for a moment rather than for a window.
//
// Driven by hand rather than by timers so the interleaving is exact: register
// idle, sweep busy, drain, sweep. The window override is long enough that no
// armed timer can fire underneath the sequence.
func TestWatchdogRestartsTheWindowWhenTheTurnDrains(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-drain", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 10*time.Second)
	liveness := installClaudeLivenessForThread(app, id)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	// t=45: the turn is running, so this window measured the turn.
	liveness.activeTurns.Store(1)
	app.sweepUnconfirmedClaudeLiveApply("cmd-1")
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); !ok {
		t.Fatal("the busy sweep consumed an apply still queued behind a turn")
	}

	// t=46: the turn drains. The CLI is only now free to read the command.
	liveness.activeTurns.Store(0)

	// t=90: an idle sweep, but the command has been in the CLI's hands for a
	// moment, not for a window. It must start a fresh one instead of deciding.
	app.sweepUnconfirmedClaudeLiveApply("cmd-1")
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); !ok {
		t.Fatal("the first idle sweep after the turn drained decided an apply the CLI had barely received")
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the axis was degraded by a window the running turn had consumed")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the still-pending xhigh", got)
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "the drained turn's successor window has only just begun")

	// t=135: a full idle window has now elapsed with no answer. Verdict time.
	app.sweepUnconfirmedClaudeLiveApply("cmd-1")
	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("the second idle sweep did not decide an apply that had a full window to itself")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("the deciding sweep left its entry in the registry")
	}
}

// TestWatchdogRestartsTheWindowWhenATurnDrainsInsideTheFirstOne is the same
// rule at the one moment no sweep can observe. A command registered while a
// turn is already running is a stdin message queued behind it; if that turn
// ends before the first window expires, the only sweep that ever runs sees an
// idle thread and a full window's worth of silence — evidence it does not
// have. The deferral is therefore stamped at REGISTRATION, not by a sweep.
func TestWatchdogRestartsTheWindowWhenATurnDrainsInsideTheFirstOne(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-drain-early", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", FastMode: true}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 80*time.Millisecond)
	liveness := installClaudeLivenessForThread(app, id)
	liveness.activeTurns.Store(1)

	prev := optimistic
	prev.FastMode = false
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{FastMode: claude.FastModeOn}, claude.LiveApplyReceipt{FastCommandUUID: "cmd-fast"})

	// Drains well inside the first window, so the t=80 sweep sees an idle
	// thread. Without the registration-time stamp it declines there.
	liveness.activeTurns.Store(0)

	assertNoRestartWithin(t, started, 150*time.Millisecond,
		"the turn holding the command drained inside the first window, so no window has measured the CLI yet")
	if _, ok := app.peekClaudeLiveConfigApply("cmd-fast"); !ok {
		t.Fatal("a sweep decided an apply whose whole window had been held by a turn")
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("the fast axis was degraded on a window the running turn had consumed")
	}

	// The successor window is a real one: with the CLI still silent through
	// it, the apply is declined exactly as an undeferred one would be.
	waitRestart(t, started, id)
	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("the successor window did not decide the unanswered fast apply")
	}
	if launchOptsForThread(t, app, id).FastMode {
		t.Fatal("launchOpts still claims fast mode is on; nothing ever confirmed it")
	}
}

// TestWatchdogSpendsATurnDeferralOnlyOnce bounds the extension. The deferral
// is a one-shot mark, so a command that was queued behind a turn buys exactly
// one extra window per observed deferral rather than an unbounded lease on
// staying pending — otherwise a single early turn would make the axis
// permanently unresolvable.
func TestWatchdogSpendsATurnDeferralOnlyOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-watchdog-deferral-once", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)
	armWatchdogWindow(app, 10*time.Second)
	liveness := installClaudeLivenessForThread(app, id)
	liveness.activeTurns.Store(1)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})
	liveness.activeTurns.Store(0)

	// First idle sweep spends the registration-time deferral.
	app.sweepUnconfirmedClaudeLiveApply("cmd-1")
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); !ok {
		t.Fatal("the first idle sweep decided instead of spending the deferral")
	}
	// Second idle sweep has nothing left to spend and must decide.
	app.sweepUnconfirmedClaudeLiveApply("cmd-1")
	waitRestart(t, started, id)
	if _, ok := app.peekClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("a spent deferral kept extending the window")
	}
}
