package triage

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/provider"
)

const modelFallbackNotificationKind = "model_refusal_fallback"

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
	OriginalModel          string `json:"originalModel"`
	FallbackModel          string `json:"fallbackModel"`
	Reason                 string `json:"reason"`
	Trigger                string `json:"trigger"`
	Category               string `json:"apiRefusalCategory"`
	Explanation            string `json:"apiRefusalExplanation"`
	RefusedUserMessageUUID string `json:"refusedUserMessageUuid"`
}

func (r *Router) handleModelFallback(evt provider.ProviderEvent) error {
	r.mu.Lock()
	sessionEpoch := r.threadEpochs[evt.ThreadID]
	r.mu.Unlock()

	var meta modelFallbackMeta
	if len(evt.Meta) > 0 {
		if err := json.Unmarshal(evt.Meta, &meta); err != nil {
			return fmt.Errorf("decode model fallback: %w", err)
		}
	}
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
	if err := r.persistTimelineNotification(evt, modelFallbackNotificationKind, reason); err != nil {
		return err
	}

	r.mu.Lock()
	if _, stopped := r.stoppedThreads[evt.ThreadID]; stopped || r.threadEpochs[evt.ThreadID] != sessionEpoch {
		r.mu.Unlock()
		return nil
	}
	r.effectiveModelByThread[evt.ThreadID] = meta.FallbackModel
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
	_, existed := r.effectiveModelByThread[threadID]
	delete(r.effectiveModelByThread, threadID)
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
// revision. Entries deliberately survive session cleanup, like threadEpochs,
// so an old goroutine can never reuse a revision after restart. MUST hold r.mu.
func (r *Router) nextEffectiveModelRevisionLocked(threadID string) uint64 {
	r.effectiveModelRevisions[threadID]++
	return r.effectiveModelRevisions[threadID]
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
