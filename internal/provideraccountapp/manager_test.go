package provideraccountapp

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
)

func newTestManager(t *testing.T) (*Manager, *provideraccounts.Store, *provideraccounts.Credentials) {
	t.Helper()
	store, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(t.TempDir(), CredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Deps{})
	if err := manager.Attach(store, credentials, ""); err != nil {
		t.Fatal(err)
	}
	return manager, store, credentials
}

func TestManagerSelectionProjectsOwnedStore(t *testing.T) {
	manager, store, _ := newTestManager(t)
	account, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "selected", Provider: string(provider.Claude), Email: "selected@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	selection := manager.Selection(string(provider.Claude))
	if selection.AccountID != account.ID || selection.Account.Email != account.Email {
		t.Fatalf("Selection() = %+v, want selected account", selection)
	}
	if selection.Generation != store.Generation(string(provider.Claude)) {
		t.Fatalf("generation = %d, want %d", selection.Generation, store.Generation(string(provider.Claude)))
	}
}

func TestManagerSelectionLeaseOrdersActivationWrite(t *testing.T) {
	manager, _, _ := newTestManager(t)
	lease := manager.SelectionLease(string(provider.Codex))
	if manager.mu.TryLock() {
		manager.mu.Unlock()
		t.Fatal("activation write lock acquired while selection lease was held")
	}
	lease.Release()
	lease.Release()
	if !manager.mu.TryLock() {
		t.Fatal("activation write lock remained held after selection lease release")
	}
	manager.mu.Unlock()
}
