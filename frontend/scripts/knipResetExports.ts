// knip preprocessor (knip.config.ts): drops the unused-export findings for
// the exports the between-test reset registry calls by name rather than by
// import. Which exports those are is calledByName's answer, the same call
// the Vitest transform registers them with (src/test/config/testResetsPlugin.ts),
// run on the module as that transform sees it: types stripped, and a
// Svelte file or module compiled.
import type { ReporterOptions } from 'knip';
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { compile, compileModule } from 'svelte/compiler';
import { transformWithOxc } from 'vite';
import { calledByName, moduleKey } from '../src/test/config/testResetsPlugin.ts';

/** `source`, the file at `path`, as the Vitest transforms hand it on. */
export async function asTransformed(path: string, source: string): Promise<string> {
  if (path.endsWith('.svelte')) return compile(source, { filename: path, generate: 'client' }).js.code;
  const code = /\.[mc]?ts$/.test(path) ? (await transformWithOxc(source, path)).code : source;
  if (/\.svelte\.[jt]s$/.test(path)) return compileModule(code, { filename: path, generate: 'client' }).js.code;
  return code;
}

/** Drops from `options` each unused-export finding the registry calls by
 * name, reading each file with findings through `read`. `root` is the
 * frontend directory, which the registry names modules from. */
export async function dropCalledByName(options: ReporterOptions, root: string, read: (path: string) => Promise<string>): Promise<ReporterOptions> {
  const { issues, counters } = options;
  for (const [filePath, symbols] of Object.entries(issues.exports)) {
    const path = resolve(options.cwd ?? root, filePath);
    const key = moduleKey(root, path);
    if (key === null) continue;
    const { passed } = calledByName(key, await asTransformed(path, await read(path)));
    for (const symbol of Object.keys(symbols)) {
      if (!passed.has(symbol)) continue;
      delete symbols[symbol];
      counters.exports -= 1;
    }
    if (Object.keys(symbols).length === 0) delete issues.exports[filePath];
  }
  return options;
}

export default (options: ReporterOptions) => dropCalledByName(options, resolve(import.meta.dirname, '..'), (path) => readFile(path, 'utf8'));
