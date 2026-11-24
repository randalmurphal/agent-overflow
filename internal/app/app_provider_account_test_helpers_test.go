package app

import (
	"testing"

	"agent-overflow/internal/provideraccounts"
)

func attachProviderAccountStoresForTest(
	t testing.TB,
	app *App,
	accounts *provideraccounts.Store,
	credentials *provideraccounts.Credentials,
) {
	t.Helper()
	app.providerAccounts = newProviderAccountManager(app)
	if err := app.providerAccounts.Attach(accounts, credentials, ""); err != nil {
		t.Fatal(err)
	}
}

// attachFileAccountStoresForTest gives app the account storage a boot
// attaches: an account store of its own and file-backed credentials under
// its provider home. An agy launch needs it, because the store the manager
// reads decides the one agy reads.
func attachFileAccountStoresForTest(t testing.TB, app *App) {
	t.Helper()
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provideraccounts.NewCredentialsWithFileKeychain(app.credentialHomeOverride, providerCredentialPolicy())
	if err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
}

func providerCredentialsForTest(t testing.TB, app *App) *provideraccounts.Credentials {
	t.Helper()
	if app.providerAccounts == nil || app.providerAccounts.CredentialStoreForTest() == nil {
		t.Fatal("provider credential store unavailable")
	}
	return app.providerAccounts.CredentialStoreForTest()
}
