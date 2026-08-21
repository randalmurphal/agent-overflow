// Package claude — parser for `command_lifecycle`-type NDJSON lines, the
// CLI's delivery ack for a user message written to its stdin.

package claude

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// parseCommandLifecycle converts a `command_lifecycle` envelope into an
// EventCommandLifecycle.
//
// Wire shape (verified 2.1.219, AO's exact flag set):
//
//	{"type":"command_lifecycle","command_uuid":"<uuid>","state":"queued"}
//
// `command_uuid` is the uuid AO stamped on the outbound user envelope's
// top-level `uuid` field, so the ack correlates to an AO row with no
// ordering assumptions — strictly better than the `user{isReplay}` echo,
// which can arrive an arbitrarily long time later (claude-wire.md
// §"Queued-message consumption"). The echo remains the row's
// confirmation signal; this is the delivery narrative alongside it.
//
// The CLI acks EVERY stdin user message, direct sends included — not just
// mid-turn ones. States: `queued` immediately on write, then `started`
// when the message reaches the model, then exactly one terminal state —
// `completed` (the consuming turn ended), `cancelled` (removed from the
// queue, or consumed into a turn that aborted or hard-failed), or
// `discarded` (2.1.224+: the session ended with the message still
// queued). `discarded` is handled exactly like `cancelled` here — both
// mean "never delivered, and the window it may have opened is over" —
// and stays a distinct state only so the cause reaches the timeline.
// The wire carries no reason field for any of them (2.1.237 schema:
// `{type, command_uuid, state, uuid, session_id}`).
//
// Unrecognised states are dropped rather than forwarded: the enum is
// undocumented, and admitting an unknown value would push a state no
// consumer has a branch for into live UI. A frame missing `command_uuid`
// is likewise dropped — it correlates to nothing.
//
// Older CLIs emit no `command_lifecycle` at all, so no consumer may treat
// its absence as a failure; see docs/references/claude-wire.md
// §command_lifecycle.
//
// PEER-STARTED TURNS. A `command_uuid` AO never minted is the CLI's own,
// and on a session with cross-session messaging on that means one thing:
// another Claude session's `SendMessage` was accepted and opened a turn
// here. The bracket is the FIRST observable of such a turn (spike
// 2026-08-21, 2.1.237: `started` precedes even the `system/init` the turn
// re-emits), so it is where the origin is decided; `Meta.origin` then
// carries `peer-session` for consumers that would otherwise attribute the
// turn to the reader. `session_peer.go` owns the ledger that answers
// "did we issue this", and answers conservatively — an unknown session
// or an overflowed ledger reads as OURS, never as a peer.
//
// This is a Parser method for one piece of wire-level correlation the
// envelope order makes safe: a provider-executed command's `<synthetic>`
// output envelope arrives inside that command's own `started`→`completed`
// pair (spike-verified 2.1.219, including a command queued mid-turn).
// Windows can nest — a mid-turn message's `started` fires inside the
// running turn's window — so the field is last-started-wins; the identity
// guard on clear keeps an outer `completed` from wiping an inner window,
// and non-synthetic envelopes are never stamped. Send-to-row correlation
// for pending sends still lives in triage; this field never outlives the
// started→completed window.
func (p *Parser) parseCommandLifecycle(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	commandUUID := readRawString(raw["command_uuid"])
	if commandUUID == "" {
		return nil, nil
	}
	state, ok := commandLifecycleState(readRawString(raw["state"]))
	if !ok {
		return nil, nil
	}
	// Resolved for EVERY frame of the bracket, not just `started`: a
	// consumer reading only the terminal frame must reach the same verdict
	// as one reading the opening frame, and `issued` is deliberately
	// non-consuming so it does.
	origin := ""
	if p.peerTurns != nil && p.peerTurns.commandUUIDIsPeerOriginated(commandUUID) {
		origin = PeerTurnOrigin
	}

	switch state {
	case provider.CommandStarted:
		p.activeCommandUUID = commandUUID
	case provider.CommandCompleted, provider.CommandCancelled, provider.CommandDiscarded:
		// Guard on identity so a late ack for an older message cannot
		// clear a newer started window. `discarded` closes the window for
		// the same reason `cancelled` does: no further output can arrive
		// for this command, and leaving the window open would let the
		// NEXT session's synthetic output inherit a dead uuid.
		if p.activeCommandUUID == commandUUID {
			p.activeCommandUUID = ""
		}
		// The bracket is over, so the ledger entry has nothing left to
		// answer for. Released AFTER the origin resolution above, or the
		// terminal frame would classify differently from its own `started`.
		if p.peerTurns != nil {
			p.peerTurns.releaseIssuedCommandUUID(commandUUID)
			// An in-flight `/rename` promotes its name only on `completed`
			// — cancelled and discarded left the peer registry on the old
			// name. See session_peer.go settlePeerRename.
			p.peerTurns.settlePeerRename(commandUUID, state)
			// Row suppression is scoped to the SAME window: the command's
			// `<synthetic>` output arrives before this frame, so releasing
			// here cannot strand a row and cannot leak the decision onto a
			// later command that happens to reuse the uuid.
			p.peerTurns.releaseSuppressedCommandResult(commandUUID)
		}
	}
	meta, err := json.Marshal(provider.CommandLifecycleMeta{
		CommandUUID: commandUUID,
		State:       state,
		Origin:      origin,
	})
	if err != nil {
		return nil, err
	}
	return []provider.ProviderEvent{{
		Kind:      provider.EventCommandLifecycle,
		ThreadID:  threadID,
		ItemID:    commandUUID,
		Meta:      meta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}

func commandLifecycleState(value string) (provider.CommandLifecycleState, bool) {
	switch provider.CommandLifecycleState(value) {
	case provider.CommandQueued:
		return provider.CommandQueued, true
	case provider.CommandStarted:
		return provider.CommandStarted, true
	case provider.CommandCompleted:
		return provider.CommandCompleted, true
	case provider.CommandCancelled:
		return provider.CommandCancelled, true
	case provider.CommandDiscarded:
		return provider.CommandDiscarded, true
	}
	return "", false
}
