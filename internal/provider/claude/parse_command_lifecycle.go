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
// when the message reaches the model, then `completed`; `cancelled` for a
// message that will never be delivered.
//
// Unrecognised states are dropped rather than forwarded: the enum is
// undocumented, and admitting an unknown value would push a state no
// consumer has a branch for into live UI. A frame missing `command_uuid`
// is likewise dropped — it correlates to nothing.
//
// Older CLIs emit no `command_lifecycle` at all, so no consumer may treat
// its absence as a failure; see docs/references/claude-wire.md
// §command_lifecycle.
func parseCommandLifecycle(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	commandUUID := readRawString(raw["command_uuid"])
	if commandUUID == "" {
		return nil, nil
	}
	state, ok := commandLifecycleState(readRawString(raw["state"]))
	if !ok {
		return nil, nil
	}
	meta, err := json.Marshal(provider.CommandLifecycleMeta{
		CommandUUID: commandUUID,
		State:       state,
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
	}
	return "", false
}
