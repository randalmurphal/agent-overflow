package provideraccounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAutoSwitchTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []Account{
		{ID: "one", Provider: "claude", Email: "one@example.com"},
		{ID: "two", Provider: "claude", Email: "two@example.com"},
		{ID: "three", Provider: "claude", Email: "three@example.com"},
	} {
		if _, err := store.Add(account); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Activate("claude", "one"); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func TestStoreSavedAccountsStartInThePool(t *testing.T) {
	store, dir := newAutoSwitchTestStore(t)
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{store, reloaded} {
		for _, id := range []string{"one", "two", "three"} {
			if !s.AutoSwitch("claude", id) {
				t.Fatalf("fresh account %s is outside the pool", id)
			}
		}
		for _, account := range s.Pool("claude").Accounts {
			if !account.AutoSwitch {
				t.Fatalf("Pool reports fresh account %s outside the pool", account.ID)
			}
		}
		if s.AutoSwitch("claude", "missing") {
			t.Fatal("an unsaved account reads as in the pool")
		}
	}
}

func TestStoreAutoSwitchOffSurvivesRewritesReorderAndReload(t *testing.T) {
	store, dir := newAutoSwitchTestStore(t)
	generation := store.Generation("claude")
	if err := store.SetAutoSwitch("claude", "one", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAutoSwitch("claude", "two", false); err != nil {
		t.Fatal(err)
	}
	if got := store.Generation("claude"); got != generation {
		t.Fatalf("SetAutoSwitch moved the generation: %d, want %d", got, generation)
	}

	if _, err := store.UpsertAndActivate(Account{
		ID: "one", Provider: "claude", Email: "one@example.com", DisplayName: "One",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateMetadata(Account{
		ID: "two", Provider: "claude", Email: "two@example.com", DisplayName: "Two",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reorder("claude", []string{"three", "two", "one"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{store, reloaded} {
		if s.AutoSwitch("claude", "one") || s.AutoSwitch("claude", "two") || !s.AutoSwitch("claude", "three") {
			t.Fatalf("pool = one:%v two:%v three:%v, want false false true",
				s.AutoSwitch("claude", "one"), s.AutoSwitch("claude", "two"), s.AutoSwitch("claude", "three"))
		}
	}
	view := reloaded.Pool("claude")
	var order []string
	for _, account := range view.Accounts {
		order = append(order, account.ID)
	}
	if strings.Join(order, ",") != "three,two,one" || view.Accounts[1].AutoSwitch || !view.Accounts[0].AutoSwitch {
		t.Fatalf("Pool = %+v", view)
	}
}

func TestStoreSetAutoSwitchIsIdempotentAndRejectsUnknownAccounts(t *testing.T) {
	store, _ := newAutoSwitchTestStore(t)
	if err := store.SetAutoSwitch("claude", "missing", false); err == nil {
		t.Fatal("SetAutoSwitch on an unsaved account succeeded")
	}
	if err := store.SetAutoSwitch("codex", "one", false); err == nil {
		t.Fatal("SetAutoSwitch on another provider's account succeeded")
	}
	for range 2 {
		if err := store.SetAutoSwitch("claude", "one", false); err != nil {
			t.Fatal(err)
		}
	}
	if store.AutoSwitch("claude", "one") {
		t.Fatal("account not taken out of the pool")
	}
	for range 2 {
		if err := store.SetAutoSwitch("claude", "one", true); err != nil {
			t.Fatal(err)
		}
	}
	if !store.AutoSwitch("claude", "one") {
		t.Fatal("account not put back in the pool")
	}
	if store.state.Providers["claude"].AutoSwitchOff != nil {
		t.Fatalf("a pool with no exclusions kept an empty map: %#v", store.state.Providers["claude"].AutoSwitchOff)
	}
}

func TestStoreRemoveForgetsAutoSwitchOff(t *testing.T) {
	store, dir := newAutoSwitchTestStore(t)
	if err := store.SetAutoSwitch("claude", "two", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAutoSwitch("claude", "three", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("claude", "two", ""); err != nil {
		t.Fatal(err)
	}
	if store.AutoSwitch("claude", "three") {
		t.Fatal("removing one account put another back in the pool")
	}
	if _, err := store.Add(Account{ID: "two", Provider: "claude", Email: "two@example.com"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{store, reloaded} {
		if !s.AutoSwitch("claude", "two") {
			t.Fatal("an account added again under a removed id stayed outside the pool")
		}
	}
}

func TestStoreAutoSelectedTracksTheKindOfSelection(t *testing.T) {
	autoSelected := func(store *Store) bool { return store.Pool("claude").AutoSelected }

	store, dir := newAutoSwitchTestStore(t)
	generation := store.Generation("claude")
	if _, err := store.ActivateAuto("claude", "two"); err != nil {
		t.Fatal(err)
	}
	if !autoSelected(store) || store.Generation("claude") != generation+1 {
		t.Fatalf("ActivateAuto: autoSelected=%v generation=%d", autoSelected(store), store.Generation("claude"))
	}
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !autoSelected(reloaded) {
		t.Fatal("AutoSelected did not survive a reload")
	}

	// Re-selecting the selected account changes neither the kind nor the generation.
	if _, err := store.Activate("claude", "two"); err != nil {
		t.Fatal(err)
	}
	if !autoSelected(store) || store.Generation("claude") != generation+1 {
		t.Fatal("re-activating the selected account changed the selection record")
	}
	if _, err := store.UpsertAndActivate(Account{ID: "two", Provider: "claude", Email: "two@example.com"}); err != nil {
		t.Fatal(err)
	}
	if !autoSelected(store) {
		t.Fatal("re-adopting the selected account cleared AutoSelected")
	}

	// Every other selection change clears it.
	if _, err := store.Activate("claude", "one"); err != nil {
		t.Fatal(err)
	}
	if autoSelected(store) {
		t.Fatal("Activate kept AutoSelected")
	}
	if _, err := store.ActivateAuto("claude", "one"); err != nil {
		t.Fatal(err)
	}
	if autoSelected(store) {
		t.Fatal("ActivateAuto of the manually selected account marked it automatic")
	}

	if _, err := store.ActivateAuto("claude", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertAndActivate(Account{ID: "three", Provider: "claude", Email: "three@example.com"}); err != nil {
		t.Fatal(err)
	}
	if autoSelected(store) {
		t.Fatal("UpsertAndActivate of another account kept AutoSelected")
	}

	if _, err := store.ActivateAuto("claude", "two"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("claude", "two", "one"); err != nil {
		t.Fatal(err)
	}
	if autoSelected(store) || store.Pool("claude").ActiveAccountID != "one" {
		t.Fatalf("Remove of the auto-selected account: %+v", store.Pool("claude"))
	}

	// Removing an account that is not selected leaves the record alone.
	if _, err := store.ActivateAuto("claude", "three"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("claude", "one", ""); err != nil {
		t.Fatal(err)
	}
	if !autoSelected(store) {
		t.Fatal("removing an unselected account cleared AutoSelected")
	}
}

func TestStoreLoadsAndSavesFilesWithoutAutoSwitchFields(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"providers":{"claude":{"activeAccountId":"one","generation":3,"accounts":[` +
		`{"id":"one","provider":"claude","addedAt":1},{"id":"two","provider":"claude","addedAt":2}]}}}`
	if err := os.WriteFile(filepath.Join(dir, stateFilename), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	view := store.Pool("claude")
	if view.AutoSelected || view.ActiveAccountID != "one" || len(view.Accounts) != 2 ||
		!view.Accounts[0].AutoSwitch || !view.Accounts[1].AutoSwitch {
		t.Fatalf("Pool of a file without the keys = %+v, want every account in the pool", view)
	}
	if err := store.Reorder("claude", []string{"two", "one"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFilename))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"autoSwitch", "autoSelected"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("saved file gained empty %q: %s", key, raw)
		}
	}
}
