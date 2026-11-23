// Every spec builds on tests/base-test.ts, so one owner registers every page
// a test opens for the stalled-page capture (src/page-stall.ts). This reads
// the suite's sources and fails a file that imports `test` from
// '@playwright/test' instead. Manual specs are investigation drivers outside
// the gate and keep their own setup.
import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import ts from 'typescript';
import { expect, test } from './base-test.js';

const TESTS = import.meta.dirname;
const OWNER = 'base-test.ts';

/** The value bindings `source` imports from '@playwright/test'. */
function playwrightValueImports(source: ts.SourceFile): string[] {
  const names: string[] = [];
  for (const statement of source.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) continue;
    if (statement.moduleSpecifier.text !== '@playwright/test') continue;
    const clause = statement.importClause;
    if (!clause || clause.isTypeOnly) continue;
    if (clause.name) names.push('default');
    const bindings = clause.namedBindings;
    if (bindings && ts.isNamespaceImport(bindings)) names.push('*');
    if (bindings && ts.isNamedImports(bindings)) {
      for (const element of bindings.elements) {
        if (!element.isTypeOnly) names.push((element.propertyName ?? element.name).text);
      }
    }
  }
  return names;
}

function suiteSources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return suiteSources(full);
    return entry.name.endsWith('.ts') ? [full] : [];
  });
}

function bypasses(file: string, text: string): string[] {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
  return playwrightValueImports(source).filter((name) => name === 'test' || name === 'default' || name === '*');
}

test('specs import test from tests/base-test.ts, not @playwright/test', () => {
  const offenders = suiteSources(TESTS)
    .filter((file) => path.basename(file) !== OWNER && !file.endsWith('.manual.spec.ts'))
    .flatMap((file) => {
      const found = bypasses(file, readFileSync(file, 'utf8'));
      return found.length > 0 ? [`${path.relative(TESTS, file)} (${found.join(', ')})`] : [];
    });
  expect(offenders, 'import { test } from ./base-test.js (or ./fixtures.js) instead').toEqual([]);
});

test('the scan recognises each way a file can take test from @playwright/test', () => {
  expect(bypasses('a.ts', "import { test, expect } from '@playwright/test';")).toEqual(['test']);
  expect(bypasses('a.ts', "import { test as pwTest } from '@playwright/test';")).toEqual(['test']);
  expect(bypasses('a.ts', "import * as pw from '@playwright/test';")).toEqual(['*']);
  expect(bypasses('a.ts', "import pw from '@playwright/test';")).toEqual(['default']);
  expect(bypasses('a.ts', "import type { Page } from '@playwright/test';")).toEqual([]);
  expect(bypasses('a.ts', "import { devices, type Page } from '@playwright/test';")).toEqual([]);
});
