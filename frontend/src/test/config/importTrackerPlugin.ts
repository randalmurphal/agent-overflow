// Hands every dynamic `import()` in a source or test module to the `unit`
// project's runner, so a file's retirement can wait for the imports the
// file started (src/test/sharedWorker.ts). The call returns the import's
// own promise, so a caller sees the same promise, value and rejection.
//
// `unit` only: `unit-isolated` gives each file a fresh VM context and the
// browser project a fresh page, so an import a file leaves in flight lands
// in that file's own module graph and window.
import { relative, sep } from 'node:path';
import { parseAst, type Plugin } from 'vite';
import { TRACK_IMPORT_KEY } from './sharedWorkerKeys.ts';

type Node = { type: string; start: number; end: number; [key: string]: unknown };

function isNode(value: unknown): value is Node {
  return typeof value === 'object' && value !== null && typeof (value as Node).type === 'string';
}

/** The `import()` expressions of `program`, outermost only. */
function importExpressions(node: Node, found: Node[]): Node[] {
  if (node.type === 'ImportExpression') {
    found.push(node);
    return found;
  }
  for (const value of Object.values(node)) {
    if (Array.isArray(value)) {
      for (const item of value) if (isNode(item)) importExpressions(item, found);
    } else if (isNode(value)) {
      importExpressions(value, found);
    }
  }
  return found;
}

/** `code` with each `import(x)` wrapped, or null when it has none. `path`
 * labels each import in a retirement failure. */
export function trackImports(code: string, path: string): string | null {
  if (!code.includes('import(')) return null;
  const imports = importExpressions(parseAst(code) as unknown as Node, []);
  if (imports.length === 0) return null;
  let out = code;
  // From the end, so earlier offsets stay valid. Each wrapper stays on
  // the import's own lines, so line numbers in stacks do not move.
  for (const node of imports.slice().reverse()) {
    const source = node.source as Node;
    const label = `${path}: import(${code.slice(source.start, source.end).replace(/\s+/g, ' ')})`;
    const tracked = `globalThis[Symbol.for(${JSON.stringify(TRACK_IMPORT_KEY)})](${code.slice(node.start, node.end)}, ${JSON.stringify(label)})`;
    out = out.slice(0, node.start) + tracked + out.slice(node.end);
  }
  return out;
}

export function importTrackerPlugin(root: string): Plugin {
  return {
    name: 'import-tracker',
    enforce: 'post',
    transform(code, id) {
      if (id.includes('?') || id.includes('/node_modules/')) return null;
      const path = relative(root, id).split(sep).join('/');
      if (!path.startsWith('src/')) return null;
      const tracked = trackImports(code, path);
      return tracked === null ? null : { code: tracked, map: null };
    },
  };
}
