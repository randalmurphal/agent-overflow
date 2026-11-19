// Product binaries start only through src/product-process.ts, which gives
// them the allowlisted launch environment (src/harness-env.ts). This reads
// the suite's sources: a file other than that helper fails when it can start
// a process and names a product binary, or when a script it writes or
// evaluates (a shebang or child_process literal) names one, so no spec can
// hand one the developer's environment.
import { readdirSync, readFileSync } from 'node:fs';
import * as path from 'node:path';
import ts from 'typescript';
import { test, expect } from '@playwright/test';

const E2E = path.resolve(import.meta.dirname, '..');
const HELPER = path.join('src', 'product-process.ts');
// This file's samples are scripts by design.
const SCAN = path.relative(E2E, import.meta.filename);
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

/**
 * The first product-binary reference in `root`'s code (comments are not
 * read). With `declarations`, an identifier is followed into the initializers
 * of the same-named variables its file declares.
 */
function productReference(root: ts.Node, declarations?: Map<string, ts.Expression[]>): string | undefined {
  let found: string | undefined;
  const productName = /^(agent-overflow|ao-)/;
  const followed = new Set<string>();
  const visit = (node: ts.Node): void => {
    if (found) return;
    if (ts.isIdentifier(node) && (node.text === 'AO_HARNESS_BIN' || node.text === 'productBinary')) {
      found = node.text;
    } else if (
      declarations &&
      ts.isIdentifier(node) &&
      !(ts.isPropertyAccessExpression(node.parent) && node.parent.name === node) &&
      !followed.has(node.text)
    ) {
      followed.add(node.text);
      for (const initializer of declarations.get(node.text) ?? []) visit(initializer);
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
  visit(root);
  return found;
}

/**
 * The scripts `source` writes or evaluates: a literal that opens with a
 * shebang or contains child_process code, as the whole template or array of
 * lines it is part of.
 */
function scripts(source: ts.SourceFile): ts.Node[] {
  const found: ts.Node[] = [];
  const visit = (node: ts.Node): void => {
    let script: ts.Node | undefined;
    if (ts.isTemplateExpression(node)) {
      const texts = [node.head.text, ...node.templateSpans.map((span) => span.literal.text)];
      if (node.head.text.startsWith('#!') || texts.some((text) => text.includes('child_process'))) script = node;
    } else if (ts.isStringLiteralLike(node) && (node.text.startsWith('#!') || node.text.includes('child_process'))) {
      script = ts.isArrayLiteralExpression(node.parent) ? node.parent : node;
    }
    if (script) found.push(script);
    else ts.forEachChild(node, visit);
  };
  visit(source);
  return found;
}

/** Each variable initializer in `source`, by the declared name. */
function declarations(source: ts.SourceFile): Map<string, ts.Expression[]> {
  const found = new Map<string, ts.Expression[]>();
  const visit = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      found.set(node.name.text, [...(found.get(node.name.text) ?? []), node.initializer]);
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
  if (binary) return `${file}: ${start} with product binary ${binary}`;
  const names = declarations(source);
  for (const script of scripts(source)) {
    const scripted = productReference(script, names);
    if (scripted) {
      const { line } = source.getLineAndCharacterOfPosition(script.getStart(source));
      return `${file}:${line + 1}: script with product binary ${scripted}`;
    }
  }
  return undefined;
}

function sources(dir: string): string[] {
  return readdirSync(path.join(E2E, dir), { withFileTypes: true, recursive: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith('.ts'))
    .map((entry) => path.relative(E2E, path.join(entry.parentPath, entry.name)));
}

test('starts product binaries only through src/product-process.ts', () => {
  const findings = [...sources('src'), ...sources('tests')]
    .filter((file) => file !== HELPER && file !== SCAN)
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
    // A script the spec writes starts a product binary without importing anything.
    `const binary = process.env.AO_HARNESS_BIN ?? 'agent-overflow';\nawait writeFile(ssh, \`#!\${process.execPath}\nrequire('node:child_process').spawn(\${JSON.stringify(binary)}, []);\`);`,
    `await writeFile(launcher, ['#!/bin/sh', \`exec '\${repo}/bin/ao-harness' "$@"\`].join('\\n'));`,
    `const tool = productBinary('ao-harness');\nconst code = \`require('child_process').execFileSync(\${JSON.stringify(tool)})\`;`,
  ];
  for (const text of bare) expect(finding('sample.ts', text), text).toBeDefined();
  const clean = [
    // A browser or git is not a product binary.
    `import { spawnContained } from '../src/harness-process.js';\nspawnContained(chromium.executablePath(), [], { memoryLimitBytes: 1 });`,
    // Naming a product binary in a comment, or importing only types, starts nothing.
    `// bin/ao-store-test is built by make harness-build\nimport { execFileSync } from 'node:child_process';\nexecFileSync('git', ['status']);`,
    `import type { ChildProcess } from 'node:child_process';\nconst binary = productBinary('agent-overflow');`,
    // A script that starts no product binary, beside a launcher the helper writes.
    `const launcher = path.join(root, 'remote');\nawait writeProductScript(launcher, productBinary('agent-overflow'), [], { memoryLimitBytes: 1 });\nawait writeFile(ssh, \`#!\${process.execPath}\nrequire('node:child_process').spawn(\${JSON.stringify(launcher)}, []);\`);`,
    `const calls = path.join(root, 'calls.log');\nconst tool = productBinary('ao-harness');\nawait writeFile(fake, ['#!/bin/sh', \`echo "$*" >> '\${calls}'\`].join('\\n'));`,
  ];
  for (const text of clean) expect(finding('sample.ts', text), text).toBeUndefined();
});
