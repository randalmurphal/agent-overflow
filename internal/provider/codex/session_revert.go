package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

// The `thread/revert` cut: the only one of upstream's three history
// truncations that keeps the thread's identity.
//
// `thread/fork { lastTurnId }` (session_fork.go) answers "give me a new
// thread that stops here"; `thread/revert { beforeTurnId }` answers "this
// thread stops here". For edit-and-resend the second is what the user
// asked for — the thread they are editing is the thread they keep — and
// AO only used the fork because before 0.148 there was no other cut that
// wasn't the deprecated, message-counting `thread/rollback`.
//
// Two properties of upstream's handler shape everything below
// (codex-rs/app-server/src/request_processors/thread_processor.rs
// `thread_revert_response`, rust-v0.149.0):
//
//   - It is refused outright on a LEGACY-history thread ("thread/revert
//     only supports paginated threads"), before it touches anything.
//     Upstream's default IS legacy, so a client that says nothing gets a
//     thread that can never be reverted; that is why AO now asks for
//     `historyMode: "paginated"` on `thread/start` wherever the server is
//     new enough (threadStartHistoryMode below). Threads AO created before
//     that — and every thread created by a pre-0.148 binary — stay legacy
//     for life, because `thread/resume` has no history-mode field to
//     change it with. Those keep falling back to the fork cut. The refusal
//     is pre-mutation, which is what makes the fallback safe rather than
//     a guess.
//   - It does NOT refuse a running turn. It submits a shutdown, waits up
//     to 10s for the thread runtime to stop, reverts the store, and
//     reloads the runtime with `has_live_in_progress_turn = false`. In
//     other words a mid-turn revert silently destroys the turn. AO
//     refuses instead (see Revert) — the app layer already interrupts
//     first on every path that can reach here, and a provider-level
//     refusal is what keeps a future caller from discovering the
//     silent-kill behavior in production.
//
// What upstream does NOT destroy is the pre-revert history: the local
// thread store writes a NEW immutable rollout referencing the retained
// prefix and moves the SQLite pointer to it
// (codex-rs/thread-store/src/local/revert_thread.rs), so the old rollout
// file survives on disk exactly like a fork's source does.
const (
	threadRevertMethod = "thread/revert"
	// threadRevertedMethod is the notification upstream sends immediately
	// after a successful revert response, on the same connection.
	threadRevertedMethod = "thread/reverted"

	// threadRevertMinimumCodexVersion is the first codex release carrying
	// `thread/revert`. A per-METHOD floor read off the handshake
	// (app_server_version.go), never a second probe, and never the
	// package-wide launch floor.
	threadRevertMinimumCodexVersion = "0.148.0"

	// paginatedThreadHistoryMode is the `thread.historyMode` value
	// upstream requires for a revert. Anything else — including the empty
	// string an app-server too old to report the field leaves behind — is
	// "not revertible", fail closed.
	paginatedThreadHistoryMode = "paginated"

	// threadRevertEchoTimeout bounds the wait for the `thread/reverted`
	// notification upstream sends immediately after the response, on the
	// same connection. Missing it is not a failure (see Revert): the
	// response is the authoritative answer for the client that ASKED.
	threadRevertEchoTimeout = 5 * time.Second
)

// ErrThreadRevertUnsupported means this connection cannot cut history in
// place — the app-server predates 0.148, or the thread is not a paginated
// one. Like ErrThreadUsageUnavailable it is a STATE answer rather than a
// failure: the caller falls back to the fork cut, which every supported
// codex answers. It is only ever returned for refusals upstream makes
// BEFORE it mutates anything, so the fallback cannot land on a
// half-reverted thread.
var ErrThreadRevertUnsupported = errors.New("codex: thread/revert unavailable")

// ErrThreadRevertAnchorUnresolvable means the `beforeTurnId` AO named is not
// a turn upstream can cut at on THIS thread's current history.
//
// It is a separate answer from ErrThreadRevertUnsupported because it says
// nothing about the connection or the thread's history mode — the same thread
// would revert at a different anchor. What makes it worth naming is that its
// dominant cause is a cut that ALREADY HAPPENED: a revert whose provider half
// succeeded and whose local half then failed leaves AO retrying with an anchor
// upstream has since dropped, and without this the retry raises a hard error
// forever (see cutCodexThreadHistory).
//
// Like ErrThreadRevertUnsupported it is only returned for refusals raised
// BEFORE upstream mutates anything: every message below comes out of
// `history_base_at_boundary` (codex-rs/thread-store/src/local/paginated_fork.rs
// @ rust-v0.149.0), which runs before the replacement rollout is written and
// long before the SQLite pointer CAS. Upstream's handler still reloads the
// thread runtime afterwards, so the connection is left with a live, unchanged
// thread the fork cut can be taken on.
var ErrThreadRevertAnchorUnresolvable = errors.New("codex: thread/revert anchor unresolvable")

// ErrThreadRevertOutcomeUnknown means the cut was written to the
// app-server and nothing answered it — a client-side timeout, a cancelled
// context, or a connection that died mid-request.
//
// It is the one error the caller must NOT read as "no cut happened".
// `thread/revert` mutates before it replies, so a lost response is
// exactly as consistent with an applied cut as with a refused one, and
// treating it as a failure is what leaves AO's history cache wider than
// the provider's while the late `thread/reverted` echo reads as a foreign
// writer's cut. The expectation is deliberately left ARMED for that echo.
//
// The caller resolves the ambiguity by ASKING — VerifyRevertBoundary
// re-reads the thread's durable turns — rather than by guessing. See
// cutCodexThreadHistory.
var ErrThreadRevertOutcomeUnknown = errors.New("codex: thread/revert outcome unknown")

// ErrThreadRevertInFlight means another `thread/revert` on this session is
// still waiting on its response and echo. Callers retry after it settles;
// see armRevertExpectation for why overlapping cuts are refused rather
// than queued.
var ErrThreadRevertInFlight = errors.New("codex: thread/revert already in flight")

// The invalid_request messages upstream raises while resolving `beforeTurnId`,
// all from paginated_fork.rs / thread_history/turn_lookup.rs at rust-v0.149.0.
// Matched on text because upstream folds every one of them onto the single
// -32600 code (thread_store_mutation_error), which the paginated-history gate
// also uses.
var threadRevertAnchorMarkers = []string{
	// find_turn: no row for this turn id in the thread's current lineage —
	// the shape a re-run of an already-applied cut takes.
	"turn not found:",
	// The turn exists but its rollout position was never persisted, so
	// there is no byte offset to cut at.
	"does not have persisted rollout positions",
	"does not have a persisted start boundary",
	// The anchor resolves outside the history this thread inherited.
	"fork boundary exceeds inherited source history",
}

// RevertedThread is the subset of upstream's ThreadRevertResponse AO
// reads.
//
// `thread.turns` is deliberately absent: upstream documents it as always
// empty on this response and points clients at `thread/turns/list` to
// re-hydrate, so there is no surviving-tail echo to validate the way
// ForkAt validates the fork's. The identity echo is checked instead, and
// it is the load-bearing one here — a revert that answered with a
// different thread id would mean AO's SessionRef no longer names the
// thread that was cut.
type RevertedThread struct {
	// ThreadID is the reverted thread. Always equal to the session's own
	// root thread; Revert fails rather than returning a mismatch.
	ThreadID string
	// EchoConfirmed reports whether the `thread/reverted` notification
	// arrived before threadRevertEchoTimeout. False is a warning, not a
	// failure — see Revert.
	EchoConfirmed bool
}

// revertExpectation records the cut AO asked for so the `thread/reverted`
// notification — which carries ONLY a threadId — can be matched to a
// boundary. Without it an echo says "something was cut" and nothing more.
type revertExpectation struct {
	threadID     string
	beforeTurnID string
	echo         chan struct{}
	closed       bool
	// inFlight is true only while the Revert call that armed this
	// expectation is still waiting on its response and echo. It goes
	// false on the way out even when the record is deliberately RETAINED
	// (missing echo, unknown outcome), which is what keeps a retained
	// record from bricking every later cut while still refusing a
	// concurrent one — see armRevertExpectation.
	inFlight atomic.Bool
}

// armRevertExpectation installs expectation as the session's pending cut,
// refusing while another Revert is still waiting on one.
//
// Concurrency here is not hypothetical: the echo carries ONLY a threadId,
// so two overlapping cuts on the same thread are indistinguishable to
// dispatchThreadReverted, and a plain Store would let the second call's
// expectation replace the first's — the first's echo would then close a
// channel nobody is waiting on while the first Revert times out on a cut
// that DID happen. Since the wire cannot tell them apart, the honest
// answer is to refuse the second one and let the caller retry after the
// first settles.
func (s *Session) armRevertExpectation(expectation *revertExpectation) error {
	for {
		prior := s.pendingRevert.Load()
		if prior != nil && prior.inFlight.Load() {
			return fmt.Errorf(
				"%w: thread %s is still cutting before turn %s",
				ErrThreadRevertInFlight, prior.threadID, prior.beforeTurnID,
			)
		}
		if s.pendingRevert.CompareAndSwap(prior, expectation) {
			return nil
		}
	}
}

// revertAnsweredUpstream reports whether a failed `thread/revert` carries
// upstream's own DECISION (a JSON-RPC error response) as opposed to a
// missing answer.
//
// Only the former proves no cut happened. A timeout, a cancelled context,
// a connection that stopped mid-request, or a write that never made it
// all leave the outcome unknown — the request may have been applied and
// its response lost — and Revert reports those as
// ErrThreadRevertOutcomeUnknown instead of as a failure. Classifying a
// never-sent request as unknown is the safe direction: the caller's
// verification finds the anchor still present and proceeds exactly as it
// would have on a hard failure.
func revertAnsweredUpstream(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr)
}

// recordThreadHistoryMode stores the `thread.historyMode` a
// thread/start or thread/resume response reported.
//
// The field is `#[experimental("thread.historyMode")]` upstream, so it
// only arrives because every AO handshake sets `experimentalApi`
// (notification_catalog.go). An app-server that omits it leaves ""
// behind, which reads as "not paginated" — the same fail-closed posture
// every version gate takes.
func (s *Session) recordThreadHistoryMode(threadResponse json.RawMessage) {
	mode := strings.TrimSpace(readNestedString(threadResponse, "thread", "historyMode"))
	s.threadHistoryMode.Store(&mode)
}

// ThreadHistoryMode returns the persisted history contract this session's
// thread was created with ("legacy", "paginated", or "" when the
// app-server did not say).
func (s *Session) ThreadHistoryMode() string {
	if mode := s.threadHistoryMode.Load(); mode != nil {
		return *mode
	}
	return ""
}

// SupportsThreadRevert reports whether an in-place cut is available on
// this connection AND this thread. Callers use it to choose between the
// two cuts before paying for a round trip; Revert re-checks both
// conditions itself, so this is an optimisation, not the guard.
func (s *Session) SupportsThreadRevert() bool {
	return s.appServerAtLeast(threadRevertMinimumCodexVersion) &&
		s.ThreadHistoryMode() == paginatedThreadHistoryMode
}

// Revert truncates THIS thread's durable history to the prefix before
// beforeTurnID — that turn and every later one are dropped — and keeps
// the thread id, its runtime, and this connection's subscriptions.
// Upstream reloads the thread runtime under the same id and explicitly
// does not require a follow-up `thread/resume` ("Full teardown would
// force clients to call thread/resume after a successful revert").
//
// beforeTurnID is the FIRST DROPPED turn, the mirror image of ForkAt's
// lastTurnID (the last KEPT turn). The two anchors are one turn apart and
// resolved separately by the app layer, because the turn between them may
// not exist in AO's own turn rows.
//
// Failure modes, in the order they are checked:
//
//   - No anchor: refused. An empty beforeTurnId is not "revert nothing",
//     it is a params error upstream, and treating it as a no-op here
//     would let a caller with a lost anchor believe history was cut.
//   - Too-old app-server / non-paginated thread:
//     ErrThreadRevertUnsupported, before any RPC.
//   - Turn in flight: refused loudly. Upstream would kill the turn (see
//     the file header); AO's callers all quiesce the thread first, so
//     reaching here mid-turn is a bug, not a case to paper over.
//   - Upstream's own "only supports paginated threads" refusal is
//     re-classified to ErrThreadRevertUnsupported: it is raised before
//     the handler touches the thread, so the caller may fall back to a
//     fork on the same connection.
//
// A missing `thread/reverted` echo is reported (EchoConfirmed=false) and
// logged, never failed: the response already carries upstream's answer,
// and failing here would abandon a cut that DID happen.
func (s *Session) Revert(ctx context.Context, beforeTurnID string) (RevertedThread, error) {
	beforeTurnID = strings.TrimSpace(beforeTurnID)
	if beforeTurnID == "" {
		return RevertedThread{}, fmt.Errorf("codex: %s: beforeTurnId is required", threadRevertMethod)
	}
	threadID := s.rootThreadID()
	if threadID == "" {
		return RevertedThread{}, fmt.Errorf("codex: %s: session has no thread id yet", threadRevertMethod)
	}
	if !s.appServerAtLeast(threadRevertMinimumCodexVersion) {
		return RevertedThread{}, fmt.Errorf(
			"%w: app-server %q predates %s",
			ErrThreadRevertUnsupported, s.AppServerVersion(), threadRevertMinimumCodexVersion,
		)
	}
	if mode := s.ThreadHistoryMode(); mode != paginatedThreadHistoryMode {
		return RevertedThread{}, fmt.Errorf(
			"%w: thread %s uses %q history, not %q",
			ErrThreadRevertUnsupported, threadID, mode, paginatedThreadHistoryMode,
		)
	}
	// Closed-session check MUST precede the activeTurnID read AND run under
	// s.mu: Close zeroes s.turn, so a post-Close call would read "idle" and
	// proceed into a request on a dead pipe (see ErrSessionClosed). Under mu
	// it is ordered against the zeroing; before Lock it leaves a preemption
	// window.
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		return RevertedThread{}, fmt.Errorf("codex: %s: %w", threadRevertMethod, ErrSessionClosed)
	}
	activeTurnID := s.turn.activeTurnID
	s.mu.Unlock()
	if activeTurnID != "" {
		return RevertedThread{}, fmt.Errorf(
			"codex: %s: turn %q is in flight on thread %s — interrupt it before reverting",
			threadRevertMethod, activeTurnID, threadID,
		)
	}

	// Armed BEFORE the write: upstream emits the notification immediately
	// after the response, and both travel the same pipe, so an
	// expectation registered after the response could still lose the race
	// with the read loop.
	expectation := &revertExpectation{
		threadID:     threadID,
		beforeTurnID: beforeTurnID,
		echo:         make(chan struct{}),
	}
	expectation.inFlight.Store(true)
	if err := s.armRevertExpectation(expectation); err != nil {
		return RevertedThread{}, err
	}
	// Only the WAIT is exclusive; the record outlives it (see below), so
	// the next Revert may replace a settled expectation but never a live
	// one.
	defer expectation.inFlight.Store(false)
	// Retention is deliberate on exactly one path — see the clears
	// below. An expectation left in place is what keeps a LATE echo from
	// reading as a foreign writer's cut.
	forget := func() { s.pendingRevert.CompareAndSwap(expectation, nil) }

	resp, err := s.sendRequest(ctx, threadRevertMethod, map[string]any{
		"threadId":     threadID,
		"beforeTurnId": beforeTurnID,
	})
	if err != nil {
		if !revertAnsweredUpstream(err) {
			// The request was written and nothing answered it: a
			// timeout, a cancelled context, or a connection that died
			// mid-flight. That is NOT proof no cut happened — upstream
			// may have applied it and lost the response — so the
			// expectation stays ARMED (a late echo of AO's own cut must
			// not raise the foreign-writer alarm) and the caller is told
			// the outcome is unknown rather than told it failed.
			return RevertedThread{}, fmt.Errorf(
				"%w: %s on thread %s before turn %s: %w",
				ErrThreadRevertOutcomeUnknown, threadRevertMethod, threadID, beforeTurnID, err,
			)
		}
		// Upstream ANSWERED, so it decided; no cut happened and any
		// later `thread/reverted` really is somebody else's.
		forget()
		return RevertedThread{}, fmt.Errorf("codex: %s: %w", threadRevertMethod, classifyThreadRevertError(err))
	}
	// Past this point the cut HAPPENED: upstream only answers
	// `thread/revert` with a result after the replacement rollout is
	// written and the SQLite pointer has moved. A body AO cannot read is
	// therefore a wire fault to shout about, never a reason to leave
	// AO's history wider than the provider's — the identity the caller
	// gets back is the one the REQUEST asserted, which is the only
	// thread upstream could have cut.
	reverted, parseErr := parseThreadRevertResponse(resp)
	switch {
	case parseErr != nil:
		log.Printf(
			"codex: %s: thread %s cut before %s but answered with a response AO could not decode (%v); "+
				"treating the cut as APPLIED — the response is only sent after the mutation lands",
			threadRevertMethod, threadID, beforeTurnID, parseErr,
		)
		reverted = RevertedThread{ThreadID: threadID}
	case reverted.ThreadID != threadID:
		log.Printf(
			"codex: %s: thread %s cut before %s but the response names thread %q; "+
				"treating the cut as APPLIED on the requested thread — the echoed id is a wire fault",
			threadRevertMethod, threadID, beforeTurnID, reverted.ThreadID,
		)
		reverted = RevertedThread{ThreadID: threadID}
	}
	reverted.EchoConfirmed = s.awaitRevertEcho(ctx, expectation)
	if reverted.EchoConfirmed {
		forget()
		return reverted, nil
	}
	// The expectation stays armed: the cut DID happen, the echo is
	// merely late, and clearing it here would turn that late arrival
	// into the unsolicited-cut alarm. The next Revert replaces it.
	log.Printf(
		"codex: %s: thread %s answered the cut before %s but sent no thread/reverted echo within %s",
		threadRevertMethod, threadID, beforeTurnID, threadRevertEchoTimeout,
	)
	return reverted, nil
}

// The read-only probe that resolves an ambiguous or refused cut.
//
// `thread/turns/list` is the cheap half of the pair upstream points
// clients at after a revert: with `itemsView: "notLoaded"` it returns
// turn SHELLS (id, status, timestamps) and no item payloads at all
// (rust-v0.149.0 codex-rs/app-server/src/request_processors/thread_processor.rs
// `paginated_thread_turns_list_response`), which is everything the
// anchor-gone question needs. `thread/read { includeTurns: true }`
// answers it too, but it hydrates every item of every turn
// (`paginated_thread_full_turns`) to do so — a whole conversation
// re-serialized to decide one membership test.
//
// Descending order is deliberate: both anchors sit at the END of the
// history, so a cut that did NOT happen is answered by the first page.
// Proving the anchor GONE still costs a full walk, which is what the
// page cap bounds.
const (
	threadTurnsListMethod = "thread/turns/list"
	// threadTurnsProbePageLimit is upstream's own maximum page size
	// (THREAD_TURNS_MAX_LIMIT); asking for more is silently clamped.
	threadTurnsProbePageLimit = 100
	// threadTurnsProbeMaxPages bounds the walk at 10k turns. Past that
	// the probe reports that it cannot answer rather than paging a
	// pathological thread forever — an unverifiable answer is a caller
	// decision, not a loop.
	threadTurnsProbeMaxPages = 100
)

// VerifyRevertBoundary answers the question every unresolved cut leaves
// open: is this thread's DURABLE history already exactly the prefix AO
// asked for?
//
// It is the one mechanism behind both unresolved shapes, because both ask
// the same thing:
//
//   - ErrThreadRevertOutcomeUnknown — the request was written and nothing
//     answered. Upstream mutates before it replies, so only the thread
//     itself can say whether the cut landed.
//   - ErrThreadRevertAnchorUnresolvable — upstream cannot resolve the
//     anchor. Its dominant cause is a cut that ALREADY happened (a revert
//     whose provider half succeeded and whose local half then failed), and
//     that is precisely "the boundary is already here".
//
// applied is true only when the durable history says ALL THREE things
// about the boundary: firstDroppedTurnID is gone, lastKeptTurnID (when AO
// can name one) survives, AND lastKeptTurnID is the NEWEST durable turn
// the thread has.
//
// The first two alone do not prove the provider tail is the prefix AO
// asked for. "Anchor gone" is also true of an anchor that never belonged
// to this thread, and of a history some other writer cut far shorter.
// "Kept turn present" is true of any history that merely CONTAINS it —
// including one that has grown past it. That gap is reachable: a revert
// to B succeeds, AO's local truncation then fails, another client appends
// X, and the retry finds the anchor unresolvable because the cut already
// happened. Without the newest-turn test this reports success, AO
// truncates SQLite to B, and the provider is left holding B+X — the two
// histories silently diverge on a thread AO believes it just converged.
// So a kept turn that is not the tail reports NOT APPLIED, which sends
// the caller to the fork cut: `thread/fork { lastTurnId: B }` is anchored
// on the surviving turn and lands on exactly the prefix that was asked
// for whether or not X exists.
//
// A history that retains NEITHER anchor is reported as an error rather
// than as "not applied": the fork cut the caller would fall back to is
// anchored on the very turn that is missing, so there is no cut left to
// take and saying so beats an obscure refusal one round trip later.
//
// Read-only in every branch. It never mutates, so it is safe to call on a
// thread whose state is unknown.
func (s *Session) VerifyRevertBoundary(ctx context.Context, lastKeptTurnID, firstDroppedTurnID string) (RevertedThread, bool, error) {
	lastKeptTurnID = strings.TrimSpace(lastKeptTurnID)
	firstDroppedTurnID = strings.TrimSpace(firstDroppedTurnID)
	if firstDroppedTurnID == "" {
		return RevertedThread{}, false, fmt.Errorf("codex: %s: beforeTurnId is required to verify a cut", threadRevertMethod)
	}
	threadID := s.rootThreadID()
	if threadID == "" {
		return RevertedThread{}, false, fmt.Errorf("codex: %s: session has no thread id yet", threadRevertMethod)
	}
	turnIDs, newestTurnID, err := s.threadTurnIDs(ctx, threadID, firstDroppedTurnID)
	if err != nil {
		return RevertedThread{}, false, err
	}
	if _, dropped := turnIDs[firstDroppedTurnID]; dropped {
		// The turn AO wanted gone is still there: no cut is in effect.
		return RevertedThread{ThreadID: threadID}, false, nil
	}
	if lastKeptTurnID != "" {
		if _, kept := turnIDs[lastKeptTurnID]; !kept {
			return RevertedThread{}, false, fmt.Errorf(
				"codex: %s: thread %s retains neither the cut anchor %s nor the last kept turn %s — "+
					"provider history is narrower than the boundary AO asked for",
				threadRevertMethod, threadID, firstDroppedTurnID, lastKeptTurnID,
			)
		}
		if newestTurnID != lastKeptTurnID {
			// The anchor is gone and the kept turn survives, but the thread
			// has moved on PAST it: something appended after the cut. The
			// provider tail is therefore not the prefix AO asked for, and
			// converging in place would truncate AO's history to a boundary
			// the provider no longer sits on. Not an error — the fork cut
			// the caller falls back to is anchored on lastKeptTurnID and
			// produces exactly that prefix.
			log.Printf(
				"codex: %s: thread %s is cut at %s but its newest durable turn is %s, not the last kept turn %s; "+
					"reporting the boundary as NOT converged so the caller forks at the kept turn instead",
				threadRevertMethod, threadID, firstDroppedTurnID, newestTurnID, lastKeptTurnID,
			)
			return RevertedThread{ThreadID: threadID}, false, nil
		}
	}
	return RevertedThread{ThreadID: threadID}, true, nil
}

// threadTurnIDs collects the ids of a paginated thread's durable turns,
// newest first, stopping as soon as stopWhenFound appears.
//
// The early exit is what keeps the common "the cut did not happen" answer
// to a single page. Absence has no early exit by construction, so the
// page cap is the bound.
//
// newest is the FIRST id the descending walk sees, which is the thread's
// newest durable turn. It is returned alongside the set because membership
// alone cannot tell a boundary that IS the tail from one the thread has
// since grown past — see VerifyRevertBoundary. Empty when the thread has
// no durable turns at all.
func (s *Session) threadTurnIDs(ctx context.Context, threadID, stopWhenFound string) (map[string]struct{}, string, error) {
	turnIDs := make(map[string]struct{})
	newest := ""
	cursor := ""
	for page := 0; page < threadTurnsProbeMaxPages; page++ {
		params := map[string]any{
			"threadId":      threadID,
			"limit":         threadTurnsProbePageLimit,
			"sortDirection": "desc",
			// Turn shells only: this probe reads ids, never content.
			"itemsView": "notLoaded",
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := s.sendRequest(ctx, threadTurnsListMethod, params)
		if err != nil {
			return nil, "", fmt.Errorf("codex: %s: %w", threadTurnsListMethod, err)
		}
		var decoded struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp, &decoded); err != nil {
			return nil, "", fmt.Errorf("codex: %s: decode response: %w", threadTurnsListMethod, err)
		}
		for _, turn := range decoded.Data {
			if turn.ID == "" {
				continue
			}
			if newest == "" {
				newest = turn.ID
			}
			turnIDs[turn.ID] = struct{}{}
			if turn.ID == stopWhenFound {
				return turnIDs, newest, nil
			}
		}
		next := strings.TrimSpace(decoded.NextCursor)
		// A repeated cursor is upstream's own pathological case
		// (it raises an internal error on one); stop rather than spin.
		if next == "" || next == cursor {
			return turnIDs, newest, nil
		}
		cursor = next
	}
	return nil, "", fmt.Errorf(
		"codex: %s: thread %s has more than %d turns; cannot verify the cut boundary",
		threadTurnsListMethod, threadID, threadTurnsProbeMaxPages*threadTurnsProbePageLimit,
	)
}

// awaitRevertEcho waits for the `thread/reverted` notification that
// upstream sends right after the response. Bounded and non-fatal: the
// caller has already been told the cut succeeded.
func (s *Session) awaitRevertEcho(ctx context.Context, expectation *revertExpectation) bool {
	timer := time.NewTimer(threadRevertEchoTimeout)
	defer timer.Stop()
	select {
	case <-expectation.echo:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// dispatchThreadReverted consumes `thread/reverted`, the authoritative
// signal that a thread's durable history was cut.
//
// It carries a threadId and NOTHING else — no boundary — so it can only
// be reconciled against a cut this session asked for. That split is the
// whole handler:
//
//   - Solicited (the pending expectation matches): release Revert's wait.
//     The app layer truncates its own history cache in the same call
//     stack, under the thread lock it already holds, so there is nothing
//     to do from the read-loop goroutine. An expectation whose wait
//     already timed out stays armed for exactly this reason: a LATE echo
//     of AO's own cut must not raise the alarm below.
//   - Unsolicited: log loudly. AO holds the thread's writer lock while a
//     session is live, so a second writer cutting it is not supposed to
//     be reachable; if it ever is, AO's cache is now WIDER than provider
//     history and the notification cannot say by how much. Guessing a
//     boundary would be worse than saying so — the log is the drift
//     alarm, exactly like warnUnclaimedNotification.
//
// Registered in sessionSideChannelNotifications (session_notifications.go),
// which is also what keeps it out of the initialize opt-out list.
func (s *Session) dispatchThreadReverted(params json.RawMessage) {
	threadID := providerThreadIDFromParams(params)
	if threadID == "" {
		log.Printf("codex: thread/reverted carried no threadId; ignoring")
		return
	}
	if pending := s.pendingRevert.Load(); pending != nil && pending.threadID == threadID {
		// `closed` needs no lock: every notification is dispatched
		// synchronously on the read-loop goroutine (jsonrpc.go readLoop ->
		// dispatchLine -> dispatchNotification), so this is the only
		// writer, and a duplicate echo must not panic on a second close.
		if !pending.closed {
			pending.closed = true
			close(pending.echo)
		}
		return
	}
	if root := s.rootThreadID(); threadID != root {
		log.Printf(
			"codex: thread/reverted for thread %s, which this session (%s) does not own; ignoring",
			threadID, root,
		)
		return
	}
	log.Printf(
		"codex: thread/reverted for thread %s was not requested by Agent Overflow — "+
			"provider history was cut by another writer at an unknown boundary and AO's "+
			"cached history for this thread may now be wider than the provider's",
		threadID,
	)
}

// classifyThreadRevertError maps upstream's pre-mutation refusals onto
// ErrThreadRevertUnsupported so the caller can fall back to the fork cut,
// and leaves everything else alone (including the writer conflict, which
// classifyThreadWriterConflict names for the user).
//
// Only refusals raised BEFORE the handler mutates anything may be mapped:
// `thread/revert` shuts the thread runtime down partway through, so an
// error from a later stage leaves a thread whose state a fork could not
// safely be built on.
func classifyThreadRevertError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		switch {
		// The method itself is unknown: a codex below 0.148, or one whose
		// experimental surface is off. Both are "use the other cut".
		case rpcErr.Code == -32601:
			return fmt.Errorf("%w: %s", ErrThreadRevertUnsupported, rpcErr.Message)
		// invalid_request from the paginated-history gate, which upstream
		// checks first thing in thread_revert_response.
		case rpcErr.Code == -32600 && strings.Contains(rpcErr.Message, "only supports paginated threads"):
			return fmt.Errorf("%w: %s", ErrThreadRevertUnsupported, rpcErr.Message)
		// invalid_request from the anchor resolution, also pre-mutation.
		case rpcErr.Code == -32600 && matchesThreadRevertAnchorRefusal(rpcErr.Message):
			return fmt.Errorf("%w: %s", ErrThreadRevertAnchorUnresolvable, rpcErr.Message)
		}
	}
	return classifyThreadWriterConflict(err)
}

func matchesThreadRevertAnchorRefusal(message string) bool {
	for _, marker := range threadRevertAnchorMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// parseThreadRevertResponse decodes upstream's ThreadRevertResponse.
//
// `turnsBackwardsCursor` / `itemsBackwardsCursor` are deliberately NOT
// decoded. They are upstream's opaque descending-pagination cursors for
// re-hydrating the retained history (`thread/turns/list` /
// `thread/items/list` with `sortDirection: "desc"`). The revert path keeps
// its own history cache and never uses these cursors, so carrying them
// produced two exported fields whose only readers were the tests that
// asserted they had been carried. The thread-identity echo below is the
// whole validation this response supports (`turns` is always empty; see
// the type doc), and if a reconciler that VERIFIES the cut is ever built,
// re-adding the decode is two lines.
func parseThreadRevertResponse(data json.RawMessage) (RevertedThread, error) {
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return RevertedThread{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Thread.ID == "" {
		return RevertedThread{}, errors.New("response missing thread.id")
	}
	return RevertedThread{ThreadID: response.Thread.ID}, nil
}

// threadStartHistoryModeMinimumCodexVersion is the floor at which AO asks
// for a paginated thread on `thread/start`.
//
// `thread/start.historyMode` itself is older than this — the field shipped
// with paginated history in 0.143, and the codex TUI has requested
// `paginated` for every non-ephemeral thread since (rust-v0.149.0,
// codex-rs/tui/src/app_server_session.rs:1689; `codex exec` does the same
// at codex-rs/exec/src/lib.rs:1187). AO nonetheless gates the opt-in at
// the `thread/revert` floor: paginated history is only worth its
// behavioral differences here because it is what makes the in-place cut
// available, and a paginated thread born on a server with no
// `thread/revert` would carry the differences with none of the benefit
// until the user upgraded.
const threadStartHistoryModeMinimumCodexVersion = threadRevertMinimumCodexVersion

// threadStartHistoryMode returns the `historyMode` to request on
// `thread/start`, or "" to say nothing and take the server's default.
//
// Upstream's default is Legacy — `ThreadHistoryMode::default()` is
// `Legacy` (rust-v0.149.0
// codex-rs/app-server-protocol/src/protocol/v2/thread_data.rs:73) and the
// local thread store does not override `default_history_mode`
// (codex-rs/thread-store/src/store.rs:76). Omitting the field is
// therefore not neutral: it is an explicit vote for legacy history, and
// it is why AO's own threads have never been revertible.
//
// The field is only valid on `thread/start`. `ThreadResumeParams` has no
// history-mode member at all (same file, :332) — a resumed thread keeps
// whatever contract its rollout recorded, so this must never be merged
// into the shared thread params bag.
func threadStartHistoryMode(codexVersion string) string {
	if !provider.CodexCLIVersionAtLeast(codexVersion, threadStartHistoryModeMinimumCodexVersion) {
		return ""
	}
	return paginatedThreadHistoryMode
}

// isHistoryPaginationUnsupported reports whether a `thread/start` failure
// was the server refusing paginated history, as opposed to a real start
// failure.
//
// The refusal AO can actually provoke is store-shaped rather than
// version-shaped: `thread/start` rejects `historyMode: "paginated"` with
// "paginated threads require thread/turns/list and thread/items/list
// support" whenever the thread store has no SQLite state database
// (rust-v0.149.0
// codex-rs/app-server/src/request_processors/thread_processor.rs:1093 and
// codex-rs/thread-store/src/local/mod.rs:586). It is raised while
// destructuring the params, before any thread is created, so retrying
// without the field is safe rather than a second half-start.
//
// The predicate mirrors upstream's own client-side downgrade
// (`is_history_pagination_unsupported`, codex-rs/tui/src/app_server_session.rs:175)
// so AO downgrades on exactly the errors the codex TUI downgrades on.
func isHistoryPaginationUnsupported(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	// -32601 MethodNotFound, -32600 InvalidRequest, -32602 InvalidParams —
	// the same three upstream's own downgrade predicate accepts.
	if rpcErr.Code == -32601 {
		return true
	}
	if rpcErr.Code != -32600 && rpcErr.Code != -32602 {
		return false
	}
	message := strings.ToLower(rpcErr.Message)
	for _, field := range []string{
		"historymode", "history mode",
		"excludeturns", "exclude turns",
		"thread/turns/list", "thread/items/list",
	} {
		if strings.Contains(message, field) {
			return true
		}
	}
	if !strings.Contains(message, "paginated") {
		return false
	}
	for _, shape := range []string{"unknown variant", "unsupported variant", "invalid enum"} {
		if strings.Contains(message, shape) {
			return true
		}
	}
	return false
}
