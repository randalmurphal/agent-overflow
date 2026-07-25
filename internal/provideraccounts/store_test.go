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

func TestStoreAdvancesGenerationWhenActiveCredentialChanges(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "one", Provider: "codex", Email: "one@example.com"}
	if _, err := store.UpsertAndActivate(account); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("codex")

	if _, err := store.UpsertAndActivateCredential(account); err != nil {
		t.Fatal(err)
	}
	if got := store.Generation("codex"); got != generation+1 {
		t.Fatalf("credential generation = %d, want %d", got, generation+1)
	}
}

func TestAdvanceActiveCredentialRequiresSelectedAccount(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("codex")

	account, err := store.AdvanceActiveCredential("codex", "one")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "one" {
		t.Fatalf("advanced account = %q, want one", account.ID)
	}
	if got := store.Generation("codex"); got != generation+1 {
		t.Fatalf("credential generation = %d, want %d", got, generation+1)
	}
	if _, err := store.AdvanceActiveCredential("codex", "other"); err == nil {
		t.Fatal("AdvanceActiveCredential() accepted an inactive account")
	}
	if got := store.Generation("codex"); got != generation+1 {
		t.Fatalf("failed advance changed generation to %d", got)
	}
}

func TestStoreRemoveActiveSelectsReplacement(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("claude")

	if err := store.Remove("claude", "two", "one"); err != nil {
		t.Fatal(err)
	}
	active, ok := store.Active("claude", time.Now())
	if !ok || active.ID != "one" {
		t.Fatalf("active account = %+v, ok=%v, want one", active, ok)
	}
	if got := store.Generation("claude"); got != generation+1 {
		t.Fatalf("generation = %d, want %d", got, generation+1)
	}
	if accounts := store.List("claude", time.Now()); len(accounts) != 1 || accounts[0].ID != "one" {
		t.Fatalf("accounts = %+v, want only one", accounts)
	}
}

func TestStoreRemoveFinalAccountClearsSelection(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("codex")

	if err := store.Remove("codex", "one", ""); err != nil {
		t.Fatal(err)
	}
	if active, ok := store.Active("codex", time.Now()); ok {
		t.Fatalf("active account = %+v, want none", active)
	}
	if got := store.Generation("codex"); got != generation+1 {
		t.Fatalf("generation = %d, want %d", got, generation+1)
	}
	if accounts := store.List("codex", time.Now()); len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestStoreRemoveInactiveLeavesSelectionGeneration(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("codex")

	if err := store.Remove("codex", "one", ""); err != nil {
		t.Fatal(err)
	}
	active, ok := store.Active("codex", time.Now())
	if !ok || active.ID != "two" {
		t.Fatalf("active account = %+v, ok=%v, want two", active, ok)
	}
	if got := store.Generation("codex"); got != generation {
		t.Fatalf("generation = %d, want unchanged %d", got, generation)
	}
}

func TestStoreRemoveRequiresValidReplacementForActiveAccount(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}

	if err := store.Remove("claude", "two", ""); err == nil {
		t.Fatal("Remove() error = nil, want missing replacement error")
	}
	active, ok := store.Active("claude", time.Now())
	if !ok || active.ID != "two" {
		t.Fatalf("active account changed after rejected removal: %+v, ok=%v", active, ok)
	}
}

func TestStoreRemoveRollsBackWhenMetadataSaveFails(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("claude")
	store.path = t.TempDir()

	if err := store.Remove("claude", "two", "one"); err == nil {
		t.Fatal("Remove() error = nil, want persistence failure")
	}
	active, ok := store.Active("claude", time.Now())
	if !ok || active.ID != "two" {
		t.Fatalf("active account after failed save = %+v, ok=%v, want two", active, ok)
	}
	if got := store.Generation("claude"); got != generation {
		t.Fatalf("generation after failed save = %d, want %d", got, generation)
	}
	if accounts := store.List("claude", time.Now()); len(accounts) != 2 {
		t.Fatalf("accounts after failed save = %+v, want both accounts", accounts)
	}
}

func TestUpdateMetadataPreservesSelectionGenerationAndUsage(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.UpsertAndActivate(Account{
		ID:       "one",
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{
		ID:       "two",
		Provider: "codex",
		Email:    "two@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate("codex", first.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := provider.RateLimitsSnapshot{
		Provider:  "codex",
		AccountID: first.ID,
		Limits: []provider.RateLimitEntry{{
			LimitID: "codex", UsedPercent: 25, WindowMins: 300,
		}},
	}
	if err := store.RememberRateLimits("codex", first.ID, snapshot); err != nil {
		t.Fatal(err)
	}
	generation := store.Generation("codex")

	updated, err := store.UpdateMetadata(Account{
		ID:               first.ID,
		Provider:         "codex",
		Email:            "one@example.com",
		SubscriptionType: "pro",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, ok := store.Active("codex", time.Now())
	if !ok || active.ID != first.ID {
		t.Fatalf("active account = %+v, ok=%v", active, ok)
	}
	if store.Generation("codex") != generation {
		t.Fatalf("generation changed from %d to %d", generation, store.Generation("codex"))
	}
	if updated.RateLimits == nil || updated.RateLimits.Limits[0].UsedPercent != 25 {
		t.Fatalf("updated rate limits = %+v", updated.RateLimits)
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
