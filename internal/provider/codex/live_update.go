package codex

import (
	"reflect"

	"agent-overflow/internal/provider"
)

// LiveUpdate is the turn-scoped config bundle a running Codex session
// re-applies to every turn/start call. Applying one takes effect on the
// next turn — Codex queues config changes and never mutates an in-flight
// turn (turn/steer carries no config fields).
type LiveUpdate struct {
	Model           string
	ReasoningEffort string
	ServiceTier     string
	ApprovalPolicy  string
	Sandbox         string
}

// PlanLiveUpdate diffs two option bundles and reports whether the delta can
// be applied to a live session through per-turn turn/start overrides. ok is
// false when any spawn-only axis differs — context window and autocompact
// token limit ride the thread/start `config` bag (model_context_window /
// model_auto_compact_token_limit) and have no per-turn override, and
// workdir / system prompt are likewise start-time-only.
//
// ResumeThreadID is session lifecycle state, not config, so it is excluded
// from the comparison.
func PlanLiveUpdate(prev, next provider.SessionOptions) (LiveUpdate, bool) {
	prevCfg := ConfigFromOptions(prev)
	nextCfg := ConfigFromOptions(next)

	update := LiveUpdate{
		Model:           nextCfg.Model,
		ReasoningEffort: nextCfg.ReasoningEffort,
		ServiceTier:     nextCfg.ServiceTier,
		ApprovalPolicy:  nextCfg.ApprovalPolicy,
		Sandbox:         nextCfg.Sandbox,
	}

	// Blank the live-appliable axes plus lifecycle fields, then require
	// everything left — the spawn-only config — to be identical.
	prevCfg.Model, nextCfg.Model = "", ""
	prevCfg.ReasoningEffort, nextCfg.ReasoningEffort = "", ""
	prevCfg.ServiceTier, nextCfg.ServiceTier = "", ""
	prevCfg.ApprovalPolicy, nextCfg.ApprovalPolicy = "", ""
	prevCfg.Sandbox, nextCfg.Sandbox = "", ""
	prevCfg.ResumeThreadID, nextCfg.ResumeThreadID = "", ""
	if !reflect.DeepEqual(prevCfg, nextCfg) {
		return LiveUpdate{}, false
	}
	return update, true
}

// ApplyLiveUpdate swaps the session's turn config; the next Send picks it
// up. Never fails: the values only reach the wire on the following
// turn/start, where a rejection surfaces through the normal send path.
func (s *Session) ApplyLiveUpdate(update LiveUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = update.Model
	s.reasoningEffort = update.ReasoningEffort
	s.serviceTier = update.ServiceTier
	s.approvalPolicy = update.ApprovalPolicy
	s.sandbox = update.Sandbox
}

// turnConfig snapshots the mutable turn-scoped config under the session
// mutex so Send composes one consistent view even if ApplyLiveUpdate lands
// concurrently.
func (s *Session) turnConfig() LiveUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LiveUpdate{
		Model:           s.model,
		ReasoningEffort: s.reasoningEffort,
		ServiceTier:     s.serviceTier,
		ApprovalPolicy:  s.approvalPolicy,
		Sandbox:         s.sandbox,
	}
}

// currentModel returns the active model under the session mutex; used by
// the read loop for usage attribution.
func (s *Session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}
