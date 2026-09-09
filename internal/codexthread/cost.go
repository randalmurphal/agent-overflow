package codexthread

import (
	"context"
	"errors"
	"log"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// Thread estimates are optional cumulative backend values, separate from the
// per-turn token ledger. Read after settlement and retry at bounded intervals
// because billing can lag execution. Never derive per-turn cost from these totals.
// Session tokens and rollback epochs fence reads against provider-thread changes.
func (a *Service) NoteThreadCost(threadID, sessionToken string) {
	if a.store == nil || threadID == "" || a.lifeCtx().Err() != nil || a.isShuttingDown() {
		return
	}
	a.costMu.Lock()
	if a.costClosed {
		a.costMu.Unlock()
		return
	}
	epoch, ok := a.claimThreadCostReadLocked(threadID, sessionToken)
	if !ok {
		a.costMu.Unlock()
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
	ctx, cancel := context.WithCancel(a.lifeCtx())
	a.costInflight[threadID].cancel = cancel
	a.costWG.Add(1)
	a.costMu.Unlock()
	go func() {
		defer a.costWG.Done()
		defer cancel()
		token := sessionToken
		delays := [...]time.Duration{15 * time.Second, 60 * time.Second, 120 * time.Second}
		retry := 0
		for {
			retryable := a.readThreadCost(ctx, threadID, token, epoch)
			if retryable && retry < len(delays) {
				next, nextEpoch, again, newTurn := a.waitThreadCostRead(ctx, threadID, epoch, delays[retry])
				if !again {
					return
				}
				token, epoch = next, nextEpoch
				if newTurn {
					retry = 0
				} else {
					retry++
				}
			} else {
				next, nextEpoch, again := a.nextThreadCostRead(threadID)
				if !again {
					return
				}
				token, epoch, retry = next, nextEpoch, 0
			}
		}
	}()
}

// claimCodexThreadCostRead takes the per-thread read slot, or marks the
// holder dirty when one is already out. False means "someone else will
// re-read for you"; the returned epoch is the fence the winning read must
// still hold when it comes back.
func (a *Service) claimThreadCostRead(threadID, sessionToken string) (uint64, bool) {
	a.costMu.Lock()
	defer a.costMu.Unlock()
	return a.claimThreadCostReadLocked(threadID, sessionToken)
}

func (a *Service) claimThreadCostReadLocked(threadID, sessionToken string) (uint64, bool) {
	if a.costClosed {
		return 0, false
	}
	if a.costInflight == nil {
		a.costInflight = make(map[string]*threadCostRead)
	}
	if slot, busy := a.costInflight[threadID]; busy {
		slot.dirty = true
		// The LATEST settle owns the re-read, so it also owns the token.
		// Keeping the first claimant's would send the rerun at a session
		// that may have been replaced since, where readCodexThreadCost
		// refuses it and the newest turn never reaches the backend.
		if slot.token != sessionToken {
			slot.epoch++
		}
		slot.token = sessionToken
		select {
		case slot.wake <- struct{}{}:
		default:
		}
		return 0, false
	}
	a.costInflight[threadID] = &threadCostRead{token: sessionToken, wake: make(chan struct{}, 1)}
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
func (a *Service) nextThreadCostRead(threadID string) (string, uint64, bool) {
	a.costMu.Lock()
	defer a.costMu.Unlock()
	slot, ok := a.costInflight[threadID]
	if !ok {
		return "", 0, false
	}
	if !slot.dirty {
		delete(a.costInflight, threadID)
		return "", 0, false
	}
	slot.dirty = false
	select {
	case <-slot.wake:
	default:
	}
	return slot.token, slot.epoch, true
}

// waitThreadCostRead retains the single-flight slot while billing settles.
// A newly settled turn wakes it immediately; rollback and cancellation release it.
func (a *Service) waitThreadCostRead(ctx context.Context, threadID string, epoch uint64, delay time.Duration) (string, uint64, bool, bool) {
	a.costMu.Lock()
	slot := a.costInflight[threadID]
	if slot == nil {
		a.costMu.Unlock()
		return "", 0, false, false
	}
	wake := slot.wake
	dirty := slot.dirty
	a.costMu.Unlock()
	if !dirty {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-wake:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	a.costMu.Lock()
	defer a.costMu.Unlock()
	slot = a.costInflight[threadID]
	if slot == nil {
		return "", 0, false, false
	}
	if ctx.Err() != nil || (!slot.dirty && slot.epoch != epoch) {
		delete(a.costInflight, threadID)
		return "", 0, false, false
	}
	newTurn := slot.dirty
	slot.dirty = false
	select {
	case <-slot.wake:
	default:
	}
	return slot.token, slot.epoch, true, newTurn
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
func (a *Service) threadCostReadIsCurrent(threadID string, epoch uint64) bool {
	a.costMu.Lock()
	defer a.costMu.Unlock()
	return a.threadCostReadIsCurrentLocked(threadID, epoch)
}

func (a *Service) threadCostReadIsCurrentLocked(threadID string, epoch uint64) bool {
	slot, ok := a.costInflight[threadID]
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
func (a *Service) persistThreadCostIfCurrent(threadID string, epoch uint64, cost store.ProviderThreadCost) (bool, error) {
	a.costMu.Lock()
	defer a.costMu.Unlock()
	if !a.threadCostReadIsCurrentLocked(threadID, epoch) {
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
func (a *Service) ForgetThreadCost(threadID string) {
	if a.store == nil || threadID == "" {
		return
	}
	// Fence any read already in flight BEFORE the delete, not after: a read
	// that started under the old SessionRef and lands a moment later would
	// otherwise write back a row describing the provider thread this call
	// exists to forget — inert under the identity comparison, but it would
	// still displace whatever the new provider thread had already stored.
	a.costMu.Lock()
	if slot, ok := a.costInflight[threadID]; ok {
		slot.epoch++
		select {
		case slot.wake <- struct{}{}:
		default:
		}
	}
	a.costMu.Unlock()

	if err := a.store.DeleteProviderThreadCost(threadID); err != nil {
		log.Printf("codex thread cost: forget for %s: %v", threadID, err)
	}
}

// readCodexThreadCost performs one read and persists the result. Blocking;
// callers run it on their own goroutine.
func (a *Service) readThreadCost(parent context.Context, threadID, sessionToken string, epoch uint64) bool {
	sess, ok := a.session(threadID)
	if !ok || sess.Session == nil || (sessionToken != "" && sess.Token != sessionToken) {
		// The session died, was replaced, or was never Codex. There is no
		// live connection to ride and this is deliberately not worth an
		// ephemeral app-server: the estimate is a nicety, and spawning a
		// process per settled turn is exactly the traffic the account-usage
		// cache exists to avoid.
		return false
	}

	// a.lifeCtx() rather than Background: the read can sit for the full
	// DefaultThreadUsageTimeout against the ChatGPT backend, and shutdown must
	// not wait on a request whose answer nothing will read.
	ctx, cancel := context.WithTimeout(parent, codex.DefaultThreadUsageTimeout)
	defer cancel()

	usage, err := sess.Session.ReadThreadUsage(ctx)
	if err != nil {
		if errors.Is(err, codex.ErrThreadUsageUnavailable) {
			// A state answer, not a failure: old binary, no ChatGPT login, no
			// billing route, or credits-only pricing. The rate-table estimate
			// stays. Not logged at all — on a pre-0.148 codex this would be
			// one line per settled turn forever.
			return false
		}
		if parent.Err() != nil {
			return false
		}
		log.Printf("codex thread cost: read for %s: %v", threadID, err)
		return true
	}

	current, alive := a.session(threadID)
	if !alive || current.Token != sess.Token || current.Session != sess.Session || ctx.Err() != nil {
		return false
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
		return false
	}
	// The fence check and the write are one critical section (see
	// persistCodexThreadCostIfCurrent). A rollback that repointed (or cleared)
	// this thread's SessionRef while the request was out makes the figure in
	// hand the OLD provider thread's total; the rollback's own next settled
	// turn re-reads.
	stored, err := a.persistThreadCostIfCurrent(threadID, epoch, store.ProviderThreadCost{
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
		return true
	}
	if !stored {
		return false
	}
	// The composer's usage chip re-queries on this thread's usage-refresh
	// version, which the turn-complete emission already bumped — but that
	// bump fired before this read returned, so the chip would show the
	// rate-table figure until the next turn. This second, narrower nudge is
	// what makes the provider figure appear on the turn it describes.
	a.emit(eventchan.UsageThreadCost, map[string]any{"threadId": threadID})
	return true
}

// OverlayProviderThreadCost replaces exactly one ungrouped lifetime-thread
// bucket with Codex's cumulative estimate when the stored provider-thread
// identity still matches the thread.
func (a *Service) OverlayProviderThreadCost(query store.UsageQuery, buckets []store.UsageBucket) []store.UsageBucket {
	if query.ThreadID == "" || query.GroupBy != "" || query.Model != "" {
		return buckets
	}
	if query.FromMillis != 0 || query.ToMillis != 0 || len(buckets) > 1 || a.store == nil {
		return buckets
	}
	cost, found, err := a.store.GetProviderThreadCost(query.ThreadID)
	if err != nil {
		log.Printf("usage stats: provider thread cost for %s: %v", query.ThreadID, err)
		return buckets
	}
	if !found || (query.Provider != "" && query.Provider != cost.Provider) {
		return buckets
	}
	if len(buckets) == 0 {
		buckets = append(buckets, store.UsageBucket{})
	}
	buckets[0].CostUSD = cost.CostUSD()
	buckets[0].UnpricedRows = buckets[0].PendingRows
	buckets[0].CostSource = cost.CostSource
	return buckets
}
