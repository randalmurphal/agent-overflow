package threadapp

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// ModelUpdate is the persisted result plus the previous row root needs to
// reconcile provider runtime state without re-reading stale selection fields.
type ModelUpdate struct {
	Previous store.Thread
	Thread   store.Thread
}

func (u ModelUpdate) ProviderChanged() bool {
	return u.Previous.Provider != u.Thread.Provider
}

func (u ModelUpdate) SelectionChanged() bool {
	return u.Previous.Provider != u.Thread.Provider ||
		u.Previous.Model != u.Thread.Model ||
		u.Previous.ReasoningEffort != u.Thread.ReasoningEffort ||
		u.Previous.FastMode != u.Thread.FastMode ||
		u.Previous.ContextWindow != u.Thread.ContextWindow ||
		u.Previous.RuntimeMode != u.Thread.RuntimeMode
}

func (s *Service) UpdateModelSelection(threadID, providerName, model string) (ModelUpdate, error) {
	database, err := s.database("update model selection")
	if err != nil {
		return ModelUpdate{}, err
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return ModelUpdate{}, fmt.Errorf("thread provider cannot be empty")
	}
	if !ValidProvider(providerName) {
		return ModelUpdate{}, fmt.Errorf("%w: %q", store.ErrInvalidProvider, providerName)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelUpdate{}, fmt.Errorf("thread model cannot be empty")
	}
	thread, err := database.GetThread(threadID)
	if err != nil {
		return ModelUpdate{}, err
	}
	normalizedModel := provider.NormalizeModelSlug(providerName, model)
	if thread.Provider == providerName && thread.Model == normalizedModel {
		return ModelUpdate{Previous: thread, Thread: thread}, nil
	}
	profile, err := s.profileForSelection(providerName, normalizedModel)
	if err != nil {
		return ModelUpdate{}, err
	}
	return s.applyModelProfile(thread, profile)
}

func ValidProvider(providerName string) bool {
	return provider.IsKnownProvider(providerName)
}

func (s *Service) profileForSelection(providerName, model string) (store.ChatModelProfile, error) {
	models, err := s.modelPolicy("load chat model profile")
	if err != nil {
		return store.ChatModelProfile{}, err
	}
	if err := models.ValidateModel(providerName, model); err != nil {
		return store.ChatModelProfile{}, err
	}
	model = provider.NormalizeModelSlug(providerName, strings.TrimSpace(model))
	if s.deps.Store != nil {
		profile, err := s.deps.Store.GetChatModelProfile(providerName, model)
		if err == nil {
			return models.Sanitize(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load chat model profile: %w", err)
		}
	}
	return models.Sanitize(chatmodel.FallbackProfileWith(models.ContextWindowOptions, providerName, model)), nil
}

// modelSettingsFromProfile is the model configuration a chosen profile
// gives a thread, its auto-compact thresholds back at their defaults. The
// store clears the resume wiring when the provider changes.
func modelSettingsFromProfile(profile store.ChatModelProfile) store.ThreadModelSettings {
	profile = chatmodel.SanitizeProfile(profile)
	return store.ThreadModelSettings{
		Provider:        profile.Provider,
		Model:           profile.Model,
		ReasoningEffort: profile.ReasoningEffort,
		FastMode:        profile.FastMode,
		ContextWindow:   profile.ContextWindow,
		RuntimeMode:     string(provider.NormalizeRuntimeMode(profile.RuntimeMode)),
	}
}

func (s *Service) applyModelProfile(previous store.Thread, profile store.ChatModelProfile) (ModelUpdate, error) {
	settings := modelSettingsFromProfile(profile)
	if err := s.deps.Store.UpdateThreadModelSettings(previous.ID, previous.Provider, settings); err != nil {
		if errors.Is(err, store.ErrThreadProviderLocked) {
			return ModelUpdate{}, fmt.Errorf("update provider: thread is locked to %s (start a new thread to use %s)", previous.Provider, settings.Provider)
		}
		return ModelUpdate{}, err
	}
	stored, err := s.deps.Store.GetThread(previous.ID)
	if err != nil {
		return ModelUpdate{}, err
	}
	return ModelUpdate{Previous: previous, Thread: stored}, nil
}

// UpdateReasoningEffort and UpdateFastMode return the row the write produced
// plus whether the write moved anything, so root can broadcast the changed row
// on `thread:updated` and stay silent when the thread already carried the
// requested value.
func (s *Service) UpdateReasoningEffort(threadID, effort string) (store.Thread, bool, error) {
	database, err := s.database("update effort")
	if err != nil {
		return store.Thread{}, false, err
	}
	models, err := s.modelPolicy("update effort")
	if err != nil {
		return store.Thread{}, false, err
	}
	thread, err := database.GetThread(threadID)
	if err != nil {
		return store.Thread{}, false, err
	}
	normalized := strings.TrimSpace(effort)
	if !models.SupportsReasoningEffort(thread.Provider, thread.Model, normalized) {
		return store.Thread{}, false, fmt.Errorf("update effort: unsupported reasoning effort %q for %s/%s", normalized, thread.Provider, thread.Model)
	}
	return database.UpdateReasoningEffort(threadID, normalized)
}

func (s *Service) UpdateFastMode(threadID string, on bool) (store.Thread, bool, error) {
	database, err := s.database("update fast mode")
	if err != nil {
		return store.Thread{}, false, err
	}
	models, err := s.modelPolicy("update fast mode")
	if err != nil {
		return store.Thread{}, false, err
	}
	if on {
		thread, err := database.GetThread(threadID)
		if err != nil {
			return store.Thread{}, false, err
		}
		if !models.SupportsFastMode(thread.Provider, thread.Model) {
			return store.Thread{}, false, fmt.Errorf("update fast mode: unsupported for %s/%s", thread.Provider, thread.Model)
		}
	}
	return database.UpdateFastMode(threadID, on)
}
