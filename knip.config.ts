// knip reports frontend files, exports, types and dependencies that nothing
// uses (`make frontend-deadcode`, docs/architecture/development.md,
// Continuous integration). It runs in its default mode, where test files
// are entry points: an export only a test reads passes, because the tests
// read product constants and seams by design (frontend/AGENTS.md).
//
// The Playwright suite imports frontend source, so its files are entries of
// a workspace whose own findings are not reported: it is there for the
// imports it makes. Its packages need not be installed for that.
import type { KnipConfig } from 'knip';
import { resolve } from 'node:path';
import { frontendWorkspace } from './frontend/knip.workspace.ts';

export default async (): Promise<KnipConfig> => ({
  workspaces: {
    '.': { entry: [], project: [] },
    frontend: await frontendWorkspace(),
    e2e: {
      entry: ['**/*.{ts,mjs}'],
      project: ['**/*.{ts,mjs}'],
      ignore: ['**'],
      // Its plugin loads playwright.config.ts, which needs e2e's packages.
      playwright: false,
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
