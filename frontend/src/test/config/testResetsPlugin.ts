// Registers each module that has a between-test reset with the test reset
// registry when the module loads (testResets.ts says what runs); every other
// source module tells the registry when it has loaded. The line
// is appended to the module's test transform only: product code carries no
// hook, and the line imports nothing, so a module gains no import-graph
// edge to the test setup and a test file selects only what it imports.
import { relative, sep } from 'node:path';
import { parseAst, type Plugin } from 'vite';
import { namedExports, RESET_NAME, RESET_REGISTRY_KEY, stepExports } from './testResets.ts';

type Node = { type: string; [key: string]: unknown };

const TEST_FILE = /\.(?:test|spec)\.[tj]s$|\.manual\.ts$/;
// What testResets.ts names, read once: every transformed module looks
// itself up here.
const NAMED = namedExports();
const STEPS = stepExports();
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

/** The line that runs `call` on the registry, failing when it is missing. */
function registryLine(key: string, call: string): string {
  const missing = `the test reset registry is not installed: the setup file must import src/test/resetRegistry.ts before ${key} loads`;
  return `
;{
  const registry = globalThis[Symbol.for(${JSON.stringify(RESET_REGISTRY_KEY)})];
  if (registry === undefined) throw new Error(${JSON.stringify(missing)});
  registry.${call};
}
`;
}

/** The exports of `code`, the transformed module `key`, that the registry
 * calls by name: `auto`, the reset-named exports with a local binding that
 * take no required argument and testResets.ts does not name, which run
 * after every test; `passed`, those and the exports a step names. Each maps
 * to its local binding. knip's filter (scripts/knipResetExports.ts) exempts
 * exactly these. */
export function calledByName(key: string, code: string): { auto: string[]; passed: Map<string, string> } {
  const named = NAMED.get(key);
  const steps = STEPS.get(key) ?? new Set<string>();
  const packaged = !key.startsWith('src/');
  if (named === undefined && (packaged || !RESET_TEXT.test(code))) return { auto: [], passed: new Map() };
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
  const resets = packaged ? [] : [...exported.keys()].filter((name) => RESET_NAME.test(name));
  const auto = resets.filter((name) =>
    !named?.has(name) && !requiresArgument(functionOf(declared.get(exported.get(name)!), declared, name)));
  for (const name of named ?? []) {
    if (!exported.has(name)) throw new Error(`testResets.ts names ${name} in ${key}, which does not export it`);
  }
  const passed = new Map([...new Set([...auto, ...steps])].map((name) => [name, exported.get(name)!]));
  return { auto, passed };
}

/** The line to append to `code`, the transformed module `key`: its
 * registration, or for a source module with nothing to register a notice
 * that it finished loading (a POSTURE step that waited on it may now run).
 * Null for a package with nothing to register. */
export function registration(key: string, code: string): string | null {
  // Before the registry loads (the setup files' own imports) no POSTURE
  // step can be waiting, so the notice needs no registry.
  const settled = !key.startsWith('src/') ? null
    : `\n;globalThis[Symbol.for(${JSON.stringify(RESET_REGISTRY_KEY)})]?.settled();\n`;
  const { auto, passed } = calledByName(key, code);
  if (passed.size === 0) return settled;
  const members = [...passed].map(([name, local]) => `${JSON.stringify(name)}: ${local}`).join(', ');
  return registryLine(key, `loaded(${JSON.stringify(key)}, { ${members} }, ${JSON.stringify(auto)})`);
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
