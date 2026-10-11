// Product binaries start only through src/product-process.ts, which gives
// them the allowlisted launch environment (src/harness-env.ts). This reads
// the suite's sources: a file other than that helper that can start a
// process and names a product binary fails, so no spec can hand one the
// developer's environment.
import { readdirSync, readFileSync } from 'node:fs';
import * as path from 'node:path';
import ts from 'typescript';
import { test, expect } from '@playwright/test';

const E2E = path.resolve(import.meta.dirname, '..');
const HELPER = path.join('src', 'product-process.ts');
const SPAWNERS = new Set(['spawn', 'spawnSync', 'exec', 'execSync', 'execFile', 'execFileSync', 'fork']);
const CHILD_PROCESS = new Set(['node:child_process', 'child_process']);

/** The first way `source` can start a process, if any. */
function spawner(source: ts.SourceFile): string | undefined {
  let found: string | undefined;
  const visit = (node: ts.Node): void => {
    if (found) return;
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier) && !node.importClause?.isTypeOnly) {
      const module = node.moduleSpecifier.text;
      const clause = node.importClause;
      const bindings = clause?.namedBindings;
      if (CHILD_PROCESS.has(module)) {
        if (clause?.name || (bindings && ts.isNamespaceImport(bindings))) found = `import of ${module}`;
        else if (bindings && ts.isNamedImports(bindings)) {
          const value = bindings.elements.find((element) => !element.isTypeOnly && SPAWNERS.has((element.propertyName ?? element.name).text));
          if (value) found = `${(value.propertyName ?? value.name).text} from ${module}`;
        }
      } else if (/harness-process\.(ts|js)$/.test(module) && bindings && ts.isNamedImports(bindings)) {
        if (bindings.elements.some((element) => !element.isTypeOnly && (element.propertyName ?? element.name).text === 'spawnContained')) {
          found = 'spawnContained';
        }
      }
    } else if (
      ts.isCallExpression(node) &&
      (node.expression.kind === ts.SyntaxKind.ImportKeyword || (ts.isIdentifier(node.expression) && node.expression.text === 'require')) &&
      node.arguments.length > 0 &&
      ts.isStringLiteral(node.arguments[0]) &&
      CHILD_PROCESS.has(node.arguments[0].text)
    ) {
      found = `dynamic import of ${node.arguments[0].text}`;
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return found;
}

/** The first product-binary reference in `source`'s code (comments are not read). */
function productReference(source: ts.SourceFile): string | undefined {
  let found: string | undefined;
  const productName = /^(agent-overflow|ao-)/;
  const visit = (node: ts.Node): void => {
    if (found) return;
    if (ts.isIdentifier(node) && (node.text === 'AO_HARNESS_BIN' || node.text === 'productBinary')) {
      found = node.text;
    } else if (
      (ts.isStringLiteralLike(node) || ts.isTemplateHead(node) || ts.isTemplateMiddle(node) || ts.isTemplateTail(node)) &&
      (node.text.includes('AO_HARNESS_BIN') || /(^|[\\/])bin[\\/](agent-overflow|ao-)/.test(node.text))
    ) {
      found = JSON.stringify(node.text);
    } else if (ts.isCallExpression(node)) {
      const args = node.arguments;
      for (let i = 0; i + 1 < args.length; i += 1) {
        const [dir, name] = [args[i], args[i + 1]];
        if (ts.isStringLiteral(dir) && dir.text === 'bin' && ts.isStringLiteral(name) && productName.test(name.text)) {
          found = `'bin', '${name.text}'`;
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return found;
}

function finding(file: string, text: string): string | undefined {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const start = spawner(source);
  const binary = start && productReference(source);
  return binary ? `${file}: ${start} with product binary ${binary}` : undefined;
}

function sources(dir: string): string[] {
  return readdirSync(path.join(E2E, dir), { withFileTypes: true, recursive: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.ts'))
    .map((entry) => path.relative(E2E, path.join(entry.parentPath, entry.name)));
}

test('starts product binaries only through src/product-process.ts', () => {
  const findings = [...sources('src'), ...sources('tests')]
    .filter((file) => file !== HELPER)
    .map((file) => finding(file, readFileSync(path.join(E2E, file), 'utf8')))
    .filter((found): found is string => found !== undefined);
  expect(findings).toEqual([]);
});

test('the scan recognises each way a source can start a product binary', () => {
  const bare = [
    `import { execFile } from 'node:child_process';\nexecFile(path.join(repo, 'bin', 'ao-harness'), []);`,
    `import * as cp from 'child_process';\ncp.spawn(new URL('../../bin/agent-overflow', import.meta.url).pathname);`,
    `const { spawn } = await import('node:child_process');\nspawn(process.env.AO_HARNESS_BIN!, []);`,
    `import { spawnContained } from '../src/harness-process.js';\nspawnContained(productBinary('ao-store-test'), [], { memoryLimitBytes: 1 });`,
    `import { execFile as run } from 'node:child_process';\nrun(\`\${root}/bin/ao-store-test\`, []);`,
  ];
  for (const text of bare) expect(finding('sample.ts', text), text).toBeDefined();
  const clean = [
    // A browser or git is not a product binary.
    `import { spawnContained } from '../src/harness-process.js';\nspawnContained(chromium.executablePath(), [], { memoryLimitBytes: 1 });`,
    // Naming a product binary in a comment, or importing only types, starts nothing.
    `// bin/ao-store-test is built by make harness-build\nimport { execFileSync } from 'node:child_process';\nexecFileSync('git', ['status']);`,
    `import type { ChildProcess } from 'node:child_process';\nconst binary = productBinary('agent-overflow');`,
  ];
  for (const text of clean) expect(finding('sample.ts', text), text).toBeUndefined();
});
