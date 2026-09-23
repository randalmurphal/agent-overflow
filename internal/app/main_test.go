package app

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/wsldistro"
)

// App integration tests historically execute from the repository root: source
// contract scans and committed fixtures use root-relative paths. Preserve that
// contract after relocating the package, then run against the shared migrated
// store template so the suite does not replay every migration per fixture.
func TestMain(m *testing.M) {
	packageDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	repoRoot := filepath.Clean(filepath.Join(packageDir, "..", ".."))
	if err := os.Chdir(repoRoot); err != nil {
		panic(err)
	}
	// A test run started from a terminal inside the app inherits the Windows
	// launcher's exports. Left set, a save or a WSL settings write under test
	// would reach the developer's real Windows Downloads folder or wsl.json.
	// Tests that exercise them set their own values with t.Setenv.
	for _, name := range []string{wsldistro.AppDataEnv, wsldistro.DownloadsEnv} {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	os.Exit(storetest.Run(m))
}
