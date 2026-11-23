package app

import (
	"fmt"
	"os"
	"testing"

	"agent-overflow/internal/memory"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
	"agent-overflow/internal/wsldistro"
)

// TestMain runs the tests against the shared migrated store template, so the
// suite does not replay every migration per fixture. It leaves the working
// directory where go test started the binary, the package directory: the
// test cache records each file a test opens by that directory, so a test
// reads a file outside the package through reporoot (repo_files_test.go).
func TestMain(m *testing.M) {
	// A session that receives memory runs this binary as its SessionStart
	// hook (os.Executable in readSessionMemory), as main() handles in the
	// shipped binary.
	if len(os.Args) > 1 && os.Args[1] == memory.HookSubcommand {
		os.Exit(memory.RunHook(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
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
	goBuildCache = resolveGoBuildCache()
	removeHome, err := detachTestProcessHome()
	if err != nil {
		panic(err)
	}
	// The store template and the fixture templates are built in the run's
	// fixture directory, which storetest.Run owns.
	code := providerpath.Guard(func() int { return storetest.Run(m) })
	if err := removeHome(); err != nil {
		fmt.Fprintf(os.Stderr, "remove detached test home: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
