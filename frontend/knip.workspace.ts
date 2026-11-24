// The frontend workspace of knip (../knip.config.ts, `make
// frontend-deadcode`): the entries knip cannot find for itself. Each rule
// below names a way a file is reached that knip cannot see; anything else
// it reports is dead, so delete it.
import type { KnipConfig } from 'knip';

type Workspace = NonNullable<KnipConfig['workspaces']>[string];
import { globSync } from 'node:fs';
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
  if (files.size === 0) throw new Error('knip.workspace.ts found no runner or alias file in vitest.config.ts test.projects');
  return [...files].map((file) => relative(import.meta.dirname, file));
}

// A child Vitest run (src/test/stalledPage, src/test/sharedWorker) runs the
// files its config's include names, which are entries here, read from the
// config. The config itself is not: the code that starts the run names it
// as `new URL('<path>', import.meta.url)`, which knip follows, so a config
// nothing runs is reported.
async function childVitestIncludes(): Promise<string[]> {
  const configs = globSync('src/**/vitest.config.ts', { cwd: import.meta.dirname });
  if (configs.length === 0) throw new Error('knip.workspace.ts found no child Vitest config under src/');
  const files: string[] = [];
  for (const config of configs) {
    const { default: child } = (await import(resolve(import.meta.dirname, config))) as { default: { test?: { include?: string[] } } };
    const include = child.test?.include ?? [];
    if (include.length === 0) throw new Error(`${config} names no include for knip.workspace.ts to read`);
    files.push(...include);
  }
  return files;
}

export const frontendWorkspace = async (): Promise<Workspace> => ({
  entry: [
    // The package scripts run the other scripts/*.mjs, which knip reads
    // from package.json. These are run by hand, each as its header says:
    // the generator of src/lib/utils/forgeEmojiTable.ts and the generator
    // of the corpus src/lib/markdown/freezeReplay.manual.ts replays.
    'scripts/generate-forge-emoji.mjs',
    'scripts/generate-freeze-replay-fixture.mjs',
    ...(await childVitestIncludes()),
    ...vitestProjectFiles(),
  ],
  // bindings/ is generated (`wails3 generate bindings`) and outside the
  // project; stores/bindings.ts, the surface over it, is checked.
  project: ['src/**/*.{ts,js,svelte,css}', 'scripts/**/*.{ts,mjs}'],
  // scripts/generate-freeze-replay-fixture.mjs reads a database through the
  // system sqlite3 CLI.
  ignoreBinaries: ['sqlite3'],
});
