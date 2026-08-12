package main

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

func TestParseEffortSetText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want provider.ReasoningEffort
		ok   bool
	}{
		{"success reply", "Set effort level to xhigh (this session only): use this for the hardest problems", provider.EffortXHigh, true},
		{"low tier", "Set effort level to low (this session only): minimal reasoning", provider.EffortLow, true},
		{"readback must not match", "Current effort level: high (session override)", "", false},
		{"invalid argument must not match", "Invalid argument: turbo. Valid: low, medium, high, xhigh, max", "", false},
		{"unrepresentable tier does not sync", "Set effort level to ultracode (this session only): maximum depth", "", false},
		{"auto tier does not sync", "Set effort level to auto (this session only): adaptive", "", false},
		{"empty rest", "Set effort level to ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseEffortSetText(tc.text)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseEffortSetText(%q) = (%q, %v), want (%q, %v)", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// seedClaudeLiveConfigThread creates a Claude thread with a registered fake
// session (no live-update surface, so a fallback reconcile takes the restart
// path) and returns the started-restart channel.
func seedClaudeLiveConfigThread(t *testing.T, app *App, id, token string, launchOpts provider.SessionOptions) chan string {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Claude)
	thread.Model = "claude-opus-5"
	thread.ReasoningEffort = string(provider.EffortXHigh)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.mu.Lock()
	app.sessions[id] = session{provider: thread.Provider, token: token, launchOpts: launchOpts}
	app.mu.Unlock()

	started := make(chan string, 4)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}
	app.configReconnectPollIntervalOverride = 10 * time.Millisecond
	app.configReconnectQuietWindowOverride = time.Nanosecond
	return started
}

func commandResultEvent(t *testing.T, commandUUID, text string) provider.ProviderEvent {
	t.Helper()
	evt := provider.ProviderEvent{Kind: provider.EventCommandResult, Content: text, ContentPresent: true}
	if commandUUID != "" {
		meta, err := json.Marshal(provider.CommandResultMeta{CommandUUID: commandUUID})
		if err != nil {
			t.Fatalf("marshal command result meta: %v", err)
		}
		evt.Meta = meta
	}
	return evt
}

func launchOptsForThread(t *testing.T, app *App, threadID string) provider.SessionOptions {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	sess, ok := app.sessions[threadID]
	if !ok {
		t.Fatalf("no session registered for %s", threadID)
	}
	return sess.launchOpts
}

// TestObserveClaudeCommandResultConfirmsEffortApply — the expected success
// text settles the pending apply with no side effects: launchOpts keeps the
// optimistically written value, no degraded mark, no restart.
func TestObserveClaudeCommandResultConfirmsEffortApply(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-live-effort-ok", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Set effort level to xhigh (this session only): deep reasoning"))

	if _, ok := app.takeClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("pending apply survived its confirmation")
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("confirmed apply marked the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the confirmed xhigh", got)
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "a live effort apply was confirmed")
}

// TestObserveClaudeCommandResultUnexpectedAnswerFallsBackToRestart — an
// answer that is not the expected state change ("Invalid argument", wording
// drift) means the session is NOT running the requested value: the axis
// reverts in launchOpts, goes degraded so the reconciler stops re-sending,
// and the deferred-restart watcher converges.
func TestObserveClaudeCommandResultUnexpectedAnswerFallsBackToRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-live-effort-bad", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Invalid argument: xhigh. Valid: low, medium, high"))

	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("declined apply did not mark the axis degraded")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
	waitRestart(t, started, id)
}

// TestObserveClaudeCommandResultFastUnavailableIsAccepted — "Fast mode
// unavailable" is an account-level gate a restart would hit identically, so
// it settles as parity: no revert, no degraded mark, no restart.
func TestObserveClaudeCommandResultFastUnavailableIsAccepted(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-live-fast-gate", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", FastMode: true}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.FastMode = false
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{FastMode: claude.FastModeOn}, claude.LiveApplyReceipt{FastCommandUUID: "cmd-fast"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-fast", "Fast mode unavailable: This account does not have usage credits enabled."))

	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("account gate marked the axis degraded — a restart hits the identical gate")
	}
	if got := launchOptsForThread(t, app, id).FastMode; !got {
		t.Fatal("launchOpts fast mode reverted — the requested value must stay so the reconciler does not loop")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "fast mode hit an account gate a restart cannot fix")
}

// TestObserveClaudeCommandResultSyncsUserTypedEffort — a user-typed /effort
// (no pending apply) already changed the session, so the thread row, the
// remembered profile, and launchOpts must follow or the next restart would
// silently undo it.
func TestObserveClaudeCommandResultSyncsUserTypedEffort(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-user-effort", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "", "Set effort level to low (this session only): minimal reasoning"))

	thread, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if thread.ReasoningEffort != string(provider.EffortLow) {
		t.Fatalf("thread effort = %q, want the user-typed low", thread.ReasoningEffort)
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortLow {
		t.Fatalf("launchOpts effort = %q, want low — a mismatch would trigger a pointless restart", got)
	}
	profile, err := app.store.GetChatModelProfile(string(provider.Claude), "claude-opus-5")
	if err != nil {
		t.Fatalf("GetChatModelProfile() error = %v", err)
	}
	if profile.ReasoningEffort != string(provider.EffortLow) {
		t.Fatalf("remembered profile effort = %q, want low", profile.ReasoningEffort)
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "the session already runs the user-typed effort")

	// The readback form must not sync: it reports state, and treating it as
	// a change would let a stale value overwrite a pending config change.
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "", "Current effort level: high (session override)"))
	thread, err = app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if thread.ReasoningEffort != string(provider.EffortLow) {
		t.Fatalf("thread effort = %q after a readback, want low unchanged", thread.ReasoningEffort)
	}
}

// TestPurgeClaudeLiveConfigStateIsTokenScoped — unregistering one session
// drops exactly its pending applies and degraded marks; another session's
// state survives.
func TestPurgeClaudeLiveConfigStateIsTokenScoped(t *testing.T) {
	app := newTestAppWithStore(t)
	opts := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortHigh}
	app.registerClaudeLiveConfigApplies("t1", "tok-a", opts,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-a"})
	app.registerClaudeLiveConfigApplies("t2", "tok-b", opts,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-b"})
	app.markClaudeLiveApplyDegraded("tok-a", claudeLiveApplyAxisFast)
	app.markClaudeLiveApplyDegraded("tok-b", claudeLiveApplyAxisFast)

	app.mu.Lock()
	app.purgeClaudeLiveConfigStateLocked("tok-a")
	app.mu.Unlock()

	if _, ok := app.takeClaudeLiveConfigApply("cmd-a"); ok {
		t.Fatal("purged session's pending apply survived")
	}
	if app.claudeLiveApplyIsDegraded("tok-a", claude.LiveUpdate{FastMode: claude.FastModeOff}) {
		t.Fatal("purged session's degraded mark survived")
	}
	if _, ok := app.takeClaudeLiveConfigApply("cmd-b"); !ok {
		t.Fatal("other session's pending apply was purged")
	}
	if !app.claudeLiveApplyIsDegraded("tok-b", claude.LiveUpdate{FastMode: claude.FastModeOff}) {
		t.Fatal("other session's degraded mark was purged")
	}
}

// TestRegisterClaudeLiveConfigAppliesEvictsStaleEntries — a session that
// died without unregistering (crash during app shutdown) cannot accumulate
// entries forever: inserts evict anything past the staleness bound.
func TestRegisterClaudeLiveConfigAppliesEvictsStaleEntries(t *testing.T) {
	app := newTestAppWithStore(t)
	opts := provider.SessionOptions{Model: "claude-opus-5"}
	app.registerClaudeLiveConfigApplies("t1", "tok-a", opts,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-old"})
	app.mu.Lock()
	stale := app.claudeLiveConfigApplies["cmd-old"]
	stale.sentAt = time.Now().Add(-claudeLiveApplyStaleAfter - time.Minute)
	app.claudeLiveConfigApplies["cmd-old"] = stale
	app.mu.Unlock()

	app.registerClaudeLiveConfigApplies("t2", "tok-b", opts,
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-new"})

	if _, ok := app.takeClaudeLiveConfigApply("cmd-old"); ok {
		t.Fatal("stale entry survived the insert-time eviction")
	}
	if _, ok := app.takeClaudeLiveConfigApply("cmd-new"); !ok {
		t.Fatal("fresh entry missing")
	}
}

// TestSessionTakePurgesClaudeLiveConfigState pins the wiring, not just the
// purge function: every registry-removal path must clear the session's
// pending applies and degraded marks, and take() is the one the deferred
// restart and StopSession use.
func TestSessionTakePurgesClaudeLiveConfigState(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-take-purge", "tok-1"
	seedClaudeLiveConfigThread(t, app, id, token, provider.SessionOptions{Model: "claude-opus-5"})
	app.registerClaudeLiveConfigApplies(id, token, provider.SessionOptions{},
		claude.LiveUpdate{Effort: "low"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})
	app.markClaudeLiveApplyDegraded(token, claudeLiveApplyAxisFast)

	if _, ok := app.sessionManager().take(id); !ok {
		t.Fatal("take() found no session")
	}
	if _, ok := app.takeClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("pending apply survived take()")
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOff}) {
		t.Fatal("degraded mark survived take()")
	}
}

// TestSupersededApplyAnswerIsIgnored — a newer apply for the same axis
// tombstones the older one; the OLD command's rejection (which arrives
// first — the CLI executes sequentially) must not degrade the axis or
// revert the value the newer apply owns.
func TestSupersededApplyAnswerIsIgnored(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-supersede", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	app.registerClaudeLiveConfigApplies(id, token, provider.SessionOptions{ReasoningEffort: provider.EffortMedium},
		claude.LiveUpdate{Effort: "max"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-old"})
	app.registerClaudeLiveConfigApplies(id, token, provider.SessionOptions{ReasoningEffort: provider.EffortMax},
		claude.LiveUpdate{Effort: "high"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-new"})

	// Old command's answer: a rejection. Must be swallowed by the tombstone.
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-old", "Invalid argument: max. Valid options are: low, medium, high"))
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "high"}) {
		t.Fatal("superseded apply's rejection degraded the axis")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the newer apply's high untouched", got)
	}

	// New command's answer confirms normally.
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-new", "Set effort level to high (this session only): standard"))
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "high"}) {
		t.Fatal("confirmed apply degraded the axis")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "the newer apply confirmed")
}

// TestRolledBackApplyAnswerIsIgnored — after a mid-sequence send failure
// the caller rolls back; a command that DID reach the wire answers into its
// tombstone and must not resolve, while launchOpts is restored so the
// restart fallback sees a genuine diff.
func TestRolledBackApplyAnswerIsIgnored(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-rollback", "tok-1"
	prev := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortHigh}
	optimistic := prev
	optimistic.ReasoningEffort = provider.EffortLow
	seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	receipt := claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1", FastCommandUUID: "cmd-2"}
	app.registerClaudeLiveConfigApplies(id, token, prev, claude.LiveUpdate{Effort: "low", FastMode: claude.FastModeOff}, receipt)
	app.rollbackClaudeLiveConfigApplies(id, token, prev, receipt)

	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the restored high", got)
	}
	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-1", "Set effort level to low (this session only): minimal"))
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q after tombstoned confirm, want high", got)
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "low"}) {
		t.Fatal("tombstoned answer degraded the axis")
	}
}

// TestCancelledCommandLifecycleRevertsApply — a queued config command the
// CLI cancels produces no output; the cancelled lifecycle mark is the only
// signal, and it must revert the optimistic write (no degraded mark — the
// command never ran) and re-arm convergence.
func TestCancelledCommandLifecycleRevertsApply(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-cancelled", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: "cmd-1", State: provider.CommandCancelled})
	if err != nil {
		t.Fatalf("marshal lifecycle meta: %v", err)
	}
	app.observeClaudeCommandLifecycle(provider.ProviderEvent{Kind: provider.EventCommandLifecycle, Meta: meta})

	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("cancelled command degraded the axis — the session's command channel is not suspect")
	}
	// The fake session has no live-update surface, so re-convergence takes
	// the restart path; what matters is that convergence re-armed at all.
	waitRestart(t, started, id)
}

// TestUncorrelatedRejectionSettlesPendingApply — a CLI that emits no
// command_lifecycle stamps no uuid on command output. A rejection text must
// still settle the pending apply (degrade + revert + restart) instead of
// stranding the optimistic write silently.
func TestUncorrelatedRejectionSettlesPendingApply(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-nolifecycle", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "", "Invalid argument: xhigh. Valid options are: low, medium, high"))

	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("uncorrelated rejection did not degrade the axis")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortHigh {
		t.Fatalf("launchOpts effort = %q, want the reverted high", got)
	}
	waitRestart(t, started, id)
}

// TestUncorrelatedSuccessConfirmsPendingApply — same no-lifecycle CLI, but
// the command succeeded: the pending apply confirms and nothing restarts.
func TestUncorrelatedSuccessConfirmsPendingApply(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-nolifecycle-ok", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.ReasoningEffort = provider.EffortHigh
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{Effort: "xhigh"}, claude.LiveApplyReceipt{EffortCommandUUID: "cmd-1"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "", "Set effort level to xhigh (this session only): deep"))

	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("uncorrelated success degraded the axis")
	}
	if got := launchOptsForThread(t, app, id).ReasoningEffort; got != provider.EffortXHigh {
		t.Fatalf("launchOpts effort = %q, want the confirmed xhigh", got)
	}
	if _, ok := app.takeClaudeLiveConfigApply("cmd-1"); ok {
		t.Fatal("pending apply survived its uncorrelated confirmation")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "the uncorrelated apply confirmed")
}

// TestUserTypedEffortWithUUIDStillSyncs — composer sends carry uuids too;
// an unrecognized uuid on an effort success must be treated as user-typed
// and sync the row, not be discarded as AO-authored.
func TestUserTypedEffortWithUUIDStillSyncs(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-user-uuid", "tok-1"
	seedClaudeLiveConfigThread(t, app, id, token,
		provider.SessionOptions{Model: "claude-opus-5", ReasoningEffort: provider.EffortXHigh})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "user-cmd-9", "Set effort level to low (this session only): minimal"))

	thread, err := app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if thread.ReasoningEffort != string(provider.EffortLow) {
		t.Fatalf("thread effort = %q, want the user-typed low", thread.ReasoningEffort)
	}
}

// TestFastModeOnConfirmsByContainment — enabling fast mode can implicitly
// switch the model and the reply may lead with that; the ON confirmation is
// containment, not prefix (claude-wire.md §"Live config commands").
func TestFastModeOnConfirmsByContainment(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-fast-on", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", FastMode: true}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.FastMode = false
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{FastMode: claude.FastModeOn}, claude.LiveApplyReceipt{FastCommandUUID: "cmd-fast"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-fast", "Model set to claude-opus-4-8. Fast mode ON"))

	if app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("successful fast enable degraded the axis")
	}
	if !launchOptsForThread(t, app, id).FastMode {
		t.Fatal("launchOpts fast mode reverted on a successful enable")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "fast mode enabled successfully")
}

// TestFastModeSDKUnavailableFallsBackToRestart — unlike the account-credits
// gate, "not available in the Agent SDK" IS fixed by a restart with the
// spawn opt-in. Reaching it means the opt-in gate was wrong; the restart is
// the correct recovery.
func TestFastModeSDKUnavailableFallsBackToRestart(t *testing.T) {
	app := newTestAppWithStore(t)
	id, token := "thread-fast-sdk", "tok-1"
	optimistic := provider.SessionOptions{Model: "claude-opus-5", FastMode: true}
	started := seedClaudeLiveConfigThread(t, app, id, token, optimistic)

	prev := optimistic
	prev.FastMode = false
	app.registerClaudeLiveConfigApplies(id, token, prev,
		claude.LiveUpdate{FastMode: claude.FastModeOn}, claude.LiveApplyReceipt{FastCommandUUID: "cmd-fast"})

	app.observeClaudeCommandResult(id, token,
		commandResultEvent(t, "cmd-fast", "Fast mode unavailable: Fast mode is not available in the Agent SDK"))

	if !app.claudeLiveApplyIsDegraded(token, claude.LiveUpdate{FastMode: claude.FastModeOn}) {
		t.Fatal("SDK gate did not degrade the axis — the restart adds the opt-in")
	}
	if launchOptsForThread(t, app, id).FastMode {
		t.Fatal("launchOpts fast mode kept true after the SDK gate declined it")
	}
	waitRestart(t, started, id)
}

// TestResolveSkipsSessionScopedEffectsWhenSessionGone — a drained answer
// for a session that was already replaced must not degrade, error, or
// restart anything: its registry state went with it and the replacement
// spawned from the row.
func TestResolveSkipsSessionScopedEffectsWhenSessionGone(t *testing.T) {
	app := newTestAppWithStore(t)
	id := "thread-stale-answer"
	started := seedClaudeLiveConfigThread(t, app, id, "tok-new", provider.SessionOptions{Model: "claude-opus-5"})

	// Pending registered under the OLD token; the session map now holds
	// tok-new (a replacement).
	pending := claudeLiveConfigApply{
		threadID:     id,
		sessionToken: "tok-old",
		axis:         claudeLiveApplyAxisEffort,
		requested:    "xhigh",
		prevEffort:   provider.EffortHigh,
	}
	app.resolveClaudeLiveConfigApply(pending, "Invalid argument: xhigh")

	if app.claudeLiveApplyIsDegraded("tok-old", claude.LiveUpdate{Effort: "xhigh"}) {
		t.Fatal("dead session's answer marked the old token degraded")
	}
	assertNoRestartWithin(t, started, 50*time.Millisecond, "the answering session was already replaced")
}

// TestLiveApplySessionConfigSerializesPerThread pins that the reconciler's
// apply section runs under the per-thread config-apply lock: a holder
// blocks a concurrent apply for the SAME thread, while another thread's
// apply proceeds — the read-modify-write over launchOpts admits exactly
// one writer per thread.
func TestLiveApplySessionConfigSerializesPerThread(t *testing.T) {
	app := newTestAppWithStore(t)
	idA, idB := "thread-serial-a", "thread-serial-b"
	seedClaudeLiveConfigThread(t, app, idA, "tok-a", provider.SessionOptions{Model: "claude-opus-5"})
	seedClaudeLiveConfigThread(t, app, idB, "tok-b", provider.SessionOptions{Model: "claude-opus-5"})

	unlock := app.configApplyLocks().Lock(idA)
	sameDone := make(chan struct{})
	otherDone := make(chan struct{})
	go func() {
		app.liveApplySessionConfig(idA)
		close(sameDone)
	}()
	go func() {
		app.liveApplySessionConfig(idB)
		close(otherDone)
	}()

	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("another thread's apply blocked on this thread's config lock — the lock must be per-thread")
	}
	select {
	case <-sameDone:
		t.Fatal("same-thread apply ran while the config-apply lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-sameDone:
	case <-time.After(2 * time.Second):
		t.Fatal("same-thread apply never ran after the lock was released")
	}
}
