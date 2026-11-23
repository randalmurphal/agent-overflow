// Registers each module that has a between-test reset with the test reset
// registry when the module loads (testResets.ts says what runs). The line
// is appended to the module's test transform only: product code carries no
// hook, and the line imports nothing, so a module gains no import-graph
// edge to the test setup and a test file selects only what it imports.
import { relative, sep } from 'node:path';
import { parseAst, type Plugin } from 'vite';
import { namedExports, RESET_NAME, RESET_REGISTRY_KEY } from './testResets.ts';

type Node = { type: string; [key: string]: unknown };

const TEST_FILE = /\.(?:test|spec)\.[tj]s$|\.manual\.ts$/;
const RESET_TEXT = /(?:reset|clear)\w*ForTest/;

/** The module key testResets.ts names a module by, or null for a module
 * that is never registered. */
export function moduleKey(root: string, file: string): string | null {
  const packaged = file.lastIndexOf('/node_modules/');
  if (packaged >= 0) return file.slice(packaged + '/node_modules/'.length);
  const path = relative(root, file).split(sep).join('/');
  if (!path.startsWith('src/') || TEST_FILE.test(path)) return null;
  return path;
}

function functionOf(node: Node | undefined, declared: Map<string, Node>, name: string, seen = new Set<string>()): Node {
  if (node?.type === 'FunctionDeclaration' || node?.type === 'FunctionExpression' || node?.type === 'ArrowFunctionExpression') return node;
  if (node?.type === 'Identifier' && !seen.has(node.name as string)) {
    seen.add(node.name as string);
    return functionOf(declared.get(node.name as string), declared, name, seen);
  }
  throw new Error(`cannot tell whether ${name} takes an argument: declare it as a function`);
}

function requiresArgument(fn: Node): boolean {
  return (fn.params as Node[]).some((param) => param.type !== 'AssignmentPattern' && param.type !== 'RestElement');
}

/** The registration to append to `code`, the transformed module `key`, or
 * null when the module has nothing to register. */
export function registration(key: string, code: string): string | null {
  const named = namedExports().get(key);
  const packaged = !key.startsWith('src/');
  if (named === undefined && (packaged || !RESET_TEXT.test(code))) return null;
  const program = parseAst(code) as unknown as { body: Node[] };
  // Top-level bindings: the function or initializer each name is bound to.
  const declared = new Map<string, Node>();
  // Exported name to local name, for exports with a local binding.
  const exported = new Map<string, string>();
  for (const statement of program.body) {
    const declaration = (statement.type === 'ExportNamedDeclaration' ? statement.declaration : statement) as Node | null;
    if (declaration?.type === 'FunctionDeclaration' || declaration?.type === 'ClassDeclaration') {
      const name = (declaration.id as Node).name as string;
      declared.set(name, declaration);
      if (statement !== declaration) exported.set(name, name);
    } else if (declaration?.type === 'VariableDeclaration') {
      for (const declarator of declaration.declarations as Node[]) {
        const id = declarator.id as Node;
        if (id.type !== 'Identifier') continue;
        declared.set(id.name as string, declarator.init as Node);
        if (statement !== declaration) exported.set(id.name as string, id.name as string);
      }
    }
    if (statement.type === 'ExportNamedDeclaration' && statement.source == null) {
      for (const specifier of statement.specifiers as Node[]) {
        const local = specifier.local as Node;
        const name = specifier.exported as Node;
        exported.set((name.name ?? name.value) as string, local.name as string);
      }
    }
  }
  const auto = packaged ? [] : [...exported.keys()].filter((name) => {
    if (!RESET_NAME.test(name)) return false;
    return !requiresArgument(functionOf(declared.get(exported.get(name)!), declared, name));
  });
  for (const name of named ?? []) {
    if (!exported.has(name)) throw new Error(`testResets.ts names ${name} in ${key}, which does not export it`);
  }
  const passed = named === undefined ? auto : [...exported.keys()];
  if (passed.length === 0) return null;
  const members = passed.map((name) => `${JSON.stringify(name)}: ${exported.get(name)}`).join(', ');
  const missing = `the test reset registry is not installed: the setup file must import src/test/resetRegistry.ts before ${key} loads`;
  return `
;{
  const registry = globalThis[Symbol.for(${JSON.stringify(RESET_REGISTRY_KEY)})];
  if (registry === undefined) throw new Error(${JSON.stringify(missing)});
  registry.loaded(${JSON.stringify(key)}, { ${members} }, ${JSON.stringify(auto)});
}
`;
}

export function testResetsPlugin(root: string): Plugin {
  return {
    name: 'test-resets',
    enforce: 'post',
    transform(code, id) {
      if (id.includes('?')) return null;
      const key = moduleKey(root, id);
      if (key === null) return null;
      const line = registration(key, code);
      return line === null ? null : { code: code + line, map: null };
    },
  };
}
