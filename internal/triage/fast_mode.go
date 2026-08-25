package triage

import (
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// FastModeStateEvent is the payload of `provider:fast_mode`, the live
// per-thread projection of what the provider says fast mode is actually
// doing right now.
//
// This is session state, never history (root CLAUDE.md principle 2): the
// CLI restates it on every `system/init` and every `result`, so the
// newest frame is the whole answer and nothing is persisted. A frontend
// that missed frames re-learns the state at the next turn boundary.
//
// Absence is silence. The router only emits when the wire carried a
// report, so a thread with no frame yet means "unknown" — the UI must not
// render that as a denial. See provider.FastModeStatus.
type FastModeStateEvent struct {
	ThreadID       string `json:"threadId"`
	State          string `json:"state,omitempty"`
	DisabledReason string `json:"disabledReason,omitempty"`
}

// emitFastModeState forwards a provider fast-mode report to the frontend.
// No-op on a nil status so callers can hand it the optional field
// straight off the wire payload without a guard of their own.
func (r *Router) emitFastModeState(threadID string, status *provider.FastModeStatus) {
	if r == nil || threadID == "" || status == nil || status.IsZero() {
		return
	}
	r.emit(eventchan.ProviderFastMode, FastModeStateEvent{
		ThreadID:       threadID,
		State:          status.State,
		DisabledReason: status.DisabledReason,
	})
}

// fastModeStatusFromTurnComplete pulls the optional fast-mode report off a
// turn-complete event. Only the wire payload carries one — a soft round
// close or a synthesized truncation is AO's own signal and knows nothing
// about the provider's fast-mode state, so neither may fabricate one.
func fastModeStatusFromTurnComplete(evt provider.ProviderEvent) *provider.FastModeStatus {
	wire, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok || wire == nil {
		return nil
	}
	return wire.FastMode
}
