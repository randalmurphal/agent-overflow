package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// threadSettingsUpdateMethod is the RPC that lands a config change on a
// Codex thread without waiting for a turn. `#[experimental("thread/settings/update")]`
// (codex-rs/app-server-protocol/src/protocol/common.rs:553), which every AO
// handshake already opts into via `capabilities.experimentalApi`.
const threadSettingsUpdateMethod = "thread/settings/update"

// settingsEchoWindow bounds how long a pushed expectation waits for its
// `thread/settings/updated` echo before it is dropped unverified.
//
// The echo is NOT guaranteed: the app-server only emits it when the new
// snapshot differs from the one it last noted for this connection
// (`note_thread_settings` returns `changed`, bespoke_event_handling.rs:1259),
// so pushing a value Codex is already running produces silence — which is
// agreement, not a failure. Expiring the expectation is how "silence means
// nothing changed" stays distinct from "silence means we were ignored":
// an ignored request is a JSON-RPC error, not a missing notification.
const settingsEchoWindow = 15 * time.Second

// ThreadSettingsPush names which axes of a planned LiveUpdate a
// `thread/settings/update` should carry.
//
//   - Model / Effort / ServiceTier are pushed on every session. They decide
//     what the next turn costs and how fast it runs, and pushing them makes
//     Codex's own thread state — what `thread/read`, the rollout, and any
//     other client on the same app-server see — match the user's current
//     selection instead of lagging by one turn.
//   - ApprovalPolicy / Sandbox / ApprovalsReviewer are NOT pushed at all.
//     The three ride `turn/start`, which AO sends for every turn on this
//     thread, so all three are re-asserted before anything executes and
//     between turns they govern nothing. A second writer for them would add
//     a way for the two to disagree and buy nothing. (They were pushed for a
//     while, on sessions where the app-server could start a turn out of its
//     own queue with no overrides at all — AO no longer puts messages there,
//     so nothing starts a turn on this thread but AO.)
//
// An axis is pushed only when it CHANGED. A push with nothing to say is
// never sent: upstream skips the core op entirely when every override is
// None (turn_processor.rs:788), so an empty request is a wasted round trip
// that also produces no echo.
type ThreadSettingsPush struct {
	Model       bool
	Effort      bool
	ServiceTier bool
}

// Empty reports whether the push has no axes to carry.
func (p ThreadSettingsPush) Empty() bool {
	return !p.Model && !p.Effort && !p.ServiceTier
}

// PlanThreadSettingsPush diffs two option bundles and reports which pushable
// axes changed. It is the companion of PlanLiveUpdate — callers apply the
// LiveUpdate first (so the session holds the new values) and then push.
//
// A model or effort that changed TO the empty string is not pushed. Empty
// means "inherit the thread default" in AO's vocabulary, and neither
// `model` nor `effort` on ThreadSettingsUpdateParams can express that:
// both are plain `Option<T>`, so an explicit JSON null deserializes to None
// and reads as "unchanged", exactly like omitting the key. turn/start has
// the same limitation, so not pushing leaves today's behavior rather than
// pretending to a clear that the wire cannot carry. `serviceTier` is the one
// exception — it is a double option and CAN be cleared, which is why fast
// mode gets its own handling in planServiceTierWrite.
func PlanThreadSettingsPush(prev, next provider.SessionOptions) ThreadSettingsPush {
	prevCfg := ConfigFromOptions(prev)
	nextCfg := ConfigFromOptions(next)
	return ThreadSettingsPush{
		Model:       nextCfg.Model != "" && nextCfg.Model != prevCfg.Model,
		Effort:      nextCfg.ReasoningEffort != "" && nextCfg.ReasoningEffort != prevCfg.ReasoningEffort,
		ServiceTier: nextCfg.ServiceTier != prevCfg.ServiceTier,
	}
}

// serviceTierWrite is the decision about what an outbound `serviceTier` key
// must carry, plus the assertion-flag value to commit once the write lands.
//
// Codex's `service_tier` is a DOUBLE option on every params struct that
// carries it (ThreadSettingsUpdateParams, TurnStartParams, ThreadStartParams):
// omitted means "leave unchanged", explicit null means "clear to standard
// routing" (SERVICE_TIER_DEFAULT_REQUEST_VALUE, "default"). Omitting the key
// when fast mode is switched OFF therefore leaves the priority tier the
// previous ON asserted in force for the rest of the session — the opt-out
// silently failing to clear what the opt-in stored.
//
// The clear is scoped to what THIS session asserted. A user whose
// config.toml selects a tier AO does not model (upstream also ships `flex`)
// keeps it as long as AO never wrote the axis; only an AO-set tier is ever
// cleared by AO.
type serviceTierWrite struct {
	include bool
	// value is the JSON value for the key. A nil value marshals as null,
	// which is the clear.
	value any
	// asserting is the tier AO will be holding once the write lands; empty
	// means the write is the clear.
	asserting string
	// clearing is the tier the write is clearing, retained so an echo can
	// tell "Codex dropped our clear" from "Codex reports some other tier".
	clearing string
}

// planServiceTierWrite decides the outbound serviceTier key under the
// session mutex without mutating the assertion record. The record moves only
// in commitServiceTierWrite, after the write is known to have landed: a
// failed clear that had already forgotten the assertion would never be
// retried, and the thread would stay on the fast tier forever.
func (s *Session) planServiceTierWrite() serviceTierWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serviceTier != "" {
		return serviceTierWrite{include: true, value: s.serviceTier, asserting: s.serviceTier}
	}
	if s.assertedServiceTier != "" {
		return serviceTierWrite{include: true, value: nil, clearing: s.assertedServiceTier}
	}
	return serviceTierWrite{}
}

// commitServiceTierWrite records that the planned write reached Codex.
func (s *Session) commitServiceTierWrite(write serviceTierWrite) {
	if !write.include {
		return
	}
	s.mu.Lock()
	s.assertedServiceTier = write.asserting
	s.mu.Unlock()
}

// settingsEchoExpectation is what a successful push expects the next
// `thread/settings/updated` to report. Only the axes the push actually
// carried are recorded, so an unpushed axis can never raise a mismatch.
type settingsEchoExpectation struct {
	model  string
	effort string
	// tierAsserted is the exact tier id the push asked Codex to run; empty
	// with tierCleared non-empty means the push asked Codex to stop running
	// tierCleared.
	tierAsserted string
	tierCleared  string
	expires      time.Time
}

// PushThreadSettings sends the named axes to Codex through
// `thread/settings/update`.
//
// Contract:
//
//   - The caller must have applied the matching LiveUpdate first; this reads
//     the session's current requested config, so the two cannot disagree.
//   - The composer-change caller must only call this BETWEEN turns
//     (`app_session_config.go#threadTurnInFlight`). Skipping the push while
//     busy loses nothing there: the same values ride the next turn/start, and
//     the RPC's value is the echo, not mutating a running turn.
//   - Every failure mode degrades to today's behavior. A codex that predates
//     the method answers with a JSON-RPC error; that is logged once per
//     session and the method is never retried on that session, so a downgrade
//     is neither a user-facing failure nor silent.
func (s *Session) PushThreadSettings(ctx context.Context, push ThreadSettingsPush) error {
	if push.Empty() {
		return nil
	}
	threadID := strings.TrimSpace(s.rootThreadID())
	if threadID == "" {
		// No thread to configure yet — the handshake params carry these
		// axes anyway.
		return nil
	}

	s.mu.Lock()
	if s.settingsUpdateUnsupported {
		s.mu.Unlock()
		return nil
	}
	model := s.model
	effort := s.reasoningEffort
	s.mu.Unlock()

	params := map[string]any{"threadId": threadID}
	expectation := settingsEchoExpectation{expires: time.Now().Add(settingsEchoWindow)}
	if push.Model && model != "" {
		params["model"] = model
		expectation.model = model
	}
	if push.Effort && effort != "" {
		params["effort"] = effort
		expectation.effort = effort
	}
	var tierWrite serviceTierWrite
	if push.ServiceTier {
		tierWrite = s.planServiceTierWrite()
		if tierWrite.include {
			params["serviceTier"] = tierWrite.value
			expectation.tierAsserted = tierWrite.asserting
			expectation.tierCleared = tierWrite.clearing
		}
	}
	if len(params) == 1 {
		// Everything the push named resolved to "nothing the wire can say".
		return nil
	}

	// Armed BEFORE the request, not after it. Upstream answers the RPC by
	// enqueueing a core op (turn_processor.rs:788) and the echo rides the
	// event stream from a separate task, so the response and the
	// notification are not ordered against each other on the wire. Arming
	// afterwards would let a fast echo land against no expectation and
	// silently skip the verification this whole path exists for; arming
	// first cannot produce a false alarm because the failure paths below
	// disarm it.
	s.mu.Lock()
	s.pendingSettingsEcho = &expectation
	s.mu.Unlock()

	if _, err := s.sendRequest(ctx, threadSettingsUpdateMethod, params); err != nil {
		s.disarmSettingsEcho(&expectation)
		if IsMethodUnsupported(err, threadSettingsUpdateMethod) {
			s.noteThreadSettingsUpdateUnsupported()
			return nil
		}
		return fmt.Errorf("codex: %s: %w", threadSettingsUpdateMethod, err)
	}

	s.commitServiceTierWrite(tierWrite)
	return nil
}

// disarmSettingsEcho drops an expectation whose request failed, so nothing AO
// never successfully asked for can be reported as a rejection. It only clears
// its OWN expectation: a concurrent push that has already replaced it is the
// live one and must survive.
func (s *Session) disarmSettingsEcho(expectation *settingsEchoExpectation) {
	s.mu.Lock()
	if s.pendingSettingsEcho == expectation {
		s.pendingSettingsEcho = nil
	}
	s.mu.Unlock()
}

// noteThreadSettingsUpdateUnsupported disables the push for this session and
// logs the downgrade exactly once. Per session, not per call: a running
// session cannot swap binaries, and a fresh session after an upgrade or
// downgrade re-learns the answer from scratch.
func (s *Session) noteThreadSettingsUpdateUnsupported() {
	s.mu.Lock()
	already := s.settingsUpdateUnsupported
	s.settingsUpdateUnsupported = true
	s.mu.Unlock()
	if already {
		return
	}
	log.Printf(
		"codex: thread %s: this codex build has no %s; config changes will apply on the next turn instead",
		s.threadID, threadSettingsUpdateMethod,
	)
}

// verifyThreadSettingsEcho compares Codex's authoritative settings snapshot
// against what the last push asked for, and returns a user-facing message
// when they disagree. Called under s.mu from reconcileThreadSettings; the
// caller emits after unlocking.
//
// The expectation is single-shot and deadline-bounded, which is what keeps
// the check honest in both directions:
//
//   - Consumed on the first echo, so a later unrelated change (a reroute
//     during a subsequent turn) is never read as a rejection of this push.
//   - Expired if no echo arrives, because "no echo" means the snapshot did
//     not change — i.e. Codex was already running what we asked for.
//
// A cleared tier is checked against the tier that was cleared, not against
// the sentinel Codex substitutes: the clear is sent as JSON null and echoes
// back as upstream's "default" request value, so comparing the literal we
// sent would report a mismatch on the success path, and hardcoding "default"
// would report one the day upstream renames its own sentinel. "Codex is
// still running the tier we asked it to drop" is the only failure the clear
// can have, and it is what this compares.
func (s *Session) verifyThreadSettingsEcho(settings ThreadSettings) string {
	expectation := s.pendingSettingsEcho
	if expectation == nil {
		return ""
	}
	s.pendingSettingsEcho = nil
	if time.Now().After(expectation.expires) {
		return ""
	}

	var mismatches []string
	if expectation.model != "" && settings.Model != "" && settings.Model != expectation.model {
		mismatches = append(mismatches, fmt.Sprintf("model %q (requested %q)", settings.Model, expectation.model))
	}
	if expectation.effort != "" && settings.ReasoningEffort != "" && settings.ReasoningEffort != expectation.effort {
		mismatches = append(mismatches, fmt.Sprintf("reasoning effort %q (requested %q)", settings.ReasoningEffort, expectation.effort))
	}
	switch {
	case expectation.tierAsserted != "" && settings.ServiceTier != expectation.tierAsserted:
		mismatches = append(mismatches, fmt.Sprintf(
			"service tier %q (requested %q)", settings.ServiceTier, expectation.tierAsserted))
	case expectation.tierCleared != "" && settings.ServiceTier == expectation.tierCleared:
		mismatches = append(mismatches, fmt.Sprintf(
			"service tier %q (requested standard routing)", settings.ServiceTier))
	}
	if len(mismatches) == 0 {
		return ""
	}
	return "Codex did not apply the requested thread settings — it is running " +
		strings.Join(mismatches, ", ") + "."
}

// emitThreadSettingsEchoError surfaces an echo mismatch as thread error
// state rather than a log line. It is not fatal: the session keeps running
// and the next turn/start re-asserts every axis, so the user sees what Codex
// actually chose instead of a silent divergence between the composer and the
// running thread.
func (s *Session) emitThreadSettingsEchoError(message string) {
	meta, _ := json.Marshal(map[string]any{"fatal": false})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventError,
		ThreadID:  s.threadID,
		Content:   message,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}
