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

func providerCredentialsForTest(t testing.TB, app *App) *provideraccounts.Credentials {
	t.Helper()
	if app.providerAccounts == nil || app.providerAccounts.CredentialStoreForTest() == nil {
		t.Fatal("provider credential store unavailable")
	}
	return app.providerAccounts.CredentialStoreForTest()
}
