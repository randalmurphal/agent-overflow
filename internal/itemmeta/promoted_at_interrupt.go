package itemmeta

import (
	"encoding/json"
	"fmt"
)

// promotedAtInterruptKey marks a queued flush user row whose same-turn
// display successors (up to the echo boundary) PRECEDE it in the
// provider transcript. Two origins produce that inversion:
//
//   - An interrupt PROMOTED the row over its turn's not-yet-persisted
//     tail: the row was bumped to the turn end at interrupt time, and
//     the interrupted round's trailing rows (final partials the
//     provider flushed while stopping) then persisted BELOW it.
//   - The echo-time tail bump of an unanchored eager flush row FAILED
//     and session-death self-heal stamped the row at its dispatch-time
//     index — output the model emitted between dispatch and
//     consumption sits after it in display order (round-10, R10-1).
//
// Either way, Claude appends the queued message's `queued_command`
// attachment only when it consumes the message, after all of that
// content.
//
// Truncation-position consumers (revert's DeleteConversationFromItem,
// fork-from-message's clone) branch on this marker: cutting "at the
// message" in PROVIDER order must keep the same-turn suffix for marked
// rows and drop it for everything else. The marker rides items.meta —
// the row's only durable home — and every later meta write goes
// through map-based merges (usermessage.MergeProviderItemID), so it
// survives the echo stamp.
const promotedAtInterruptKey = "promoted_at_interrupt"

// promotedEchoBoundaryKey records, on a marked row, the highest
// same-turn item_index that PRECEDES the queued message in the
// provider transcript. Sampled at echo time, when the CLI consumed the
// message mid-loop: rows persisted before the echo precede the
// queued_command attachment in provider order; rows persisted after it
// are the response (provider-order AFTER). Without the boundary the
// marker predicate cannot tell those apart and would retain response
// rows the provider slice cuts. Absent when the echo never arrived —
// every same-turn non-user successor then precedes the attachment.
const promotedEchoBoundaryKey = "promoted_echo_boundary"

// PromotionState is the decoded truncation-relevant slice of a user
// row's meta: whether the row was interrupt-promoted, and — when its
// echo was seen — the provider-order boundary index.
type PromotionState struct {
	Promoted bool
	// EchoBoundary is valid only when HasEchoBoundary is true.
	EchoBoundary    int
	HasEchoBoundary bool
}

// DecodePromotionState reads the promotion marker and echo boundary
// from raw. Empty meta decodes to the zero state; malformed meta is an
// error — the truncation predicates that branch on this state must not
// silently degrade to display-order cuts on corrupt data.
func DecodePromotionState(raw string) (PromotionState, error) {
	if raw == "" {
		return PromotionState{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return PromotionState{}, fmt.Errorf("itemmeta: decode promotion state: %w", err)
	}
	if m == nil {
		// A literal JSON null unmarshals into a nil map without error;
		// non-empty meta that decodes to no object at all is corrupt,
		// not an unpromoted anchor.
		return PromotionState{}, fmt.Errorf("itemmeta: decode promotion state: meta is JSON null, not an object")
	}
	var state PromotionState
	if raw, present := m[promotedAtInterruptKey]; present {
		v, ok := raw.(bool)
		if !ok {
			return PromotionState{}, fmt.Errorf("itemmeta: decode promotion state: %s is %T, want bool", promotedAtInterruptKey, raw)
		}
		state.Promoted = v
	}
	if raw, present := m[promotedEchoBoundaryKey]; present {
		v, ok := raw.(float64)
		if !ok || v != float64(int(v)) || v < 0 {
			return PromotionState{}, fmt.Errorf("itemmeta: decode promotion state: %s is %v (%T), want a non-negative integer", promotedEchoBoundaryKey, raw, raw)
		}
		state.EchoBoundary = int(v)
		state.HasEchoBoundary = true
	}
	if state.HasEchoBoundary && !state.Promoted {
		// No writer produces a boundary without the marker:
		// MarkPromotedEchoBoundary is only called on marked rows, and
		// the marker is never cleared. A boundary on an unmarked row is
		// corruption, and the truncation predicates that would silently
		// take the display-order cut must error instead (round-13,
		// C13-3).
		return PromotionState{}, fmt.Errorf("itemmeta: decode promotion state: %s present without %s", promotedEchoBoundaryKey, promotedAtInterruptKey)
	}
	return state, nil
}

// MarkPromotedAtInterrupt returns raw with the promoted-at-interrupt
// marker set. An empty meta becomes a one-key object. Malformed metas
// return an error — the caller is about to persist this row and must
// not silently drop the marker.
func MarkPromotedAtInterrupt(raw string) (string, error) {
	return mergeKey(raw, promotedAtInterruptKey, true)
}

// MarkPromotedEchoBoundary returns raw with the echo boundary set.
// Callers stamp it only on rows that already carry the promotion
// marker (checked via DecodePromotionState) — the boundary is
// meaningless without it.
func MarkPromotedEchoBoundary(raw string, boundary int) (string, error) {
	return mergeKey(raw, promotedEchoBoundaryKey, boundary)
}

func mergeKey(raw, key string, value any) (string, error) {
	merged := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &merged); err != nil {
			return "", err
		}
		if merged == nil {
			merged = map[string]any{}
		}
	}
	merged[key] = value
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
