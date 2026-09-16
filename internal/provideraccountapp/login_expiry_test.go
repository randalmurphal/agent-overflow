package provideraccountapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

// claudeLogin builds credential bytes whose OAuth session ends at sessionEnd.
// The access-token expiry is always live, so nothing in these fixtures can
// pass or fail for the wrong reason: the only thing under test is the
// refresh token's own deadline.
func claudeLogin(token string, sessionEnd time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"refresh-%s","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		token,
		token,
		time.Now().Add(8*time.Hour).UnixMilli(),
		sessionEnd.UnixMilli(),
	))
}

// seedClaudeAccounts registers one active account holding the canonical
// credential and one saved account holding its own slot credential, with the
// canonical fingerprint remembered the way a real activation leaves it — so
// the reconciliation every account mutation starts with has nothing to probe.
func seedClaudeAccounts(
	t *testing.T,
	manager *Manager,
	store *provideraccounts.Store,
	credentials *provideraccounts.Credentials,
	canonical []byte,
	savedID string,
	saved []byte,
) {
	t.Helper()
	claudeName := string(provider.Claude)
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: savedID, Provider: claudeName, Email: savedID + "@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(claudeName, savedID, saved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "current", Provider: claudeName, Email: "current@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(claudeName, "current", canonical); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteNativeCredentialForTest(claudeName, canonical); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.rememberProviderCredentialFingerprintLocked(claudeName, canonical)
	manager.mu.Unlock()
}

// Activating an expired login is what kills it: the CLI's first act is a token
// refresh, and past the refresh token's deadline that refresh answers
// invalid_grant and blanks the credential. So the switch declines, and the
// answer carries the one recovery rather than an error the client would have
// to interpret.
func TestSwitchDeclinesAnExpiredLoginAndAsksForSignIn(t *testing.T) {
	manager, store, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	expiredAt := time.Now().Add(-48 * time.Hour)
	live := claudeLogin("live", time.Now().Add(20*24*time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, live, "expired", claudeLogin("dead", expiredAt))

	result, err := manager.SwitchProviderAccount(claudeName, "expired")
	if err != nil {
		t.Fatalf("SwitchProviderAccount(expired) = %v, want the declined answer rather than an error", err)
	}
	if !result.SignInRequired {
		t.Fatal("declined switch did not ask the client to sign this account in")
	}
	if !result.NeedsLogin || result.Active {
		t.Fatalf("declined switch reported needsLogin=%v active=%v, want true/false", result.NeedsLogin, result.Active)
	}
	if result.RefreshTokenExpiresAt != expiredAt.UnixMilli() {
		t.Fatalf(
			"declined switch reported expiry %d, want the credential's own %d",
			result.RefreshTokenExpiresAt,
			expiredAt.UnixMilli(),
		)
	}

	// Nothing moved: the working account is still selected and still holds the
	// canonical store.
	active, ok := store.Active(claudeName, time.Now())
	if !ok || active.ID != "current" {
		t.Fatalf("active account = %+v, want the switch to have changed nothing", active)
	}
	canonical, err := credentials.ReadCredential(claudeName, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(live) {
		t.Fatalf("canonical credential = %s, want the working login untouched", canonical)
	}
	// The deadline is durable, so the card still reads "expired" after a
	// restart, before anything re-reads the slot.
	saved, ok := store.Get(claudeName, "expired", time.Now())
	if !ok || saved.RefreshTokenExpiresAt != expiredAt.UnixMilli() {
		t.Fatalf("saved metadata = %+v, want the recorded session deadline", saved)
	}
}

// A switch that goes through commits credential bytes for both accounts, so
// both deadlines are recorded from the bytes in hand.
func TestSwitchRecordsBothAccountsLoginDeadlines(t *testing.T) {
	manager, store, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	currentEnd := time.Now().Add(5 * 24 * time.Hour)
	targetEnd := time.Now().Add(26 * 24 * time.Hour)
	seedClaudeAccounts(
		t, manager, store, credentials,
		claudeLogin("current", currentEnd),
		"target", claudeLogin("target", targetEnd),
	)

	result, err := manager.SwitchProviderAccount(claudeName, "target")
	if err != nil {
		t.Fatalf("SwitchProviderAccount(target): %v", err)
	}
	if result.RefreshTokenExpiresAt != targetEnd.UnixMilli() {
		t.Fatalf("switched account expiry = %d, want %d", result.RefreshTokenExpiresAt, targetEnd.UnixMilli())
	}
	outgoing, ok := store.Get(claudeName, "current", time.Now())
	if !ok || outgoing.RefreshTokenExpiresAt != currentEnd.UnixMilli() {
		t.Fatalf("outgoing account = %+v, want its own deadline recorded too", outgoing)
	}
}

// The listing is what both account surfaces render. An expired login has to
// read as "needs a sign-in" there, and carry the date it ended, without any
// prior write having recorded it — the common case for an account saved before
// AO captured deadlines at all.
func TestListingReportsAnExpiredLoginFromTheCredential(t *testing.T) {
	manager, store, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	expiredAt := time.Now().Add(-time.Hour)
	seedClaudeAccounts(
		t, manager, store, credentials,
		claudeLogin("current", time.Now().Add(10*24*time.Hour)),
		"expired", claudeLogin("dead", expiredAt),
	)

	accounts, err := manager.ListProviderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	var expired *ManagedAccount
	for i := range accounts {
		if accounts[i].ID == "expired" {
			expired = &accounts[i]
		}
		if accounts[i].SignInRequired {
			t.Fatalf("listing set signInRequired on %s; it belongs to a declined switch only", accounts[i].ID)
		}
	}
	if expired == nil {
		t.Fatalf("listing = %+v, want the expired account listed", accounts)
	}
	if !expired.NeedsLogin {
		t.Fatal("expired login listed as usable")
	}
	if expired.RefreshTokenExpiresAt != expiredAt.UnixMilli() {
		t.Fatalf(
			"listed expiry = %d, want the credential's %d",
			expired.RefreshTokenExpiresAt,
			expiredAt.UnixMilli(),
		)
	}
	// The metadata store is not written by a read.
	if saved, _ := store.Get(claudeName, "expired", time.Now()); saved.RefreshTokenExpiresAt != 0 {
		t.Fatalf("listing persisted %d; the listing is a read", saved.RefreshTokenExpiresAt)
	}
}

// Boot, recheck, and transfer validation all reach the canonical probe. None
// of them may spawn the CLI onto an expired login: the probe IS a refresh, and
// the refresh is what destroys the credential. newTestManager's poisoned
// binary fails the test if anything spawns anyway.
func TestCanonicalProbeRefusesAnExpiredLoginWithoutSpawning(t *testing.T) {
	manager, store, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	seedClaudeAccounts(
		t, manager, store, credentials,
		claudeLogin("dead", time.Now().Add(-time.Minute)),
		"other", claudeLogin("other", time.Now().Add(10*24*time.Hour)),
	)

	_, err := manager.RunAccountProbe(ProbeRequest{
		ProviderName: claudeName,
		Probe: func(context.Context) (provider.AccountInfo, error) {
			t.Fatal("the identity probe ran against an expired login")
			return provider.AccountInfo{}, nil
		},
	})
	if !errors.Is(err, errClaudeLoginExpired) {
		t.Fatalf("RunAccountProbe on an expired canonical login = %v, want errClaudeLoginExpired", err)
	}
}

// Reconciliation runs at the head of every account mutation. It must stand
// down on an expired canonical login rather than fail: refusing here would
// leave the user unable to switch away from the dead account OR to sign it
// back in, which is the lockout the husk path already avoids.
func TestExternalReconciliationStandsDownOnAnExpiredLogin(t *testing.T) {
	manager, store, credentials := newTestManager(t)
	claudeName := string(provider.Claude)
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "current", Provider: claudeName, Email: "current@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately NO remembered fingerprint: an unobserved canonical
	// credential is exactly what sends this path to the probe.
	if err := credentials.WriteNativeCredentialForTest(
		claudeName,
		claudeLogin("dead", time.Now().Add(-time.Minute)),
	); err != nil {
		t.Fatal(err)
	}

	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatalf("ReconcileExternalProviderAccount on an expired login = %v, want a quiet stand-down", err)
	}
	manager.mu.RLock()
	_, observed := manager.fingerprints[claudeName]
	manager.mu.RUnlock()
	if observed {
		t.Fatal("the expired credential was blessed as observed; a later sign-in would not reconcile")
	}
}

// A usage refresh is the other way an expired login gets spent: the selected
// account's refresh happens in the canonical home through the CLI, and the
// inactive account's HTTP read earns a 401 that says nothing. Both decline,
// with the account-scoped instruction.
func TestUsageRefreshDeclinesAnExpiredLogin(t *testing.T) {
	for _, selected := range []bool{true, false} {
		name := "inactive account"
		if selected {
			name = "selected account"
		}
		t.Run(name, func(t *testing.T) {
			manager, store, credentials := newTestManager(t)
			claudeName := string(provider.Claude)
			expired := claudeLogin("dead", time.Now().Add(-time.Minute))
			live := claudeLogin("live", time.Now().Add(10*24*time.Hour))
			canonical, saved := live, expired
			target := "expired"
			if selected {
				canonical, saved = expired, live
				target = "current"
			}
			seedClaudeAccounts(t, manager, store, credentials, canonical, "expired", saved)
			if selected {
				// seedClaudeAccounts leaves "current" selected; its credential
				// is the canonical one, which is the expired login here.
				if err := credentials.WriteAccountCredential(claudeName, "current", expired); err != nil {
					t.Fatal(err)
				}
			}

			err := manager.RefreshProviderAccountUsage(claudeName, target)
			if !errors.Is(err, errClaudeLoginExpired) {
				t.Fatalf("RefreshProviderAccountUsage(%s) = %v, want the expired-login verdict", target, err)
			}
			if !strings.Contains(err.Error(), target+"@example.com") {
				t.Fatalf("error %q does not name the account that needs signing in", err)
			}
		})
	}
}
