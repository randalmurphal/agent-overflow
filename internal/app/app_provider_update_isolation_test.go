package app

import (
	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/providerupdate/updatefake"
)

// isolateProviderUpdates gives app a stub update runner and the release
// hosts' fake feed. Both builds' isolateE2EProviderSpawns call it: even the
// providersmoke gate must not replace the developer's provider CLI or
// depend on the release hosts.
func isolateProviderUpdates(app *App) {
	app.providerUpdateRunner = kerneltest.StubProviderUpdateRunner()
	app.providerUpdateFeed = updatefake.New()
	app.providerUpdateClientOverride = isolatedProviderUpdateClient(app.providerUpdateFeed)
}
