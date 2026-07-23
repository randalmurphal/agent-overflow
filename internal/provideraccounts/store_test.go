package provideraccounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestStorePersistsMetadataAndAccountScopedLimits(t *testing.T) {
	configDir := t.TempDir()
	store, err := NewStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.UpsertAndActivate(Account{
		ID:       "account-one",
		Provider: "codex",
		Email:    "person@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := provider.RateLimitsSnapshot{
		Provider:  "codex",
		AccountID: account.ID,
		Limits: []provider.RateLimitEntry{{
			LimitID:     "spark",
			LimitName:   "Spark",
			UsedPercent: 46,
			WindowMins:  300,
			ResetsAt:    time.Now().Add(time.Hour).Unix(),
		}},
	}
	if err := store.RememberRateLimits("codex", account.ID, snapshot); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Active("codex", time.Now())
	if !ok || got.Email != account.Email || got.RateLimits == nil {
		t.Fatalf("Active() = %+v, %v", got, ok)
	}
	if got.RateLimits.AccountID != account.ID ||
		len(got.RateLimits.Limits) != 1 ||
		got.RateLimits.Limits[0].LimitID != "spark" {
		t.Fatalf("persisted limits = %+v", got.RateLimits)
	}
	raw, err := os.ReadFile(filepath.Join(configDir, stateFilename))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"accessToken", "refreshToken", "secret-token"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("metadata contains credential marker %q", forbidden)
		}
	}
}

func TestStoreAdvancesGenerationOnlyWhenSelectionChanges(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	firstGeneration := store.Generation("claude")
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got := store.Generation("claude"); got != firstGeneration {
		t.Fatalf("same account generation = %d, want %d", got, firstGeneration)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got := store.Generation("claude"); got != firstGeneration+1 {
		t.Fatalf("switched generation = %d, want %d", got, firstGeneration+1)
	}
}

func TestStoreResetsExpiredLastKnownLimitsWithoutMutatingPersistedReading(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Unix(2_000_000, 0)
	account, err := store.UpsertAndActivate(Account{
		ID:       "one",
		Provider: "claude",
		RateLimits: &provider.RateLimitsSnapshot{
			Limits: []provider.RateLimitEntry{{
				LimitID:     "session",
				UsedPercent: 88,
				WindowMins:  300,
				ResetsAt:    reset.Unix(),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.Active("claude", reset.Add(time.Second))
	if !ok {
		t.Fatal("active account missing")
	}
	if got.RateLimits.Limits[0].UsedPercent != 0 {
		t.Fatalf("expired UsedPercent = %v, want 0", got.RateLimits.Limits[0].UsedPercent)
	}
	if account.RateLimits.Limits[0].UsedPercent != 88 {
		t.Fatal("read normalization mutated the caller's snapshot")
	}
}
