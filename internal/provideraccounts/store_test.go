package provideraccounts

import (
	"errors"
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

// Callers write the credential slot keyed by the ID they supply, then
// commit metadata. Quietly substituting the stored ID for a matching
// email would strand those bytes in an unreferenced slot and leave the
// real account on whatever stale credential it already had, so the
// mismatch has to surface instead.
func TestStoreRejectsAnIDThatContradictsASavedEmail(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{
		ID:       "saved-id",
		Provider: "claude",
		Email:    "one@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	_, err = store.UpsertAndActivate(Account{
		ID:       "fresh-id",
		Provider: "claude",
		Email:    "ONE@example.com",
	})
	if !errors.Is(err, ErrAccountIDMismatch) {
		t.Fatalf("err = %v, want ErrAccountIDMismatch", err)
	}
	accounts := store.List("claude", time.Now())
	if len(accounts) != 1 || accounts[0].ID != "saved-id" {
		t.Fatalf("rejected upsert mutated the account set: %+v", accounts)
	}

	// The same email under its own ID is an ordinary update.
	updated, err := store.UpsertAndActivate(Account{
		ID:          "saved-id",
		Provider:    "claude",
		Email:       "one@example.com",
		DisplayName: "One",
	})
	if err != nil {
		t.Fatalf("update under the saved id: %v", err)
	}
	if updated.ID != "saved-id" || updated.DisplayName != "One" {
		t.Fatalf("update = %+v", updated)
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

// The home stamp is what stops a metadata store from authorizing slot
// destruction against a provider home it never described (a scratch
// --data-dir paired with a real $HOME — the 2026-07-29 incident class).
// First claim binds and persists; a matching claim is a yes; a foreign
// claim is a no that leaves the original binding untouched.
func TestClaimProviderHomeBindsOnceAndRefusesForeignHomes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	claimed, matched, err := store.ClaimProviderHome(home)
	if err != nil || !matched || claimed != filepath.Clean(home) {
		t.Fatalf("first claim = (%q, %v, %v), want (%q, true, nil)", claimed, matched, err, home)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	claimed, matched, err = reloaded.ClaimProviderHome(home)
	if err != nil || !matched || claimed != filepath.Clean(home) {
		t.Fatalf("matching claim after reload = (%q, %v, %v), want (%q, true, nil)", claimed, matched, err, home)
	}

	foreign := filepath.Join(dir, "other-home")
	claimed, matched, err = reloaded.ClaimProviderHome(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("foreign home claim matched; the prune gate would act on someone else's slots")
	}
	if claimed != filepath.Clean(home) {
		t.Fatalf("foreign claim rebound the store to %q, want original %q kept", claimed, home)
	}

	if _, _, err := reloaded.ClaimProviderHome("  "); err == nil {
		t.Fatal("blank home claim succeeded, want error")
	}
}

// The session deadline has exactly one author: the caller that has read the
// credential. Identity writers carry no expiry at all, so a write that is
// silent about it must leave it alone rather than erase the countdown.
func TestRefreshTokenExpirySurvivesIdentityWrites(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "one", Provider: "claude", Email: "one@example.com"}
	if _, err := store.UpsertAndActivate(account); err != nil {
		t.Fatal(err)
	}

	const deadline int64 = 1_802_592_000_000
	noted, changed, err := store.NoteRefreshTokenExpiry("claude", "one", deadline)
	if err != nil || !changed || noted.RefreshTokenExpiresAt != deadline {
		t.Fatalf("NoteRefreshTokenExpiry = (%+v, %v, %v), want the deadline recorded", noted, changed, err)
	}
	if _, changed, err := store.NoteRefreshTokenExpiry("claude", "one", deadline); err != nil || changed {
		t.Fatalf("re-noting the same deadline reported changed=%v (%v), want no write", changed, err)
	}

	// A re-adoption and an identity enrichment both rewrite the row from a
	// provider identity response, which never carries an expiry.
	if _, err := store.UpsertAndActivate(account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateMetadata(Account{
		ID: "one", Provider: "claude", Email: "one@example.com", OrgID: "org-1",
	}); err != nil {
		t.Fatal(err)
	}
	saved, ok := store.Get("claude", "one", time.Now())
	if !ok || saved.RefreshTokenExpiresAt != deadline {
		t.Fatalf("saved account = %+v, want the deadline preserved across identity writes", saved)
	}

	// The credential-reading author, and only it, may clear the field: a
	// credential that names no deadline is one whose countdown must stop being
	// shown.
	if _, changed, err := store.NoteRefreshTokenExpiry("claude", "one", 0); err != nil || !changed {
		t.Fatalf("clearing the deadline reported changed=%v (%v), want the clear to land", changed, err)
	}
	if saved, _ := store.Get("claude", "one", time.Now()); saved.RefreshTokenExpiresAt != 0 {
		t.Fatalf("saved account expiry = %d, want it cleared", saved.RefreshTokenExpiresAt)
	}

	// A row that has gone (removed under a credential transaction) is not an
	// error: there is simply nothing to record.
	if _, changed, err := store.NoteRefreshTokenExpiry("claude", "missing", deadline); err != nil || changed {
		t.Fatalf("NoteRefreshTokenExpiry(missing) = (%v, %v), want a quiet no-op", changed, err)
	}
}

// The deadline is persisted metadata, so it has to survive the file round trip
// that a restart performs.
func TestRefreshTokenExpiryPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "one", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	const deadline int64 = 1_802_592_000_000
	if _, _, err := store.NoteRefreshTokenExpiry("claude", "one", deadline); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.Get("claude", "one", time.Now())
	if !ok || saved.RefreshTokenExpiresAt != deadline {
		t.Fatalf("reloaded account = %+v, want the deadline to have persisted", saved)
	}
}
