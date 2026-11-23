// Loads `?raw` imports of repository files outside frontend/ (Go sources,
// shared test vectors, docs) for the test projects. Vite's own `?raw` of
// such a file loads, but `vitest --changed` does not see the import, so a
// change to the file would not select the tests that read it. A module
// this hook loads is seen.
import { relative } from 'node:path';
import type { Plugin } from 'vite';

export function repoTextPlugin(root: string): Plugin {
  return {
    name: 'repo-text',
    enforce: 'pre',
    async load(id) {
      const [file, query] = id.split('?');
      if (query !== 'raw' || !relative(root, file).startsWith('..')) return null;
      this.addWatchFile(file);
      const text = await this.fs.readFile(file, { encoding: 'utf8' });
      return { code: `export default ${JSON.stringify(text)};`, map: null };
    },
  };
}
