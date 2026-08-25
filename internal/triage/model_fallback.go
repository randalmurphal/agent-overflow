package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

// The three system subtypes that mean "the turn survived, on a different model
// than the user asked for". They share one event and one row shape because the
// user-visible consequence is identical; only the CAUSE differs, and the cause
// is what the persisted `kind` reports. Flattening all three onto the refusal
// kind told the timeline every fallback was a safety refusal — which for a
// credits-consent switch or an unavailable model is simply false.
//
// Produced by claude/parse_system.go#parseModelFallbackEvent as meta.kind.
const (
	modelRefusalFallbackNotificationKind = "model_refusal_fallback"
	modelAvailabilityFallbackKind        = "model_fallback"
	modelConsentFallbackKind             = "model_consent_fallback"
)

// modelFallbackNotificationKind is the kind a fallback event lands on when the
// wire named no subtype at all. It fails toward the refusal kind on purpose:
// that is the one the frontend renders as a warning, and under-reporting a
// safety refusal is the worse of the two mistakes.
const modelFallbackNotificationKind = modelRefusalFallbackNotificationKind

// isModelFallbackNotificationKind gates what a wire value may become. An
// unknown subtype from a newer CLI is not forwarded as a row kind — the
// frontend has no branch for it — but the event still persists under the
// default.
func isModelFallbackNotificationKind(kind string) bool {
	switch kind {
	case modelRefusalFallbackNotificationKind, modelAvailabilityFallbackKind, modelConsentFallbackKind:
		return true
	default:
		return false
	}
}

// ModelFallbackEvent is the live wire projection for an automatic provider
// fallback. EffectiveModel is empty when the provider session ended and the
// frontend should return to the requested threads.model value.
type ModelFallbackEvent struct {
	ThreadID       string `json:"threadId"`
	RequestedModel string `json:"requestedModel,omitempty"`
	EffectiveModel string `json:"effectiveModel,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Category       string `json:"category,omitempty"`
	Revision       uint64 `json:"revision"`
}

type modelFallbackMeta struct {
	// Kind is the wire subtype (see the constants above). Empty on a producer
	// that predates it, and on the Codex path, which has no subtype at all.
	Kind                   string `json:"kind,omitempty"`
	OriginalModel          string `json:"originalModel"`
	FallbackModel          string `json:"fallbackModel"`
	Reason                 string `json:"reason"`
	Trigger                string `json:"trigger"`
	Category               string `json:"apiRefusalCategory"`
	Explanation            string `json:"apiRefusalExplanation"`
	RefusedUserMessageUUID string `json:"refusedUserMessageUuid"`
	// Choice and PersistedAsDefault are model_consent_fallback's own two
	// fields: WHICH consent choice was taken, and whether it was written back
	// as the account default rather than applying for this session only.
	// Omitted rather than zeroed — `false` means "this session only", which
	// the composed sentence already says.
	Choice             string `json:"choice,omitempty"`
	PersistedAsDefault bool   `json:"persistedAsDefault,omitempty"`
}

func (r *Router) handleModelFallback(evt provider.ProviderEvent) error {
	sessionEpoch := r.ThreadEpoch(evt.ThreadID)

	var meta modelFallbackMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("decode model fallback: %w", err)
		}
	}
	meta.Kind = strings.TrimSpace(meta.Kind)
	meta.Choice = truncateRunes(strings.TrimSpace(meta.Choice), maxModelFallbackLabelRunes)
	meta.OriginalModel = strings.TrimSpace(meta.OriginalModel)
	meta.OriginalModel = truncateRunes(meta.OriginalModel, maxModelFallbackModelRunes)
	meta.FallbackModel = truncateRunes(strings.TrimSpace(meta.FallbackModel), maxModelFallbackModelRunes)
	meta.Reason = truncateRunes(strings.TrimSpace(meta.Reason), maxModelFallbackReasonRunes)
	meta.Category = truncateRunes(strings.TrimSpace(meta.Category), maxModelFallbackLabelRunes)
	meta.Explanation = truncateRunes(strings.TrimSpace(meta.Explanation), maxModelFallbackExplanationRunes)
	meta.Trigger = truncateRunes(strings.TrimSpace(meta.Trigger), maxModelFallbackLabelRunes)
	meta.RefusedUserMessageUUID = truncateRunes(strings.TrimSpace(meta.RefusedUserMessageUUID), maxModelFallbackIDRunes)
	if meta.FallbackModel == "" {
		return fmt.Errorf("decode model fallback: empty fallback model")
	}
	boundedMeta, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode bounded model fallback: %w", err)
	}
	evt.Meta = boundedMeta

	reason := truncateRunes(firstNonEmptyString(strings.TrimSpace(evt.Content), meta.Reason), maxModelFallbackReasonRunes)
	if reason == "" {
		reason = fmt.Sprintf("%s could not handle this request and switched to %s.", meta.OriginalModel, meta.FallbackModel)
	}
	notificationKind := modelFallbackNotificationKind
	if isModelFallbackNotificationKind(meta.Kind) {
		notificationKind = meta.Kind
	}
	if err := r.persistTimelineNotification(evt, notificationKind, reason); err != nil {
		return err
	}

	r.mu.Lock()
	if id := r.identityIfPresent(evt.ThreadID); id != nil && (id.stopped || id.epoch != sessionEpoch) {
		r.mu.Unlock()
		return nil
	}
	fallbackState := r.state(evt.ThreadID)
	fallbackState.effectiveModel = meta.FallbackModel
	fallbackState.effectiveModelSet = true
	revision := r.nextEffectiveModelRevisionLocked(evt.ThreadID)
	r.mu.Unlock()
	r.emit("provider:model_fallback", ModelFallbackEvent{
		ThreadID:       evt.ThreadID,
		RequestedModel: meta.OriginalModel,
		EffectiveModel: meta.FallbackModel,
		Reason:         reason,
		Category:       meta.Category,
		Revision:       revision,
	})
	return nil
}

const (
	maxModelFallbackReasonRunes      = 1000
	maxModelFallbackExplanationRunes = 300
	maxModelFallbackModelRunes       = 128
	maxModelFallbackLabelRunes       = 64
	maxModelFallbackIDRunes          = 128
)

// ClearEffectiveModel drops the session-scoped model override and tells live
// panes to return to the durable requested model. It is intentionally narrower
// than CleanupThread so a provider self-exit can clear this projection without
// sweeping final-frame triage state owned by the normal teardown path.
func (r *Router) ClearEffectiveModel(threadID string) bool {
	if r == nil || strings.TrimSpace(threadID) == "" {
		return false
	}
	r.mu.Lock()
	existed := false
	if st := r.threadStateIfPresent(threadID); st != nil {
		existed = st.effectiveModelSet
		st.effectiveModel, st.effectiveModelSet = "", false
	}
	var revision uint64
	if existed {
		revision = r.nextEffectiveModelRevisionLocked(threadID)
	}
	r.mu.Unlock()
	if existed {
		r.emit("provider:model_fallback", ModelFallbackEvent{ThreadID: threadID, Revision: revision})
	}
	return existed
}

// nextEffectiveModelRevisionLocked returns the next per-thread projection
// revision. It lives on threadIdentity, which deliberately survives session
// cleanup, so an old goroutine can never reuse a revision after restart.
// MUST hold r.mu.
func (r *Router) nextEffectiveModelRevisionLocked(threadID string) uint64 {
	id := r.identity(threadID)
	id.effectiveModelRevision++
	return id.effectiveModelRevision
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
