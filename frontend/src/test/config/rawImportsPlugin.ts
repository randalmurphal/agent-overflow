// Makes `vitest --changed` see the files a module imports with `?raw`.
//
// In the happy-dom projects the walk reads the client environment's
// module graph. There Vite marks a `?raw` import of a file it does not
// treat as JavaScript or CSS (a Go source, JSON, HTML, Markdown) as
// `?import&raw`, so the walk records the module as a watched file under
// its query id, and a change to the file itself never reaches the
// importer. Watching the file from the importer records it under its own
// path, which is the path the walk compares with the changed files.
import { extname } from 'node:path';
import { parseAst, type Plugin } from 'vite';

type Node = { type: string; [key: string]: unknown };

// Script modules: by this plugin's turn their code is JavaScript.
const SCRIPT_EXTENSIONS = new Set(['.js', '.jsx', '.ts', '.tsx', '.mjs', '.mts', '.cjs', '.cts', '.svelte']);
// The extensions Vite imports as modules (its `knownJsSrcRE` and CSS
// languages): their `?raw` imports are walked already.
const MODULE_EXTENSIONS = new Set([
  ...SCRIPT_EXTENSIONS,
  '.css', '.less', '.sass', '.scss', '.styl', '.stylus', '.pcss', '.postcss',
]);

function isRaw(id: string): boolean {
  const query = id.split('?')[1];
  return query !== undefined && query.split('&').includes('raw');
}

/** The specifiers of the module's static and literal dynamic imports. */
function importSources(code: string): string[] {
  const sources: string[] = [];
  const visit = (node: unknown): void => {
    if (!node || typeof node !== 'object') return;
    if (Array.isArray(node)) {
      node.forEach(visit);
      return;
    }
    const n = node as Node;
    if ((n.type === 'ImportDeclaration' || n.type === 'ImportExpression') && (n.source as Node | undefined)?.type === 'Literal') {
      const value = (n.source as Node).value;
      if (typeof value === 'string') sources.push(value);
    }
    for (const key of Object.keys(n)) if (key !== 'type') visit(n[key]);
  };
  visit(parseAst(code));
  return sources;
}

export function rawImportsPlugin(): Plugin {
  return {
    name: 'raw-imports',
    // After the glob plugin expands `import.meta.glob` into imports, and
    // before import analysis records the module's imports.
    enforce: 'post',
    async transform(code, id) {
      if (id.includes('/node_modules/') || id.includes('?') || !SCRIPT_EXTENSIONS.has(extname(id)) || !code.includes('raw')) return null;
      for (const source of importSources(code)) {
        if (!isRaw(source)) continue;
        const resolved = await this.resolve(source, id);
        if (resolved === null) throw new Error(`${id} imports ${source}, which does not resolve`);
        const file = resolved.id.split('?')[0];
        if (!MODULE_EXTENSIONS.has(extname(file))) this.addWatchFile(file);
      }
      return null;
    },
  };
}
