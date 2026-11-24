// knip reports frontend files, exports, types and dependencies that nothing
// uses (`make frontend-deadcode`, docs/architecture/development.md,
// Continuous integration). It runs in its default mode, where test files
// are entry points: an export only a test reads passes, because the tests
// read product constants and seams by design (frontend/AGENTS.md).
//
// Each rule below names a way a file is reached that knip cannot see.
// Anything else it reports is dead: delete it. An export that only code
// outside frontend/ reads (the Playwright suite) carries a `@public` tag
// with that reader named beside it.
import type { KnipConfig } from 'knip';
import { isAbsolute, relative, resolve } from 'node:path';
import vitestConfig from './vitest.config.ts';

// knip's Vitest plugin does not read an inline project's runner or
// resolve.alias, so the runner and the files the aliases substitute for a
// module are entries here, read from the projects themselves.
function vitestProjectFiles(): string[] {
  const files = new Set<string>();
  for (const project of vitestConfig.test?.projects ?? []) {
    if (typeof project !== 'object' || project === null) continue;
    if (typeof project.test?.runner === 'string') files.add(resolve(import.meta.dirname, project.test.runner));
    const aliases = project.resolve?.alias;
    for (const alias of Array.isArray(aliases) ? aliases : []) {
      if (isAbsolute(alias.replacement)) files.add(alias.replacement);
    }
  }
  if (files.size === 0) throw new Error('knip.config.ts found no runner or alias file in vitest.config.ts test.projects');
  return [...files].map((file) => relative(import.meta.dirname, file));
}

const config: KnipConfig = {
  entry: [
    // CLIs run with `node scripts/<name>.mjs`.
    'scripts/*.mjs',
    // Files a child Vitest runs under its own config (src/test/stalledPage,
    // src/test/sharedWorker).
    'src/**/*.fixture.ts',
    'src/**/vitest.config.ts',
    // Child-process workloads, loaded by path through runNodeWorkload.
    'src/**/*.workload.ts',
    ...vitestProjectFiles(),
  ],
  // bindings/ is generated (`wails3 generate bindings`) and outside the
  // project; stores/bindings.ts, the surface over it, is checked.
  project: ['src/**/*.{ts,js,svelte,css}', 'scripts/**/*.{ts,mjs}'],
  // An export also used in its own file is not dead, only wider than it
  // needs to be.
  ignoreExportsUsedInFile: true,
  // Two names for one value (REPLAY_TIMEOUT_MS = RPC_TIMEOUT_MS) are two
  // meanings, not dead code.
  rules: { duplicates: 'off' },
  // scripts/generate-freeze-replay-fixture.mjs reads a database through the
  // system sqlite3 CLI.
  ignoreBinaries: ['sqlite3'],
  // The between-test resets call reset-named exports and the exports their
  // steps name by name, not by import (src/test/config/testResets.ts).
  preprocessor: './scripts/knipResetExports.ts',
};

export default config;
