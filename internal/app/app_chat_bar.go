package app

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
	"agent-overflow/internal/usermessage"
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
	return a.discussionService().StartByID(threadID, strings.TrimSpace(discussionID))
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

// Composer slash commands (spec §5.2, D31). A composer message containing the
// word `/name` — at any word position — is expanded at SEND TIME, on this side
// of the wire:
//
//	provider payload = <exactly what the user typed> + "\n\n" + <command block>
//	stored user row  = <exactly what the user typed>, meta.command = "name"
//
// Typed text first because it is the instruction; the block second because it
// is context. Nothing is inserted into the composer, and nothing is added to
// the session's system prompt — a per-thread system prompt would invalidate
// provider prompt caching for every turn to serve one message that asked for
// it.
//
// The table below is the ONE Go-side registry. Its frontend twin (the menu and
// the composer's accent colouring) is
// `frontend/src/lib/components/composer/slashCommands.ts`; the two are
// deliberately parallel rather than generated, and this one is authoritative —
// a word the frontend colours but this table does not know expands to nothing
// and is marked as nothing.

// composerCommand is one registered command. Adding a command is one entry:
// the name the user types (without the slash) and the resolver that produces
// the block appended to the provider payload.
type composerCommand struct {
	name string
	// expand resolves the block for one thread. An error fails the send
	// loudly — a command the user invoked that silently sent nothing extra
	// is worse than a refused send, because nothing on screen would say the
	// context never arrived.
	expand func(a *App, threadID string) (string, error)
}

func composerCommands() []composerCommand {
	return []composerCommand{{name: "workflow", expand: (*App).workflowComposerBlock}}
}

// lookupComposerCommand finds a registered command by name.
func lookupComposerCommand(name string) (composerCommand, bool) {
	for _, command := range composerCommands() {
		if command.name == name {
			return command, true
		}
	}
	return composerCommand{}, false
}

// firstRegisteredComposerCommand returns the first command-shaped word in
// content that this table claims.
//
// Words the table does not claim are skipped rather than ending the search: a
// message can mention `/tmp` and still invoke `/workflow` in the same sentence,
// and an unknown `/foo` is ordinary text, not an error.
func firstRegisteredComposerCommand(content string) (composerCommand, bool) {
	for _, name := range usermessage.CommandWords(content) {
		if command, ok := lookupComposerCommand(name); ok {
			return command, true
		}
	}
	return composerCommand{}, false
}

// expandComposerCommand resolves a composer command invoked anywhere in content.
//
// Returns the provider-bound payload and the recognised command name. Content
// that names no registered command comes back unchanged with an empty name.
//
// A command named more than once expands ONCE — the block is context, and the
// same context twice is only cost. Which occurrence "won" is not a question
// the payload can answer anyway: the block is appended after the whole typed
// text either way.
//
// A registered command whose block resolves EMPTY still reports its name: the
// message was a command invocation and the row must say so, even when the
// project had nothing to add.
func (a *App) expandComposerCommand(threadID, content string) (string, string, error) {
	command, ok := firstRegisteredComposerCommand(content)
	if !ok {
		return content, "", nil
	}
	block, err := command.expand(a, threadID)
	if err != nil {
		return "", "", fmt.Errorf("expand /%s: %w", command.name, err)
	}
	block = strings.TrimSpace(block)
	if block == "" {
		return content, command.name, nil
	}
	return strings.TrimRight(content, " \t\r\n") + "\n\n" + block, command.name, nil
}
