package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
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
//  3. observeClaudeCommandResult matches output to pending apply. Expected
//     text confirms; anything else reverts the launchOpts axis, marks the
//     (session, axis) pair degraded so the reconciler stops retrying it
//     live, surfaces the CLI's answer as thread error state, and arms the
//     deferred-restart watcher — the same convergence fallback every other
//     non-live-appliable change uses.
//  4. A command the CLI cancels instead of executing (its
//     command_lifecycle reaches `cancelled` — e.g. the queued message died
//     with an interrupt) reverts the axis without degrading, and the
//     watcher re-converges: the command never ran, so the session is not
//     suspect, just not updated.
//
// A pending entry whose confirmation never arrives is dropped with its
// session: entries are keyed to the session token and purged whenever the
// session leaves the registry (unregister, take, takeIdle,
// snapshotAndClear), and the replacement session spawns from the thread
// row, which already holds the requested config. The 24h insert-time
// eviction is a backstop for tokens that never pass through any of those.

type claudeLiveConfigApply struct {
	threadID     string
	sessionToken string
	axis         string // claudeLiveApplyAxisEffort | claudeLiveApplyAxisFast
	// requested is the /effort tier or the /fast argument ("on"/"off").
	requested string
	// prevEffort / prevFast restore the axis in launchOpts if the CLI
	// declines.
	prevEffort provider.ReasoningEffort
	prevFast   bool
	sentAt     time.Time
	// defunct marks an entry whose answer no longer decides anything — the
	// apply was superseded by a newer one for the same axis, or rolled back
	// after a mid-sequence send failure. The entry is kept (not deleted) as
	// a tombstone so the command's late answer is recognized as AO-authored
	// and ignored, rather than misread as a user-typed command. Deleting it
	// would be wrong twice over: the answer would fall through to the
	// user-typed path, and — for a supersede — the OLD command's rejection
	// could arrive first and would otherwise be the only pending answer in
	// sight.
	defunct bool
}

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
)

// claudeLiveApplyKey identifies one (session, axis) pair in the degraded
// set. A struct key rather than string concatenation so the token's
// character set can never ambiguate the split.
type claudeLiveApplyKey struct {
	sessionToken string
	axis         string
}

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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeLiveConfigApplies == nil {
		a.claudeLiveConfigApplies = make(map[string]claudeLiveConfigApply)
	}
	for id, pending := range a.claudeLiveConfigApplies {
		if now.Sub(pending.sentAt) > claudeLiveApplyStaleAfter {
			delete(a.claudeLiveConfigApplies, id)
		}
	}
	supersede := func(axis string) {
		for id, pending := range a.claudeLiveConfigApplies {
			if pending.sessionToken == sessionToken && pending.axis == axis && !pending.defunct {
				pending.defunct = true
				a.claudeLiveConfigApplies[id] = pending
			}
		}
	}
	if receipt.EffortCommandUUID != "" {
		supersede(claudeLiveApplyAxisEffort)
		a.claudeLiveConfigApplies[receipt.EffortCommandUUID] = claudeLiveConfigApply{
			threadID:     threadID,
			sessionToken: sessionToken,
			axis:         claudeLiveApplyAxisEffort,
			requested:    update.Effort,
			prevEffort:   prevOpts.ReasoningEffort,
			prevFast:     prevOpts.FastMode,
			sentAt:       now,
		}
	}
	if receipt.FastCommandUUID != "" {
		supersede(claudeLiveApplyAxisFast)
		a.claudeLiveConfigApplies[receipt.FastCommandUUID] = claudeLiveConfigApply{
			threadID:     threadID,
			sessionToken: sessionToken,
			axis:         claudeLiveApplyAxisFast,
			requested:    string(update.FastMode),
			prevEffort:   prevOpts.ReasoningEffort,
			prevFast:     prevOpts.FastMode,
			sentAt:       now,
		}
	}
}

// rollbackClaudeLiveConfigApplies undoes one registration after its
// ApplyLiveUpdate failed mid-sequence: the pending entries become defunct
// tombstones and launchOpts is restored to the pre-apply snapshot, so the
// retry sees a genuine diff instead of a false "already converged". The
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
	prevOpts provider.SessionOptions,
	receipt claude.LiveApplyReceipt,
) {
	a.mu.Lock()
	for _, id := range []string{receipt.EffortCommandUUID, receipt.FastCommandUUID} {
		if pending, ok := a.claudeLiveConfigApplies[id]; ok {
			pending.defunct = true
			a.claudeLiveConfigApplies[id] = pending
		}
	}
	a.mu.Unlock()
	a.sessionManager().updateLaunchOpts(threadID, sessionToken, prevOpts)
}

// takeClaudeLiveConfigApply consumes the pending apply for a command uuid.
func (a *App) takeClaudeLiveConfigApply(commandUUID string) (claudeLiveConfigApply, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.claudeLiveConfigApplies[commandUUID]
	if ok {
		delete(a.claudeLiveConfigApplies, commandUUID)
	}
	return pending, ok
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
	a.mu.Lock()
	defer a.mu.Unlock()
	var taken []claudeLiveConfigApply
	for id, pending := range a.claudeLiveConfigApplies {
		if pending.sessionToken == sessionToken && pending.axis == axis {
			if !pending.defunct {
				taken = append(taken, pending)
			}
			delete(a.claudeLiveConfigApplies, id)
		}
	}
	return taken
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if update.Effort != "" {
		if _, ok := a.claudeLiveApplyDegraded[claudeLiveApplyKey{sessionToken, claudeLiveApplyAxisEffort}]; ok {
			return true
		}
	}
	if update.FastMode != claude.FastModeUnchanged {
		if _, ok := a.claudeLiveApplyDegraded[claudeLiveApplyKey{sessionToken, claudeLiveApplyAxisFast}]; ok {
			return true
		}
	}
	return false
}

func (a *App) markClaudeLiveApplyDegraded(sessionToken, axis string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeLiveApplyDegraded == nil {
		a.claudeLiveApplyDegraded = make(map[claudeLiveApplyKey]struct{})
	}
	a.claudeLiveApplyDegraded[claudeLiveApplyKey{sessionToken, axis}] = struct{}{}
}

// purgeClaudeLiveConfigStateLocked drops every pending apply and degraded
// mark belonging to a session. Called (with a.mu held) from every
// session-registry removal path — unregister, take, takeIdle,
// snapshotAndClear — so no matter how a session leaves, its replacement
// starts with a clean slate and a drained read loop's late command output
// finds nothing to act on.
func (a *App) purgeClaudeLiveConfigStateLocked(sessionToken string) {
	for id, pending := range a.claudeLiveConfigApplies {
		if pending.sessionToken == sessionToken {
			delete(a.claudeLiveConfigApplies, id)
		}
	}
	for key := range a.claudeLiveApplyDegraded {
		if key.sessionToken == sessionToken {
			delete(a.claudeLiveApplyDegraded, key)
		}
	}
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
			if !pending.defunct {
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

// observeClaudeCommandLifecycle watches for the CLI cancelling a pending
// live-config command before executing it (the queued message died with an
// interrupt or shutdown). No output will ever arrive for it, so the
// optimistic launchOpts write is reverted here — without a degraded mark:
// the command never ran, so the session's command channel is not suspect —
// and the watcher re-converges the row's value through a fresh apply.
func (a *App) observeClaudeCommandLifecycle(evt provider.ProviderEvent) {
	var meta provider.CommandLifecycleMeta
	if err := json.Unmarshal(evt.Meta, &meta); err != nil || meta.State != provider.CommandCancelled {
		return
	}
	pending, ok := a.takeClaudeLiveConfigApply(meta.CommandUUID)
	if !ok || pending.defunct {
		return
	}
	if !a.revertClaudeLiveApplyAxis(pending) {
		return
	}
	a.schedulePendingConfigReconnect(pending.threadID)
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

// isFastCommandReply reports whether text is unmistakably the output of a
// /fast command.
func isFastCommandReply(text string) bool {
	return strings.HasPrefix(text, "Fast mode ON") ||
		strings.HasPrefix(text, "Fast mode OFF") ||
		strings.HasPrefix(text, "Fast mode unavailable:") ||
		strings.HasPrefix(text, "Usage: /fast")
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
func (a *App) resolveClaudeLiveConfigApply(pending claudeLiveConfigApply, text string) {
	switch pending.axis {
	case claudeLiveApplyAxisEffort:
		if tier, ok := parseEffortSetText(text); ok && string(tier) == pending.requested {
			return // confirmed; launchOpts already holds the value
		}
	case claudeLiveApplyAxisFast:
		switch {
		case pending.requested == string(claude.FastModeOn) && strings.Contains(text, "Fast mode ON"):
			// Containment, not prefix: enabling can implicitly switch the
			// model and the reply may lead with that. claude-wire.md
			// §"Live config commands".
			return
		case pending.requested == string(claude.FastModeOff) && strings.HasPrefix(text, "Fast mode OFF"):
			return
		case strings.HasPrefix(text, "Fast mode unavailable:") && !strings.Contains(text, fastModeSDKUnavailableText):
			// Account-level gate (e.g. extra-usage credits off). A restart
			// would hit the identical gate — this is exactly the state a
			// spawn with the fastMode opt-in produces, and the session's
			// fast_mode_state on every result keeps the UI truthful. The
			// requested value stays in launchOpts so the reconciler does
			// not loop; nothing to surface beyond the visible command row.
			log.Printf("thread %s: live fast-mode apply declined by CLI: %s", pending.threadID, firstLine(text))
			return
		}
	}

	// The CLI answered with something other than the expected state change
	// ("Invalid argument: …", "Usage: …", the SDK fast-mode gate, or
	// wording drift on a newer CLI). The session is NOT running the
	// requested value: restore the axis in launchOpts, stop trusting this
	// session's command channel for the axis, surface the answer, and let
	// the deferred-restart watcher converge the honest way.
	if !a.revertClaudeLiveApplyAxis(pending) {
		// The session is gone or replaced — its registry state was purged
		// with it, the replacement spawned from the thread row, and an
		// error about a dead session would only mislead.
		return
	}
	a.markClaudeLiveApplyDegraded(pending.sessionToken, pending.axis)
	// Wire-route, not synthetic: this runs on the provider read loop, and
	// the error must respect the stopped-thread gate exactly like the
	// command output that triggered it (invariant 29).
	a.emitWireErrorToThread(pending.threadID, fmt.Sprintf(
		"live %s change was not accepted by the Claude CLI (%q); restarting the session to apply it",
		pending.axis, firstLine(text)))
	a.schedulePendingConfigReconnect(pending.threadID)
}

// revertClaudeLiveApplyAxis restores a pending apply's axis in launchOpts to
// its pre-apply value. Reports false when the session is gone or replaced —
// the caller must then skip every session-scoped side effect.
func (a *App) revertClaudeLiveApplyAxis(pending claudeLiveConfigApply) bool {
	return a.sessionManager().mutateLaunchOpts(pending.threadID, pending.sessionToken, func(opts *provider.SessionOptions) {
		switch pending.axis {
		case claudeLiveApplyAxisEffort:
			opts.ReasoningEffort = pending.prevEffort
		case claudeLiveApplyAxisFast:
			opts.FastMode = pending.prevFast
		}
	})
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
	if !a.sessionManager().mutateLaunchOpts(threadID, sessionToken, func(opts *provider.SessionOptions) {
		opts.ReasoningEffort = tier
	}) {
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
	a.emitEvent("thread:updated", triage.ThreadUpdateEvent{Action: "full", Thread: &sanitized})
}

// firstLine truncates a command answer to its first line for error surfaces.
func firstLine(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return text[:idx]
	}
	return text
}
