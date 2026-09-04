package store

import (
	"database/sql"
	"fmt"
	"strings"
)

const maxChatBarFavorites = 30

func normalizeChatBarFavorite(fav ChatBarFavorite) (ChatBarFavorite, error) {
	fav.Kind = strings.TrimSpace(fav.Kind)
	fav.Provider = strings.TrimSpace(fav.Provider)
	fav.Value = strings.TrimSpace(fav.Value)
	fav.Label = strings.TrimSpace(fav.Label)
	if fav.Value == "" {
		return ChatBarFavorite{}, fmt.Errorf("store: chat bar favorite value cannot be empty")
	}
	if fav.Label == "" {
		fav.Label = fav.Value
	}
	switch fav.Kind {
	case "model":
		if _, ok := legalProviders[fav.Provider]; !ok {
			return ChatBarFavorite{}, fmt.Errorf("%w: %q", ErrInvalidProvider, fav.Provider)
		}
	case "discussion":
		fav.Provider = ""
	default:
		return ChatBarFavorite{}, fmt.Errorf("store: invalid chat bar favorite kind %q", fav.Kind)
	}
	return fav, nil
}

// ListChatBarFavorites returns starred composer-menu entries newest first.
func (s *Store) ListChatBarFavorites() ([]ChatBarFavorite, error) {
	rows, err := s.reader().Query(
		`SELECT kind, provider, value, label, created_at
		   FROM chat_bar_favorites
		  ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list chat bar favorites: %w", err)
	}
	defer rows.Close()

	var favorites []ChatBarFavorite
	for rows.Next() {
		var fav ChatBarFavorite
		if err := rows.Scan(&fav.Kind, &fav.Provider, &fav.Value, &fav.Label, &fav.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan chat bar favorite: %w", err)
		}
		favorites = append(favorites, fav)
	}
	return favorites, rows.Err()
}

// AddChatBarFavorite stars a model or discussion. Re-adding an existing
// favorite refreshes its label but preserves the original created_at so
// accidental double-clicks do not reshuffle the list.
func (s *Store) AddChatBarFavorite(fav ChatBarFavorite) error {
	normalized, err := normalizeChatBarFavorite(fav)
	if err != nil {
		return err
	}
	if normalized.CreatedAt == 0 {
		normalized.CreatedAt = nowMillis()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin add chat bar favorite: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(kind, provider, value) DO UPDATE SET label = excluded.label`,
		normalized.Kind, normalized.Provider, normalized.Value, normalized.Label, normalized.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: add chat bar favorite %s/%s/%s: %w", normalized.Kind, normalized.Provider, normalized.Value, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM chat_bar_favorites
		  WHERE (kind, provider, value) IN (
		  	SELECT kind, provider, value
		  	  FROM chat_bar_favorites
		  	 ORDER BY created_at DESC
		  	 LIMIT -1 OFFSET ?
		  )`,
		maxChatBarFavorites,
	); err != nil {
		return fmt.Errorf("store: trim chat bar favorites: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit add chat bar favorite: %w", err)
	}
	return nil
}

// RemoveChatBarFavorite unstars a model or discussion. Missing rows are a
// no-op so stale UI state can reconcile without surfacing an error.
func (s *Store) RemoveChatBarFavorite(kind, provider, value string) error {
	fav, err := normalizeChatBarFavorite(ChatBarFavorite{
		Kind:     kind,
		Provider: provider,
		Value:    value,
		Label:    value,
	})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`DELETE FROM chat_bar_favorites WHERE kind = ? AND provider = ? AND value = ?`,
		fav.Kind, fav.Provider, fav.Value,
	)
	if err != nil {
		return fmt.Errorf("store: remove chat bar favorite %s/%s/%s: %w", fav.Kind, fav.Provider, fav.Value, err)
	}
	return nil
}

func normalizeChatModelProfile(profile ChatModelProfile) (ChatModelProfile, error) {
	profile.Provider = strings.TrimSpace(profile.Provider)
	if _, ok := legalProviders[profile.Provider]; !ok {
		return ChatModelProfile{}, fmt.Errorf("%w: %q", ErrInvalidProvider, profile.Provider)
	}
	profile.Model = strings.TrimSpace(profile.Model)
	if profile.Model == "" {
		return ChatModelProfile{}, fmt.Errorf("store: chat model profile model cannot be empty")
	}
	profile.ReasoningEffort = normalizeEffort(strings.TrimSpace(profile.ReasoningEffort))
	if _, ok := legalEfforts[profile.ReasoningEffort]; !ok {
		return ChatModelProfile{}, fmt.Errorf("%w: %q", ErrInvalidEffort, profile.ReasoningEffort)
	}
	if !legalEffortForProvider(profile.Provider, profile.ReasoningEffort) {
		return ChatModelProfile{}, fmt.Errorf("%w: %s/%s", ErrInvalidEffort, profile.Provider, profile.ReasoningEffort)
	}
	if profile.ContextWindow == 0 {
		profile.ContextWindow = 1000000
	}
	if !validContextWindow(profile.ContextWindow) {
		return ChatModelProfile{}, fmt.Errorf("%w: %d", ErrInvalidContextWindow, profile.ContextWindow)
	}
	if !validAutoCompactPercent(profile.AutoCompactStandardPercent) {
		return ChatModelProfile{}, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, profile.AutoCompactStandardPercent)
	}
	if !validAutoCompactPercent(profile.AutoCompactExtendedPercent) {
		return ChatModelProfile{}, fmt.Errorf("%w: %d", ErrInvalidAutoCompactPercent, profile.AutoCompactExtendedPercent)
	}
	profile.RuntimeMode = normalizeRuntimeMode(strings.TrimSpace(profile.RuntimeMode))
	if profile.UpdatedAt == 0 {
		profile.UpdatedAt = nowMillis()
	}
	return profile, nil
}

// UpsertChatModelProfile remembers the current chat-bar settings for a
// provider/model pair. The newest profile is the seed for new draft threads.
func (s *Store) UpsertChatModelProfile(profile ChatModelProfile) error {
	normalized, err := normalizeChatModelProfile(profile)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window,
			auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, model) DO UPDATE SET
			reasoning_effort = excluded.reasoning_effort,
			fast_mode = excluded.fast_mode,
			context_window = excluded.context_window,
			auto_compact_standard_percent = excluded.auto_compact_standard_percent,
			auto_compact_extended_percent = excluded.auto_compact_extended_percent,
			runtime_mode = excluded.runtime_mode,
			updated_at = excluded.updated_at`,
		normalized.Provider, normalized.Model, normalized.ReasoningEffort, boolToInt(normalized.FastMode),
		normalized.ContextWindow, normalized.AutoCompactStandardPercent, normalized.AutoCompactExtendedPercent,
		normalized.RuntimeMode, normalized.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert chat model profile %s/%s: %w", normalized.Provider, normalized.Model, err)
	}
	return nil
}

// GetChatModelProfile returns a remembered provider/model profile.
func (s *Store) GetChatModelProfile(providerName, model string) (ChatModelProfile, error) {
	row := s.reader().QueryRow(
		`SELECT provider, model, reasoning_effort, fast_mode, context_window,
		        auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode, updated_at
		   FROM chat_model_profiles
		  WHERE provider = ? AND model = ?`,
		strings.TrimSpace(providerName), strings.TrimSpace(model),
	)
	return scanChatModelProfile(row)
}

// LatestChatModelProfile returns the most recently observed chat profile.
func (s *Store) LatestChatModelProfile() (ChatModelProfile, error) {
	row := s.reader().QueryRow(
		`SELECT provider, model, reasoning_effort, fast_mode, context_window,
		        auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode, updated_at
		   FROM chat_model_profiles
		  ORDER BY updated_at DESC
		  LIMIT 1`,
	)
	return scanChatModelProfile(row)
}

// LatestChatModelProfileForProvider returns the newest profile for one provider.
func (s *Store) LatestChatModelProfileForProvider(providerName string) (ChatModelProfile, error) {
	row := s.reader().QueryRow(
		`SELECT provider, model, reasoning_effort, fast_mode, context_window,
		        auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode, updated_at
		   FROM chat_model_profiles
		  WHERE provider = ?
		  ORDER BY updated_at DESC
		  LIMIT 1`,
		strings.TrimSpace(providerName),
	)
	return scanChatModelProfile(row)
}

// ClearChatModelProfiles removes every remembered chat-bar profile. The
// newest surviving row is the app-wide seed for the next "+ New" draft
// (LatestChatModelProfile), so this is how the harness reset stops one
// test's last-used provider from leaking into the next test's draft
// default. Production never calls it: a user's remembered profiles are
// theirs to keep.
func (s *Store) ClearChatModelProfiles() error {
	if _, err := s.db.Exec(`DELETE FROM chat_model_profiles`); err != nil {
		return fmt.Errorf("store: clear chat model profiles: %w", err)
	}
	return nil
}

func scanChatModelProfile(scanner interface{ Scan(...any) error }) (ChatModelProfile, error) {
	var profile ChatModelProfile
	var fastMode int
	if err := scanner.Scan(
		&profile.Provider,
		&profile.Model,
		&profile.ReasoningEffort,
		&fastMode,
		&profile.ContextWindow,
		&profile.AutoCompactStandardPercent,
		&profile.AutoCompactExtendedPercent,
		&profile.RuntimeMode,
		&profile.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return ChatModelProfile{}, err
		}
		return ChatModelProfile{}, fmt.Errorf("store: scan chat model profile: %w", err)
	}
	profile.FastMode = fastMode != 0
	return profile, nil
}
