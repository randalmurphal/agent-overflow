package codex

import (
	"log"

	"agent-overflow/internal/provider"
)

// ExternalTurnOriginQueue is the typed origin marker stamped on every event
// belonging to a turn this session did NOT start.
//
// It rides in `Meta.origin` on the `EventTurnStart` and on the `EventUserText`
// the injected turn echoes, so the app layer can persist and the frontend can
// later render an injected prompt as something other than "the user typed
// this here". It is deliberately a value, not a boolean: the queue is only the
// first external producer, and a second one (a future remote-control surface,
// say) must be distinguishable rather than folded into "not ours".
//
// Naming matches upstream's own vocabulary for the surface — the queue
// extension (codex-rs/ext/queue) is what injects these.
const ExternalTurnOriginQueue = "external-queue"

// turnOrigin records who started a turn this session is watching.
type turnOrigin uint8

const (
	turnOriginLocal    turnOrigin = iota // AO sent the turn/start RPC
	turnOriginExternal                   // it arrived without one
)

// Externally queued turns — why any of this exists.
//
// Every app-server backed by a LOCAL thread store installs the queued-item
// extension unconditionally (codex-rs/app-server/src/extensions.rs
// `install(&mut builder, queue_service)`); AO's does too, and there is no
// initialize capability that opts out of it. That extension runs
// `QueuedItemService::watch_external_messages`
// (codex-rs/ext/queue/src/service.rs), a 10-second poll of SQLite's cheap
// `data_version` on `state_5.sqlite`. When the version moves it asks the
// durable revision index which of the LOADED threads changed, emits
// `thread/queue/changed` for each, and spawns a dispatch task that calls
// `thread.start_turn_if_idle` for any thread that is idle and has pending
// external work — retrying every 10s while the thread stays busy.
//
// The producer is `codex queue --thread <uuid> --message <text>`
// (codex-rs/cli/src/queue_cmd.rs), which only writes the SQLite row
// (codex-rs/state/queue_migrations/0001_queued_items.sql) and exits. It does
// not need a running app-server, does not take the thread writer lock, and
// therefore works while AO holds the thread.
//
// What AO sees, in order: `thread/queue/changed`, then — up to ~10s later —
// `turn/started` and a full `item/*` stream including an `item/completed`
// `userMessage` AO never sent, with no `turn/start` RPC of its own.
//
// Both halves of that sequence were traced in codex-source at rust-v0.149.0,
// not observed against a live binary; the installed CLI here is 0.147.0.
//
// The adoption rules below are what make the rest of the session correct for
// such a turn: `activeTurnID` has to be set or Interrupt and Steer address
// nothing, and the echoed user message has to be marked or it persists as if
// the person sitting in front of Agent Overflow typed it.

// beginLocalTurnStart records that AO is about to write a `turn/start`.
//
// It is a COUNTER rather than a turn id because AO does not know the turn id
// until the response lands, and `turn/started` can beat that response onto the
// read loop. Without the counter, that race would classify AO's own turn as
// external.
func (s *Session) beginLocalTurnStart() {
	s.mu.Lock()
	s.pendingLocalTurnStarts++
	s.mu.Unlock()
}

// abandonLocalTurnStart releases a claim whose `turn/start` failed outright.
//
// NOT called on a request TIMEOUT: an unacknowledged turn/start may still have
// created a turn (IsAmbiguousTurnStartTimeout is the existing name for that
// ambiguity), and releasing the claim would make its `turn/started` look
// externally injected.
func (s *Session) abandonLocalTurnStart() {
	s.mu.Lock()
	if s.pendingLocalTurnStarts > 0 {
		s.pendingLocalTurnStarts--
	}
	s.mu.Unlock()
}

// noteAmbiguousLocalTurnStart marks the claim left outstanding by a
// `turn/start` that timed out AFTER its write.
//
// The claim itself is deliberate (abandonLocalTurnStart's doc): the turn may
// exist, and releasing it would make its `turn/started` look injected. What
// this adds is the knowledge that the claim might describe NOTHING — a write
// that never became a turn — so a later proof can retire it instead of
// leaving it to be consumed by the next unrelated `turn/started`.
//
// Guarded on there actually being a pending claim: when the timed-out turn's
// `turn/started` already arrived, adoptTurnStart consumed the claim and there
// is no ambiguity left to record.
func (s *Session) noteAmbiguousLocalTurnStart() {
	s.mu.Lock()
	if s.pendingLocalTurnStarts > 0 {
		s.ambiguousLocalTurnStarts++
	}
	s.mu.Unlock()
}

// dropAmbiguousLocalTurnStartsLocked releases the claims held only for a
// timed-out `turn/start`, once the caller has established that no turn is
// still owed to them.
//
// Two callers, both holding proofs rather than heuristics:
//
//   - a later `turn/start` RESPONSE naming a turn that is ALREADY classified —
//     the racing `turn/started` consumed a claim for it, so the claim this
//     response would have bound is surplus;
//   - a turn COMPLETION — upstream runs one turn at a time per thread, so a
//     turn created by the timed-out request must have started (and consumed
//     its claim) before any other turn could finish.
//
// Bounded by the pending count: when the ambiguous claim was consumed by the
// very turn it was held for, there is nothing to release and only the
// ambiguity marker clears.
//
// Residual, stated because it is the reason this is a reconciliation and not
// a fix: a genuinely external turn that arrives BEFORE either proof still
// consumes the surplus claim and reads as local. That is the same fail-safe
// direction the whole classifier takes — a wrong "local" costs a marker, a
// wrong "external" mislabels the user's own message.
func (s *Session) dropAmbiguousLocalTurnStartsLocked(because string) {
	if s.ambiguousLocalTurnStarts == 0 {
		return
	}
	dropped := s.ambiguousLocalTurnStarts
	if dropped > s.pendingLocalTurnStarts {
		dropped = s.pendingLocalTurnStarts
	}
	s.pendingLocalTurnStarts -= dropped
	s.ambiguousLocalTurnStarts = 0
	if dropped > 0 {
		log.Printf("codex: thread %s: released %d turn/start claim(s) left by a timed-out request (%s)",
			s.threadID, dropped, because)
	}
}

// bindLocalTurnStart attaches a claim to the turn id the `turn/start` response
// named. A no-op when the racing `turn/started` already consumed the claim.
func (s *Session) bindLocalTurnStart(turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turnID == "" {
		// Response carried no turn id (older/odd app-server). Nothing to bind
		// the claim to, so release it rather than letting it absorb an
		// unrelated later turn.
		if s.pendingLocalTurnStarts > 0 {
			s.pendingLocalTurnStarts--
		}
		return
	}
	if _, seen := s.turnOrigins[turnID]; seen {
		// The racing `turn/started` already classified this turn and took a
		// claim for it, so anything still outstanding was not for this turn.
		// If a timed-out request is what left it there, this response is the
		// proof that its turn never happened.
		s.dropAmbiguousLocalTurnStartsLocked("a later turn/start named an already-classified turn")
		return
	}
	if s.pendingLocalTurnStarts > 0 {
		s.pendingLocalTurnStarts--
	}
	s.rememberTurnOriginLocked(turnID, turnOriginLocal)
}

// turnAdoption is what a `turn/started` observation resolved to.
type turnAdoption uint8

const (
	// turnAdoptionLocal — this session asked for the turn (or is treating it
	// as ours, which is the fail-safe direction).
	turnAdoptionLocal turnAdoption = iota
	// turnAdoptionExternal — nothing in this session can account for it.
	turnAdoptionExternal
	// turnAdoptionUndecided — the only candidates are self-queued claims, and
	// which claim (if any) this turn belongs to is not knowable yet. The
	// dispatched turn's `userMessage` echoes the `clientId` that decides it;
	// resolveUserEchoOrigin is the authority, and nothing is recorded here.
	turnAdoptionUndecided
)

// adoptTurnStart classifies an observed `turn/started`.
//
// Fail-safe direction: a turn is external ONLY when there is no outstanding
// local claim, no outstanding self-queued claim, and no record of one.
// Misreading an AO turn as external would mislabel the user's own message;
// misreading an external turn as local costs only the marker, and the turn
// still gets adopted as active either way.
//
// Self-queued claims DEFER rather than decide. A turn AO put in the
// PROVIDER's queue starts without a `turn/start` of ours — the app-server's
// idle hook dispatches it (thread_queue.go) — but so does a row a foreign
// producer (`codex queue --thread …`) wrote, and the provider drains one FIFO
// containing both. Popping the oldest claim at `turn/started` would hand AO's
// claim to whichever row happened to be at the head, so a foreign message
// ahead of AO's in the queue would render as the user's own. The echo carries
// `clientId` (ThreadItem::UserMessage, rust-v0.149.0
// codex-rs/app-server-protocol/src/protocol/v2/item.rs:236) and that is what
// decides it.
func (s *Session) adoptTurnStart(turnID string) turnAdoption {
	if turnID == "" {
		return turnAdoptionLocal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if origin, seen := s.turnOrigins[turnID]; seen {
		if origin == turnOriginExternal {
			return turnAdoptionExternal
		}
		return turnAdoptionLocal
	}
	if s.pendingLocalTurnStarts > 0 {
		s.pendingLocalTurnStarts--
		s.rememberTurnOriginLocked(turnID, turnOriginLocal)
		return turnAdoptionLocal
	}
	if s.hasSelfQueuedClaimsLocked() {
		return turnAdoptionUndecided
	}
	if !s.rememberTurnOriginLocked(turnID, turnOriginExternal) {
		// At the origin-map cap nothing was recorded, so turnIsExternal will
		// later answer "local" for this turn. Reporting external here would
		// stamp the turn start and leave the user echo unstamped — a marker
		// disagreeing with itself is worse than the fail-safe direction.
		return turnAdoptionLocal
	}
	return turnAdoptionExternal
}

// resolveUserEchoOrigin decides who authored the user message an injected or
// queue-dispatched turn just echoed, and reports true for "not this app".
//
// clientID is the echo's `clientId`. For a turn AO queued it is the optimistic
// row id AO passed to `thread/queue/add`; for a `codex queue` row it is a v7
// UUID that CLI mints itself
// (rust-v0.149.0 codex-rs/tui/src/session_queue_commands.rs:48), so the two
// can never collide.
//
// This is where an undecided `turn/started` becomes a verdict, and the verdict
// is recorded so the rest of the turn reads consistently.
//
// `decided` reports whether THIS call is what classified the turn, and exists
// only so the one-line-per-turn adoption log stays one line per turn: a turn
// already classified external at its `turn/started` was logged there.
func (s *Session) resolveUserEchoOrigin(turnID, clientID string) (external, decided bool) {
	if turnID == "" {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if origin, seen := s.turnOrigins[turnID]; seen {
		return origin == turnOriginExternal, false
	}
	if !s.hasSelfQueuedClaimsLocked() {
		// No queue participation to reason about. An unrecorded turn reads
		// local, exactly like turnIsExternal.
		return false, false
	}
	if s.takeSelfQueuedClaimForClientLocked(clientID) {
		s.rememberTurnOriginLocked(turnID, turnOriginLocal)
		return false, true
	}
	// AO has rows in the provider queue but this dispatch is none of them:
	// somebody else's row was ahead in the FIFO. The claims stay untouched —
	// AO's own messages are still waiting.
	s.rememberTurnOriginLocked(turnID, turnOriginExternal)
	return true, true
}

// turnIsExternal reports a previously classified turn. Unknown turn ids read
// as local — an item whose turn start was never observed is not evidence of
// injection.
func (s *Session) turnIsExternal(turnID string) bool {
	if turnID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnOrigins[turnID] == turnOriginExternal
}

func (s *Session) forgetTurnOrigin(turnID string) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	delete(s.turnOrigins, turnID)
	s.mu.Unlock()
}

// maxTrackedTurnOrigins bounds the origin map. Entries are dropped on
// `turn/completed`, so a healthy session holds at most one; the cap only
// matters for turns whose completion never arrives (provider crash mid-turn,
// a child stream that outlives its parent). At the cap the map stops growing
// and later turns read as local, which is the safe direction.
const maxTrackedTurnOrigins = 64

// rememberTurnOriginLocked records a classification and reports whether it
// actually landed. At the cap it does not, and the caller must fall back to
// the safe direction — an unrecorded turn reads LOCAL from turnIsExternal, so
// a caller that assumed the write succeeded would stamp the turn start
// external and leave every later event of the same turn unstamped.
func (s *Session) rememberTurnOriginLocked(turnID string, origin turnOrigin) bool {
	if s.turnOrigins == nil {
		s.turnOrigins = make(map[string]turnOrigin, 2)
	}
	if _, seen := s.turnOrigins[turnID]; !seen && len(s.turnOrigins) >= maxTrackedTurnOrigins {
		return false
	}
	s.turnOrigins[turnID] = origin
	return true
}

// stampExternalOrigin marks an event as belonging to an externally injected
// turn. Applied to the turn start itself and to the user-message echo, which
// are the two rows a reader would otherwise attribute to this app's user.
func stampExternalOrigin(evt *provider.ProviderEvent) {
	evt.Meta = mergeMetaKeys(evt.Meta, map[string]any{"origin": ExternalTurnOriginQueue})
}

// logExternalTurnAdopted is the one info-level line for an injected turn. It
// is per turn, not per event, and carries no message content.
func logExternalTurnAdopted(threadID, turnID string) {
	log.Printf(
		"codex: adopted externally queued turn %s on thread %s (no turn/start of ours); "+
			"origin=%s",
		turnID, threadID, ExternalTurnOriginQueue,
	)
}
