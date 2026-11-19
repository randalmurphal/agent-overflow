package app

import (
	"fmt"

	"agent-overflow/internal/store"
)

// The ledger state of the requests a thread's tool rows made, whether or not
// its Agents listing still shows them.

// ThreadRequestState is one request a thread made as the ledger holds it:
// its state, the caller's timeline row that delivered its answer once the
// provider confirmed that row, whether or not any pane has it loaded, and
// whether the answer has reached the caller at all.
type ThreadRequestState struct {
	Token      string `json:"token"`
	State      string `json:"state"`
	WakeItemID string `json:"wakeItemId,omitempty"`
	// Answered is set once the answer reached the caller: in its confirmed
	// wake row, or in the reply of a call that collected it (a thread_status
	// or a listing), which is the delivery and leaves no wake row.
	Answered bool `json:"answered"`
}

// maxThreadRequestStates bounds one GetThreadRequestStates read.
const maxThreadRequestStates = 256

// GetThreadRequestStates reads the requests among tokens that threadID made,
// for the tool rows that made them, whether the Agents listing still shows
// them or not. A token threadID did not make is left out.
//
//ao:scope threads:read
func (a *App) GetThreadRequestStates(threadID string, tokens []string) ([]ThreadRequestState, error) {
	if len(tokens) > maxThreadRequestStates {
		return nil, fmt.Errorf("read request states: %d tokens, at most %d", len(tokens), maxThreadRequestStates)
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if token != "" && !seen[token] {
			seen[token] = true
			unique = append(unique, token)
		}
	}
	deliveries, err := a.store.ThreadRequestDeliveries(threadID, unique)
	if err != nil {
		return nil, fmt.Errorf("read request states: %w", err)
	}
	out := make([]ThreadRequestState, 0, len(deliveries))
	for _, d := range deliveries {
		out = append(out, ThreadRequestState{
			Token: d.Token, State: d.State, WakeItemID: d.WakeItemID,
			Answered: d.WakeItemID != "" || threadRequestDeliveredInline(d),
		})
	}
	return out, nil
}

// threadRequestDeliveredInline reports that a call's reply carried the
// request's answer or its late reply to the caller (requestState,
// claimInlineDelivery). The mark is written once that reply is, so no wake
// row follows it.
func threadRequestDeliveredInline(d store.ThreadRequestDelivery) bool {
	return (d.DeliveredHow == store.ThreadWakeInline && d.DeliveredAt != 0) ||
		(d.LateDeliveredHow == store.ThreadWakeInline && d.LateDeliveredAt != 0)
}
