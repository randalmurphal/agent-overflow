// knip reports frontend and e2e files, exports, types and dependencies that
// nothing uses (`make frontend-deadcode`, docs/architecture/development.md,
// Continuous integration). It runs in its default mode, where test files
// are entry points: an export only a test reads passes, because the tests
// read product constants and seams by design (frontend/AGENTS.md).
//
// Each workspace's entries are what a runner starts, each named with the
// runner that starts it; anything the entries do not reach is dead. The
// Playwright suite imports frontend source, so its imports count for the
// frontend workspace too. Its packages need not be installed.
import type { KnipConfig } from 'knip';
import { resolve } from 'node:path';
import { frontendWorkspace } from './frontend/knip.workspace.ts';

export default async (): Promise<KnipConfig> => ({
  workspaces: {
    '.': { entry: [], project: [] },
    frontend: await frontendWorkspace(),
    e2e: {
      entry: [
        // playwright.config.ts runs every spec under tests/ in a project
        // (test-files.ts); playwright.manual.config.ts runs *.manual.spec.ts.
        'tests/**/*.spec.ts',
        // playwright.android.config.ts.
        'android/*.spec.ts',
        // bin/ao-harness-e2e runs playwright.config.ts, and the manual
        // config for --freeze-repro (cmd/ao-harness-e2e/main.go);
        // scripts/release-upgrade-check.sh runs the manual config and
        // scripts/android-smoke.sh the Android one.
        'playwright.config.ts',
        'playwright.manual.config.ts',
        'playwright.android.config.ts',
        // tests/stalled-page-capture.spec.ts starts a nested run of this
        // config by path, which runs the fixture its testMatch names.
        'tests/stalled-page/playwright.config.ts',
        'tests/stalled-page/blocked-page.fixture.ts',
        // bin/ao-harness-e2e runs these: --flow, and --typecheck's wait
        // rules (cmd/ao-harness-e2e/main.go, typecheck.go).
        'scripts/run-functional-flow.ts',
        'scripts/check-waits.ts',
        // scripts/android-smoke.sh and main_android_fixture_unix_test.go.
        'scripts/build-android-bundle-fixture.ts',
        // Run by hand, each as its header says: a flicker diagnostic, the
        // generator of the fixtures tests/freeze-repro.manual.spec.ts
        // replays, and the perf rigs (rigs/README.md).
        'scripts/flicker-repro.mjs',
        'scripts/generate-freeze-repro.mjs',
        'rigs/{churn,coldload,gcsplit,glide,heapsoak,sendlag,storm}.mjs',
      ],
      project: ['**/*.{ts,mjs}'],
      // Its plugin reads no RegExp testMatch, which every project here
      // uses, and loads playwright.config.ts, which needs e2e's packages.
      playwright: false,
      // tests/compact-shell-origin.spec.ts loads fflate's UMD build into a
      // page by its path under node_modules.
      ignoreDependencies: ['fflate'],
      // The package scripts and scripts/build-android-bundle-fixture.ts run
      // go; scripts/freeze-repro/db.mjs reads a database through the system
      // sqlite3 CLI.
      ignoreBinaries: ['go', 'sqlite3'],
    },
  },
  // An export also used in its own file is not dead, only wider than it
  // needs to be.
  ignoreExportsUsedInFile: true,
  // Two names for one value (REPLAY_TIMEOUT_MS = RPC_TIMEOUT_MS) are two
  // meanings, not dead code.
  rules: { duplicates: 'off' },
  // The between-test resets call reset-named exports and the exports their
  // steps name by name, not by import (frontend/src/test/config/testResets.ts).
  // A path from here: knip resolves a relative one from the process's cwd.
  preprocessor: resolve(import.meta.dirname, 'frontend/scripts/knipResetExports.ts'),
});
