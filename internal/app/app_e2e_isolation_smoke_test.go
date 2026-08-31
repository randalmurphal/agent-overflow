//go:build providersmoke

package app

import "testing"

// isolateE2EProviderSpawns is a deliberate no-op under the providersmoke build
// tag. The smoke gate (providersmoke_test.go) exists to exercise production's
// DEFAULT binary resolution against the real, authenticated CLIs — poisoning
// the binary settings or detaching HOME would defeat the only assertion a mock
// cannot make. That build is manual-only (`make provider-smoke`), documented
// as spending real tokens, and never part of `make go-test` / `make verify`.
func isolateE2EProviderSpawns(t *testing.T, app *App) {
	t.Helper()
}
