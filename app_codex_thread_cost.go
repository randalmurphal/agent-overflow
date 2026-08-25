package main

import (
	"context"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// Codex's own cumulative cost estimate for a thread.
//
// WHAT THIS IS. Codex reports no cost anywhere on its turn wire, so every
// Codex `usage_ledger` row persists tokens alone (`cost_source='none'`) and
// AO prices them at query time from `internal/usagecost`. Since codex 0.148
// there is a second, independent answer available: `account/usage/read`
// accepts a `threadId` and returns `threadUsage.estimatedUsageUsdMicros` —
// the BACKEND's estimate of what that thread has cost, cumulative over the
// thread's whole life.
//
// WHY IT DOES NOT REPLACE THE LEDGER. The figure is cumulative, optional
// ("populated when a thread was requested and its billing route is
// available"), and is upstream's own estimate rather than a settled charge.
// So it is stored BESIDE the ledger, in `provider_thread_cost`, and the rate
// table remains the answer whenever the estimate is absent — an older codex,
// an API-key login, a thread with no billing route, or a credits-only
// account. Nothing about the token accounting changes.
//
// WHY IT IS NOT A PER-TURN DELTA. Subtracting the previous total to
// manufacture a per-turn figure would attribute the whole difference to the
// last turn, and the difference is not a turn's cost: the backend restates
// its estimate as it learns more, a resumed thread accumulates spend from
// sessions AO never observed, and any read that fails leaves a gap the next
// delta silently absorbs. AO already has an exact per-turn decomposition —
// its own ledger. Keeping the provider figure at the grain the provider
// states it at (the thread) means the two are never in a position to
// contradict each other row by row: one is the thread's total, the other is
// the turns.
//
// WHEN IT RUNS. Once per settled top-level Codex turn, after the turn is
// already persisted, off the provider read loop, single-flighted per thread.
// Never on a timer and never while idle. Every failure is logged and leaves
// the rate-table fallback in place; nothing about a turn's completion depends
// on it.

// usageThreadCostEvent nudges the thread-scoped usage surfaces after a
// provider figure lands. It carries only the thread id: every usage surface
// refetches through GetUsageStats rather than trusting a pushed number, which
// is the same rule provider:turn_completed's refresh bump follows.
const usageThreadCostEvent = "usage:thread_cost"

// noteCodexThreadCost fires the post-turn thread-cost read for threadID.
//
// Called from the provider event fan-out on EventTurnComplete AFTER
// triage.Handle has persisted the turn and its usage rows (per core principle
// 3, the provider figure is recorded on completion, not accumulated in
// memory). It returns immediately: the read itself runs on its own goroutine
// because the app-server forwards it to the ChatGPT backend.
//
// sessionToken pins the session that completed the turn. A session replaced
// between the settle and the read is a different process — possibly on a
// different login — and its estimate would be attributed to this thread
// wrongly, so the goroutine re-checks the token rather than re-resolving the
// thread's current session.
func (a *App) noteCodexThreadCost(threadID, sessionToken string) {
	if a.store == nil || threadID == "" {
		return
	}
	epoch, ok := a.claimCodexThreadCostRead(threadID, sessionToken)
	if !ok {
		// A read for this thread is already out. It is NOT enough to drop
		// this turn: the in-flight request may have been sent — and the
		// backend may have computed its total — before this turn completed,
		// so its answer can predate the very turn that triggered us. The
		// claim is marked dirty instead (and takes over the slot's session
		// token), and the owner re-reads once when it lands, which costs at
		// most one extra round trip per burst and leaves the stored figure
		// describing a state at or after the last settled turn.
		return
	}
	go func() {
		token := sessionToken
		for {
			a.readCodexThreadCost(threadID, token, epoch)
			next, nextEpoch, again := a.nextCodexThreadCostRead(threadID)
			if !again {
				return
			}
			token, epoch = next, nextEpoch
		}
	}()
}

// claimCodexThreadCostRead takes the per-thread read slot, or marks the
// holder dirty when one is already out. False means "someone else will
// re-read for you"; the returned epoch is the fence the winning read must
// still hold when it comes back.
func (a *App) claimCodexThreadCostRead(threadID, sessionToken string) (uint64, bool) {
	a.codexThreadCost.mu.Lock()
	defer a.codexThreadCost.mu.Unlock()
	if a.codexThreadCost.inflight == nil {
		a.codexThreadCost.inflight = make(map[string]*codexThreadCostRead)
	}
	if slot, busy := a.codexThreadCost.inflight[threadID]; busy {
		slot.dirty = true
		// The LATEST settle owns the re-read, so it also owns the token.
		// Keeping the first claimant's would send the rerun at a session
		// that may have been replaced since, where readCodexThreadCost
		// refuses it and the newest turn never reaches the backend.
		slot.token = sessionToken
		return 0, false
	}
	a.codexThreadCost.inflight[threadID] = &codexThreadCostRead{token: sessionToken}
	return 0, true
}

// nextCodexThreadCostRead ends one pass: it reports the token and epoch for
// another one, or RELEASES the slot and reports false.
//
// The release and the dirty check are one critical section on purpose. Split
// apart — take the flag, return, release in a defer — a settle landing in
// between marks a slot that is about to be deleted, and its claimant has
// already been told "someone else will re-read for you". The wakeup is lost
// and the stored figure stays behind the last settled turn until some future
// turn happens to settle while the slot is free.
func (a *App) nextCodexThreadCostRead(threadID string) (string, uint64, bool) {
	a.codexThreadCost.mu.Lock()
	defer a.codexThreadCost.mu.Unlock()
	slot, ok := a.codexThreadCost.inflight[threadID]
	if !ok {
		return "", 0, false
	}
	if !slot.dirty {
		delete(a.codexThreadCost.inflight, threadID)
		return "", 0, false
	}
	slot.dirty = false
	return slot.token, slot.epoch, true
}

// codexThreadCostReadIsCurrent reports whether a read that started at epoch
// may still be written back. False after forgetCodexThreadCost bumped the
// fence: the answer in hand describes a provider thread this AO thread has
// since stopped being.
//
// Observation only. A caller that is about to WRITE must not ask this and then
// write — the rollback can bump the fence and delete the row in the gap, and
// the write would resurrect it. persistCodexThreadCostIfCurrent is the one
// that answers and writes without a gap.
func (a *App) codexThreadCostReadIsCurrent(threadID string, epoch uint64) bool {
	a.codexThreadCost.mu.Lock()
	defer a.codexThreadCost.mu.Unlock()
	return a.codexThreadCostReadIsCurrentLocked(threadID, epoch)
}

func (a *App) codexThreadCostReadIsCurrentLocked(threadID string, epoch uint64) bool {
	slot, ok := a.codexThreadCost.inflight[threadID]
	return ok && slot.epoch == epoch
}

// persistCodexThreadCostIfCurrent stores one read's answer, but only while the
// epoch it started under is still the live one.
//
// The check and the write are ONE critical section, and that is the whole
// point of the function. Split apart, a rollback landing between them bumps
// the fence and deletes the row after the check passed, and the write then
// restores exactly the figure the rollback removed — a total describing a
// Codex thread this AO thread no longer points at. forgetCodexThreadCost takes
// the same mutex to bump the fence, so its delete is strictly ordered after any
// write that got past the check; either the write happens and is then deleted,
// or the fence moved first and there is no write.
//
// Holding the mutex across a SQLite write is deliberate and cheap: every other
// holder (claim, end-of-pass, forget) is a few map operations, and the
// alternative is a store-level compare-and-set for one row that has exactly
// one writer.
//
// The fence is the LIVE half of the invalidation and is still needed after
// v68's stored provider-thread identity: a read that started before a rollback
// carries the OLD thread's figure, and the row it would write names the OLD
// thread — which the rollback has already moved the AO thread away from, so
// the row would be inert but the write would still cost a round trip and
// overwrite a fresher one. The fence refuses it outright.
func (a *App) persistCodexThreadCostIfCurrent(threadID string, epoch uint64, cost store.ProviderThreadCost) (bool, error) {
	a.codexThreadCost.mu.Lock()
	defer a.codexThreadCost.mu.Unlock()
	if !a.codexThreadCostReadIsCurrentLocked(threadID, epoch) {
		return false, nil
	}
	if err := a.store.PutProviderThreadCost(cost); err != nil {
		return false, err
	}
	return true, nil
}

// forgetCodexThreadCost drops a thread's stored provider figure.
//
// The row is keyed by the AO thread id but DESCRIBES the provider thread it
// was read from, so any operation that repoints (or clears) the thread's
// SessionRef leaves the stored total describing a provider thread this AO
// thread no longer is: a Codex rollback that forks into a new provider thread
// carries a smaller history, and a rollback to turn 0 clears the ref entirely.
//
// Callers: the conversation-rollback path (app_conversation_rollback.go), on
// every Codex rollback that repoints or clears SessionRef. Deliberately not
// wired into the store's delete path — dropping the THREAD takes the row with
// it through the schema.
//
// A FAILED delete is HARMLESS, and structurally so. Since migration v68 the
// row records the provider thread it describes, and GetProviderThreadCost only
// answers when that still matches the AO thread's SessionRef — which the
// rollback has already moved. So a row this call could not delete is a row
// nothing can read: the usage surfaces fall back to the rate table exactly as
// they would have if the delete had landed, and the next settled turn
// overwrites it. That is why the failure is logged rather than raised on the
// thread; there is no degradation left to tell the user about, and it does not
// depend on this process staying alive to stay true.
func (a *App) forgetCodexThreadCost(threadID string) {
	if a.store == nil || threadID == "" {
		return
	}
	// Fence any read already in flight BEFORE the delete, not after: a read
	// that started under the old SessionRef and lands a moment later would
	// otherwise write back a row describing the provider thread this call
	// exists to forget — inert under the identity comparison, but it would
	// still displace whatever the new provider thread had already stored.
	a.codexThreadCost.mu.Lock()
	if slot, ok := a.codexThreadCost.inflight[threadID]; ok {
		slot.epoch++
	}
	a.codexThreadCost.mu.Unlock()

	if err := a.store.DeleteProviderThreadCost(threadID); err != nil {
		log.Printf("codex thread cost: forget for %s: %v", threadID, err)
	}
}

// readCodexThreadCost performs one read and persists the result. Blocking;
// callers run it on their own goroutine.
func (a *App) readCodexThreadCost(threadID, sessionToken string, epoch uint64) {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.codex == nil || (sessionToken != "" && sess.token != sessionToken) {
		// The session died, was replaced, or was never Codex. There is no
		// live connection to ride and this is deliberately not worth an
		// ephemeral app-server: the estimate is a nicety, and spawning a
		// process per settled turn is exactly the traffic the account-usage
		// cache exists to avoid.
		return
	}

	// a.lifeCtx() rather than Background: the read can sit for the full
	// DefaultThreadUsageTimeout against the ChatGPT backend, and shutdown must
	// not wait on a request whose answer nothing will read.
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codex.DefaultThreadUsageTimeout)
	defer cancel()

	usage, err := sess.codex.ReadThreadUsage(ctx)
	if err != nil {
		if errors.Is(err, codex.ErrThreadUsageUnavailable) {
			// A state answer, not a failure: old binary, no ChatGPT login, no
			// billing route, or credits-only pricing. The rate-table estimate
			// stays. Not logged at all — on a pre-0.148 codex this would be
			// one line per settled turn forever.
			return
		}
		log.Printf("codex thread cost: read for %s: %v", threadID, err)
		return
	}

	usdMicros := int64(0)
	if usage.USDMicros != nil {
		usdMicros = *usage.USDMicros
	}
	// The stored row names the PROVIDER thread the figure describes, and the
	// backend's own echo is where that name comes from: ReadThreadUsage has
	// already refused an echo that disagrees with the thread it asked about,
	// so this is the id the backend attributed the estimate to. Reading the AO
	// thread's SessionRef here instead would be wrong in exactly the case the
	// column exists for — a rollback repoints the row before it fences this
	// read, so the row would be stamped with the NEW provider thread while
	// holding the OLD one's total.
	//
	// An echo the wire omitted leaves the figure unattributable. It is stored
	// as nothing rather than as an unnamed row: the rate-table fallback is
	// already correct, and a row that cannot be checked against the thread is
	// the shape v68 removed.
	sessionRef := usage.ThreadID
	if sessionRef == "" {
		log.Printf("codex thread cost: read for %s carried no thread id; keeping the rate-table estimate", threadID)
		return
	}
	// The fence check and the write are one critical section (see
	// persistCodexThreadCostIfCurrent). A rollback that repointed (or cleared)
	// this thread's SessionRef while the request was out makes the figure in
	// hand the OLD provider thread's total; the rollback's own next settled
	// turn re-reads.
	stored, err := a.persistCodexThreadCostIfCurrent(threadID, epoch, store.ProviderThreadCost{
		ThreadID:      threadID,
		SessionRef:    sessionRef,
		Provider:      string(provider.Codex),
		CostSource:    store.ProviderThreadCostSourceEstimate,
		CostUSDMicros: usdMicros,
		CreditsMicros: usage.CreditsMicros,
		UpdatedAt:     time.Now().UnixMilli(),
	})
	if err != nil {
		log.Printf("codex thread cost: persist for %s: %v", threadID, err)
		return
	}
	if !stored {
		return
	}
	// The composer's usage chip re-queries on this thread's usage-refresh
	// version, which the turn-complete emission already bumped — but that
	// bump fired before this read returned, so the chip would show the
	// rate-table figure until the next turn. This second, narrower nudge is
	// what makes the provider figure appear on the turn it describes.
	a.emit(usageThreadCostEvent, map[string]any{"threadId": threadID})
}
