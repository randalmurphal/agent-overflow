package provideraccountapp

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func TestSetProviderAccountAutoSwitchPersistsAndNudgesTheListing(t *testing.T) {
	manager, store, credentials, sink := signInManager(t)
	claudeName := string(provider.Claude)
	seedClaudeAccounts(t, manager, store, credentials,
		claudeLogin("current", time.Now().Add(time.Hour)),
		"other", claudeLogin("other", time.Now().Add(time.Hour)))

	listed, err := manager.ListProviderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range listed {
		if !account.AutoSwitch {
			t.Fatalf("fresh account %s is listed outside the pool", account.ID)
		}
	}

	if err := manager.SetProviderAccountAutoSwitch(claudeName, "other", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.events, []string{"changed"}) {
		t.Fatalf("events = %v, want one listing nudge", sink.events)
	}
	listed, err = manager.ListProviderAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range listed {
		if account.AutoSwitch != (account.ID != "other") {
			t.Fatalf("listed %s AutoSwitch = %v", account.ID, account.AutoSwitch)
		}
	}
	if store.AutoSwitch(claudeName, "other") {
		t.Fatal("taking the account out was not stored")
	}

	sink.events = nil
	if err := manager.SetProviderAccountAutoSwitch(claudeName, "missing", false); err == nil {
		t.Fatal("an unsaved account was taken out of the pool")
	}
	if err := manager.SetProviderAccountAutoSwitch("gemini", "other", true); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if len(sink.events) != 0 {
		t.Fatalf("a refused change emitted %v", sink.events)
	}
}

func TestAutoSwitchIsRecordedUntilAManualSwitch(t *testing.T) {
	manager, store, credentials, _ := signInManager(t)
	claudeName := string(provider.Claude)
	seedClaudeAccounts(t, manager, store, credentials,
		claudeLogin("current", time.Now().Add(time.Hour)),
		"other", claudeLogin("other", time.Now().Add(time.Hour)))

	result, err := manager.AutoSwitchProviderAccount(claudeName, "other")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Active || result.ID != "other" || !result.AutoSwitch {
		t.Fatalf("AutoSwitchProviderAccount = %+v", result)
	}
	if pool := manager.AutoSwitchPool(claudeName); pool.SelectedID != "other" || !pool.AutoSelected {
		t.Fatalf("after auto switch pool = %+v, want other auto-selected", pool)
	}

	if _, err := manager.SwitchProviderAccount(claudeName, "current"); err != nil {
		t.Fatal(err)
	}
	if pool := manager.AutoSwitchPool(claudeName); pool.SelectedID != "current" || pool.AutoSelected {
		t.Fatalf("after manual switch pool = %+v, want current chosen by a person", pool)
	}
}

// Reconciling an outside login switches, and a person made that choice.
func TestAnOutsideLoginClearsAnAutomaticSelection(t *testing.T) {
	manager, store, credentials, _ := reconcileSeed(t, time.Now().Add(20*24*time.Hour))
	claudeName := string(provider.Claude)
	if _, err := manager.AutoSwitchProviderAccount(claudeName, "other"); err != nil {
		t.Fatal(err)
	}
	if !manager.AutoSwitchPool(claudeName).AutoSelected {
		t.Fatal("auto switch was not recorded")
	}
	if err := credentials.WriteNativeCredentialForTest(
		claudeName,
		claudeLoginAs("new@example.com", "external", time.Now().Add(30*24*time.Hour)),
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReconcileExternalProviderAccount(claudeName); err != nil {
		t.Fatal(err)
	}
	pool := manager.AutoSwitchPool(claudeName)
	if pool.SelectedID == "other" || pool.SelectedID != activeID(t, store) || pool.AutoSelected {
		t.Fatalf("after outside login pool = %+v, want the new login chosen by a person", pool)
	}
}

func TestAutoSwitchPoolFollowsCardOrderFromMetadata(t *testing.T) {
	manager, store, _ := newTestManager(t)
	claudeName := string(provider.Claude)
	for _, account := range []provideraccounts.Account{
		{ID: "a", Provider: claudeName, Email: "a@example.com"},
		{ID: "b", Provider: claudeName, DisplayName: "Bee"},
		{ID: "c", Provider: claudeName},
	} {
		if _, err := store.Add(account); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Activate(claudeName, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.NoteRefreshTokenExpiry(claudeName, "b", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.NoteRefreshTokenExpiry(claudeName, "c", time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetProviderAccountAutoSwitch(claudeName, "b", false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReorderProviderAccounts(claudeName, []string{"c", "a", "b"}); err != nil {
		t.Fatal(err)
	}

	pool := manager.AutoSwitchPool(claudeName)
	want := []AutoSwitchAccount{
		{ID: "c", Label: "c", InPool: true},
		{ID: "a", Label: "a@example.com", InPool: true},
		{ID: "b", Label: "Bee", LoginExpired: true},
	}
	if pool.SelectedID != "a" || pool.AutoSelected || !slices.Equal(pool.Accounts, want) {
		t.Fatalf("pool = %+v, want selected a with %+v", pool, want)
	}
	if got := manager.AutoSwitchPool(string(provider.Codex)); got.SelectedID != "" || len(got.Accounts) != 0 {
		t.Fatalf("empty provider pool = %+v", got)
	}
}
