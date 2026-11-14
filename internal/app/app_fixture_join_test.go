package app

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// Thread request work started in a test is over before the fixture's
// directories are removed: it still resolves the provider binary the
// fixture installed, never the bare default a deleted settings file gives.
func TestFixturesJoinThreadRequestWorkBeforeTheirDirectoriesGo(t *testing.T) {
	t.Parallel()
	fixtures := map[string]func(*testing.T) *App{
		"setupE2EApp":         func(t *testing.T) *App { app, _ := setupE2EApp(t); return app },
		"newTestAppWithStore": newTestAppWithStore,
	}
	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			done := make(chan string, 1)
			t.Run("fixture", func(t *testing.T) {
				app := build(t)
				testOver := make(chan struct{})
				t.Cleanup(func() { close(testOver) })
				if !app.goThreadRequestSettling(func() {
					<-testOver
					// Long enough for an unjoined fixture to finish its
					// cleanup first.
					time.Sleep(50 * time.Millisecond)
					done <- app.providerBinaryPath(string(provider.Claude))
				}) {
					t.Fatal("the fixture refused thread request work")
				}
			})
			select {
			case binary := <-done:
				if !filepath.IsAbs(binary) {
					t.Fatalf("request work resolved the provider binary %q after the fixture's settings were removed", binary)
				}
			default:
				t.Fatal("the fixture's cleanup returned while its thread request work was still running")
			}
		})
	}
}
