package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// handleUserText routes the wire-confirmation envelope for a user message.
// Four branches (pending-send matching is by IDENTITY when the entry was
// registered with an expected wire id — Claude-family sends mint the uuid
// the CLI echoes back — and FIFO otherwise; see consumeMatchingPendingSend):
//
//  1. AO-initiated direct send: a matching pending-send entry exists
//     for an already-persisted `user:<turnIndex>` row. Stamp
//     `provider_item_id` onto that row, merging the new key into the
//     existing meta so attachments / source-plan refs survive.
//
//  2. AO-initiated queued send: a matching pending-send entry carries
//     a deferred row. Persist the `user:<turnIndex>:flush:<n>` row only
//     after the provider echo supplies a stable `provider_item_id`, so chat
//     history does not get ahead of provider context.
//
//  3. Wire-only, PARENTED: a subagent's task prompt echoed as user-role
//     content. No pending-send match (it isn't an AO send). Persist a
//     nested `user:wire:<provider_item_id>` user_text row under its
//     launching tool_call — genuine conversation content.
//
//  4. Wire-only, TOP-LEVEL: no pending-send match and no parent tool.
//     The pending-send registry is the ground truth for "did WE send
//     this", so an unmatched top-level echo is provider-injected context
//     (an unrecognized CLI wrapper, a cascade injection), NOT
//     user-authored. Persist it as a non-user `notification` row
//     (`injected:wire:<provider_item_id>`), never a user bubble. This
//     closes the class where an unrecognized wrapper (the 2.1.x
//     `<agent-message>` subagent report) surfaced as a top-level user
//     message — incident 2026-07.
//
// Both wire-only branches dedup via wireOnlyUserTextSeen so a
// session-resume replay doesn't double-write. An empty
// `provider_item_id` is a non-stable envelope — skip rather than mint a
// non-stable id we can't dedup on.
func (r *Router) handleUserText(evt provider.ProviderEvent) error {
	if evt.ThreadID == "" {
		return nil
	}
	// ONE decode for the whole classification chain below. Five separate
	// readers each unmarshalling the same envelope is four wasted passes per
	// top-level echo, on the provider read loop.
	meta := decodeUserTextMeta(evt.Meta)
	providerItemID := meta.text("provider_item_id")

	// The §E6 resume prompt: the parser's own row for the message that
	// opened a resumed agent round. It carries no provider uuid (the
	// rebind `system/task_started` has none to give) and is not an AO
	// send, so every branch below would refuse it — the pending-send FIFO
	// on provenance, and the wire-only branches on the missing id. Its
	// identity is the carrier scope and its placement is the transcript
	// root; see persistResumePromptRow.
	if meta.flag(provider.MetaSubagentResumePromptKey) {
		return r.persistResumePromptRow(evt, meta)
	}

	// The FIFO is only consulted for an echo that could BE one of this app's
	// sends. Two provenance flags say positively that it is not — the Codex
	// session's `external-queue` origin stamp (another producer's row
	// dispatched off the provider's own queue) and Claude's cross-session
	// peer envelope — and both would otherwise pop a real pending entry
	// whenever that entry has no expected wire id to defend itself with.
	// The cost of the mispop is two wrong rows, not one: the foreign message
	// takes the user's optimistic row, and the user's own echo then arrives
	// with nothing to match and persists as "Injected provider context".
	//
	// Checked BEFORE the pop rather than inside it because provenance is a
	// property of the ECHO; the registry has no way to see it.
	_, isPeerEcho := meta.crossSessionPeer()
	foreignEcho := isPeerEcho || meta.text("origin") == externalQueueOrigin

	if eventParentID(evt) == "" && !foreignEcho {
		// Pop and handle under the thread's flush anchor lock so the popped snapshot's
		// AnchoredAtInterrupt / WasDeferred state is truthful: the
		// interrupt paths (PromoteQuietFlushSends,
		// EagerPersistDeferredFlushSends) hold the same mutex across their
		// claim + store write, so a claim this echo observes is a claim
		// whose bump/persist already committed — and a failed write was
		// already unclaimed. The confirmed hook (message-anchor record)
		// runs inside the lock too; it is already synchronous on this
		// read loop, and the only new waiters are the rare interrupt
		// paths, which need the ordering more than the latency.
		handled, err := func() (bool, error) {
			anchor := r.flushAnchor(evt.ThreadID)
			anchor.Lock()
			defer anchor.Unlock()
			pending, ok := r.consumeMatchingPendingSendForEcho(
				evt.ThreadID, providerItemID, meta.text(userEchoClientIDMetaKey))
			if !ok {
				return false, nil
			}
			// Stash the echo's wire identity on the popped entry BEFORE
			// the fallible handlers: if a write fails, the reinserted
			// EchoConsumed entry must still carry the transcript uuid and
			// parent so the session-death self-heal can stamp them — the
			// echo won't necessarily be re-delivered (round-6, R6-1).
			pending.stashEchoIdentity(providerItemID, meta.text("parent_uuid"))
			var handleErr error
			if pending.DeferredItem != nil {
				handleErr = r.persistDeferredUserText(&pending, providerItemID, evt, meta)
			} else {
				handleErr = r.attachProviderItemIDToUserRow(evt.ThreadID, &pending, providerItemID, evt, meta)
			}
			if handleErr != nil && !r.isWireOnlyUserTextSeen(evt.ThreadID, providerItemID) {
				// This echo IS the consumption boundary even though the
				// write failed: record the message anchor now so fork /
				// revert-on-interrupt can slice at this message without
				// waiting for a retry or session-death self-heal. The
				// anchor row doesn't depend on the failed stamp, only on
				// the item row existing (FK) — so record for found rows
				// (quiet persists committed at dispatch); deferred rows
				// that never persisted stay honestly anchor-less until
				// their write lands (round-10, R10-2).
				r.recordEchoBoundaryAnchor(evt.ThreadID, &pending)
				// Both paths mark the dedup set exactly at their durable
				// point, so unseen means nothing committed for this
				// envelope: reinsert the popped entry so a re-delivered
				// echo retries instead of persisting an injected-context
				// duplicate. The reinsert marks the entry EchoConsumed —
				// a session-death drain self-heals the timeline row from
				// DeferredItem / QuietItem rather than restoring the
				// message to the draft, because this echo proved the
				// provider context already contains it (round-5, R5-3).
				r.reinsertPendingSendHead(evt.ThreadID, pending)
			}
			return true, handleErr
		}()
		if handled || err != nil {
			return err
		}
		if r.HasPendingSendForThread(evt.ThreadID) {
			// Sends are outstanding but this echo matched none of their
			// expected ids — either an injected envelope arriving during
			// the queue-wait window (routed to the injected-context row
			// below, working as intended) or, if it recurs for every
			// send, Claude no longer honoring the supplied uuid
			// (binary-contract drift; re-spike per
			// docs/references/spike-policy.md).
			log.Printf("triage: top-level wire user echo %s on %s matched no pending send while sends await confirmation — treating as provider-injected content", providerItemID, evt.ThreadID)
		}
	}

	if providerItemID == "" {
		// No id to dedup on — skip rather than mint a non-stable id.
		// A wire-only envelope without a provider_item_id is a
		// non-cascading replay we can't correlate.
		return nil
	}
	if !r.markWireOnlyUserTextSeen(evt.ThreadID, providerItemID) {
		return nil // duplicate — already persisted on prior arrival
	}
	if eventParentID(evt) != "" {
		// Parented: a subagent's task prompt echoed as user-role content,
		// nested under its launching tool_call. Genuine conversation
		// content — persist as the nested user_text row.
		return r.persistWireOnlySubagentPrompt(evt, providerItemID)
	}
	if peer, isPeer := meta.crossSessionPeer(); isPeer {
		// A message another Claude session on this machine sent to this
		// one. It reaches the top-level wire-only branch for a structural
		// reason — the CLI minted the uuid, so no pending-send entry can
		// ever match it — but unlike the rest of this branch its
		// provenance is POSITIVELY known: the parser only sets these flags
		// on a structured `origin.kind == "peer"` envelope or a balanced
		// `<cross-session-message>` wrapper. Real conversation content with
		// a named author, so it persists as a message rather than as
		// "Injected provider context".
		return r.persistPeerSessionMessage(evt, providerItemID, peer)
	}
	if meta.text("origin") == externalQueueOrigin {
		// The user echo of a turn another Codex process queued onto this
		// thread (`codex queue --thread`). AO never sent it, so no
		// pending-send entry matches, but the session stamped its origin
		// from the turn/started attribution — a named author, so it is a
		// user row, not injected context.
		return r.persistExternalOriginMessage(evt, providerItemID, "queue", externalQueueOrigin, nil)
	}
	if meta.flag("command_echo") {
		// A command-INPUT metadata echo (`<command-name>` triple) whose send
		// this process didn't register — a session-resume replay, or a
		// command issued by another client on the same session. The XML is
		// CLI bookkeeping, not conversation content; matched echoes stamp
		// the optimistic user row above, and an unmatched one has no row to
		// stamp and nothing worth showing.
		log.Printf("triage: dropping unmatched command-input echo %s on %s", providerItemID, evt.ThreadID)
		return nil
	}
	// Top-level echo that matched no pending send: provider-injected
	// context, NOT user-authored. Persist as a non-user notification so it
	// can never masquerade as a user message.
	return r.persistInjectedContextNotification(evt, providerItemID)
}

func (r *Router) persistDeferredUserText(pending *pendingSend, providerItemID string, evt provider.ProviderEvent, meta userTextMeta) error {
	if pending.DeferredItem == nil {
		return nil
	}
	if r.deferredPersistGate != nil {
		r.deferredPersistGate()
	}
	item := *pending.DeferredItem
	if providerItemID == "" {
		log.Printf("triage: handleUserText popped deferred pending entry %s/%s but wire echo carried no provider_item_id — leaving Zone 2 unconfirmed; check parser coverage for the wire shape", item.ThreadID, item.ID)
		return nil
	}
	now := eventTimestampMillis(evt)
	item.CreatedAt = now
	item.UpdatedAt = now

	// The parent uuid is merged into the SAME persist as the item id:
	// the anchor's copy below is a separate follow-up write that can
	// fail after this commit, and the already-cut revert retry needs a
	// durable parent it can slice through (round-5, R5-8).
	//
	// Placement is decided by the turn's occupancy at the FIRST echo,
	// stashed across failures (EchoTurnWasEmpty): an occupied turn means
	// pre-dispatch content correctly precedes the prompt (steer shape),
	// so the standard MAX+1 append applies — capturing a tail index at
	// dispatch time was the original ordering bug (streaming rows
	// occupied the captured slot, TestHandleUserText_DeferredFlush_*).
	// An EMPTY turn is the prompt's own (this echo opens it): everything
	// that lands in it later is the prompt's response, so the persist —
	// and every retry / self-heal after a failed first attempt, by which
	// time the response occupies 0..n — goes to the turn HEAD, keeping
	// the prompt above its own response (round-7, R7-4). ThreadID /
	// TurnIndex / Kind / Role / Status are guaranteed populated by the
	// dispatcher's row construction in app_flush_queue.go.
	parentUUID := meta.text("parent_uuid")
	var persisted store.Item
	persistErr := func() error {
		if !pending.EchoConsumed {
			_, hasRows, sampleErr := r.store.MaxItemIndexForTurn(item.ThreadID, item.TurnIndex)
			if sampleErr != nil {
				// A failed sample must still record occupancy: aborting
				// here would reinsert the entry as EchoConsumed with the
				// zero value, and the retry — by which time the response
				// occupies the turn — would append the prompt below its
				// own response (round-14, D14-1). The turn-open state is
				// first-echo information the router already holds and is
				// exact except for an open-but-still-rowless turn, where
				// it prefers the steer-shape append.
				r.mu.Lock()
				open, hasOpen := 0, false
				if st := r.threadStateIfPresent(item.ThreadID); st != nil {
					open, hasOpen = st.openTurn, st.openTurnSet
				}
				r.mu.Unlock()
				pending.recordFirstEchoTurnOccupancy(!hasOpen || open != item.TurnIndex)
				log.Printf("triage: sample deferred turn occupancy for %s/%s: %v — falling back to turn-open state (empty=%v)",
					item.ThreadID, item.ID, sampleErr, pending.EchoTurnWasEmpty)
			} else {
				pending.recordFirstEchoTurnOccupancy(!hasRows)
			}
		}
		mergedMeta, err := usermessage.MergeProviderIDs(item.Meta, providerItemID, parentUUID)
		if err != nil {
			return fmt.Errorf("triage: merge provider ids into deferred %s/%s meta: %w", item.ThreadID, item.ID, err)
		}
		item.Meta = mergedMeta
		if pending.EchoTurnWasEmpty {
			persisted, err = r.persistUserPromptAtTurnHead(item)
		} else {
			persisted, err = r.persistItemWithEmit(item, nil, nil, true)
		}
		if err != nil {
			return fmt.Errorf("triage: persist deferred user_text %s/%s: %w", item.ThreadID, item.ID, err)
		}
		return nil
	}()
	// The echo proves the provider consumed the queued message; if no
	// init opened its logical turn (Claude's mid-loop consumption emits
	// none), open it now — turns row, open-turn state, round re-mint,
	// `provider:turn_started` — on BOTH outcomes: the provider advanced
	// regardless of our write, and the response that follows on this
	// serial read loop must attribute to this turn. Left unopened on
	// failure, those rows would land under the still-open predecessor
	// while the replay retry / session-death self-heal later put the
	// user row in item.TurnIndex, permanently separating the prompt from
	// its response (round-6, R6-3). On success the open runs AFTER the
	// persist so the user-row upsert precedes `provider:turn_started`
	// (the normal direct-send emission order); the active-turn registry
	// still learns the new index before any response content because the
	// read loop is serial (moving-RESPONSE-pill bug).
	// pending.InterruptedTurnIndex feeds the predecessor settle: the
	// interrupt paths stamp it pre-ack
	// (MarkFlushSendsInterrupted), so an echo that beats the
	// interrupt ack still settles the cut turn "interrupted" rather
	// than "end_turn" (round-6, R6-4); it is -1 outside interrupts.
	r.openQueuedEchoTurn(item.ThreadID, item.TurnIndex, now, pending.InterruptedTurnIndex)
	if persistErr != nil {
		return persistErr
	}
	// Replay guard, recorded the moment the row is durable and BEFORE the
	// fallible follow-ups below: the pending entry is already popped, so
	// any error path from here on would otherwise leave a re-delivered
	// echo of this consumed envelope unmatched — and the wire-only branch
	// would persist an injected-context duplicate of the user's message.
	r.markWireOnlyUserTextSeen(item.ThreadID, providerItemID)
	r.mu.Lock()
	confirmedHook := r.flushUserTextConfirmed
	r.mu.Unlock()
	if confirmedHook != nil {
		confirmedHook(item.ThreadID, persisted)
	}
	if err := r.store.UpdateMessageAnchorProviderIDs(item.ThreadID, item.ID, providerItemID, parentUUID); err != nil {
		return fmt.Errorf("triage: update message anchor provider ids: %w", err)
	}
	return nil
}

// userTextMeta is ONE decode of an EventUserText's wire meta, shared by every
// classifier the handler runs. The keys it answers are all top-level scalars
// the two parsers stamp:
//
//   - `provider_item_id` — Claude's replay envelope uuid / Codex's userMessage
//     item id. Both Phase B and Phase C set it as a top-level string.
//   - `parent_uuid` — the echo's transcript parent, the slice-through point a
//     revert retry needs.
//   - `command_echo` — the Claude parser's flag for the CLI's command-INPUT
//     metadata echo (`<command-name>` triple; parse_user_replay.go). A matched
//     echo stamps the optimistic user row like any direct send; the flag exists
//     so the UNMATCHED top-level branch drops the raw XML instead of persisting
//     it as an injected-context row.
//   - `origin` — the provider's attribution for an echo AO did not send.
//   - `cross_session_*` — peer-delivery provenance.
//
// Every accessor is a defensive parse: absent, malformed, or wrong-typed reads
// as the zero value, so no caller repeats the same shape.
type userTextMeta map[string]json.RawMessage

func decodeUserTextMeta(raw json.RawMessage) userTextMeta {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

func (m userTextMeta) text(key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func (m userTextMeta) flag(key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value
}

// readProviderItemIDFromMeta is the one-shot form, for callers that hold only
// the raw meta (a persisted row's, not a live event's).
func readProviderItemIDFromMeta(meta json.RawMessage) string {
	return decodeUserTextMeta(meta).text("provider_item_id")
}

// crossSessionPeer is the provenance of a message delivered to this
// session by another Claude session, as the Claude parser reported it.
type crossSessionPeer struct {
	// From is the peer's address and the reply handle (a unix socket
	// path). May be empty on an older CLI shape.
	From string
	// Name is the peer's registered display name. May be empty; the
	// renderer falls back to From, and then to an unnamed label.
	Name string
}

// crossSessionPeer reports whether the Claude parser flagged this
// EventUserText as a cross-session delivery, and lifts the peer's provenance.
//
// `cross_session_message` alone gates the branch. The two provenance strings
// are best-effort: the flag means "another session sent this", and a delivery
// whose author cannot be named is still not something the user typed.
func (m userTextMeta) crossSessionPeer() (crossSessionPeer, bool) {
	if !m.flag("cross_session_message") {
		return crossSessionPeer{}, false
	}
	return crossSessionPeer{
		From: m.text("cross_session_from"),
		Name: m.text("cross_session_from_name"),
	}, true
}

// persistPeerSessionMessage creates the `user_text` row for a message
// another Claude session sent to this one (Claude Code's cross-session
// inbox).
//
// Role is `user` because that is what the model saw: the CLI injects the
// delivery as a user-role turn, and a transcript that showed it as a
// system notice would not explain why the assistant answered. The meta is
// what keeps that from being a lie about WHO — `origin` names the peer
// session in the same field and vocabulary Codex's external-queue rows
// use, so the frontend's one attribution branch labels both.
//
// `wire_only` is set for the usual reason it is set on this path: no
// draft, no attachments, and no local send to reconcile against. It is
// also what keeps the edit-and-resend pencil off a row whose text this
// app never owned.
//
// The id is deterministic from the wire id (`user:peer:<id>`) so a
// session-resume replay upserts the same row, and lives in its own
// prefix so it can never collide with an AO send's `user:<turnIndex>`.
func (r *Router) persistPeerSessionMessage(evt provider.ProviderEvent, providerItemID string, peer crossSessionPeer) error {
	fields := map[string]any{"cross_session_message": true}
	if peer.From != "" {
		fields["cross_session_from"] = peer.From
	}
	if peer.Name != "" {
		fields["cross_session_from_name"] = peer.Name
	}
	return r.persistExternalOriginMessage(evt, providerItemID, "peer", crossSessionMessageOrigin, fields)
}

// persistExternalOriginMessage creates the `user_text` row for a top-level
// wire-only user echo whose provenance the provider POSITIVELY named via
// `Meta.origin` — a Claude peer-session delivery or a Codex turn queued
// from outside this app (`codex queue`). Both are real conversation
// content with a known non-AO author, so they persist as user rows the
// frontend's one attribution branch labels by `origin`, rather than as
// "Injected provider context".
//
// idSlug keeps each origin in its own deterministic id space
// (`user:<slug>:<wire id>`) so a resume replay upserts the same row and
// nothing collides with an AO send's `user:<turnIndex>`.
func (r *Router) persistExternalOriginMessage(evt provider.ProviderEvent, providerItemID, idSlug, origin string, extra map[string]any) error {
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("triage: resolve turn index for %s message on %s: %w", origin, evt.ThreadID, err)
		}
		turnIndex = last
	}

	fields := map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
		"origin":           origin,
	}
	for key, value := range extra {
		fields[key] = value
	}
	metaBytes, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("triage: encode %s message meta: %w", origin, err)
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        fmt.Sprintf("user:%s:%s", idSlug, providerItemID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemUserText),
		Role:      "user",
		Status:    statusCompleted,
		Summary:   evt.Content,
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

// externalQueueOrigin mirrors `codex.ExternalTurnOriginQueue` — the origin
// the Codex session stamps on the user echo of a turn it did not start
// (`codex queue --thread`). Duplicated for the same provider-agnostic
// reason as crossSessionMessageOrigin and pinned by
// `TestExternalQueueOriginMatchesTheProviderConstant`.
const externalQueueOrigin = "external-queue"

// userEchoClientIDMetaKey is where the Codex adapter puts a `userMessage`
// echo's own `clientId` (protocol_item.go's userMessageClientIDMetaKey — same
// key, and TestUserEchoClientIDKeyMatchesTheProviderConstant is what keeps the
// two spellings from drifting). For a turn AO queued it is the optimistic row
// id AO passed to `thread/queue/add`, which is what makes it a pending-send
// key; every other producer's is a uuid AO never minted. Absent on every path
// that does not go through the provider's queue.
const userEchoClientIDMetaKey = "client_id"

// crossSessionMessageOrigin mirrors `claude.PeerTurnOrigin`. Duplicated
// rather than imported because triage is provider-agnostic by contract —
// it may not reach into a provider package for a vocabulary term — and
// pinned against the original by
// `TestPeerMessageOriginMatchesTheProviderConstant`.
const crossSessionMessageOrigin = "peer-session"

// attachProviderItemIDToUserRow merges `provider_item_id` onto the
// existing AO-persisted user row's meta and re-emits the upsert. The
// row's Summary stays untouched: AO already trimmed/cleaned content on
// enqueue, and overwriting from the wire would clobber attachments
// rendering and revision provenance the user-facing summary already
// reflects.
//
// Eager flush rows (`user:<turn>:flush:<n>`) get one extra step before
// the stamp: they were persisted quietly at dispatch time, before the
// rows the model emitted between dispatch and this echo, so they are
// repositioned to the turn tail here (BumpItemToTurnEnd) so the queued
// message lands AFTER that content — where Claude actually consumed it.
// This mirrors the interrupt-promote path (PromoteQuietFlushSends).
// Direct sends (`user:<turn>`) and steers (`user:<turn>:steer:<n>`) are
// already at their intended position and must not move, so the
// reposition is scoped to `:flush:` exactly as the interrupt path is.
//
// EXCEPT when the interrupt handler already placed the row
// (AnchoredAtInterrupt — set by the quiet promote, the deferred eager
// persist, and the Codex re-send registration): the row is then already
// at its user-visible position — the interrupt cut point — and rows
// landing between the interrupt and this echo are the interrupted
// turn's post-interrupt tail, which the user watched stream BELOW the
// message. Re-bumping here would leapfrog the message over that tail
// and persist an order the live view never showed (queued message
// reordering bug, 2026-07-03/07-05). Anchored rows stamp only.
//
// Runs under the thread's flush anchor lock (the handleUserText pop holds it): the
// interrupt paths' claim + store write completed or unclaimed before
// this ran, so pending.AnchoredAtInterrupt is truthful and the row meta
// read below reflects any committed promote.
//
// Missing-row with a QuietItem on the pending entry means the interrupt
// path's eager persist FAILED after the message was consumed from the
// queue (the entry was unclaimed, so we land in the un-anchored path):
// the echo proves the provider has the message, so self-heal by
// persisting the retained copy — the timeline must not lose a message
// the provider context contains. Missing-row WITHOUT a retained copy is
// the bounded send-error edge case (the AO send errored after
// RegisterPendingSend but before the optimistic persist): log and
// return nil rather than panic — the send-failure path has already
// surfaced an error row to the user, and a stranded pending entry would
// only mis-route the next wire user_text.
func (r *Router) attachProviderItemIDToUserRow(threadID string, pending *pendingSend, providerItemID string, evt provider.ProviderEvent, meta userTextMeta) error {
	aoItemID := pending.AOItemID
	existing, found, err := r.store.GetThreadItem(threadID, aoItemID)
	if err != nil {
		return fmt.Errorf("triage: load user row %s/%s for provider_item_id stamp: %w", threadID, aoItemID, err)
	}
	selfHealed := false
	if !found {
		if pending.QuietItem == nil {
			log.Printf("triage: handleUserText pending match for %s/%s but row absent — skipping stamp", threadID, aoItemID)
			return nil
		}
		log.Printf("triage: handleUserText pending match for %s/%s but row absent — re-persisting the retained copy (eager interrupt persist must have failed)", threadID, aoItemID)
		if err := r.persistItem(*pending.QuietItem, nil); err != nil {
			return fmt.Errorf("triage: self-heal persist for %s/%s: %w", threadID, aoItemID, err)
		}
		reloaded, reFound, reErr := r.store.GetThreadItem(threadID, aoItemID)
		if reErr != nil {
			return fmt.Errorf("triage: reload self-healed row %s/%s: %w", threadID, aoItemID, reErr)
		}
		if !reFound {
			return fmt.Errorf("triage: self-healed row %s/%s vanished after persist", threadID, aoItemID)
		}
		existing = reloaded
		selfHealed = true
	}

	// A deferred-origin row that was eager-persisted during interrupt owns
	// a fresh turn index. Its echo normally lands after the next pickup's
	// system.init opened that turn (no-op here), but when the interrupt
	// raced the CLI's mid-loop queue drain the echo arrives on the still-
	// live old round — open the logical turn exactly like the deferred
	// persist path would have. openQueuedEchoTurn's guards make this safe
	// on replays (settled or already-passed indexes are refused).
	if pending.WasDeferred {
		// pending.InterruptedTurnIndex names the turn the eager-persisting
		// interrupt provably cut short; the settle inside records
		// "interrupted" only when the still-open predecessor IS that turn
		// (a sibling queued message's turn drained naturally and settles
		// "end_turn") — and the settlement claim would block the
		// interrupt's own truncated result from recording that later.
		//
		// Opened BEFORE the meta stamp below — the inverse of
		// persistDeferredUserText's persist-then-open — because here the
		// row already exists at pending.TurnIndex: if the stamp fails
		// with the turn unopened, the response would allocate under the
		// still-open predecessor and sort ABOVE the message.
		r.openQueuedEchoTurn(threadID, pending.TurnIndex, eventTimestampMillis(evt), pending.InterruptedTurnIndex)
	}

	// Item id and parent uuid merge together — the same store tx stamps
	// both, so the parent (the already-cut retry's slice-through point)
	// can't be lost to a failed anchor follow-up (round-5, R5-8).
	parentUUID := meta.text("parent_uuid")
	mergedMeta, err := usermessage.MergeProviderIDs(existing.Meta, providerItemID, parentUUID)
	if err != nil {
		return fmt.Errorf("triage: merge provider ids into %s/%s meta: %w", threadID, aoItemID, err)
	}
	if mergedMeta == existing.Meta {
		if providerItemID != "" {
			// The row already durably carries this id, so the echo is
			// consumed regardless of what the anchor update below does
			// — record the replay guard first (see the main path's twin).
			r.markWireOnlyUserTextSeen(threadID, providerItemID)
		}
		if err := r.store.UpdateMessageAnchorProviderIDs(threadID, aoItemID, providerItemID, parentUUID); err != nil {
			return fmt.Errorf("triage: update message anchor provider ids: %w", err)
		}
		if providerItemID == "" {
			// We popped a pending-send marker but the wire echo carried
			// no stable id, so meta isn't updated and no upsert emits.
			// The frontend's queue-confirm gate keys on the meta-stamp,
			// so this leaves the queue overlay stuck until the thread
			// is reloaded. Loud log because the most likely cause is a
			// parser gap for a new wire shape — a silent stuck-UI is
			// the worst presentation.
			log.Printf("triage: handleUserText popped pending entry %s/%s but wire echo carried no provider_item_id — queue-confirm path will not fire; check parser coverage for the wire shape", threadID, aoItemID)
			return nil
		}
		// Otherwise: provider_item_id already equals providerItemID. This
		// is the expected path for an AO send that minted the id up front
		// (app_send.go stamps it before persist) and Claude echoed it back
		// verbatim, as well as for a genuine duplicate (session-resume
		// replay). The row already carries the id, so skip the redundant
		// write + emit; UpdateMessageAnchorProviderIDs above still folds in
		// parent_uuid, which only the echo knows.
		return nil
	}

	if existingID := usermessage.ReadProviderItemID(existing.Meta); existingID != "" && existingID != providerItemID {
		// The row already carried a DIFFERENT provider_item_id. For a
		// direct send this means Claude did not honour the top-level uuid
		// we supplied (claude.Session.Send) — a binary-contract drift. We
		// overwrite to the echoed id (the real transcript uuid, the correct
		// slice anchor) and self-heal the anchor below, but log loudly:
		// a silent drift would mean every fast send→escape quietly
		// regressed from the UUID-keyed slice to the ordinal-walk fallback.
		// Re-spike the uuid contract per docs/references/spike-policy.md.
		log.Printf("triage: handleUserText user row %s/%s pre-stamped provider_item_id %q but wire echo carried %q — Claude did not honour the supplied uuid; overwriting to the echoed id and re-checking the uuid contract", threadID, aoItemID, existingID, providerItemID)
	}

	// An interrupt-promoted row consumed here (mid-loop — no init between
	// the promote and this echo) is about to have its RESPONSE persist in
	// the same turn, below it. Record the provider-order boundary — the
	// turn's current max item_index — so revert / fork can tell the
	// interrupted tail (provider-order BEFORE the queued_command
	// attachment) from that response (provider-order AFTER). The read
	// loop is serial, so every same-turn row that precedes the
	// attachment was DISPATCHED before this echo — but a row deferred
	// behind a mid-settle stream (invariant 11) has no item_index yet
	// and would otherwise drain after this sample, land above the
	// boundary, and be cut as "response" on revert while the session
	// slice retains it (round-6, R6-2). Drain the queue first; when
	// that persisted anything, the promoted row re-bumps below so
	// display order matches provider order (those rows were never
	// user-visible, so no watched tail leapfrogs). Streaming rows are
	// immune: they persist their row synchronously at first content.
	// The meta decode is reliable because the flush anchor lock ordered
	// this pop after the promote's bump-and-mark committed — a claimed
	// entry always shows its marker here. Deferred-origin rows
	// (WasDeferred) never carry the promotion marker, so state.Promoted
	// gates them out.
	boundary := -1
	rebumpOverDrained := false
	unanchoredEagerBump := pending.Shape == sendShapeFlush && !pending.AnchoredAtInterrupt && !selfHealed
	if pending.AnchoredAtInterrupt {
		state, stateErr := itemmeta.DecodePromotionState(existing.Meta)
		if stateErr != nil {
			return fmt.Errorf("triage: decode promotion state for %s/%s: %w", threadID, aoItemID, stateErr)
		}
		if state.Promoted {
			// Under the thread's drain lock: a settle-goroutine drain
			// pops the queue map before its rows are committed, so an
			// unlocked check here could see an empty queue while
			// handed-off rows are still in flight — they would then
			// persist above the boundary sampled below and be cut as
			// "response" on revert although the session slice keeps
			// them (round-7, R7-3). Holding the lock through the
			// sample is sufficient: queue appends happen only on this
			// serial read loop, so the queue cannot refill afterwards.
			err := func() error {
				drainLock := r.drainLock(threadID)
				drainLock.Lock()
				defer drainLock.Unlock()
				if r.hasQueuedInterruptItems(threadID) {
					if drainErr := r.drainInterruptQueueLocked(threadID, false); drainErr != nil {
						// Per-item failures are already logged inside the
						// drain; the boundary below stays correct for the
						// rows that did persist.
						log.Printf("triage: drain deferred rows before promoted echo boundary %s/%s: %v", threadID, aoItemID, drainErr)
					}
					rebumpOverDrained = true
				}
				maxIdx, ok, maxErr := r.store.MaxItemIndexForTurn(threadID, existing.TurnIndex)
				if maxErr != nil {
					return fmt.Errorf("triage: resolve promoted echo boundary for %s/%s: %w", threadID, aoItemID, maxErr)
				}
				if ok {
					boundary = maxIdx
				}
				return nil
			}()
			if err != nil {
				return err
			}
			// Stash for the reinsert path: if the write below fails, a
			// session-death self-heal cannot recompute this value — by
			// then the response rows have persisted into the same MAX
			// (round-6, R6-1).
			pending.recordEchoPromotedBoundary(boundary)
		}
	} else if unanchoredEagerBump {
		// The unanchored eager row is about to bump to the turn tail. If
		// that bump FAILS and the session dies before a replay, the
		// self-heal can only stamp metadata — the row stays at its
		// dispatch-time index, ahead of output that preceded the queued
		// command in the transcript, and the revert cut at that index
		// would remove retained provider-prefix history (round-10,
		// R10-1). Sample the turn's provider-order boundary NOW (same
		// drain-first discipline as the promoted branch: rows deferred
		// behind a mid-settle stream were dispatched before this echo
		// and belong below the sample) and stash it for the self-heal,
		// which marks the healed row promoted-with-boundary so the cut
		// follows provider order even though the display position is
		// unrecoverable. The local `boundary` stays -1: on a SUCCESSFUL
		// bump the row sits at the tail and needs no marker. Draining
		// here also keeps display order true on success — a post-bump
		// drain would sort dispatched-before rows above the attachment.
		func() {
			drainLock := r.drainLock(threadID)
			drainLock.Lock()
			defer drainLock.Unlock()
			if r.hasQueuedInterruptItems(threadID) {
				if drainErr := r.drainInterruptQueueLocked(threadID, false); drainErr != nil {
					// Per-item failures are already logged inside the
					// drain; the sample below stays correct for the rows
					// that did persist.
					log.Printf("triage: drain deferred rows before eager flush bump %s/%s: %v", threadID, aoItemID, drainErr)
				}
			}
			maxIdx, ok, maxErr := r.store.MaxItemIndexForTurn(threadID, existing.TurnIndex)
			if maxErr != nil {
				// The stash only serves the bump-failure self-heal; a
				// failed sample degrades that heal to the pre-boundary
				// posture instead of failing an echo whose bump may
				// still succeed.
				log.Printf("triage: sample eager flush echo boundary for %s/%s: %v", threadID, aoItemID, maxErr)
				return
			}
			if ok {
				pending.recordEchoPromotedBoundary(maxIdx)
			}
		}()
	}
	if pending.NeedsTailRebump {
		// An earlier promoted echo's sibling re-bump failed to move this
		// row over the content it drained (rebumpAnchoredQuietSiblings):
		// it still sits below rows that precede it in provider order, and
		// the anchored path above would skip the bump. This echo is the
		// repair point — force the turn-tail bump; the boundary sampled
		// above keeps the revert cut on provider order (round-11, R11-5).
		rebumpOverDrained = true
	}
	transform := func(meta string) (string, error) {
		merged, mergeErr := usermessage.MergeProviderIDs(meta, providerItemID, parentUUID)
		if mergeErr != nil {
			return "", mergeErr
		}
		if boundary >= 0 {
			return itemmeta.MarkPromotedEchoBoundary(merged, boundary)
		}
		return merged, nil
	}

	// Both write shapes below are single-transaction store operations that
	// read the row's CURRENT meta inside the tx. The flush anchor lock already
	// keeps the interrupt promote out of this window; the tx-scoped merge
	// is the second line of defense for any writer the mutex doesn't
	// cover.
	var persisted store.Item
	if unanchoredEagerBump || rebumpOverDrained {
		// Eager flush rows were persisted quietly at dispatch, before the
		// rows the model emitted between dispatch and this echo. Reposition
		// to the turn tail so the queued message lands AFTER that content,
		// matching where Claude consumed it — the echo-side mirror of the
		// interrupt promote (PromoteQuietFlushSends). Scoped to :flush: so
		// direct sends and steers, already at their intended slot, never
		// move — and skipped when the interrupt handler already anchored
		// the row (see the doc comment above) or when the self-heal just
		// persisted it at the turn tail. An anchored row DOES re-bump when
		// the boundary drain above persisted deferred rows past it
		// (rebumpOverDrained): those rows precede the attachment in
		// provider order, so the message belongs back at the tail — and
		// they were never displayed, so no watched order changes (R6-2).
		persisted, err = r.store.BumpItemToTurnEnd(threadID, aoItemID, transform, eventTimestampMillis(evt))
		if err != nil {
			return fmt.Errorf("triage: reposition flush row %s/%s to turn tail: %w", threadID, aoItemID, err)
		}
		if rebumpOverDrained {
			// The re-bump leapfrogged later-FIFO anchored siblings in the
			// same turn: they now sit below both the drained rows and this
			// row, inverting provider order — and the user-row cut
			// predicate (item_index >= anchor) would KEEP them on a revert
			// at this message while the session slice removes them. Bump
			// them back above in FIFO order (round-7, R7-2).
			r.rebumpAnchoredQuietSiblings(threadID, aoItemID, existing.TurnIndex, eventTimestampMillis(evt))
		}
	} else {
		persisted, _, err = r.store.UpdateItemMetaMerge(threadID, aoItemID, transform, eventTimestampMillis(evt))
		if err != nil {
			return fmt.Errorf("triage: stamp provider_item_id on %s/%s: %w", threadID, aoItemID, err)
		}
	}
	// Replay guard, recorded the moment the stamp is durable and BEFORE
	// the fallible follow-ups below: the pending entry is already popped,
	// so any error path from here on would otherwise leave a re-delivered
	// echo of this consumed envelope unmatched — and the wire-only branch
	// would persist an injected:wire:* duplicate of the user's message.
	r.markWireOnlyUserTextSeen(threadID, providerItemID)
	r.emitItemUpsertWithActivity(persisted, false)
	// Eager quiet flush rows reach their final state only here — bumped
	// to the turn tail with provider ids stamped — so this is their
	// anchor-record moment, mirroring persistDeferredUserText's hook for
	// deferred rows. Without it a queued message dispatched into an
	// active turn carries no message anchor and can't be a fork /
	// revert-on-interrupt slice point. Direct sends (QueueItemID == "")
	// recorded at send time. Skipped when a FAILED first echo already
	// recorded at the boundary (AnchorRecordedAtEcho);
	// UpdateMessageAnchorProviderIDs below still folds this echo's ids
	// into that earlier record.
	if pending.QueueItemID != "" && !pending.AnchorRecordedAtEcho {
		r.mu.Lock()
		confirmedHook := r.flushUserTextConfirmed
		r.mu.Unlock()
		if confirmedHook != nil {
			confirmedHook(threadID, persisted)
		}
	}
	// AFTER the hook, deliberately: the flush row's anchor is created
	// (or replaced) inside the hook — an update running before it would
	// stamp zero rows or a doomed row. (The hook's record mirrors both
	// ids from the freshly stamped item meta, round-5 R5-8, so this
	// update is mainly for anchors that PRE-EXIST the echo — direct
	// sends recorded at send time — whose rows still need the echo's
	// ids folded in.)
	if err := r.store.UpdateMessageAnchorProviderIDs(threadID, aoItemID, providerItemID, parentUUID); err != nil {
		return fmt.Errorf("triage: update message anchor provider ids: %w", err)
	}
	return nil
}

// recordEchoBoundaryAnchor runs the confirmed hook for a queued flush
// entry whose echo handling failed pre-durability, so the message
// anchor exists from the true consumption boundary — the echo —
// instead of whenever a retry or session-death self-heal later gets
// the row durable (round-10, R10-2). Only an EXISTING row can carry an
// anchor (the anchor row's item FK): quiet shapes persisted at
// dispatch record here; a deferred shape whose persist failed stays
// anchor-less until its write lands — its record then runs at that
// (late, but earliest possible) moment. Caller holds the thread's
// flush anchor lock, same as the success-path hook sites.
func (r *Router) recordEchoBoundaryAnchor(threadID string, pending *pendingSend) {
	if pending.QueueItemID == "" || pending.AnchorRecordedAtEcho {
		return
	}
	r.mu.Lock()
	confirmedHook := r.flushUserTextConfirmed
	r.mu.Unlock()
	if confirmedHook == nil {
		return
	}
	row, found, err := r.store.GetThreadItem(threadID, pending.AOItemID)
	if err != nil {
		log.Printf("triage: lookup for echo-boundary anchor record %s/%s: %v", threadID, pending.AOItemID, err)
		return
	}
	if !found {
		return
	}
	confirmedHook(threadID, row)
	pending.markAnchorRecordedAtEcho()
}

// persistWireOnlySubagentPrompt creates or reconciles a nested `user_text`
// row for user-role content parented by a subagent launch. The opening prompt
// uses the launch-scoped identity from provider.SubagentOpeningPromptItemID:
// Claude's tool input creates that row before the child can produce output,
// then the transcript echo supplies its provider uuid without moving it.
// Later user-role deliveries keep their provider-keyed `user:wire:<id>` rows.
//
// The turn is the LAUNCH's turn, never the thread's current one
// (invariant 10). A backgrounded agent's prompt can arrive from the
// transcript backfill long after the launching turn closed, and filing
// it under whatever turn happens to be open would put the agent's own
// opening line in a different turn from the card it belongs to — the
// pane reads a scope's rows within the launch's turn, so it would simply
// vanish. turnIndexForEvent resolves the launch row and only falls back
// to the current turn when there is no launch to read.
//
// This is the PARENTED wire-only branch only. A top-level wire-only echo
// (no parent) is not a subagent prompt and not an AO send — it is
// provider-injected context, routed to persistInjectedContextNotification.
func (r *Router) persistWireOnlySubagentPrompt(evt provider.ProviderEvent, providerItemID string) error {
	turnIndex, err := r.turnIndexForEvent(evt)
	if err != nil {
		return fmt.Errorf("triage: resolve turn index for wire-only subagent prompt on %s: %w", evt.ThreadID, err)
	}

	parentID := eventParentID(evt)
	itemID := fmt.Sprintf("user:wire:%s", providerItemID)
	openingPrompt := decodeUserTextMeta(evt.Meta).flag(provider.MetaSubagentOpeningPromptKey)
	canonicalID := provider.SubagentOpeningPromptItemID(parentID)
	canonical, canonicalFound, err := r.store.GetThreadItem(evt.ThreadID, canonicalID)
	if err != nil {
		return fmt.Errorf("triage: inspect opening subagent prompt %s/%s: %w", evt.ThreadID, canonicalID, err)
	}
	claimed := false
	if canonicalFound {
		wasOpening, provisional, boundProviderItemID, stateErr := subagentOpeningPromptState(canonical.Meta)
		if stateErr != nil {
			return fmt.Errorf("triage: decode opening subagent prompt %s/%s: %w", evt.ThreadID, canonicalID, stateErr)
		}
		if openingPrompt || (provisional && canonical.Summary == evt.Content) || (wasOpening && boundProviderItemID == providerItemID) {
			itemID = canonicalID
			openingPrompt = true
			turnIndex = canonical.TurnIndex
			claimed = true
		}
	} else if openingPrompt {
		legacyID := fmt.Sprintf("user:wire:%s", providerItemID)
		legacy, legacyFound, legacyErr := r.store.GetThreadItem(evt.ThreadID, legacyID)
		if legacyErr != nil {
			return fmt.Errorf("triage: inspect legacy opening subagent prompt %s/%s: %w", evt.ThreadID, legacyID, legacyErr)
		}
		if legacyFound {
			itemID = legacyID
			turnIndex = legacy.TurnIndex
		} else {
			itemID = canonicalID
		}
		claimed = true
	}

	// Nothing claimed the content by the launch-scope identity, so this
	// may still be the prompt that opened a RESUMED round (§E6): a row
	// already standing under this same parent, minted from the rebind
	// envelope and waiting for the uuid the transcript is delivering
	// now. Binding it in place is what keeps the resumed round's opening
	// line from re-appearing as a second `user:wire:<uuid>` row below
	// the answer it asked for.
	resumeCarrierID := ""
	if !claimed {
		provisional, found, err := r.store.FindProvisionalSubagentPrompt(evt.ThreadID, parentID, evt.Content)
		if err != nil {
			return fmt.Errorf("triage: find provisional subagent prompt %s/%s: %w", evt.ThreadID, parentID, err)
		}
		if found {
			itemID = provisional.ID
			turnIndex = provisional.TurnIndex
			resumeMeta := decodeUserTextMeta(json.RawMessage(provisional.Meta))
			openingPrompt = openingPrompt || resumeMeta.flag(provider.MetaSubagentOpeningPromptKey)
			resumeCarrierID = resumeMeta.text(provider.MetaResumeCarrierIDKey)
		}
	}

	metaFields := map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
	}
	if openingPrompt {
		metaFields[provider.MetaSubagentOpeningPromptKey] = true
	}
	if resumeCarrierID != "" {
		// The row stays a resume prompt after binding — the round it
		// opened is what the carrier id names — but it is no longer
		// provisional: the uuid is now the provider's own.
		metaFields[provider.MetaSubagentResumePromptKey] = true
		metaFields[provider.MetaResumeCarrierIDKey] = resumeCarrierID
	}
	metaBytes, err := json.Marshal(metaFields)
	if err != nil {
		return fmt.Errorf("triage: encode wire-only subagent prompt meta: %w", err)
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemUserText),
		Role:      "user",
		Status:    statusCompleted,
		Summary:   evt.Content,
		ParentID:  parentID,
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

// persistInjectedContextNotification creates a non-user `notification`
// row for provider-injected context that reached the wire-only branch at
// TOP LEVEL (no parent tool, no pending-send match). Such content was NOT
// authored by the user — the pending-send registry is authoritative for
// AO-initiated sends — so it must never persist as a `user_text` bubble.
// This is the safety net that keeps any unrecognized CLI wrapper (a new
// upstream `<...>` shape we don't yet catalogue) from masquerading as the
// user: recognized noise is suppressed upstream in the provider parser;
// whatever slips through surfaces here as a clearly-non-user row.
//
// Role is `system` and kind is `notification`, so the frontend renders it
// through NotificationRow (a subtle system line), not the user bubble.
// The id prefix `injected:wire:` (not `user:wire:`) keeps it out of the
// user-message id space while staying deterministic from the wire id for
// resume-replay idempotency. Summary is a rune-safe preview; the raw body
// is duplicated in the subagent transcript for the known agent-report
// case, and the provider-events log retains the full wire line for any
// genuinely novel shape, so we do not stash a separate payload.
func (r *Router) persistInjectedContextNotification(evt provider.ProviderEvent, providerItemID string) error {
	turnIndex, ok := r.openTurnIndex(evt.ThreadID)
	if !ok {
		last, err := r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			return fmt.Errorf("triage: resolve turn index for injected context on %s: %w", evt.ThreadID, err)
		}
		turnIndex = last
	}

	metaBytes, err := json.Marshal(map[string]any{
		"provider_item_id": providerItemID,
		"wire_only":        true,
		"injected":         true,
	})
	if err != nil {
		return fmt.Errorf("triage: encode injected-context meta: %w", err)
	}

	now := eventTimestampMillis(evt)
	item := store.Item{
		ID:        fmt.Sprintf("injected:wire:%s", providerItemID),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		Kind:      string(provider.ItemNotification),
		Role:      "system",
		Status:    statusCompleted,
		Summary:   injectedContextSummary(evt.Content),
		Meta:      string(metaBytes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return r.persistItem(item, nil)
}

// injectedContextSummaryMaxRunes bounds the injected-context notification
// summary. Injected bodies can be large (a full subagent report); the row
// is a subtle single-line marker, so a rune-safe preview is enough.
const injectedContextSummaryMaxRunes = 200

// injectedContextSummary builds the notification summary for injected
// content: a fixed label plus a rune-safe, single-line preview of the
// body. Rune-safe (not byte-clipped) so a multi-byte character at the
// cutoff is never split into an invalid rune.
func injectedContextSummary(content string) string {
	preview := strings.TrimSpace(content)
	if i := strings.IndexByte(preview, '\n'); i >= 0 {
		preview = strings.TrimSpace(preview[:i])
	}
	runes := []rune(preview)
	if len(runes) > injectedContextSummaryMaxRunes {
		preview = string(runes[:injectedContextSummaryMaxRunes]) + "…"
	}
	if preview == "" {
		return "Injected provider context"
	}
	return "Injected provider context: " + preview
}
