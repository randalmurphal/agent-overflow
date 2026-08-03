package codex

import (
	"reflect"

	"agent-overflow/internal/provider"
)

// LiveUpdate is the turn-scoped config bundle a running Codex session
// re-applies to every turn/start call. Applying one takes effect on the
// next turn — Codex queues config changes and never mutates an in-flight
// turn (turn/steer carries no config fields).
//
// Every runtime-mode axis lives here, so no runtime-mode transition on Codex
// needs a session restart: `approvalPolicy`, `sandboxPolicy`, and
// `approvalsReviewer` are all TurnStartParams fields upstream documents as
// applying "for this turn and subsequent turns". (Contrast Claude, where
// read-only's `--disallowedTools` is spawn-only.)
//
// `thread/settings/update` pushes the model / effort / service-tier subset of
// this block into Codex between turns as well — see ThreadSettingsPush. It is
// additive: the turn/start overrides below are unchanged and remain the
// authority, so a push that fails, times out, or hits a codex that predates
// the method leaves exactly today's behavior.
type LiveUpdate struct {
	Model             string
	ReasoningEffort   string
	ServiceTier       string
	ApprovalPolicy    string
	Sandbox           string
	ApprovalsReviewer string
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
		Model:             nextCfg.Model,
		ReasoningEffort:   nextCfg.ReasoningEffort,
		ServiceTier:       nextCfg.ServiceTier,
		ApprovalPolicy:    nextCfg.ApprovalPolicy,
		Sandbox:           nextCfg.Sandbox,
		ApprovalsReviewer: threadApprovalsReviewer(nextCfg),
	}

	// Blank the live-appliable axes plus lifecycle fields, then require
	// everything left — the spawn-only config — to be identical.
	prevCfg.Model, nextCfg.Model = "", ""
	prevCfg.ReasoningEffort, nextCfg.ReasoningEffort = "", ""
	prevCfg.ServiceTier, nextCfg.ServiceTier = "", ""
	prevCfg.ApprovalPolicy, nextCfg.ApprovalPolicy = "", ""
	prevCfg.Sandbox, nextCfg.Sandbox = "", ""
	prevCfg.ApprovalsReviewer, nextCfg.ApprovalsReviewer = "", ""
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
	s.approvalsReviewer = update.ApprovalsReviewer
}

// turnConfig snapshots the mutable turn-scoped config under the session
// mutex so Send composes one consistent view even if ApplyLiveUpdate lands
// concurrently.
func (s *Session) turnConfig() LiveUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LiveUpdate{
		Model:             s.model,
		ReasoningEffort:   s.reasoningEffort,
		ServiceTier:       s.serviceTier,
		ApprovalPolicy:    s.approvalPolicy,
		Sandbox:           s.sandbox,
		ApprovalsReviewer: s.approvalsReviewer,
	}
}

// currentModel returns the model the just-finished work actually ran on,
// for usage attribution. Codex's own report wins when it has made one:
// a reroute or a guardian downgrade changes the billed model without AO
// asking, and attributing those tokens to the model we requested would
// put them on the wrong ledger row. Falls back to the requested model
// when Codex has reported nothing (no `thread/settings/updated` yet).
func (s *Session) currentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observedSettingsKnown && s.observedSettings.Model != "" {
		return s.observedSettings.Model
	}
	return s.model
}
