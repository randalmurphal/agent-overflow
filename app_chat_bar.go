package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// rememberChatModelProfile persists the thread's chat-model setup as
// the "last used" profile so the chat bar can rehydrate without
// rebuilding from scratch. No-ops on discussion threads (the
// deliberation runtime picks its own provider/model per participant)
// or workflow-saga threads and when the thread carries no usable provider/model pair.
func (a *App) rememberChatModelProfile(thread store.Thread) {
	if a.store == nil || threadmode.IsSagaOwned(thread.Mode) {
		return
	}
	if strings.TrimSpace(thread.Provider) == "" || strings.TrimSpace(thread.Model) == "" {
		return
	}
	profile := chatmodel.ProfileFromThread(thread)
	if latest, err := a.store.LatestChatModelProfile(); err == nil {
		if chatmodel.SameProfile(latest, profile) {
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("chat profile: load latest before remember: %v", err)
	}
	if err := a.store.UpsertChatModelProfile(profile); err != nil {
		log.Printf("chat profile: remember %s/%s for thread %s: %v", thread.Provider, thread.Model, thread.ID, err)
	}
}

// seedChatModelProfile picks the best stored chat-model profile for the
// given (provider, model) inputs and falls back to the registry default
// when nothing is remembered.
//
// Resolution order:
//   - both blank → most recent profile across providers, else fallback
//   - provider only → most recent profile for that provider, else fallback
//   - model only → infer provider, then look up the (provider, model) row
//   - both set → look up the (provider, model) row, else fallback
//
// When the provider has to be inferred (both-blank or model-only cases),
// the choice is informed by which provider binaries resolve on PATH so a
// Codex-only environment doesn't seed a Claude default that won't work.
func (a *App) seedChatModelProfile(providerName, model string) store.ChatModelProfile {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)

	available := a.availableTextGenerationProviders()

	switch {
	case providerName == "" && model == "":
		if a.store != nil {
			profile, err := a.store.LatestChatModelProfile()
			if err == nil {
				return a.visibleSeedProfile(chatmodel.SanitizeProfile(profile))
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("chat profile: load latest: %v", err)
			}
		}
		return a.visibleSeedProfile(chatmodel.FallbackProfile("", "", available...))
	case providerName != "" && model == "":
		if a.store != nil {
			profile, err := a.store.LatestChatModelProfileForProvider(providerName)
			if err == nil {
				return a.visibleSeedProfile(chatmodel.SanitizeProfile(profile))
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.Printf("chat profile: load latest for provider %s: %v", providerName, err)
			}
		}
		// providerName is explicit here, so FallbackProvider is never
		// consulted — omit the availability arg.
		return a.visibleSeedProfile(chatmodel.FallbackProfile(providerName, ""))
	case providerName == "" && model != "":
		providerName = chatmodel.FallbackProvider(available...)
	}
	model = provider.NormalizeModelSlug(providerName, model)

	if a.store != nil {
		profile, err := a.store.GetChatModelProfile(providerName, model)
		if err == nil {
			return chatmodel.SanitizeProfile(profile)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("chat profile: load %s/%s: %v", providerName, model, err)
		}
	}
	return chatmodel.FallbackProfile(providerName, model)
}

// visibleSeedProfile guards the "seed the composer from history" paths
// against hidden models: when the remembered profile's model is hidden
// from pickers, seed the provider's first visible catalog model with
// fresh defaults instead. Explicitly requested models bypass this —
// hiding is a picker-display preference, not a hard ban, and existing
// threads keep whatever model they carry. Settings are snapshotted
// once so the hidden check and the visible scan agree.
func (a *App) visibleSeedProfile(profile store.ChatModelProfile) store.ChatModelProfile {
	hidden := a.currentSettings().HiddenModelsForProvider(profile.Provider)
	if !slices.Contains(hidden, profile.Model) {
		return profile
	}
	return chatmodel.FallbackProfile(profile.Provider, firstVisibleModel(profile.Provider, hidden))
}

// firstVisibleModel returns the first static-catalog model not in the
// hidden list, falling back to the catalog head when everything is
// hidden — the settings UI prevents that state, but a hand-mangled
// file must not strand the composer without a model.
//
// Codex caveat, accepted: pickers and the hide-list operate on the
// live app-server catalog, but this seed-path scan deliberately reads
// the static registry — spawning the codex binary just to seed a
// draft would be worse than occasionally seeding a slug the live
// catalog has since dropped (the composer surfaces that immediately
// and the user re-picks).
func firstVisibleModel(providerName string, hidden []string) string {
	for _, model := range provider.ModelsForProvider(providerName) {
		if !slices.Contains(hidden, model.Slug) {
			return model.Slug
		}
	}
	return chatmodel.FallbackModelForProvider(providerName)
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
