package codex

import (
	"encoding/json"
	"log"

	"agent-overflow/internal/provider"
)

// Per-turn usage accounting for Codex sessions.
//
// Codex has no per-turn usage signal on the wire: `turn/completed` carries
// no token fields, and `thread/tokenUsage/updated` reports a cumulative
// `total` (plus the last request's `last`). Verified in codex-rs source:
//
//   - `total` accumulates via TokenUsageInfo::append_last_usage add_assign
//     (protocol.rs) and is NEVER reset — not by compaction
//     (recompute_token_usage rewrites only `last`, core/src/session/mod.rs)
//     and not by resume (the rollout's last TokenCount seeds it).
//   - Exception: fill_to_context_window (the ContextWindowExceeded
//     sentinel) DESTROYS `total` — it pegs totalTokens to the context
//     window and zeroes the component fields. Deltas across that event
//     are garbage; the accounting re-baselines on it.
//   - Wire `inputTokens` INCLUDES `cachedInputTokens`
//     (TokenUsage::non_cached_input subtracts); the normalized shape
//     reports non-cached input + cache-read separately so it sums with
//     Claude rows. `reasoningOutputTokens` is already inside
//     `outputTokens` and is carried as informational detail only.
//
// Per-turn usage is therefore the delta of `total` between turn
// boundaries. Live-verified 2026-07-03 on codex-cli 0.142.5 (see
// codex-wire.md §thread/tokenUsage/updated): the turn's final
// tokenUsage notification arrives BEFORE turn/completed, the cumulative
// persists exactly across resume, and `thread/resume` emits a pre-turn
// seed notification carrying the historical cumulative — so the
// baseline normally comes from that seed. The skip-first-resumed-turn
// path below is a backstop for a resume where no seed arrives: without
// a baseline the first turn's delta would swallow the thread's entire
// history, and skipping one turn beats double-counting; every later
// turn is exact.
//
// Codex reports no USD cost anywhere on the wire (only an opaque credits
// balance), so ledger rows from this provider carry tokens only.
//
// All methods run on the session read-loop goroutine (dispatchNotification
// → updateNotificationState); no locking needed, mirroring the parser-side
// state in the claude package.

// codexWireTokenBreakdown is the full TokenUsageBreakdown wire shape
// (camelCase, app-server-protocol thread.rs). The context-meter path in
// protocol_thread.go keeps its own minimal struct; this one exists for
// accounting, which needs every component.
type codexWireTokenBreakdown struct {
	TotalTokens           int `json:"totalTokens"`
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
}

func (b codexWireTokenBreakdown) toTokenUsage() provider.TokenUsage {
	nonCached := b.InputTokens - b.CachedInputTokens
	if nonCached < 0 {
		nonCached = 0
	}
	return provider.TokenUsage{
		InputTokens:           nonCached,
		CacheReadInputTokens:  b.CachedInputTokens,
		OutputTokens:          b.OutputTokens,
		ReasoningOutputTokens: b.ReasoningOutputTokens,
	}
}

func (b codexWireTokenBreakdown) isZero() bool { return b == codexWireTokenBreakdown{} }

// usageAccounting tracks the cumulative→delta state for one session.
type usageAccounting struct {
	// latest is the most recent cumulative `total` observation.
	latest    codexWireTokenBreakdown
	latestSet bool
	// accounted is the cumulative value already attributed to settled
	// turns; the next turn's usage is latest - accounted.
	accounted codexWireTokenBreakdown
	// baselined reports whether `accounted` is a trustworthy baseline.
	// Fresh threads start baselined at zero. Resumed threads start
	// unbaselined and either baseline from a pre-first-turn observation
	// (the historical seed) or skip the first turn's accounting.
	baselined bool
	// firstTurnStarted flips on the first turn/started; after that an
	// observation can no longer be treated as a pure historical seed.
	firstTurnStarted bool
}

func newUsageAccounting(resumed bool) usageAccounting {
	return usageAccounting{baselined: !resumed}
}

// observe folds a `thread/tokenUsage/updated` params payload into the
// accounting state. Zero-total payloads are ignored (some notifications
// carry only rate-limit refreshes).
func (a *usageAccounting) observe(params json.RawMessage) {
	var payload struct {
		TokenUsage struct {
			Total              codexWireTokenBreakdown `json:"total"`
			ModelContextWindow int                     `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		log.Printf("codex: usage accounting: malformed tokenUsage notification: %v", err)
		return
	}
	total := payload.TokenUsage.Total
	if total.isZero() {
		return
	}

	// ContextWindowExceeded sentinel (fill_to_context_window): totalTokens
	// pegged to the window with zeroed components. The cumulative is
	// destroyed from here on, so re-baseline — the delta across this event
	// is unattributable and is dropped, matching what Codex itself did to
	// the data.
	window := payload.TokenUsage.ModelContextWindow
	exceededSentinel := window > 0 && total.TotalTokens == window &&
		total.InputTokens == 0 && total.OutputTokens == 0
	if exceededSentinel {
		a.latest = total
		a.latestSet = true
		a.accounted = total
		a.baselined = true
		return
	}

	if !a.baselined && !a.firstTurnStarted {
		// Historical seed on a resumed thread: everything observed before
		// the first turn belongs to already-accounted history.
		a.accounted = total
		a.baselined = true
	}
	a.latest = total
	a.latestSet = true
}

// onTurnStart marks that observations can no longer be pre-turn seeds.
func (a *usageAccounting) onTurnStart() {
	a.firstTurnStarted = true
}

// settleTurn returns the per-turn usage delta at a turn boundary and
// advances the accounted baseline. Returns zero usage (IsZero) when the
// turn produced nothing attributable — no observations, an unbaselined
// resume, or a cumulative that moved backwards (re-baselines defensively).
func (a *usageAccounting) settleTurn() provider.TokenUsage {
	if !a.latestSet {
		return provider.TokenUsage{}
	}
	if !a.baselined {
		// First turn after a resume with no pre-turn seed: the delta would
		// include the thread's whole history. Skip it and baseline here so
		// every subsequent turn is exact.
		a.accounted = a.latest
		a.baselined = true
		return provider.TokenUsage{}
	}
	if a.latest.TotalTokens < a.accounted.TotalTokens {
		// Cumulative moved backwards — never observed on a healthy wire.
		// Re-baseline rather than emit garbage.
		a.accounted = a.latest
		return provider.TokenUsage{}
	}
	delta := codexWireTokenBreakdown{
		TotalTokens:           a.latest.TotalTokens - a.accounted.TotalTokens,
		InputTokens:           max(a.latest.InputTokens-a.accounted.InputTokens, 0),
		CachedInputTokens:     max(a.latest.CachedInputTokens-a.accounted.CachedInputTokens, 0),
		OutputTokens:          max(a.latest.OutputTokens-a.accounted.OutputTokens, 0),
		ReasoningOutputTokens: max(a.latest.ReasoningOutputTokens-a.accounted.ReasoningOutputTokens, 0),
	}
	a.accounted = a.latest
	return delta.toTokenUsage()
}

// attachTurnUsage stamps the per-turn usage delta onto a parent-thread
// turn-complete event. Interrupted/failed turns still account — their
// tokens were consumed. Model attribution is the session's configured
// model: Codex cannot attribute per-model (review/compact model usage
// rolls into the same cumulative), and mid-session reroutes are not
// tracked onto Session.model, so this is the best available label.
func (s *Session) attachTurnUsage(meta *provider.WireTurnCompleteMeta) {
	usage := s.usageAcct.settleTurn()
	if usage.IsZero() {
		return
	}
	meta.Usage = &usage
	meta.ModelUsage = []provider.ModelTokenUsage{{Model: s.currentModel(), TokenUsage: usage}}
}
