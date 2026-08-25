package triage

import (
	"encoding/json"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// CommandLifecycleEvent is the payload of `provider:command_lifecycle` —
// Claude's delivery narrative for a user message AO wrote to the CLI's
// stdin, projected onto the AO row the message belongs to.
//
// Live UI state only. The message's durable record is its `user_text`
// row, and the wire `user{isReplay}` echo remains what confirms the row
// entered context; this channel answers the different question "what is
// happening to the message I just sent" while it is still pending above
// the composer.
//
// Older CLIs emit no lifecycle frames at all, so a consumer must render
// correctly from the queue events alone. Every field here is additive
// detail on top of that baseline — never a precondition for it.
type CommandLifecycleEvent struct {
	ThreadID string `json:"threadId"`
	// CommandUUID is the client-minted envelope uuid the CLI echoed back.
	CommandUUID string `json:"commandUuid"`
	// UserItemID is the AO row the uuid was registered against
	// (`user:<turn>`, `user:<turn>:flush:<n>`, `user:<turn>:steer:<n>`).
	// Empty when the ack arrived for a uuid this router never registered
	// — a message from a previous process, or a CLI-minted id.
	UserItemID string                         `json:"userItemId,omitempty"`
	State      provider.CommandLifecycleState `json:"state"`
	// Delivery is set only on `started`, and reports whether the message
	// was drained INTO the turn that was running when it was queued
	// ("mid_turn" — the CLI redirected that turn) or began a turn of its
	// own ("new_turn"). Empty on every other state.
	Delivery CommandDelivery `json:"delivery,omitempty"`
}

// CommandDelivery classifies where a queued message actually landed.
type CommandDelivery string

const (
	// CommandDeliveredMidTurn — the message reached the model while the
	// round it was queued into was still open, i.e. Claude's queue
	// processor drained it between a tool result and the next API round
	// and the running turn changed course.
	CommandDeliveredMidTurn CommandDelivery = "mid_turn"
	// CommandDeliveredNewTurn — the round the message was queued into had
	// already closed (or none was open), so the message ran as its own
	// turn.
	CommandDeliveredNewTurn CommandDelivery = "new_turn"
)

// commandLifecycleEntry is the per-message correlation the router keeps
// between the `queued` ack and the message's terminal state.
//
// Two things must be remembered, and neither can be recovered later:
//
//   - UserItemID, because the pending-send FIFO entry it comes from is
//     POPPED by the wire echo, and the echo can arrive before `started`.
//     Resolving lazily would work for one arrival order and silently
//     lose the mapping for the other.
//   - RoundIDAtQueue, because "was this delivered mid-turn" is a
//     comparison against the round that was open at ENQUEUE time. By the
//     time `started` lands that round may have closed and a new one
//     opened, and the two are indistinguishable after the fact.
type commandLifecycleEntry struct {
	UserItemID     string
	RoundIDAtQueue string
}

// maxCommandLifecycleEntriesPerThread bounds the correlation map. Entries
// are released on the terminal ack (`completed` / `cancelled`) and swept
// by cleanupThread, so in the normal case the map holds only what is
// actually in flight — at most the flush queue's own cap. The bound
// exists for the pathological case where a CLI stops sending terminal
// acks: past it, new entries are refused (the lifecycle detail degrades
// to nothing, which is exactly the older-CLI baseline) rather than
// growing router memory for the process lifetime.
const maxCommandLifecycleEntriesPerThread = 64

// handleCommandLifecycle routes a `command_lifecycle` ack onto the
// frontend, resolving the AO row it belongs to and, on `started`,
// classifying whether the message was delivered mid-turn.
//
// The state machine must survive both arrival orders against the wire
// echo, and the absence of any ack at all:
//
//   - ack before echo: the `queued` ack registers the entry from the
//     still-live pending-send FIFO.
//   - echo before ack: the entry registered at `queued` outlives the
//     FIFO pop, so `started` still resolves.
//   - no acks (older CLI): nothing registers, nothing emits, and the
//     queue events alone drive the UI exactly as they did before.
//
// A `started` ack with no registered entry still emits — the state is
// true and the frontend simply has no row to attach it to — but carries
// no Delivery, because classifying without the enqueue-time round would
// mean guessing.
func (r *Router) handleCommandLifecycle(evt provider.ProviderEvent) error {
	var meta provider.CommandLifecycleMeta
	if len(evt.Meta) == 0 {
		return nil
	}
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		log.Printf("triage: unmarshal command lifecycle meta: %v", err)
		return nil
	}
	if meta.CommandUUID == "" || meta.State == "" {
		return nil
	}

	out := CommandLifecycleEvent{
		ThreadID:    evt.ThreadID,
		CommandUUID: meta.CommandUUID,
		State:       meta.State,
	}

	switch meta.State {
	case provider.CommandQueued:
		entry := commandLifecycleEntry{
			UserItemID:     r.pendingSendItemIDForProviderID(evt.ThreadID, meta.CommandUUID),
			RoundIDAtQueue: r.openRoundID(evt.ThreadID),
		}
		r.rememberCommandLifecycle(evt.ThreadID, meta.CommandUUID, entry)
		out.UserItemID = entry.UserItemID
	case provider.CommandStarted:
		entry, ok := r.peekCommandLifecycle(evt.ThreadID, meta.CommandUUID)
		if ok {
			out.UserItemID = entry.UserItemID
			out.Delivery = classifyCommandDelivery(entry.RoundIDAtQueue, r.openRoundID(evt.ThreadID))
		}
	default:
		// completed / cancelled are terminal: release the correlation in
		// the same step that reports it.
		entry, ok := r.takeCommandLifecycle(evt.ThreadID, meta.CommandUUID)
		if ok {
			out.UserItemID = entry.UserItemID
		}
	}

	r.emit(eventchan.ProviderCommandLifecycle, out)
	return nil
}

// classifyCommandDelivery compares the round open when the message was
// queued against the round open when it reached the model.
//
// Same non-empty round id ⇒ the CLI drained it INTO that turn. Anything
// else — the round closed, a different round opened, or none was open at
// enqueue time (a send with no turn running) — is a turn of its own.
func classifyCommandDelivery(roundAtQueue, roundAtStart string) CommandDelivery {
	if roundAtQueue != "" && roundAtQueue == roundAtStart {
		return CommandDeliveredMidTurn
	}
	return CommandDeliveredNewTurn
}

func (r *Router) rememberCommandLifecycle(threadID, commandUUID string, entry commandLifecycleEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	byUUID := st.commandLifecycle
	if byUUID == nil {
		byUUID = make(map[string]commandLifecycleEntry)
		st.commandLifecycle = byUUID
	}
	if _, exists := byUUID[commandUUID]; !exists && len(byUUID) >= maxCommandLifecycleEntriesPerThread {
		log.Printf("triage: command lifecycle map full for thread %s (%d entries); dropping correlation for %s",
			threadID, len(byUUID), commandUUID)
		return
	}
	byUUID[commandUUID] = entry
}

func (r *Router) peekCommandLifecycle(threadID, commandUUID string) (commandLifecycleEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return commandLifecycleEntry{}, false
	}
	entry, ok := st.commandLifecycle[commandUUID]
	return entry, ok
}

func (r *Router) takeCommandLifecycle(threadID, commandUUID string) (commandLifecycleEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return commandLifecycleEntry{}, false
	}
	entry, ok := st.commandLifecycle[commandUUID]
	if ok {
		delete(st.commandLifecycle, commandUUID)
		if len(st.commandLifecycle) == 0 {
			st.commandLifecycle = nil
		}
	}
	return entry, ok
}

// pendingSendItemIDForProviderID looks up the AO row id registered under
// a client-minted wire id. Read-only — the FIFO entry stays put; only the
// wire echo may consume it.
func (r *Router) pendingSendItemIDForProviderID(threadID, providerItemID string) string {
	if providerItemID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pending := range r.pendingSendsLocked(threadID) {
		if pending.ExpectedProviderItemID == providerItemID {
			return pending.AOItemID
		}
	}
	return ""
}
