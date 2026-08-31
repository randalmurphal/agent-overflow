package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/sessionruntime"
	"agent-overflow/internal/triage"
)

// This file owns the asynchronous half of Claude's live config applies.
//
// The reconciler (app_session_config.go) morphs a live session onto the
// thread row's config without a restart where the wire allows it. Model and
// permission mode ride control_requests the CLI acks synchronously; effort
// and fast mode ride the CLI's provider-executed `/effort` and `/fast`
// slash commands, whose only "ack" is the command output arriving later as
// an EventCommandResult. The CLI answers an argument it rejects with a
// non-error text ("Invalid argument: …") rather than a wire failure, so a
// fire-and-forget apply could leave the session's launch-options snapshot
// claiming a config the process never adopted.
//
// The contract here closes that loop:
//
//  1. ApplyLiveUpdate invokes its preSend hook — after validation, before
//     any wire write — with the client-minted uuids of the command sends
//     (LiveApplyReceipt). The reconciler registers each as a pending apply
//     alongside the value it then optimistically writes into launchOpts and
//     the value to restore on failure. Registration strictly precedes the
//     send, so a confirmation can never arrive unmatched.
//  2. The parser stamps the same uuid onto the command output's
//     CommandResultMeta (correlated from the command_lifecycle window).
//  3. observeClaudeCommandResult matches output to pending apply. The
//     verdict then comes from the CLI's STRUCTURED read-back where the
//     binary offers one — `get_settings.applied.effort` states the tier
//     that will actually be sent to the API — and from the reply TEXT only
//     where it does not (an older CLI answering the subtype with
//     "Unsupported control request subtype"). Text matching is a UI string
//     with no compatibility contract: a reworded reply reads as a rejection
//     and costs the user a restart, which is exactly what the structured
//     path removes. Confirmation is a no-op; anything else reverts the
//     launchOpts axis, marks the (session, axis) pair degraded so the
//     reconciler stops retrying it live, surfaces the CLI's answer as
//     thread error state, and arms the deferred-restart watcher — the same
//     convergence fallback every other non-live-appliable change uses.
//  4. A command the CLI cancels instead of executing (its
//     command_lifecycle reaches `cancelled` — e.g. the queued message died
//     with an interrupt) reverts the axis without degrading, and the
//     watcher re-converges: the command never ran, so the session is not
//     suspect, just not updated.
//
//  5. Silence is a THIRD outcome, not a rest state. A queued command can
//     die without either channel saying so (the CLI drops it, the process
//     wedges, a lifecycle frame goes missing), and the optimistic
//     launchOpts write would then claim a config the session is not
//     running for the rest of its life — the same wrong-state a rejection
//     produces, just with nothing to react to. So every registration arms
//     a bounded watchdog (claudeLiveApplyConfirmAfter). It re-arms while
//     the thread has a turn in flight, because a slash command written to
//     stdin genuinely waits for the turn to drain; otherwise it settles
//     the entry the same way an answer would — through the structured
//     read-back when the session offers one (the command may well have RUN
//     and only its output went missing), and through
//     declineClaudeLiveConfigApply when it does not, which reverts the
//     axis, surfaces the silence as thread error state, and hands
//     convergence to the deferred restart.
//
// Entries are keyed to the session token and purged whenever the session
// leaves the registry (unregister, take, takeIdle, snapshotAndClear), and
// the replacement session spawns from the thread row, which already holds
// the requested config — so a session that dies with an apply outstanding
// needs no watchdog verdict. The 24h insert-time eviction is a backstop for
// tokens that never pass through any of those.

type claudeLiveConfigApply = sessionruntime.ClaudeLiveApply

const (
	claudeLiveApplyAxisEffort = "effort"
	claudeLiveApplyAxisFast   = "fast"
	// claudeLiveApplyStaleAfter bounds the pending-apply registry: entries
	// older than this are evicted on the next insert. Generous on purpose —
	// a command queued behind a long turn confirms hours later at worst,
	// and a stale entry's only cost is a few bytes; the eviction exists so
	// tokens that somehow never pass through a registry-removal purge
	// cannot accumulate entries forever.
	claudeLiveApplyStaleAfter = 24 * time.Hour
	// claudeLiveApplyConfirmAfter bounds how long AO waits for a live
	// config command's answer before deciding for itself. Long enough that
	// a CLI busy with something other than a turn still wins the race (the
	// commands themselves are local and answer in milliseconds), short
	// enough that a user who changed effort and got silence learns about
	// it in the same sitting. A turn in flight does not consume this
	// window — the sweep re-arms instead, since the command is legitimately
	// queued behind the turn — and neither does a window that merely
	// OVERLAPPED one: the window has to be measured against a session that
	// could actually have answered, so the first idle sweep after a
	// deferral restarts it rather than spending it (see
	// sweepUnconfirmedClaudeLiveApply).
	claudeLiveApplyConfirmAfter = 45 * time.Second
)

// registerClaudeLiveConfigApplies records the command sends of one
// ApplyLiveUpdate so their EventCommandResult confirmations can be matched.
// Called from the apply's preSend hook — before any command reaches the
// wire. prevOpts is the launch-options snapshot the session ran before this
// apply. An older pending entry for the same (session, axis) is superseded:
// its answer no longer decides anything (the newer apply's does), and
// resolving it late would revert a value the newer apply owns.
func (a *App) registerClaudeLiveConfigApplies(
	threadID, sessionToken string,
	prevOpts provider.SessionOptions,
	update claude.LiveUpdate,
	receipt claude.LiveApplyReceipt,
) {
	now := time.Now()
	// Read BEFORE entering sessionruntime — threadTurnInFlight resolves the
	// session through the same non-reentrant Manager.
	//
	// A command registered while a turn is running has not been delivered to
	// the CLI's command router yet; it is a stdin user message sitting behind
	// that turn. Stamping the deferral here rather than waiting for a sweep to
	// observe it is what covers the turn that drains INSIDE the first window:
	// the sweep only samples at its own expiry, so a turn that ends a second
	// before it would otherwise look like a session that had the whole window
	// to answer and did not.
	busy := a.threadTurnInFlight(threadID)
	applies := make(map[string]claudeLiveConfigApply, 2)
	if receipt.EffortCommandUUID != "" {
		applies[receipt.EffortCommandUUID] = claudeLiveConfigApply{
			ThreadID:        threadID,
			SessionToken:    sessionToken,
			Axis:            claudeLiveApplyAxisEffort,
			Requested:       update.Effort,
			PreviousEffort:  prevOpts.ReasoningEffort,
			PreviousFast:    prevOpts.FastMode,
			SentAt:          now,
			DeferredForTurn: busy,
		}
	}
	if receipt.FastCommandUUID != "" {
		applies[receipt.FastCommandUUID] = claudeLiveConfigApply{
			ThreadID:        threadID,
			SessionToken:    sessionToken,
			Axis:            claudeLiveApplyAxisFast,
			Requested:       string(update.FastMode),
			PreviousEffort:  prevOpts.ReasoningEffort,
			PreviousFast:    prevOpts.FastMode,
			SentAt:          now,
			DeferredForTurn: busy,
		}
	}
	a.sessionManager().runtime.RegisterClaudeLiveApplies(applies, now, claudeLiveApplyStaleAfter)
	// Nothing else in this file ever runs if the answer never comes; the
	// watchdog is what makes an unanswered apply a bounded state.
	for _, id := range []string{receipt.EffortCommandUUID, receipt.FastCommandUUID} {
		if id != "" {
			a.armClaudeLiveApplyConfirmWatchdog(id)
		}
	}
}

// claudeLiveApplyConfirmWindow is claudeLiveApplyConfirmAfter, or the test
// override when one is set.
func (a *App) claudeLiveApplyConfirmWindow() time.Duration {
	if override := a.sessionManager().runtime.ClaudeLiveApplyConfirmAfterOverride(); override > 0 {
		return override
	}
	return claudeLiveApplyConfirmAfter
}

// armClaudeLiveApplyConfirmWatchdog schedules the unconfirmed-apply sweep
// for one command uuid. Timer creation only, so it is safe to call with
// The sweep runs on the timer's own goroutine and enters the Manager itself.
func (a *App) armClaudeLiveApplyConfirmWatchdog(commandUUID string) {
	time.AfterFunc(a.claudeLiveApplyConfirmWindow(), func() {
		a.sweepUnconfirmedClaudeLiveApply(commandUUID)
	})
}

// sweepUnconfirmedClaudeLiveApply decides an apply the CLI never answered.
//
// Four ways to be a no-op, all of them the normal case: the entry is gone
// (answered, or purged with its session), the entry is a tombstone
// (superseded or rolled back — whoever tombstoned it owns the axis now), the
// thread has a turn in flight, or the thread has JUST stopped having one.
//
// The first two turn cases are one rule stated at two times. `/effort` and
// `/fast` are stdin user messages that queue behind a running turn, so a
// window that elapsed while a turn held the command measured the TURN, not
// the CLI's willingness to answer — and the sweep samples only at its own
// expiry, so "a turn is running now" and "a turn was running for part of
// this window" are equally disqualifying. A command registered at t=0 and
// re-armed at t=45 whose turn drains at t=46 has been in the CLI's hands for
// one second when the t=90 sweep fires; deciding there declines an apply
// that was about to be confirmed, degrades the axis, and schedules a restart
// nobody needed.
//
// So a deferral is remembered on the entry and the first idle sweep SPENDS
// it on a fresh full window instead of a verdict. The cost is bounded and
// one-sided: at most one extra window per observed deferral, and only ever
// waiting longer before declining.
//
// Otherwise the entry is consumed here, exactly as an answer would consume
// it, and settled without one.
func (a *App) sweepUnconfirmedClaudeLiveApply(commandUUID string) {
	if a.shuttingDown.Load() || a.lifeCtx().Err() != nil {
		return
	}
	pending, ok := a.peekClaudeLiveConfigApply(commandUUID)
	if !ok || pending.Defunct {
		return
	}
	if a.threadTurnInFlight(pending.ThreadID) {
		a.noteClaudeLiveApplyDeferredForTurn(commandUUID)
		a.armClaudeLiveApplyConfirmWatchdog(commandUUID)
		return
	}
	if a.takeClaudeLiveApplyTurnDeferral(commandUUID) {
		// The turn that was holding this command has drained (or never
		// overlapped a full window). The CLI may not have picked the command
		// up until a moment ago, so this window starts now.
		a.armClaudeLiveApplyConfirmWatchdog(commandUUID)
		return
	}
	// Peek-then-take, not take-then-restore: a confirmation that lands in
	// the gap wins, because takeClaudeLiveConfigApply reports it gone and
	// the sweep drops out. Restoring an entry the answer had already
	// consumed would be the double-settle this ordering avoids.
	pending, ok = a.takeClaudeLiveConfigApply(commandUUID)
	if !ok || pending.Defunct {
		return
	}
	a.settleUnconfirmedClaudeLiveApply(pending)
}

// settleUnconfirmedClaudeLiveApply is the verdict on silence.
//
// Silence is not evidence that the command failed: the effort axis has a
// structured read-back that states what the session is ACTUALLY running, so
// a command that ran and lost only its output confirms here rather than
// costing a restart. Everything else declines — launchOpts must never
// outlive the evidence for it, and the deferred restart converges the row
// the honest way.
//
// Never runs on the read loop: the watchdog owns its own goroutine, which
// is what makes the get_settings round trip legal here.
func (a *App) settleUnconfirmedClaudeLiveApply(pending claudeLiveConfigApply) {
	if pending.Axis == claudeLiveApplyAxisEffort && a.claudeSessionMaySupportGetSettings(pending) {
		applied, err := a.readClaudeAppliedSettingsStep(pending.ThreadID, pending.SessionToken)
		if a.claudeLiveApplySuperseded(pending) {
			// A newer apply landed while the read was out; its settle owns
			// the axis. Same window, same rule as
			// settleClaudeEffortApplyFromSettings.
			return
		}
		if err == nil && applied != nil && applied.Effort != "" {
			if sameClaudeEffortTier(applied.Effort, pending.Requested) {
				return // the command ran; only its answer went missing
			}
			a.declineClaudeLiveConfigApply(pending, fmt.Sprintf(
				"the Claude CLI never answered the live effort change and reports effort %q, not the requested %q; restarting the session to apply it",
				applied.Effort, pending.Requested))
			return
		}
	}
	a.declineClaudeLiveConfigApply(pending, fmt.Sprintf(
		"the Claude CLI never answered the live %s change; restarting the session to apply it",
		pending.Axis))
}

// claudeLiveApplySuperseded reports whether a NEWER apply has been registered
// for this entry's (session, axis) since it was sent.
//
// The effort axis settles asynchronously (settleClaudeEffortApplyFromSettings
// reads `get_settings` on its own goroutine, after the entry has already been
// consumed out of the registry), so rapid effort changes interleave: the
// read-back for change #1 can return AFTER change #2 has landed, see the NEW
// tier in `applied.effort`, compare it against change #1's `requested`, and
// "helpfully" decline — restoring #1's prevEffort and scheduling a restart to
// undo a change the user just made. The generation stamp is the entry's way
// of noticing it is answering a question nobody is asking any more.
func (a *App) claudeLiveApplySuperseded(pending claudeLiveConfigApply) bool {
	if pending.Generation == 0 {
		return false
	}
	return a.sessionManager().runtime.ClaudeLiveApplySuperseded(pending)
}

// rollbackClaudeLiveConfigApplies undoes one registration after its
// ApplyLiveUpdate failed mid-sequence: the pending entries become defunct
// tombstones and launchOpts is restored to restoreOpts, so the
// retry sees a genuine diff instead of a false "already converged".
//
// restoreOpts is the pre-apply snapshot with the axes that DID land folded
// back in (claude.CommitLiveUpdate) — not the raw snapshot. The axes ahead
// of the failure really are applied on the wire, and the restart that
// converges the rest is deferred until the thread is quiet; claiming the
// session still runs the old ones would aim the live-first retry at axes
// that already landed instead of the one that failed. The
// retry is LIVE-FIRST — the watcher re-runs liveApplySessionConfig before
// ever considering a restart (fireDeferredConfigReconnectLocked), because
// a restart can be deferred indefinitely by a busy thread and is strictly
// a last resort. A command that DID reach the wire before the failure
// answers into its tombstone and is ignored.
//
// The wholesale rollback can in principle make the live retry re-send a
// command that already landed (a duplicate zero-cost transcript row), but
// the window is vacuous: sends are strictly ordered (set_model →
// set_permission_mode → /effort → /fast) and the sequence aborts on the
// first failure, so a sent-then-rolled-back command requires /effort's
// write succeeding and the ADJACENT /fast write failing — a stdin pipe
// that breaks between two back-to-back writes belongs to a dying process,
// and a dead session is purged with its registry state and lazily
// respawned from the thread row, never retried by the watcher at all.
// Selective per-axis rollback would buy nothing real for that.
func (a *App) rollbackClaudeLiveConfigApplies(
	threadID, sessionToken string,
	restoreOpts provider.SessionOptions,
	receipt claude.LiveApplyReceipt,
) {
	a.sessionManager().runtime.RollbackClaudeLiveApplies([]string{receipt.EffortCommandUUID, receipt.FastCommandUUID})
	a.sessionManager().updateLaunchOpts(threadID, sessionToken, restoreOpts)
}

// peekClaudeLiveConfigApply reads the pending apply for a command uuid
// without consuming it. The watchdog's first look: it must be able to decide
// that an entry is somebody else's business (tombstoned, or queued behind a
// turn) and leave it exactly where it found it.
func (a *App) peekClaudeLiveConfigApply(commandUUID string) (claudeLiveConfigApply, bool) {
	return a.sessionManager().runtime.PeekClaudeLiveApply(commandUUID)
}

// takeClaudeLiveConfigApply consumes the pending apply for a command uuid.
func (a *App) takeClaudeLiveConfigApply(commandUUID string) (claudeLiveConfigApply, bool) {
	return a.sessionManager().runtime.TakeClaudeLiveApply(commandUUID)
}

// noteClaudeLiveApplyDeferredForTurn records that this command was observed
// waiting behind a running turn. Idempotent, and deliberately a no-op for an
// entry that is gone or tombstoned — neither has a verdict left to defer.
func (a *App) noteClaudeLiveApplyDeferredForTurn(commandUUID string) {
	a.sessionManager().runtime.NoteClaudeLiveApplyDeferredForTurn(commandUUID)
}

// takeClaudeLiveApplyTurnDeferral clears the deferral mark and reports
// whether it was set — the sweep's "spend one fresh window instead of
// deciding" gate.
//
// Consuming it is what bounds the extension: a second idle sweep finds
// nothing to spend and settles. The mark is cleared under the same lock that
// reads it, so two sweeps for one uuid (there is only ever one armed timer,
// but the entry is shared state) cannot each be granted the same window.
func (a *App) takeClaudeLiveApplyTurnDeferral(commandUUID string) bool {
	return a.sessionManager().runtime.TakeClaudeLiveApplyTurnDeferral(commandUUID)
}

// takeClaudeLiveConfigAppliesForAxis consumes every pending apply for one
// (session, axis) pair, returning the live ones (tombstones are dropped
// silently). Fallback matcher for CLIs that emit no command_lifecycle
// frames: their command output carries no uuid, but a reply text that is
// unmistakably an /effort or /fast answer must still settle the pending
// apply — otherwise a rejection on such a CLI would strand the optimistic
// launchOpts write silently, the exact wrong-state this file exists to
// prevent.
func (a *App) takeClaudeLiveConfigAppliesForAxis(sessionToken, axis string) []claudeLiveConfigApply {
	return a.sessionManager().runtime.TakeClaudeLiveAppliesForAxis(sessionToken, axis)
}

// claudeLiveApplyIsDegraded reports whether this session already failed a
// live apply on any axis the update carries. The reconciler consults it
// before ApplyLiveUpdate so a session that answered /effort unexpectedly
// once is restarted rather than asked again (and again, on every watcher
// poll). Deliberately update-wide, not axis-wide: a bundled change that
// includes a degraded axis takes the restart path even though its other
// axes could apply live — splitting the bundle would leave the session
// half-matching the row with no record of which half, and the restart
// converges everything at once.
func (a *App) claudeLiveApplyIsDegraded(sessionToken string, update claude.LiveUpdate) bool {
	return a.sessionManager().runtime.ClaudeLiveApplyIsDegraded(
		sessionToken, update, claudeLiveApplyAxisEffort, claudeLiveApplyAxisFast,
	)
}

func (a *App) markClaudeLiveApplyDegraded(sessionToken, axis string) {
	a.sessionManager().runtime.MarkClaudeLiveApplyDegraded(sessionToken, axis)
}

// observeClaudeCommandResult inspects every Claude command output for two
// concerns: settling a pending AO-initiated live-config apply (matched by
// uuid, or by unmistakable reply text when the CLI stamps no uuid), and
// syncing thread state when the USER ran /effort themselves from the
// composer — the CLI applies that immediately, so the thread row and
// launch-options snapshot must follow or the next restart would silently
// undo what the user asked the session for.
func (a *App) observeClaudeCommandResult(threadID, sessionToken string, evt provider.ProviderEvent) {
	text := strings.TrimSpace(evt.Content)
	if text == "" {
		return
	}
	var meta provider.CommandResultMeta
	if len(evt.Meta) > 0 {
		_ = json.Unmarshal(evt.Meta, &meta)
	}
	if meta.CommandUUID != "" {
		if pending, ok := a.takeClaudeLiveConfigApply(meta.CommandUUID); ok {
			if !pending.Defunct {
				a.resolveClaudeLiveConfigApply(pending, text)
			}
			// A defunct entry is an AO command whose apply was superseded
			// or rolled back — its answer decides nothing.
			return
		}
		// A uuid with no entry at all is a user-typed composer command (the
		// composer's sends carry uuids too, and a modern CLI stamps them the
		// same way) — fall through to the user-typed handling below.
	} else {
		// No uuid: the CLI predates command_lifecycle and cannot correlate.
		// If a pending apply exists for the axis this text unambiguously
		// answers, settle it — a rejection on such a CLI would otherwise
		// strand the optimistic launchOpts write silently.
		switch {
		case isEffortCommandReply(text):
			if pending := a.takeClaudeLiveConfigAppliesForAxis(sessionToken, claudeLiveApplyAxisEffort); len(pending) > 0 {
				for _, p := range pending {
					a.resolveClaudeLiveConfigApply(p, text)
				}
				return
			}
		case isFastCommandReply(text):
			if pending := a.takeClaudeLiveConfigAppliesForAxis(sessionToken, claudeLiveApplyAxisFast); len(pending) > 0 {
				for _, p := range pending {
					a.resolveClaudeLiveConfigApply(p, text)
				}
				return
			}
		}
	}
	// User-typed command (or an already-settled duplicate, which the sync
	// below makes idempotent). `/fast` output is deliberately not synced to
	// the thread row — enabling it can implicitly switch the model too
	// ("model set to …"), and the passive fast_mode_state key on every
	// result already keeps live state truthful; auto-editing the row on top
	// of that is guesswork.
	if tier, ok := parseEffortSetText(text); ok {
		a.syncThreadEffortFromWire(threadID, sessionToken, tier)
	}
}

// observeClaudeCommandLifecycle watches for the CLI retiring a pending
// live-config command without executing it (the queued message died with an
// interrupt or shutdown — `cancelled`; or the session ended with it still
// queued — `discarded`, Claude 2.1.224+). No output will ever arrive for it
// either way, so the optimistic launchOpts write is reverted here — without
// a degraded mark: the command never ran, so the session's command channel
// is not suspect — and the watcher re-converges the row's value through a
// fresh apply.
func (a *App) observeClaudeCommandLifecycle(evt provider.ProviderEvent) {
	var meta provider.CommandLifecycleMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		return
	}
	if meta.State != provider.CommandCancelled && meta.State != provider.CommandDiscarded {
		return
	}
	pending, ok := a.takeClaudeLiveConfigApply(meta.CommandUUID)
	if !ok || pending.Defunct {
		return
	}
	if !a.revertClaudeLiveApplyAxis(pending) {
		return
	}
	a.schedulePendingConfigReconnect(pending.ThreadID)
}

// effortSetTextPrefix is the CLI's success reply to `/effort <tier>`
// ("Set effort level to low (this session only): …", verified 2.1.219).
// `/effort current` answers "Current effort level: …" and must NOT match:
// it reports state, and treating it as a change would let a stale row value
// overwrite a pending config change.
const effortSetTextPrefix = "Set effort level to "

// isEffortCommandReply reports whether text is the output of an /effort
// command: success, readback, usage, or the CLI's non-error rejection. The
// "Invalid argument:" prefix is not effort-exclusive on its face, but this
// matcher only runs against uncorrelated output while an effort apply is
// pending — and misattributing some other command's rejection there costs
// one honest restart, while missing /effort's own rejection costs a
// silently wrong launchOpts for the session's life. Safe direction wins.
func isEffortCommandReply(text string) bool {
	return strings.HasPrefix(text, effortSetTextPrefix) ||
		strings.HasPrefix(text, "Current effort level") ||
		strings.HasPrefix(text, "Usage: /effort") ||
		strings.HasPrefix(text, "Invalid argument:")
}

// The LOCAL /fast command's complete reply vocabulary, read out of the
// installed 2.1.237 bundle rather than guessed. Its handler (`Ksw`, and the
// toggle `eFi` it tails into) has exactly four return sites, and every reply
// the CLI can produce is one of these five shapes:
//
//	<glyph> Fast mode ON[ \u00b7 model set to <m>] \u00b7 <plan>[ (this session only)]
//	Fast mode OFF[ (this session only)]
//	Fast mode unavailable: <reason>           // eFi, every availability gate
//	<bare reason>                             // Ksw, fast mode off environment-wide
//	Unknown argument "<x>". Use: /fast [on|off]
//
// Matching is by containment on every arm, because the CLI does not lead with
// the state word: enabling can implicitly switch the model, and the ON line
// carries a glyph first, so a prefix test matches one spelling out of five.
const (
	fastModeOnText          = "Fast mode ON"
	fastModeOffText         = "Fast mode OFF"
	fastModeGatePrefixText  = "Fast mode unavailable:"
	fastModeBadArgumentText = "Use: /fast [on|off]"
)

// isFastModeGateReply reports whether text says fast mode is UNAVAILABLE
// rather than toggled.
//
// Two spellings, because the gate is reported from two places. `eFi` prefixes
// its reason with "Fast mode unavailable: " and reaches every case in the
// reason table (org preference, credits, network, pending, model not allowed,
// …). `Ksw` short-circuits BEFORE the toggle when fast mode is off for the
// whole process and returns the reason BARE — and since its own guard is the
// same `!Ru()` the reason table branches on first, only two reasons can come
// out of it: `disabled_by_env` and `not_first_party`.
//
// Callers must still exclude fastModeSDKUnavailableText, which contains the
// first of these as a substring while meaning the opposite thing about
// restarts. See the constant.
func isFastModeGateReply(text string) bool {
	return strings.Contains(text, fastModeGatePrefixText) ||
		// vib("disabled_by_env") — also a substring of the Agent SDK reason.
		strings.Contains(text, "Fast mode is not available") ||
		// vib("not_first_party") — a non-Anthropic API base URL.
		strings.Contains(text, "Fast mode is only available when using the Anthropic API")
}

// isFastCommandReply reports whether text is unmistakably the output of a
// /fast command, in any of its five shapes.
//
// The gate and bad-argument arms are why this is not just the ON / OFF pair:
// on the uncorrelated-CLI path a refused /fast that matched nothing would fall
// through to the user-typed handling and leave the pending apply to time out
// into a restart, when the answer to route it was already in hand.
//
// Every caller already requires a pending fast apply on the session, so a
// wider match costs at worst one settled apply and a narrower one costs a
// silent degrade. That asymmetry is also why the vocabulary is re-read from
// the bundle whenever the CLI moves: this matcher used to carry a
// "Usage: /fast" arm, a string 2.1.237 does not contain anywhere at all.
func isFastCommandReply(text string) bool {
	return strings.Contains(text, fastModeOnText) ||
		strings.Contains(text, fastModeOffText) ||
		strings.Contains(text, fastModeBadArgumentText) ||
		isFastModeGateReply(text)
}

// parseEffortSetText extracts the tier from a `/effort` success reply,
// accepting only tiers AO models as thread config (claude.IsLiveEffortTier
// — the same vocabulary the send side validates, so the two cannot drift).
// "ultracode" and "auto" are real CLI tiers a user can set by hand, but
// they have no thread-row representation (the per-provider CHECK constraint
// would reject them), so they deliberately do not sync — the session honors
// them, AO just cannot record them.
func parseEffortSetText(text string) (provider.ReasoningEffort, bool) {
	rest, ok := strings.CutPrefix(text, effortSetTextPrefix)
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || !claude.IsLiveEffortTier(fields[0]) {
		return "", false
	}
	return provider.ReasoningEffort(fields[0]), true
}

// fastModeSDKUnavailableText is the /fast reply on a process spawned
// without the fastMode settings opt-in (fast_mode_disabled_reason
// "sdk_opt_in_required", verified 2.1.219). Unlike the account-credits
// gate, a restart WITH the opt-in fixes this — so it takes the failure
// path. ApplyLiveUpdate's spawnedWithFastModeOptIn gate should make it
// unreachable; reaching it means that gate was wrong, and the restart is
// the correct recovery either way.
const fastModeSDKUnavailableText = "Fast mode is not available in the Agent SDK"

// resolveClaudeLiveConfigApply settles one pending apply against the CLI's
// answer.
//
// Runs on the provider read loop. The effort axis has a structured verdict
// available (`get_settings.applied.effort`), but reading it is a control
// round-trip whose response arrives on THIS goroutine — so it is handed to a
// goroutine, which then settles through the same tail as the text path.
func (a *App) resolveClaudeLiveConfigApply(pending claudeLiveConfigApply, text string) {
	if pending.Axis == claudeLiveApplyAxisEffort && a.claudeSessionMaySupportGetSettings(pending) {
		go a.settleClaudeEffortApplyFromSettings(pending, text)
		return
	}
	a.settleClaudeLiveConfigApplyFromText(pending, text)
}

// settleClaudeEffortApplyFromSettings confirms (or declines) an effort apply
// against `get_settings.applied.effort` — the CLI's own statement of the tier
// it resolved, "what will actually be sent to the API". Falls back to the
// reply text when the read-back is unavailable: an older CLI without the
// subtype (recorded once per session, so the wire is not asked twice), or a
// round-trip that failed or raced the session's teardown.
//
// Never runs on the read loop; see resolveClaudeLiveConfigApply.
func (a *App) settleClaudeEffortApplyFromSettings(pending claudeLiveConfigApply, text string) {
	applied, err := a.readClaudeAppliedSettingsStep(pending.ThreadID, pending.SessionToken)
	if a.claudeLiveApplySuperseded(pending) {
		// A newer effort apply landed while this read was out. Its own
		// settle owns the verdict; deciding anything here — confirm OR
		// decline — would be a verdict about a tier that is no longer
		// requested, and the decline path would actively revert the newer
		// one. Checked AFTER the read rather than before so the window it
		// closes is the whole round trip, which is where the race lives.
		return
	}
	if err != nil || applied == nil {
		if err != nil && !errors.Is(err, claude.ErrGetSettingsUnsupported) {
			log.Printf("thread %s: effort read-back failed, falling back to the CLI's reply text: %v", pending.ThreadID, err)
		}
		a.settleClaudeLiveConfigApplyFromText(pending, text)
		return
	}
	if applied.Effort == "" {
		// The CLI stated NOTHING about effort. `applied.effort` is explicit
		// null on a model that declares no tiers (see AppliedSettings) — an
		// answer about the MODEL, not about this request — so reading it as
		// "the session runs a different tier" would restart the session on
		// every apply against such a model. Fall through to the reply text,
		// which is the only statement the CLI made about the command.
		a.settleClaudeLiveConfigApplyFromText(pending, text)
		return
	}
	if sameClaudeEffortTier(applied.Effort, pending.Requested) {
		return // confirmed by the CLI's own resolved value
	}
	// The session is running a different tier than AO asked for. This is
	// the authoritative answer even when the reply text looked like a
	// success — a settings layer AO does not control can outrank the
	// request, and launchOpts must never claim a config the process is not
	// running.
	a.declineClaudeLiveConfigApply(pending, fmt.Sprintf(
		"live effort change was not accepted by the Claude CLI (it reports effort %q, requested %q); restarting the session to apply it",
		applied.Effort, pending.Requested))
}

// sameClaudeEffortTier compares AO's requested tier against the CLI's own
// spelling of the resolved one. AO stores the lowercase slugs
// (provider.ReasoningEffortsForProvider("claude")); `applied.effort` is whatever the running
// binary writes, and a display-layer spelling ("X-High", "x high") names the
// same tier. Comparing the raw strings made every such spelling read as a
// rejection and restarted a session that had in fact accepted the change.
func sameClaudeEffortTier(a, b string) bool {
	return normalizeClaudeEffortTier(a) == normalizeClaudeEffortTier(b)
}

func normalizeClaudeEffortTier(effort string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(effort)) {
		switch r {
		case '-', '_', ' ':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// settleClaudeLiveConfigApplyFromText settles one pending apply against the
// CLI's reply TEXT. The only verdict for the fast axis, and the fallback for
// effort on a CLI without `get_settings`.
func (a *App) settleClaudeLiveConfigApplyFromText(pending claudeLiveConfigApply, text string) {
	switch pending.Axis {
	case claudeLiveApplyAxisEffort:
		if tier, ok := parseEffortSetText(text); ok && string(tier) == pending.Requested {
			return // confirmed; launchOpts already holds the value
		}
	case claudeLiveApplyAxisFast:
		switch {
		case pending.Requested == string(claude.FastModeOn) && strings.Contains(text, fastModeOnText):
			// Containment, not prefix: enabling can implicitly switch the
			// model and the reply may lead with that. claude-wire.md
			// §"Live config commands".
			return
		case pending.Requested == string(claude.FastModeOff) && strings.Contains(text, fastModeOffText):
			// Containment for the same reason the ON arm uses it: the bundle
			// ships separator- and space-prefixed spellings of this line too.
			return
		case isFastModeGateReply(text) && !strings.Contains(text, fastModeSDKUnavailableText):
			// An availability gate (extra-usage credits off, org preference,
			// fast mode disabled for the whole process, …). A restart would
			// hit the identical gate — this is exactly the state a spawn with
			// the fastMode opt-in produces, and the session's fast_mode_state
			// on every result keeps the UI truthful. The requested value
			// stays in launchOpts so the reconciler does not loop; nothing to
			// surface beyond the visible command row.
			//
			// The SDK reason is carved out on purpose and is why this cannot
			// simply match "Fast mode is not available": that string is a
			// SUBSTRING of the Agent SDK reason, which a restart DOES fix.
			log.Printf("thread %s: live fast-mode apply declined by CLI: %s", pending.ThreadID, firstLine(text))
			return
		}
	}

	// The CLI answered with something other than the expected state change
	// ("Invalid argument: …", `Unknown argument "x". Use: /fast [on|off]`,
	// the SDK fast-mode gate, or wording drift on a newer CLI).
	a.declineClaudeLiveConfigApply(pending, fmt.Sprintf(
		"live %s change was not accepted by the Claude CLI (%q); restarting the session to apply it",
		pending.Axis, firstLine(text)))
}

// declineClaudeLiveConfigApply is the shared tail for "the session is NOT
// running the requested value", whichever channel proved it: restore the axis
// in launchOpts, stop trusting this session's command channel for the axis,
// surface the reason, and let the deferred-restart watcher converge the
// honest way.
func (a *App) declineClaudeLiveConfigApply(pending claudeLiveConfigApply, message string) {
	if !a.revertClaudeLiveApplyAxis(pending) {
		// The session is gone or replaced — its registry state was purged
		// with it, the replacement spawned from the thread row, and an
		// error about a dead session would only mislead.
		return
	}
	a.markClaudeLiveApplyDegraded(pending.SessionToken, pending.Axis)
	// Wire-route, not synthetic: this answers command output from the
	// provider read loop, and the error must respect the stopped-thread
	// gate exactly like the output that triggered it (invariant 29).
	a.emitWireErrorToThread(pending.ThreadID, message)
	a.schedulePendingConfigReconnect(pending.ThreadID)
}

// revertClaudeLiveApplyAxis restores a pending apply's axis in launchOpts to
// its pre-apply value. Reports false when the session is gone or replaced —
// the caller must then skip every session-scoped side effect.
func (a *App) revertClaudeLiveApplyAxis(pending claudeLiveConfigApply) bool {
	switch pending.Axis {
	case claudeLiveApplyAxisEffort:
		return a.sessionManager().runtime.UpdateReasoningEffort(
			pending.ThreadID, pending.SessionToken, pending.PreviousEffort,
		)
	case claudeLiveApplyAxisFast:
		return a.sessionManager().runtime.UpdateFastMode(
			pending.ThreadID, pending.SessionToken, pending.PreviousFast,
		)
	default:
		return false
	}
}

// syncThreadEffortFromWire follows a user-typed `/effort <tier>` that the
// live session has already adopted: launchOpts first (so the row update's
// reconcile sees a session already matching and does not restart anything),
// then the thread row and the remembered per-model profile. The launchOpts
// write doubles as the liveness gate — if the session this output came from
// is gone or replaced, nothing persistent may change (a torn-down session's
// read loop drains its tail after teardown, and a stale echo must not
// rewrite the row the user's next session spawns from).
func (a *App) syncThreadEffortFromWire(threadID, sessionToken string, tier provider.ReasoningEffort) {
	if a.store == nil {
		return
	}
	if !a.sessionManager().runtime.UpdateReasoningEffort(threadID, sessionToken, tier) {
		return
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("thread %s: effort sync: load thread: %v", threadID, err)
		return
	}
	if thread.ReasoningEffort == string(tier) {
		return
	}
	thread.ReasoningEffort = string(tier)
	sanitized := a.sanitizeThreadModelSettings(thread)
	if sanitized.ReasoningEffort != string(tier) {
		// The thread's model does not support this tier as row config
		// (e.g. an effortless model). The session still runs it — that is
		// between the user and the CLI — but the row cannot record it.
		log.Printf("thread %s: effort sync: tier %s not representable for model %s", threadID, tier, thread.Model)
		return
	}
	if err := a.store.UpdateThread(sanitized); err != nil {
		log.Printf("thread %s: effort sync: persist: %v", threadID, err)
		return
	}
	a.rememberChatModelProfile(sanitized)
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: "full", Thread: &sanitized})
}

// firstLine truncates a command answer to its first line for error surfaces.
func firstLine(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return text[:idx]
	}
	return text
}

// claudeGetSettingsTimeout bounds a `get_settings` round trip. The CLI
// answers it out of band (no turn, no API call), so this is a local-IPC
// deadline: it exists so a wedged CLI cannot hold a confirmation goroutine
// for the session's whole control timeout.
const claudeGetSettingsTimeout = 5 * time.Second

// claudeSessionForToken resolves the live Claude session behind a pending
// apply, refusing a session that has been replaced since. Nil means "gone" —
// every caller must then skip its session-scoped side effects.
func (a *App) claudeSessionForToken(threadID, sessionToken string) *claude.Session {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.Token != sessionToken || sess.Claude == nil {
		return nil
	}
	return sess.Claude
}

// claudeSessionMaySupportGetSettings reports whether it is worth asking this
// session for a structured read-back. False for a dead or replaced session,
// and for one that already answered the subtype with an unsupported-subtype
// error — the CLI's answer to that cannot change while the process lives, so
// asking again would put a failing control_request on stdin before every
// confirmation.
func (a *App) claudeSessionMaySupportGetSettings(pending claudeLiveConfigApply) bool {
	sess := a.claudeSessionForToken(pending.ThreadID, pending.SessionToken)
	return sess != nil && !sess.GetSettingsUnsupported()
}

// readClaudeAppliedSettingsStep is the read-back behind a test seam, used by
// every caller that reads `applied` off the reconciler's path (the effort
// settle, the unconfirmed-apply watchdog, the model read-back): the round
// trip needs a live provider process, and the branches worth pinning (an
// empty tier, a differently-spelled tier, a superseded read-back, and the
// round trip NOT being made at all) are decided by what the CLI ANSWERED,
// not by how the answer was fetched. Production always runs
// readClaudeAppliedSettings.
func (a *App) readClaudeAppliedSettingsStep(threadID, sessionToken string) (*claude.AppliedSettings, error) {
	read := a.sessionManager().runtime.ReadClaudeAppliedSettingsStep()
	if read != nil {
		return read(threadID, sessionToken)
	}
	return a.readClaudeAppliedSettings(threadID, sessionToken)
}

// readClaudeAppliedSettings performs one `get_settings` round-trip and
// returns its `applied` object. Also logs any project-level settings source
// found overriding what AO requested — a repository's `.claude/settings.json`
// naming a different model or effortLevel is a real explanation for a session
// that will not converge, and it is invisible everywhere else.
//
// Returns (nil, nil) when the session is gone; (nil, err) with
// claude.ErrGetSettingsUnsupported when the CLI predates the subtype.
func (a *App) readClaudeAppliedSettings(threadID, sessionToken string) (*claude.AppliedSettings, error) {
	sess := a.claudeSessionForToken(threadID, sessionToken)
	if sess == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), claudeGetSettingsTimeout)
	defer cancel()
	snapshot, err := sess.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	for _, notice := range sess.SettingsOverrides() {
		log.Printf("thread %s: project settings (%s) set %s=%q, overriding the %q AO requested",
			threadID, notice.Source, notice.Field, notice.Configured, notice.Requested)
	}
	return snapshot.Applied, nil
}

// readBackClaudeAppliedModel records what the CLI actually resolved after a
// set_model landed. The control_response is a bare success even when the CLI
// stepped a FAMILY ALIAS down to a different concrete model (telemetry
// `model_switch: "family_alias_stepped_down"`), so this is the only channel
// that can state the applied model.
//
// MODEL ONLY. A system-prompt swap rides the same set_model, but
// claude.AppliedSettings carries model, effort, advisor and ultracode and
// nothing about the prompt, so there is no prompt to verify here — an empty
// `requested` therefore returns before the round trip rather than after it,
// instead of putting a control_request on stdin to read an answer that cannot
// contain the fact being checked.
//
// Observation only: nothing is reverted on a mismatch. The CLI accepted the
// request and the session is running a legitimate model; forcing a restart
// would only produce the same step-down. The applied value is stamped on the
// session's live state (claude.Session.AppliedSettingsSnapshot) and logged.
//
// Fire-and-forget from a goroutine — never from the read loop.
func (a *App) readBackClaudeAppliedModel(threadID, sessionToken, requested string) {
	if requested == "" {
		return
	}
	// No pre-guard on the session: readClaudeAppliedSettings resolves it
	// itself and answers (nil, nil) when it is gone, so a second resolve
	// here would only add a window in which the two disagree.
	applied, err := a.readClaudeAppliedSettingsStep(threadID, sessionToken)
	if err != nil {
		if !errors.Is(err, claude.ErrGetSettingsUnsupported) {
			log.Printf("thread %s: model read-back failed: %v", threadID, err)
		}
		return
	}
	if applied == nil || applied.Model == "" {
		return
	}
	if provider.NormalizeModelSlug(string(provider.Claude), applied.Model) !=
		provider.NormalizeModelSlug(string(provider.Claude), requested) {
		log.Printf("thread %s: claude accepted set_model %q but reports running %q", threadID, requested, applied.Model)
	}
}
