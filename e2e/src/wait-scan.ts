// The gate's wait rules over the Node side of the suite, run by
// scripts/check-waits.ts beside the launcher's typecheck. A page's
// `waitForTimeout` throws at run time instead (tests/fixtures.ts); this scan
// covers what a fixture cannot reach. Every file of the gate, each spec the
// Playwright config runs and every file of the suite one imports, may not:
//
//   - sleep: `waitForTimeout`, a promise a timer resolves (however the timer
//     is reached: `setTimeout`, `window.setTimeout`, `globalThis['setTimeout']`),
//     or a `timers/promises` sleep. A product timer the spec crosses gets a
//     knob (main_harness.go harnessTimers, set by `harnessTiming`) or the page
//     clock (`page.clock`); a settle waits on its condition (`expect.poll`);
//     an absence ends at a barrier (`HarnessApp.watchEvents`).
//   - turn a wait's failure into a value: `.then(onDone, () => false)` or
//     `.catch(() => …)` on a wait, which passes once the wait times out and
//     so costs the whole timeout to say nothing happened. A wait is a call
//     named like one (`waitFor…`, `poll`, `toPass`, `expect…`) or a call
//     handed a timeout.
//   - declare tests with Playwright's own `test` under tests/: a spec takes
//     `test` or `gateTest` from tests/fixtures.ts, whose fixtures make a page
//     sleep throw and end each event watch with its test.
//
// WAIT_ALLOWED names the sites that must hold for a time, each with its
// reason. Files are parsed one at a time and no tree outlives its scan.
import { readFileSync, readdirSync } from 'node:fs';
import * as path from 'node:path';
import ts from 'typescript';

export interface WaitFinding {
  file: string;
  line: number;
  rule: 'sleep' | 'expiring wait' | 'bare test';
  /** The function the finding sits in, by name; '' at module level. */
  within: string;
  text: string;
}

/** A site that must hold for a time: a file, its function, and why. */
export interface WaitAllowance {
  /** Relative to the e2e root, with forward slashes. */
  file: string;
  function: string;
  reason: string;
}

export const WAIT_ALLOWED: readonly WaitAllowance[] = [
  {
    file: 'src/pointer.ts',
    function: 'pressToDragText',
    reason:
      'macOS starts a text drag only from a press held past its delay (150 ms in Chromium); a move before ' +
      'that starts a new selection. The platform gives no event for the delay passing.',
  },
  {
    file: 'tests/gate-fixtures.spec.ts',
    function: 'attemptPageSleep',
    reason: 'Proves that the gate fixture makes a page sleep throw; it never sleeps.',
  },
  {
    file: 'src/harness-process.ts',
    function: 'captureProcessIdentity',
    reason: 'An exec replaces the launch wrapper with no event the parent can await; the identity is read until two reads agree.',
  },
  {
    file: 'src/harness-process.ts',
    function: 'waitForOwnedTreeExit',
    reason: 'A process group leaving has no event; only its leader exit does. The group is probed until it is empty.',
  },
  {
    file: 'src/harness-process.ts',
    function: 'terminateWindowsTree',
    reason: 'A Windows process tree leaving has no event; its members are probed until they are gone.',
  },
];

const TIMER_PROMISES = new Set(['node:timers/promises', 'timers/promises']);
const TIMERS = new Set(['setTimeout', 'setInterval']);

function unwrap(node: ts.Expression): ts.Expression {
  while (ts.isParenthesizedExpression(node) || ts.isAsExpression(node) || ts.isNonNullExpression(node) || ts.isAwaitExpression(node)) {
    node = node.expression;
  }
  return node;
}

/** The name a call reaches its callee by: `f`, `a.f`, `a['f']`. */
function calleeName(callee: ts.Expression): string {
  callee = unwrap(callee);
  if (ts.isIdentifier(callee)) return callee.text;
  if (ts.isPropertyAccessExpression(callee)) return callee.name.text;
  if (ts.isElementAccessExpression(callee) && ts.isStringLiteralLike(callee.argumentExpression)) return callee.argumentExpression.text;
  return '';
}

/** The literal-valued handlers a rejection is turned into a value with. */
function answersALiteral(node: ts.Node | undefined): boolean {
  if (!node || !(ts.isArrowFunction(node) || ts.isFunctionExpression(node))) return false;
  let body: ts.Node = node.body;
  if (ts.isBlock(body)) {
    if (body.statements.length === 0) return true;
    if (body.statements.length !== 1) return false;
    const only = body.statements[0];
    if (!ts.isReturnStatement(only)) return false;
    if (!only.expression) return true;
    body = only.expression;
  }
  body = unwrap(body as ts.Expression);
  return (
    body.kind === ts.SyntaxKind.TrueKeyword ||
    body.kind === ts.SyntaxKind.FalseKeyword ||
    body.kind === ts.SyntaxKind.NullKeyword ||
    ts.isNumericLiteral(body) ||
    ts.isStringLiteralLike(body) ||
    (ts.isIdentifier(body) && body.text === 'undefined') ||
    ts.isVoidExpression(body)
  );
}

const WAIT_NAME = /^(waitFor|poll$|toPass$|expect)|wait/i;

/** Whether a call is handed a timeout: a `timeout:` option or a literal of a second or more. */
function handedATimeout(call: ts.CallExpression): boolean {
  return call.arguments.some((arg) => {
    arg = unwrap(arg);
    if (ts.isNumericLiteral(arg)) return Number(arg.text.replace(/_/g, '')) >= 1000;
    return ts.isObjectLiteralExpression(arg) && arg.properties.some((p) => p.name !== undefined && ts.isIdentifier(p.name) && p.name.text === 'timeout');
  });
}

/** Whether `expr`, or a call it is chained from, is a wait. */
function isWait(expr: ts.Expression): boolean {
  for (let at: ts.Expression | undefined = unwrap(expr); at; ) {
    if (ts.isCallExpression(at)) {
      if (WAIT_NAME.test(calleeName(at.expression)) || handedATimeout(at)) return true;
      at = ts.isPropertyAccessExpression(at.expression) || ts.isElementAccessExpression(at.expression) ? unwrap(at.expression.expression) : undefined;
    } else if (ts.isPropertyAccessExpression(at) || ts.isElementAccessExpression(at)) {
      at = unwrap(at.expression);
    } else {
      return false;
    }
  }
  return false;
}

/** The resolve parameter of the `new Promise` executor `node` sits in, if any. */
function enclosingExecutor(node: ts.Node): { resolve: string } | undefined {
  for (let at: ts.Node | undefined = node.parent; at; at = at.parent) {
    if (
      (ts.isArrowFunction(at) || ts.isFunctionExpression(at)) &&
      ts.isNewExpression(at.parent) &&
      calleeName(at.parent.expression) === 'Promise' &&
      at.parameters.length > 0 &&
      ts.isIdentifier(at.parameters[0].name)
    ) {
      return { resolve: at.parameters[0].name.text };
    }
  }
  return undefined;
}

/**
 * Whether a timer callback settles its promise by resolving it whenever it
 * runs: `resolve` itself, or a function whose own body calls `resolve`
 * outside any branch or nested function. A callback that resolves only on a
 * condition, or rejects, is a check or a deadline rather than a sleep.
 */
function alwaysResolves(callback: ts.Expression, resolve: string): boolean {
  callback = unwrap(callback);
  const callsResolve = (expr: ts.Expression | undefined): boolean => {
    if (!expr) return false;
    expr = unwrap(expr);
    if (ts.isVoidExpression(expr)) return callsResolve(expr.expression);
    return ts.isCallExpression(expr) && ts.isIdentifier(unwrap(expr.expression)) && (unwrap(expr.expression) as ts.Identifier).text === resolve;
  };
  if (ts.isIdentifier(callback)) return callback.text === resolve;
  if (ts.isCallExpression(callback) && calleeName(callback.expression) === 'bind') {
    const target = unwrap((callback.expression as ts.PropertyAccessExpression).expression);
    return ts.isIdentifier(target) && target.text === resolve;
  }
  if (!(ts.isArrowFunction(callback) || ts.isFunctionExpression(callback))) return false;
  if (!ts.isBlock(callback.body)) return callsResolve(callback.body);
  return callback.body.statements.some((statement) =>
    (ts.isExpressionStatement(statement) && callsResolve(statement.expression)) ||
    (ts.isReturnStatement(statement) && callsResolve(statement.expression)));
}

/** The name of the function `node` sits in: a declaration, a method, or a named const. */
function enclosingFunctionName(node: ts.Node): string {
  for (let at: ts.Node | undefined = node.parent; at; at = at.parent) {
    if ((ts.isFunctionDeclaration(at) || ts.isMethodDeclaration(at)) && at.name && ts.isIdentifier(at.name)) return at.name.text;
    if ((ts.isArrowFunction(at) || ts.isFunctionExpression(at)) && ts.isVariableDeclaration(at.parent) && ts.isIdentifier(at.parent.name)) {
      return at.parent.name.text;
    }
  }
  return '';
}

/** Every wait rule `source` breaks. */
export function scanWaits(source: ts.SourceFile): WaitFinding[] {
  const findings: WaitFinding[] = [];
  const report = (node: ts.Node, rule: WaitFinding['rule']) => {
    const { line } = source.getLineAndCharacterOfPosition(node.getStart(source));
    findings.push({
      file: source.fileName,
      line: line + 1,
      rule,
      within: enclosingFunctionName(node),
      text: node.getText(source).split('\n')[0].slice(0, 120),
    });
  };
  const visit = (node: ts.Node): void => {
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier) && TIMER_PROMISES.has(node.moduleSpecifier.text)) {
      report(node, 'sleep');
    } else if (ts.isCallExpression(node)) {
      const name = calleeName(node.expression);
      if (name === 'waitForTimeout') {
        report(node, 'sleep');
      } else if (TIMERS.has(name) && node.arguments.length > 0) {
        // A timer whose firing resolves a promise is a sleep.
        const executor = enclosingExecutor(node);
        if (executor && alwaysResolves(node.arguments[0], executor.resolve)) report(node, 'sleep');
      } else if (name === 'then' && answersALiteral(node.arguments[1]) && ts.isPropertyAccessExpression(node.expression) && isWait(node.expression.expression)) {
        report(node, 'expiring wait');
      } else if (name === 'catch' && answersALiteral(node.arguments[0]) && ts.isPropertyAccessExpression(node.expression) && isWait(node.expression.expression)) {
        report(node, 'expiring wait');
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return findings;
}

/** The file that may take `test` from '@playwright/test', relative to the e2e root. */
const FIXTURES = 'tests/fixtures.ts';

/** Each import of Playwright's own `test` value in `source`. */
export function bareTestImports(source: ts.SourceFile): WaitFinding[] {
  const findings: WaitFinding[] = [];
  for (const statement of source.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) continue;
    if (statement.moduleSpecifier.text !== '@playwright/test' || statement.importClause?.isTypeOnly) continue;
    const bindings = statement.importClause?.namedBindings;
    if (!bindings || !ts.isNamedImports(bindings)) continue;
    for (const element of bindings.elements) {
      if (element.isTypeOnly || (element.propertyName ?? element.name).text !== 'test') continue;
      const { line } = source.getLineAndCharacterOfPosition(statement.getStart(source));
      findings.push({ file: source.fileName, line: line + 1, rule: 'bare test', within: '', text: `${statement.getText(source).replace(/\s+/g, ' ')} (use ${FIXTURES})` });
    }
  }
  return findings;
}

function parse(file: string, text = readFileSync(file, 'utf8')): ts.SourceFile {
  return ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
}

/** The test patterns of one Playwright project, as the runner applies them. */
interface ProjectFiles {
  testDir: string;
  testMatch: RegExp[];
  testIgnore: RegExp[];
}

/** Playwright's default testMatch. */
const DEFAULT_TEST_MATCH = /\.(spec|test)\.(c|m)?[jt]sx?$/;

type Patterns = RegExp | string | readonly (RegExp | string)[] | undefined;

function patterns(value: Patterns, fallback: RegExp[]): RegExp[] {
  if (value === undefined) return fallback;
  const list = Array.isArray(value) ? value : [value];
  return list.map((pattern) => {
    if (!(pattern instanceof RegExp)) {
      throw new Error(`the wait scan reads RegExp test patterns only; playwright.config.ts has ${JSON.stringify(pattern)}`);
    }
    return pattern;
  });
}

interface ConfigShape {
  testDir?: string;
  testMatch?: Patterns;
  testIgnore?: Patterns;
  projects?: Array<{ testDir?: string; testMatch?: Patterns; testIgnore?: Patterns }>;
}

/** Each project's file rules from a loaded Playwright config in `configDir`. */
export function projectFiles(config: ConfigShape, configDir: string): ProjectFiles[] {
  const projects = config.projects && config.projects.length > 0 ? config.projects : [{}];
  return projects.map((project) => ({
    testDir: path.resolve(configDir, project.testDir ?? config.testDir ?? '.'),
    testMatch: patterns(project.testMatch ?? config.testMatch, [DEFAULT_TEST_MATCH]),
    testIgnore: patterns(project.testIgnore ?? config.testIgnore, []),
  }));
}

function walk(dir: string, out: string[]): void {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else out.push(full);
  }
}

/** The spec files the Playwright projects run. */
export function specFiles(projects: ProjectFiles[]): string[] {
  const specs = new Set<string>();
  for (const project of projects) {
    const files: string[] = [];
    walk(project.testDir, files);
    for (const file of files) {
      if (project.testMatch.some((re) => re.test(file)) && !project.testIgnore.some((re) => re.test(file))) specs.add(file);
    }
  }
  return [...specs].sort();
}

/**
 * Scans `specs` and every file under `root` they import, directly or
 * through another, one file at a time. Returns the findings that no entry
 * of `allowed` covers, an allowance no finding used, and the files read.
 */
export function scanGate(
  root: string,
  specs: string[],
  allowed: readonly WaitAllowance[] = WAIT_ALLOWED,
): { findings: WaitFinding[]; unused: WaitAllowance[]; files: number } {
  const pending = [...specs];
  const seen = new Set<string>();
  const findings: WaitFinding[] = [];
  const used = new Set<WaitAllowance>();
  while (pending.length > 0) {
    const file = pending.pop()!;
    if (seen.has(file)) continue;
    seen.add(file);
    const source = parse(file);
    const relative = path.relative(root, file).split(path.sep).join('/');
    if (relative.startsWith('tests/') && relative !== FIXTURES) findings.push(...bareTestImports(source));
    for (const finding of scanWaits(source)) {
      const allowance = allowed.find((a) => a.file === relative && a.function === finding.within);
      if (allowance) used.add(allowance);
      else findings.push(finding);
    }
    for (const statement of source.statements) {
      if (!(ts.isImportDeclaration(statement) || ts.isExportDeclaration(statement))) continue;
      const specifier = statement.moduleSpecifier;
      if (!specifier || !ts.isStringLiteral(specifier) || !specifier.text.startsWith('.')) continue;
      const target = path.resolve(path.dirname(file), specifier.text).replace(/\.js$/, '.ts');
      if (!target.startsWith(root + path.sep) || target.includes(`${path.sep}node_modules${path.sep}`)) continue;
      if (target.endsWith('.ts') && !seen.has(target)) pending.push(target);
    }
  }
  return { findings, unused: allowed.filter((a) => !used.has(a)), files: seen.size };
}

/**
 * Samples each rule must flag and must leave alone, scanned before the
 * suite so a rule that stopped matching fails here rather than letting
 * every later sleep through.
 */
export const WAIT_SCAN_SAMPLES: { flagged: string[]; allowed: string[]; bareTest: string[]; fixtureTest: string[] } = {
  flagged: [
    `await page.waitForTimeout(500);`,
    `await page['waitForTimeout'](500);`,
    `await new Promise((resolve) => setTimeout(resolve, 4_000));`,
    `await new Promise<void>((r) => { setTimeout(() => r(), 10); });`,
    `await new Promise((r) => window.setTimeout(r, 5000));`,
    `await new Promise((r) => globalThis['setTimeout'](r, 5000));`,
    `await new Promise((resolve) => setTimeout(() => { return resolve(); }, 100));`,
    `await new Promise((resolve) => setTimeout(() => { done = true; resolve(undefined); }, 100));`,
    `await new Promise((resolve) => setTimeout(resolve.bind(null, 1), 100));`,
    `await new Promise((resolve) => setTimeout(() => void resolve(), 100));`,
    `await page.evaluate((ms) => new Promise((resolve) => setTimeout(resolve, ms)), 100);`,
    `import { setTimeout as sleep } from 'node:timers/promises';`,
    `const woken = harness.waitForEvent('x', undefined, 30_000).then(() => true, () => false);`,
    `const woken = wakeInput(here, home, 'x', 20_000).then(() => true, () => { return false; });`,
    `await harness.waitForEvent('provider:turn_completed').catch(() => undefined);`,
    `const ready = await expect.poll(read).toBe('ready').catch(() => false);`,
    `const shown = await locator.isVisible({ timeout: 5_000 }).catch(() => false);`,
  ],
  allowed: [
    `await expect.poll(() => count()).toBe(1);`,
    `const timer = setTimeout(() => reject(new Error('timed out')), 5_000);`,
    `await new Promise((resolve, reject) => { const t = setTimeout(() => reject(new Error('late')), ms); done.then(resolve); });`,
    `await harness.rpc('StopSession', id).catch(() => undefined);`,
    `await Promise.all(document.getAnimations().map((a) => a.finished.catch(() => undefined)));`,
    `const value = await read().then((v) => v, (error) => { throw error; });`,
    `const value = await read().then(use, () => false);`,
    `setTimeout(() => controller.abort(), 1_000);`,
    `await new Promise((resolve) => { const t = setTimeout(() => { kill(); child.once('close', () => resolve(undefined)); }, ms); });`,
    `await new Promise((resolve) => { const tick = () => setTimeout(() => { if (done()) resolve(1); else tick(); }, 0); tick(); });`,
  ],
  bareTest: [
    `import { test, expect } from '@playwright/test';`,
    `import { expect, test as pwTest, type Page } from '@playwright/test';`,
  ],
  fixtureTest: [
    `import { expect, devices, type Page } from '@playwright/test';\nimport { gateTest as test } from './fixtures.js';`,
    `import type { test } from '@playwright/test';`,
    `import { type test, expect } from '@playwright/test';`,
  ],
};

/** Fails when a sample is not judged as it says. */
export function checkWaitScanSamples(): string[] {
  const problems: string[] = [];
  const judge = (kind: string, samples: string[], rule: (source: ts.SourceFile) => WaitFinding[], flag: boolean) => {
    samples.forEach((sample, i) => {
      const found = rule(parse(`sample-${kind}-${i}.ts`, sample)).length > 0;
      if (found !== flag) problems.push(`wait scan ${found ? 'flagged' : 'missed'} the ${kind} sample: ${sample}`);
    });
  };
  judge('flagged', WAIT_SCAN_SAMPLES.flagged, scanWaits, true);
  judge('allowed', WAIT_SCAN_SAMPLES.allowed, scanWaits, false);
  judge('bareTest', WAIT_SCAN_SAMPLES.bareTest, bareTestImports, true);
  judge('fixtureTest', WAIT_SCAN_SAMPLES.fixtureTest, bareTestImports, false);
  return problems;
}
