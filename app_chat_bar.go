package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func fallbackChatProvider() string {
	return string(provider.Claude)
}

func fallbackChatModelForProvider(providerName string) string {
	models := provider.ModelsForProvider(providerName)
	if len(models) == 0 {
		return ""
	}
	return models[0].Slug
}

func fallbackChatModelProfile(providerName, model string) store.ChatModelProfile {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		providerName = fallbackChatProvider()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = fallbackChatModelForProvider(providerName)
	}
	return store.ChatModelProfile{
		Provider:                   providerName,
		Model:                      model,
		ReasoningEffort:            string(provider.DefaultReasoningEffortForModel(providerName, model, provider.DefaultReasoningEffort)),
		FastMode:                   false,
		ContextWindow:              defaultContextWindowForProviderModel(providerName, model, 0),
		AutoCompactStandardPercent: 0,
		AutoCompactExtendedPercent: 0,
		RuntimeMode:                string(provider.DefaultRuntimeMode),
	}
}

func chatModelProfileFromThread(thread store.Thread) store.ChatModelProfile {
	effort := provider.CoerceReasoningEffortForModel(
		thread.Provider,
		thread.Model,
		provider.NormalizeReasoningEffort(thread.ReasoningEffort),
	)
	fastMode := thread.FastMode && supportsStoredFastMode(thread.Provider, thread.Model)
	return store.ChatModelProfile{
		Provider:                   thread.Provider,
		Model:                      thread.Model,
		ReasoningEffort:            string(effort),
		FastMode:                   fastMode,
		ContextWindow:              thread.ContextWindow,
		AutoCompactStandardPercent: thread.AutoCompactStandardPercent,
		AutoCompactExtendedPercent: thread.AutoCompactExtendedPercent,
		RuntimeMode:                string(provider.NormalizeRuntimeMode(thread.RuntimeMode)),
	}
}

func (a *App) rememberChatModelProfile(thread store.Thread) {
	if a.store == nil || thread.Mode == "discussion" {
		return
	}
	if strings.TrimSpace(thread.Provider) == "" || strings.TrimSpace(thread.Model) == "" {
		return
	}
	profile := chatModelProfileFromThread(thread)
	if latest, err := a.store.LatestChatModelProfile(); err == nil {
		if sameChatModelProfile(latest, profile) {
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("chat profile: load latest before remember: %v", err)
	}
	if err := a.store.UpsertChatModelProfile(profile); err != nil {
		log.Printf("chat profile: remember %s/%s for thread %s: %v", thread.Provider, thread.Model, thread.ID, err)
	}
}

func sameChatModelProfile(a, b store.ChatModelProfile) bool {
	return a.Provider == b.Provider &&
		a.Model == b.Model &&
		a.ReasoningEffort == b.ReasoningEffort &&
		a.FastMode == b.FastMode &&
		a.ContextWindow == b.ContextWindow &&
		a.AutoCompactStandardPercent == b.AutoCompactStandardPercent &&
		a.AutoCompactExtendedPercent == b.AutoCompactExtendedPercent &&
		provider.NormalizeRuntimeMode(a.RuntimeMode) == provider.NormalizeRuntimeMode(b.RuntimeMode)
}

func (a *App) seedChatModelProfile(providerName, model string) store.ChatModelProfile {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	switch {
	case providerName == "" && model == "":
		if a.store != nil {
			profile, err := a.store.LatestChatModelProfile()
			if err == nil {
				return sanitizeChatModelProfile(profile)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("chat profile: load latest: %v", err)
			}
		}
		return fallbackChatModelProfile("", "")
	case providerName != "" && model == "":
		if a.store != nil {
			profile, err := a.store.LatestChatModelProfileForProvider(providerName)
			if err == nil {
				return sanitizeChatModelProfile(profile)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("chat profile: load latest for provider %s: %v", providerName, err)
			}
		}
		return fallbackChatModelProfile(providerName, "")
	case providerName == "" && model != "":
		providerName = fallbackChatProvider()
	}
	model = provider.NormalizeModelSlug(providerName, model)

	if a.store != nil {
		profile, err := a.store.GetChatModelProfile(providerName, model)
		if err == nil {
			return sanitizeChatModelProfile(profile)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("chat profile: load %s/%s: %v", providerName, model, err)
		}
	}
	return fallbackChatModelProfile(providerName, model)
}

func sanitizeChatModelProfile(profile store.ChatModelProfile) store.ChatModelProfile {
	profile.Model = provider.NormalizeModelSlug(profile.Provider, profile.Model)
	profile.ReasoningEffort = string(provider.CoerceReasoningEffortForModel(
		profile.Provider,
		profile.Model,
		provider.NormalizeReasoningEffort(profile.ReasoningEffort),
	))
	profile.ContextWindow = sanitizeContextWindowForProviderModel(profile.Provider, profile.Model, profile.ContextWindow)
	if !supportsStoredFastMode(profile.Provider, profile.Model) {
		profile.FastMode = false
	}
	return profile
}

func sanitizeContextWindowForProviderModel(providerName, model string, tokens int) int {
	options := provider.ContextWindowOptionsForModel(providerName, model)
	if len(options) == 0 {
		if tokens > 0 {
			return tokens
		}
		return provider.DefaultContextWindowForModel(providerName, model, 0)
	}
	if contextWindowSupported(options, tokens) {
		return tokens
	}
	return provider.DefaultContextWindowForModel(providerName, model, options[0].Tokens)
}

func supportsStoredFastMode(providerName, model string) bool {
	model = provider.NormalizeModelSlug(providerName, model)
	candidate, found := provider.FindModel(providerName, model)
	if !found {
		return providerName == string(provider.Codex) && strings.TrimSpace(model) != ""
	}
	return modelHasCapability(candidate, provider.ModelCapabilityFastMode)
}

func modelHasCapability(model provider.ModelInfo, capability string) bool {
	for _, existing := range model.Capabilities {
		if existing == capability {
			return true
		}
	}
	return false
}

func (a *App) ListChatBarFavorites() ([]store.ChatBarFavorite, error) {
	if a.store == nil {
		return nil, fmt.Errorf("list chat bar favorites: store unavailable")
	}
	return a.store.ListChatBarFavorites()
}

func (a *App) SetChatBarFavorite(fav store.ChatBarFavorite, starred bool) ([]store.ChatBarFavorite, error) {
	if a.store == nil {
		return nil, fmt.Errorf("set chat bar favorite: store unavailable")
	}
	var err error
	if starred {
		err = a.store.AddChatBarFavorite(fav)
	} else {
		err = a.store.RemoveChatBarFavorite(fav.Kind, fav.Provider, fav.Value)
	}
	if err != nil {
		return nil, err
	}
	return a.store.ListChatBarFavorites()
}

func (a *App) StartDiscussionByID(threadID, discussionID string) error {
	if a.store == nil || a.channels == nil {
		return fmt.Errorf("discussion services unavailable")
	}
	parent, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	if err := a.ensureDiscussionCanStart(parent); err != nil {
		return err
	}
	def, err := a.store.GetDiscussionDefByID(strings.TrimSpace(discussionID))
	if err != nil {
		return err
	}
	if err := a.ensureDiscussionDefinitionInScope(parent, def); err != nil {
		return err
	}
	return a.startDiscussionWithDefinition(parent, def)
}

func (a *App) ensureDiscussionDefinitionInScope(parent store.Thread, def store.DiscussionDefinition) error {
	if def.Scope != "project" {
		return nil
	}
	projectPath, err := a.projectPathForThread(parent)
	if err != nil {
		return err
	}
	if def.ProjectID == projectPath {
		return nil
	}
	return fmt.Errorf("discussion %q belongs to a different project", def.Name)
}

func (a *App) projectPathForThread(thread store.Thread) (string, error) {
	if strings.TrimSpace(thread.ProjectID) == "" {
		return "", fmt.Errorf("thread %s has no project", thread.ID)
	}
	project, err := a.store.GetProject(thread.ProjectID)
	if err != nil {
		return "", err
	}
	return project.Path, nil
}
