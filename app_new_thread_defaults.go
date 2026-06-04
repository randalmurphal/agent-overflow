package main

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// NewThreadDefaultsUpdate persists chat-bar defaults for future threads without
// requiring a thread row. ProjectID is used only to return the same defaults
// projection the draft-placeholder flow already consumes.
type NewThreadDefaultsUpdate struct {
	ProjectID                  string `json:"projectId"`
	Provider                   string `json:"provider,omitempty"`
	Model                      string `json:"model,omitempty"`
	ReasoningEffort            string `json:"reasoningEffort,omitempty"`
	FastMode                   *bool  `json:"fastMode,omitempty"`
	ContextWindow              int    `json:"contextWindow,omitempty"`
	AutoCompactStandardPercent *int   `json:"autoCompactStandardPercent,omitempty"`
	AutoCompactExtendedPercent *int   `json:"autoCompactExtendedPercent,omitempty"`
	RuntimeMode                string `json:"runtimeMode,omitempty"`
}

// UpdateNewThreadDefaults updates the provider/model profile used to seed
// future draft placeholders and newly-created threads. It intentionally does
// not mutate any existing thread row.
func (a *App) UpdateNewThreadDefaults(update NewThreadDefaultsUpdate) (ThreadDefaults, error) {
	if a.store == nil {
		return ThreadDefaults{}, fmt.Errorf("update new thread defaults: store unavailable")
	}
	projectID := strings.TrimSpace(update.ProjectID)
	if projectID == "" {
		return ThreadDefaults{}, fmt.Errorf("update new thread defaults: projectId is required")
	}
	if _, err := a.store.GetProject(projectID); err != nil {
		return ThreadDefaults{}, fmt.Errorf("update new thread defaults: resolve project %s: %w", projectID, err)
	}

	profile, err := a.newThreadDefaultsProfile(update)
	if err != nil {
		return ThreadDefaults{}, err
	}
	profile.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpsertChatModelProfile(profile); err != nil {
		return ThreadDefaults{}, err
	}
	return a.GetThreadDefaults(CreateThreadOptions{
		ProjectID: projectID,
		Provider:  profile.Provider,
		Model:     profile.Model,
	})
}

func (a *App) newThreadDefaultsProfile(update NewThreadDefaultsUpdate) (store.ChatModelProfile, error) {
	providerName := strings.TrimSpace(update.Provider)
	if providerName != "" && !validThreadProvider(providerName) {
		return store.ChatModelProfile{}, fmt.Errorf("%w: %q", store.ErrInvalidProvider, providerName)
	}
	model := strings.TrimSpace(update.Model)
	profile := a.seedChatModelProfile(providerName, model)
	profile = chatmodel.SanitizeProfile(profile)
	if providerName != "" {
		profile.Provider = providerName
	}
	if model != "" {
		profile.Model = provider.NormalizeModelSlug(profile.Provider, model)
	}
	if strings.TrimSpace(profile.Provider) == "" || strings.TrimSpace(profile.Model) == "" {
		return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: provider and model are required")
	}

	if effort := strings.TrimSpace(update.ReasoningEffort); effort != "" {
		if !provider.ReasoningEffortSupportedForModel(profile.Provider, profile.Model, effort) {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: unsupported reasoning effort %q for %s/%s", effort, profile.Provider, profile.Model)
		}
		profile.ReasoningEffort = effort
	} else {
		profile.ReasoningEffort = string(provider.CoerceReasoningEffortForModel(
			profile.Provider,
			profile.Model,
			provider.NormalizeReasoningEffort(profile.ReasoningEffort),
		))
	}

	if update.FastMode != nil {
		if *update.FastMode && !a.supportsFastModeForModel(profile.Provider, profile.Model) {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: fast mode unsupported for %s/%s", profile.Provider, profile.Model)
		}
		profile.FastMode = *update.FastMode
	} else if profile.FastMode && !a.supportsFastModeForModel(profile.Provider, profile.Model) {
		profile.FastMode = false
	}

	if update.ContextWindow != 0 {
		options := chatmodel.ContextWindowOptions(profile.Provider, profile.Model)
		if len(options) > 0 && !chatmodel.ContextWindowSupported(options, update.ContextWindow) {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: unsupported context window %d for %s/%s", update.ContextWindow, profile.Provider, profile.Model)
		}
		profile.ContextWindow = update.ContextWindow
	}
	options := chatmodel.ContextWindowOptions(profile.Provider, profile.Model)
	if len(options) > 0 && !chatmodel.ContextWindowSupported(options, profile.ContextWindow) {
		profile.ContextWindow = provider.DefaultContextWindowForModel(profile.Provider, profile.Model, options[0].Tokens)
	}

	if update.AutoCompactStandardPercent != nil {
		if !provider.IsValidAutoCompactPercent(*update.AutoCompactStandardPercent) {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: auto-compact standard percent must be between 0 and 90")
		}
		profile.AutoCompactStandardPercent = *update.AutoCompactStandardPercent
	}
	if update.AutoCompactExtendedPercent != nil {
		if !provider.IsValidAutoCompactPercent(*update.AutoCompactExtendedPercent) {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: auto-compact extended percent must be between 0 and 90")
		}
		profile.AutoCompactExtendedPercent = *update.AutoCompactExtendedPercent
	}

	if runtimeMode := strings.TrimSpace(update.RuntimeMode); runtimeMode != "" {
		parsed, err := threadmode.ParseRuntime(runtimeMode)
		if err != nil {
			return store.ChatModelProfile{}, fmt.Errorf("update new thread defaults: %w", err)
		}
		profile.RuntimeMode = string(parsed)
	} else {
		profile.RuntimeMode = string(provider.NormalizeRuntimeMode(profile.RuntimeMode))
	}

	return profile, nil
}
