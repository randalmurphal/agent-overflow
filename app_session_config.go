package main

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
)

// This file owns applying thread-config changes (model, effort, fast mode,
// runtime mode, context window, ...) to a live provider session.
//
// The old policy was "any session-affecting change kills and respawns the
// provider process", which aborted in-flight turns and reaped backgrounded
// tasks. The current policy:
//
//  1. Live-apply when the provider supports it — Claude via the set_model /
//     set_permission_mode control_requests (verified on claude 2.1.205) plus
//     the /effort and /fast provider-executed commands (verified 2.1.219;
//     confirmed asynchronously, see app_claude_live_config.go), Codex via
//     the per-turn turn/start overrides it re-reads every Send. No process
//     restart, in-flight work untouched.
//  2. Otherwise restart — but never while the thread is busy. A restart
//     kills the working turn AND any live backgrounded tasks, so it is
//     deferred until the thread is fully quiet (no active turn, no queued
//     or in-flight sends, no running background tasks) and then fired by
//     a per-thread watcher.
//
// The reconciler is convergence-based rather than delta-based: it diffs the
// live session's launch options (session.launchOpts) against the thread
// row's current options, so stacked changes and changes that landed while a
// restart was pending all settle in one pass.

const (
	// defaultConfigReconnectPollInterval is how often the deferred-restart
	// watcher re-checks a busy thread.
	defaultConfigReconnectPollInterval = time.Second
	// defaultConfigReconnectQuietWindow is the minimum session inactivity
	// before a deferred restart may fire. It closes the gap where a send
	// was just written but the wire hasn't opened the turn yet (triage's
	// ActiveTurn is still nil for a few ms), mirroring the idle reaper's
	// activity-floor technique.
	defaultConfigReconnectQuietWindow = 3 * time.Second
)

func (a *App) configReconnectPollInterval() time.Duration {
	if a.configReconnectPollIntervalOverride > 0 {
		return a.configReconnectPollIntervalOverride
	}
	return defaultConfigReconnectPollInterval
}

func (a *App) configReconnectQuietWindow() time.Duration {
	if a.configReconnectQuietWindowOverride > 0 {
		return a.configReconnectQuietWindowOverride
	}
	return defaultConfigReconnectQuietWindow
}

// reconcileSessionConfig converges the thread's live session (if any) onto
// the thread row's current config: live-apply when possible, deferred
// restart otherwise. Safe to call with or without the per-thread action
// lock held — it never takes it; restarts happen on the watcher goroutine,
// which does.
func (a *App) reconcileSessionConfig(threadID string) {
	if a.liveApplySessionConfig(threadID) {
		return
	}
	a.schedulePendingConfigReconnect(threadID)
}

// liveApplySessionConfig attempts to bring the live session in line with
// the thread row without a restart. Returns true when the session now
// matches the row (or there is no session to reconcile — the next lazy
// start reads the row directly); false when only a restart can converge.
//
// One apply at a time per thread (configApplyLocks): the body is a
// read-modify-write over session.launchOpts, and two concurrent reconciles
// would both plan against the same snapshot and both send the same change
// — a duplicate zero-cost command at best, an out-of-order launchOpts
// commit at worst. Serialization also fixes the commit order: whoever
// sends last also commits last.
func (a *App) liveApplySessionConfig(threadID string) bool {
	unlock := a.configApplyLocks().Lock(threadID)
	defer unlock()
	// A start already in flight read the thread row at some point before
	// this change persisted; wait for it so we diff against the session
	// that actually exists.
	if _, err := a.waitForStartingSession(threadID); err != nil {
		// The in-flight start failed; there is no session to reconcile and
		// the failed start already surfaced its own error.
		return true
	}
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return true
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("thread %s: config reconcile: load thread: %v", threadID, err)
		return true
	}
	// Mirror startSessionNow's sanitize step (without persisting) so the
	// comparison sees the same coerced view a spawn would.
	opts, _, err := a.buildSessionOptions(a.sanitizeThreadModelSettings(thread))
	if err != nil {
		log.Printf("thread %s: config reconcile: build options: %v", threadID, err)
		a.emitErrorToThread(threadID, "config change could not be applied: "+err.Error())
		return true
	}

	switch {
	case sess.claude != nil:
		update, ok := claude.PlanLiveUpdate(sess.launchOpts, opts)
		if !ok {
			return false
		}
		// A session that already answered a live-config command with
		// something other than the expected state change is not asked
		// again — the watcher would otherwise re-send the same command
		// on every poll. See app_claude_live_config.go.
		if a.claudeLiveApplyIsDegraded(sess.token, update) {
			return false
		}
		// The command-channel axes (effort, fast mode) confirm
		// asynchronously via their EventCommandResult. The preSend hook
		// fires after validation and before any wire write: registration
		// and the optimistic launchOpts write must both precede the send,
		// or a fast confirmation could arrive unmatched — and a fast
		// REJECTION's revert could then be clobbered by the optimistic
		// write, leaving launchOpts claiming a config the session refused
		// (which would also make the restart fallback see "already
		// converged" and skip the restart).
		var rollback func()
		err = sess.claude.ApplyLiveUpdate(a.lifeCtx(), update, func(receipt claude.LiveApplyReceipt) {
			a.registerClaudeLiveConfigApplies(threadID, sess.token, sess.launchOpts, update, receipt)
			a.sessionManager().updateLaunchOpts(threadID, sess.token, opts)
			rollback = func() {
				a.rollbackClaudeLiveConfigApplies(threadID, sess.token, sess.launchOpts, receipt)
			}
		})
		if err != nil {
			if rollback != nil {
				rollback()
			}
			// ErrLiveUpdateRequiresRestart is the expected "can't do this
			// one live" signal (bypassPermissions escalation, fast-mode
			// enable without the spawn opt-in, /effort or /fast not
			// advertised, transcript pending the resume-at repair);
			// anything else is a wire failure. Either way the restart path
			// is the convergence fallback — surface unexpected failures
			// first.
			if err != claude.ErrLiveUpdateRequiresRestart {
				log.Printf("thread %s: claude live config update failed: %v", threadID, err)
				a.emitErrorToThread(threadID, "live config change failed, restarting session to apply: "+err.Error())
			}
			return false
		}
		return true
	case sess.codex != nil:
		update, ok := codex.PlanLiveUpdate(sess.launchOpts, opts)
		if !ok {
			return false
		}
		push := codex.PlanThreadSettingsPush(sess.launchOpts, opts)
		sess.codex.ApplyLiveUpdate(update)
		a.pushCodexThreadSettings(threadID, sess.codex, push)
	default:
		// claudetui has no live-update surface; restart is the only path.
		return false
	}

	a.sessionManager().updateLaunchOpts(threadID, sess.token, opts)
	return true
}

// codexSettingsPushTimeout bounds the `thread/settings/update` round trip.
// The app-server answers as soon as it has queued the core op, so this is a
// local-IPC deadline, not a model-work one — it exists so a wedged
// app-server can't hold a binding call for the 30s default.
const codexSettingsPushTimeout = 5 * time.Second

// pushCodexThreadSettings lands the model / effort / service-tier part of a
// live config change on the Codex thread immediately, instead of leaving it
// to ride the next turn/start.
//
// Two rules, both deliberate:
//
//  1. **Between turns only.** A push while a turn is in flight is skipped
//     entirely rather than deferred. Nothing is lost by skipping: the same
//     values are already in the session's turn config, so the next
//     turn/start asserts them exactly as it did before this call existed.
//     (The check is best-effort against a concurrent send from another
//     goroutine — but that race is benign in both directions, because a push
//     that lands beside a turn/start writes the very values that turn/start
//     is itself carrying.)
//
//  2. **Never user-facing on failure.** A failed or unsupported push is a
//     lost optimization, not a lost setting. It is logged; the thread's
//     behavior is unchanged. Codex's own rejection of an override arrives
//     separately as an `error` notification, which is already thread error
//     state, and an echo that disagrees with the push is surfaced by
//     verifyThreadSettingsEcho.
func (a *App) pushCodexThreadSettings(threadID string, sess *codex.Session, push codex.ThreadSettingsPush) {
	if sess == nil || push.Empty() {
		return
	}
	if a.threadTurnInFlight(threadID) {
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexSettingsPushTimeout)
	defer cancel()
	if err := sess.PushThreadSettings(ctx, push); err != nil {
		log.Printf("thread %s: codex thread/settings/update failed, change will apply on the next turn: %v", threadID, err)
	}
}

// threadTurnInFlight reports whether a provider turn is currently running on
// the thread. Narrower than threadConfigBusy on purpose: that one answers
// "would restarting the session destroy live work" (and so also weighs
// background tasks, queued sends, and a quiet window), while this answers
// only "is a turn running right now", which is the sole thing a settings
// push must not land beside.
func (a *App) threadTurnInFlight(threadID string) bool {
	if a.triage != nil {
		if live := a.triage.LiveStateSnapshotForThread(threadID); live.ActiveTurn != nil {
			return true
		}
	}
	if sess, ok := a.sessionManager().get(threadID); ok && sess.liveness != nil {
		return sess.liveness.activeTurns.Load() > 0
	}
	return false
}

// schedulePendingConfigReconnect arms the per-thread deferred-restart
// watcher. Idempotent — a second config change while one is pending folds
// into the same watcher, which reconciles against the latest thread row
// when it fires.
func (a *App) schedulePendingConfigReconnect(threadID string) {
	a.mu.Lock()
	if a.pendingConfigReconnects == nil {
		a.pendingConfigReconnects = make(map[string]bool)
	}
	if a.pendingConfigReconnects[threadID] {
		a.mu.Unlock()
		return
	}
	a.pendingConfigReconnects[threadID] = true
	a.mu.Unlock()
	go a.pendingConfigReconnectWatcher(threadID)
}

func (a *App) clearPendingConfigReconnect(threadID string) {
	a.mu.Lock()
	delete(a.pendingConfigReconnects, threadID)
	a.mu.Unlock()
}

// pendingConfigReconnectWatcher waits for the thread to go fully quiet and
// then restarts its session so the pending config takes effect. It re-runs
// the live-apply check right before restarting: if the session died and a
// fresh one spawned from the new row (or a newer change became
// live-appliable), the restart is skipped.
func (a *App) pendingConfigReconnectWatcher(threadID string) {
	defer a.clearPendingConfigReconnect(threadID)
	ticker := time.NewTicker(a.configReconnectPollInterval())
	defer ticker.Stop()
	for {
		if a.shuttingDown.Load() {
			return
		}
		if !a.hasActiveSession(threadID) {
			// No live session: the next lazy start reads the thread row
			// with the new config. Nothing to restart.
			return
		}
		if !a.threadConfigBusy(threadID) {
			unlock := a.threadLocks().Lock(threadID)
			done := a.fireDeferredConfigReconnectLocked(threadID)
			unlock()
			if done {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-a.lifeCtx().Done():
			return
		}
	}
}

// fireDeferredConfigReconnectLocked performs the deferred restart under the
// per-thread action lock (serializing against sends, steers, and other
// thread mutations). Returns false when the thread turned busy again
// between the watcher's check and lock acquisition — the watcher keeps
// waiting.
func (a *App) fireDeferredConfigReconnectLocked(threadID string) bool {
	if a.threadConfigBusy(threadID) {
		return false
	}
	// The world may have moved while we waited: the session may have been
	// replaced (new launch config already matches) or the remaining delta
	// may have become live-appliable. Re-check before killing anything.
	if a.liveApplySessionConfig(threadID) {
		return true
	}
	if err := a.startSession(context.Background(), threadID); err != nil {
		log.Printf("thread %s: deferred config reconnect failed: %v", threadID, err)
		a.emitErrorToThread(threadID, "config change failed to apply on session restart: "+err.Error())
	}
	return true
}

// threadConfigBusy reports whether restarting the thread's session now
// would destroy live work: an active turn, queued or in-flight sends,
// recent wire activity (a just-written send whose turn hasn't opened yet),
// or running backgrounded tasks (a restart reaps their processes).
func (a *App) threadConfigBusy(threadID string) bool {
	if a.triage != nil {
		live := a.triage.LiveStateSnapshotForThread(threadID)
		if live.ActiveTurn != nil || len(live.QueueItems) > 0 || len(live.FlushedItems) > 0 {
			return true
		}
	}

	a.flushDispatchMu.Lock()
	inflight := a.flushDispatchInflightItems[threadID]
	a.flushDispatchMu.Unlock()
	if inflight > 0 {
		return true
	}

	if sess, ok := a.sessionManager().get(threadID); ok && sess.liveness != nil {
		if sess.liveness.activeTurns.Load() > 0 {
			return true
		}
		last := time.Unix(0, sess.liveness.lastActivityUnixNano.Load())
		if time.Since(last) < a.configReconnectQuietWindow() {
			return true
		}
	}

	running, err := a.hasRunningBackgroundTasks(threadID)
	if err != nil {
		// Can't verify — err on the side of not killing anything.
		log.Printf("thread %s: config reconcile busy check: %v", threadID, err)
		return true
	}
	return running
}
