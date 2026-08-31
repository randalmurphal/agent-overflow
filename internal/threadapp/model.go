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

func (s *Service) UpdateModel(threadID, model string) (ModelUpdate, error) {
	database, err := s.database("update model")
	if err != nil {
		return ModelUpdate{}, err
	}
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return ModelUpdate{}, fmt.Errorf("thread model cannot be empty")
	}
	thread, err := database.GetThread(threadID)
	if err != nil {
		return ModelUpdate{}, err
	}
	normalizedModel := provider.NormalizeModelSlug(thread.Provider, trimmedModel)
	if thread.Model == normalizedModel {
		return ModelUpdate{Previous: thread, Thread: thread}, nil
	}
	profile, err := s.profileForSelection(thread.Provider, normalizedModel)
	if err != nil {
		return ModelUpdate{}, err
	}
	return s.applyModelProfile(thread, profile)
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

func (s *Service) UpdateProvider(threadID, providerName string) (ModelUpdate, error) {
	database, err := s.database("update provider")
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
	thread, err := database.GetThread(threadID)
	if err != nil {
		return ModelUpdate{}, fmt.Errorf("update provider: %w", err)
	}
	if thread.Provider == providerName {
		return ModelUpdate{Previous: thread, Thread: thread}, nil
	}
	profile, err := s.latestProviderProfile(providerName)
	if err != nil {
		return ModelUpdate{}, err
	}
	return s.applyModelProfile(thread, profile)
}

func ValidProvider(providerName string) bool {
	switch providerName {
	case string(provider.Claude), string(provider.Codex), string(provider.ClaudeTUI):
		return true
	default:
		return false
	}
}

func (s *Service) profileForSelection(providerName, model string) (store.ChatModelProfile, error) {
	models, err := s.modelPolicy("load chat model profile")
	if err != nil {
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
	return models.Sanitize(chatmodel.FallbackProfile(providerName, model)), nil
}

func (s *Service) latestProviderProfile(providerName string) (store.ChatModelProfile, error) {
	models, err := s.modelPolicy("load latest chat model profile")
	if err != nil {
		return store.ChatModelProfile{}, err
	}
	if s.deps.Store != nil {
		profile, err := s.deps.Store.LatestChatModelProfileForProvider(providerName)
		if err == nil {
			return models.Sanitize(profile), nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.ChatModelProfile{}, fmt.Errorf("load latest chat model profile for provider %s: %w", providerName, err)
		}
	}
	return models.Sanitize(chatmodel.FallbackProfile(providerName, "")), nil
}

func ThreadWithModelProfile(thread store.Thread, profile store.ChatModelProfile) store.Thread {
	profile = chatmodel.SanitizeProfile(profile)
	providerChanged := thread.Provider != profile.Provider
	thread.Provider = profile.Provider
	thread.Model = profile.Model
	thread.ReasoningEffort = profile.ReasoningEffort
	thread.FastMode = profile.FastMode
	thread.ContextWindow = profile.ContextWindow
	thread.RuntimeMode = string(provider.NormalizeRuntimeMode(profile.RuntimeMode))
	thread.AutoCompactStandardPercent = 0
	thread.AutoCompactExtendedPercent = 0
	if providerChanged {
		thread.SessionRef = ""
		thread.PendingForkRef = ""
		thread.PendingForkResumeAt = ""
	}
	return thread
}

func (s *Service) applyModelProfile(previous store.Thread, profile store.ChatModelProfile) (ModelUpdate, error) {
	updated := ThreadWithModelProfile(previous, profile)
	var err error
	if previous.Provider == updated.Provider {
		err = s.deps.Store.UpdateThread(updated)
	} else {
		err = s.deps.Store.UpdateThreadIfProviderSwitchAllowed(updated, previous.Provider)
	}
	if err != nil {
		if errors.Is(err, store.ErrThreadProviderLocked) {
			return ModelUpdate{}, fmt.Errorf("update provider: thread is locked to %s (start a new thread to use %s)", previous.Provider, updated.Provider)
		}
		return ModelUpdate{}, err
	}
	stored, err := s.deps.Store.GetThread(updated.ID)
	if err != nil {
		return ModelUpdate{}, err
	}
	return ModelUpdate{Previous: previous, Thread: stored}, nil
}

func (s *Service) UpdateReasoningEffort(threadID, effort string) (store.Thread, error) {
	database, err := s.database("update effort")
	if err != nil {
		return store.Thread{}, err
	}
	models, err := s.modelPolicy("update effort")
	if err != nil {
		return store.Thread{}, err
	}
	thread, err := database.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	normalized := strings.TrimSpace(effort)
	if !models.SupportsReasoningEffort(thread.Provider, thread.Model, normalized) {
		return store.Thread{}, fmt.Errorf("update effort: unsupported reasoning effort %q for %s/%s", normalized, thread.Provider, thread.Model)
	}
	if err := database.UpdateReasoningEffort(threadID, normalized); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}

func (s *Service) UpdateFastMode(threadID string, on bool) (store.Thread, error) {
	database, err := s.database("update fast mode")
	if err != nil {
		return store.Thread{}, err
	}
	models, err := s.modelPolicy("update fast mode")
	if err != nil {
		return store.Thread{}, err
	}
	if on {
		thread, err := database.GetThread(threadID)
		if err != nil {
			return store.Thread{}, err
		}
		if !models.SupportsFastMode(thread.Provider, thread.Model) {
			return store.Thread{}, fmt.Errorf("update fast mode: unsupported for %s/%s", thread.Provider, thread.Model)
		}
	}
	if err := database.UpdateFastMode(threadID, on); err != nil {
		return store.Thread{}, err
	}
	return database.GetThread(threadID)
}
