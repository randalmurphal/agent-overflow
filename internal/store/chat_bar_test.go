package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestChatBarFavoritesRoundTripAndDedupe(t *testing.T) {
	s := newTestStore(t)

	if err := s.AddChatBarFavorite(ChatBarFavorite{
		Kind:      "model",
		Provider:  "claude",
		Value:     "claude-opus-4-7",
		Label:     "Claude Opus 4.7",
		CreatedAt: 100,
	}); err != nil {
		t.Fatalf("AddChatBarFavorite(model): %v", err)
	}
	if err := s.AddChatBarFavorite(ChatBarFavorite{
		Kind:      "discussion",
		Value:     "discussion-1",
		Label:     "Architects",
		CreatedAt: 200,
	}); err != nil {
		t.Fatalf("AddChatBarFavorite(discussion): %v", err)
	}
	if err := s.AddChatBarFavorite(ChatBarFavorite{
		Kind:      "model",
		Provider:  "claude",
		Value:     "claude-opus-4-7",
		Label:     "Opus",
		CreatedAt: 300,
	}); err != nil {
		t.Fatalf("AddChatBarFavorite(model duplicate): %v", err)
	}

	favorites, err := s.ListChatBarFavorites()
	if err != nil {
		t.Fatalf("ListChatBarFavorites: %v", err)
	}
	if len(favorites) != 2 {
		t.Fatalf("favorites len = %d, want 2: %#v", len(favorites), favorites)
	}
	if favorites[0].Kind != "discussion" || favorites[0].Value != "discussion-1" {
		t.Fatalf("first favorite = %#v, want newest discussion", favorites[0])
	}
	if favorites[1].Label != "Opus" {
		t.Fatalf("duplicate did not refresh label: %#v", favorites[1])
	}
	if favorites[1].CreatedAt != 100 {
		t.Fatalf("duplicate changed createdAt = %d, want 100", favorites[1].CreatedAt)
	}

	if err := s.RemoveChatBarFavorite("model", "claude", "claude-opus-4-7"); err != nil {
		t.Fatalf("RemoveChatBarFavorite: %v", err)
	}
	favorites, err = s.ListChatBarFavorites()
	if err != nil {
		t.Fatalf("ListChatBarFavorites after remove: %v", err)
	}
	if len(favorites) != 1 || favorites[0].Kind != "discussion" {
		t.Fatalf("favorites after remove = %#v, want only discussion", favorites)
	}
}

func TestChatBarFavoritesValidationAndCap(t *testing.T) {
	s := newTestStore(t)

	if err := s.AddChatBarFavorite(ChatBarFavorite{
		Kind:  "model",
		Value: "missing-provider",
	}); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("AddChatBarFavorite missing provider error = %v, want ErrInvalidProvider", err)
	}
	if err := s.AddChatBarFavorite(ChatBarFavorite{
		Kind:     "discussion",
		Provider: "claude",
		Value:    "discussion-1",
	}); err != nil {
		t.Fatalf("AddChatBarFavorite discussion with provider should normalize: %v", err)
	}

	for i := 0; i < maxChatBarFavorites+5; i++ {
		if err := s.AddChatBarFavorite(ChatBarFavorite{
			Kind:      "model",
			Provider:  "codex",
			Value:     fmt.Sprintf("gpt-%02d", i),
			Label:     fmt.Sprintf("GPT %02d", i),
			CreatedAt: int64(1000 + i),
		}); err != nil {
			t.Fatalf("AddChatBarFavorite cap row %d: %v", i, err)
		}
	}
	favorites, err := s.ListChatBarFavorites()
	if err != nil {
		t.Fatalf("ListChatBarFavorites: %v", err)
	}
	if len(favorites) != maxChatBarFavorites {
		t.Fatalf("favorites len = %d, want cap %d", len(favorites), maxChatBarFavorites)
	}
	for _, fav := range favorites {
		if fav.Value == "gpt-00" {
			t.Fatalf("oldest favorite survived cap trim: %#v", favorites)
		}
	}
}

func TestChatModelProfileLatestAndProviderLookup(t *testing.T) {
	s := newTestStore(t)

	profiles := []ChatModelProfile{
		{
			Provider:        "claude",
			Model:           "claude-sonnet-4-6",
			ReasoningEffort: "medium",
			FastMode:        true,
			ContextWindow:   200000,
			RuntimeMode:     "approval-required",
			UpdatedAt:       100,
		},
		{
			Provider:        "codex",
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			FastMode:        false,
			ContextWindow:   1000000,
			RuntimeMode:     "full-access",
			UpdatedAt:       200,
		},
	}
	for _, profile := range profiles {
		if err := s.UpsertChatModelProfile(profile); err != nil {
			t.Fatalf("UpsertChatModelProfile(%s/%s): %v", profile.Provider, profile.Model, err)
		}
	}

	latest, err := s.LatestChatModelProfile()
	if err != nil {
		t.Fatalf("LatestChatModelProfile: %v", err)
	}
	if latest.Provider != "codex" || latest.Model != "gpt-5.5" {
		t.Fatalf("latest = %#v, want codex/gpt-5.5", latest)
	}

	claude, err := s.LatestChatModelProfileForProvider("claude")
	if err != nil {
		t.Fatalf("LatestChatModelProfileForProvider: %v", err)
	}
	if claude.Model != "claude-sonnet-4-6" || !claude.FastMode || claude.RuntimeMode != "approval-required" {
		t.Fatalf("claude profile = %#v", claude)
	}

	loaded, err := s.GetChatModelProfile("claude", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("GetChatModelProfile: %v", err)
	}
	if loaded.ContextWindow != 200000 || loaded.ReasoningEffort != "medium" {
		t.Fatalf("loaded profile = %#v", loaded)
	}
}

func TestChatModelProfileValidation(t *testing.T) {
	s := newTestStore(t)

	err := s.UpsertChatModelProfile(ChatModelProfile{
		Provider:      "claude",
		Model:         "opus",
		ContextWindow: -1,
	})
	if !errors.Is(err, ErrInvalidContextWindow) {
		t.Fatalf("invalid context error = %v, want ErrInvalidContextWindow", err)
	}

	err = s.UpsertChatModelProfile(ChatModelProfile{
		Provider:                   "claude",
		Model:                      "opus",
		ContextWindow:              1000000,
		AutoCompactExtendedPercent: 91,
	})
	if !errors.Is(err, ErrInvalidAutoCompactPercent) {
		t.Fatalf("invalid auto-compact error = %v, want ErrInvalidAutoCompactPercent", err)
	}

	_, err = s.LatestChatModelProfile()
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("LatestChatModelProfile empty error = %v, want sql.ErrNoRows", err)
	}
}
