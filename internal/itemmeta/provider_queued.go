package itemmeta

import "encoding/json"

// providerQueuedKey marks a user_text row whose message the PROVIDER's
// own queue has taken ownership of: a Codex `thread/queue/add` on an
// app-server >= 0.148. This app no longer queues that way (mid-turn
// messages go to `turn/steer`), so the rows that carry the marker are ones
// an earlier version wrote, and the readers below still serve them.
//
// It exists because that ownership outlives every in-process record of
// it. Once the add is accepted the message is durable in codex's own
// SQLite: it survives the session, the app-server, and Agent Overflow
// itself, and `QueuedItemService::on_thread_idle` dispatches it on the
// next resume. Two recoveries need to know that, and one of them runs
// in a process that never saw the add:
//
//   - Session death: every other unconfirmed queued message is one AO
//     still owes the provider, so restoring it to the composer draft is
//     the recovery. Restoring THIS one hands the user a draft of a
//     prompt that is also scheduled to run.
//   - App restart: the dispatched turn's `userMessage` echo carries the
//     row id back as `clientId`. Without a durable record that the row
//     is already persisted and waiting for that echo, the echo has no
//     pending send to claim and lands as injected provider context.
//
// The row's meta is its only durable home, and every later meta write
// goes through a map-based merge (usermessage.MergeProviderItemID, the
// promotion markers above), so the marker survives the echo stamp.
//
// It is a MARKER, not a lifecycle: nothing clears it when the message
// runs. "This row entered the provider's queue rather than being sent
// straight to a turn" stays true forever, and the two consumers above
// both scope themselves to rows the provider still lists as queued.
const providerQueuedKey = "providerQueued"

// providerQueueHandoffKey rides alongside it while the handover is
// still UNPROVEN.
//
// The marker above had to go on BEFORE the `thread/queue/add` write —
// an add that lands and is never acked is exactly the case where this
// process may not come back to stamp anything — so on its own it
// cannot tell "the provider has this message" from "AO was about to
// ask it to". Those two need opposite recoveries, and the difference
// only becomes visible to a LATER process:
//
//   - Proven (`providerQueued` alone): absent from the provider's queue
//     means it already dispatched. Leave the row as history.
//   - Unproven (`providerQueued` + this key): absent from the
//     provider's queue overwhelmingly means the add never landed, so
//     the message has no owner at all and belongs back in the composer.
//
// Without the split, the second case is stranded forever: the row is
// marked provider-owned, so every recovery path steps around it, and
// the provider it names never took it.
//
// The earlier version set it true before the write and wrote it false
// the moment the app-server's ack (or a `thread/queue/list` read-back)
// proved the row exists. An ambiguous add — written, never acked, and
// unverifiable — left it true: the next session start asks the queue
// instead of guessing.
const providerQueueHandoffKey = "providerQueueHandoff"

// IsProviderQueueHandoffPending reports whether raw names a row whose
// hand-off to the provider queue was never confirmed.
//
// Cannot fail, for the same reason IsProviderQueued cannot: its callers
// are recovery paths, and corrupt meta reads as "not pending", which
// leaves the row exactly where it is instead of restoring a message the
// provider may already own.
func IsProviderQueueHandoffPending(raw string) bool {
	if raw == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	pending, _ := m[providerQueueHandoffKey].(bool)
	return pending
}

// IsProviderQueued reports whether raw carries the marker.
//
// Unlike DecodePromotionState this cannot fail: its callers are
// recovery paths choosing between "restore this message" and "leave it
// with the provider", and both run at a session death where erroring
// out would strand the message entirely. Corrupt or non-object meta
// reads as unmarked, which is the direction that keeps the message
// recoverable by hand.
func IsProviderQueued(raw string) bool {
	if raw == "" {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	marked, _ := m[providerQueuedKey].(bool)
	return marked
}
