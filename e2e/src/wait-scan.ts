// The gate's wait rules, read from the suite's sources (scripts/check-waits.ts
// runs them before Playwright starts). A gate spec, and every tests/ helper
// a gate spec imports, may not:
//
//   - sleep: `waitForTimeout`, a promise a timer resolves, or a
//     `timers/promises` sleep. A product timer the spec crosses gets a knob
//     (main_harness.go harnessTimers, set by `harnessTiming`) or the page
//     clock (`page.clock`); a settle waits on its condition (`expect.poll`);
//     an absence ends at a barrier (`HarnessApp.watchEvents`).
//   - turn a wait's failure into a value: `.then(onDone, () => false)` or
//     `waitFor…(…).catch(() => …)`, which passes once the wait times out and
//     so costs the whole timeout to say nothing happened.
//
// Manual specs (`*.manual.spec.ts`) and the infrastructure in src/, whose
// polls of process state have no event to wait on, are outside the rules.
import { readFileSync, readdirSync } from 'node:fs';
import * as path from 'node:path';
import ts from 'typescript';

export interface WaitFinding {
  file: string;
  line: number;
  rule: 'sleep' | 'expiring wait';
  text: string;
}

const TIMER_PROMISES = new Set(['node:timers/promises', 'timers/promises']);

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
  while (ts.isParenthesizedExpression(body) || ts.isAsExpression(body)) body = body.expression;
  return (
    body.kind === ts.SyntaxKind.TrueKeyword ||
    body.kind === ts.SyntaxKind.FalseKeyword ||
    body.kind === ts.SyntaxKind.NullKeyword ||
    ts.isNumericLiteral(body) ||
    ts.isStringLiteralLike(body) ||
    (ts.isIdentifier(body) && body.text === 'undefined') ||
    (ts.isVoidExpression(body))
  );
}

/** Whether `expr` is a call whose callee is named like a wait. */
function isWaitCall(expr: ts.Expression): boolean {
  while (ts.isParenthesizedExpression(expr) || ts.isAwaitExpression(expr)) expr = expr.expression;
  if (!ts.isCallExpression(expr)) return false;
  const callee = expr.expression;
  const name = ts.isIdentifier(callee) ? callee.text : ts.isPropertyAccessExpression(callee) ? callee.name.text : '';
  return /wait/i.test(name);
}

/** The resolve parameter of the `new Promise` executor `node` sits in, if any. */
function enclosingResolve(node: ts.Node): string | undefined {
  for (let at: ts.Node | undefined = node.parent; at; at = at.parent) {
    if (
      (ts.isArrowFunction(at) || ts.isFunctionExpression(at)) &&
      ts.isNewExpression(at.parent) &&
      ts.isIdentifier(at.parent.expression) &&
      at.parent.expression.text === 'Promise' &&
      at.parameters.length > 0 &&
      ts.isIdentifier(at.parameters[0].name)
    ) {
      return at.parameters[0].name.text;
    }
  }
  return undefined;
}

/** Whether a timer callback only settles the promise with `resolve`. */
function onlyResolves(callback: ts.Expression, resolve: string): boolean {
  if (ts.isIdentifier(callback)) return callback.text === resolve;
  if (!(ts.isArrowFunction(callback) || ts.isFunctionExpression(callback))) return false;
  let body: ts.Node = callback.body;
  if (ts.isBlock(body)) {
    if (body.statements.length !== 1 || !ts.isExpressionStatement(body.statements[0])) return false;
    body = body.statements[0].expression;
  }
  return ts.isCallExpression(body) && ts.isIdentifier(body.expression) && body.expression.text === resolve;
}

/** Every wait rule `source` breaks. */
export function scanWaits(source: ts.SourceFile): WaitFinding[] {
  const findings: WaitFinding[] = [];
  const report = (node: ts.Node, rule: WaitFinding['rule']) => {
    const { line } = source.getLineAndCharacterOfPosition(node.getStart(source));
    findings.push({ file: source.fileName, line: line + 1, rule, text: node.getText(source).split('\n')[0].slice(0, 120) });
  };
  const visit = (node: ts.Node): void => {
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier) && TIMER_PROMISES.has(node.moduleSpecifier.text)) {
      report(node, 'sleep');
    } else if (ts.isCallExpression(node)) {
      const callee = node.expression;
      if (ts.isPropertyAccessExpression(callee) && callee.name.text === 'waitForTimeout') {
        report(node, 'sleep');
      } else if (ts.isIdentifier(callee) && callee.text === 'setTimeout' && node.arguments.length > 0) {
        const resolve = enclosingResolve(node);
        if (resolve && onlyResolves(node.arguments[0], resolve)) report(node, 'sleep');
      } else if (ts.isPropertyAccessExpression(callee) && callee.name.text === 'then' && answersALiteral(node.arguments[1])) {
        report(node, 'expiring wait');
      } else if (
        ts.isPropertyAccessExpression(callee) &&
        callee.name.text === 'catch' &&
        answersALiteral(node.arguments[0]) &&
        isWaitCall(callee.expression)
      ) {
        report(node, 'expiring wait');
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return findings;
}

function parse(file: string, text = readFileSync(file, 'utf8')): ts.SourceFile {
  return ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
}

/**
 * The gate's sources under `testsDir`: every spec Playwright runs as a gate
 * (`*.spec.ts` that is not `*.manual.spec.ts`) and every file under
 * `testsDir` one of them imports, directly or through another.
 */
export function gateSources(testsDir: string): ts.SourceFile[] {
  const pending = readdirSync(testsDir)
    .filter((name) => name.endsWith('.spec.ts') && !name.endsWith('.manual.spec.ts'))
    .map((name) => path.join(testsDir, name));
  const seen = new Map<string, ts.SourceFile>();
  while (pending.length > 0) {
    const file = pending.pop()!;
    if (seen.has(file)) continue;
    const source = parse(file);
    seen.set(file, source);
    for (const statement of source.statements) {
      if (!(ts.isImportDeclaration(statement) || ts.isExportDeclaration(statement))) continue;
      const specifier = statement.moduleSpecifier;
      if (!specifier || !ts.isStringLiteral(specifier) || !specifier.text.startsWith('.')) continue;
      const target = path.resolve(path.dirname(file), specifier.text).replace(/\.js$/, '.ts');
      if (path.dirname(target) === testsDir && !seen.has(target)) pending.push(target);
    }
  }
  return [...seen.values()];
}

/**
 * Samples each rule must flag and must leave alone, scanned before the
 * suite so a rule that stopped matching fails here rather than letting
 * every later sleep through.
 */
export const WAIT_SCAN_SAMPLES: { flagged: string[]; allowed: string[] } = {
  flagged: [
    `await page.waitForTimeout(500);`,
    `await new Promise((resolve) => setTimeout(resolve, 4_000));`,
    `await new Promise<void>((r) => { setTimeout(() => r(), 10); });`,
    `await page.evaluate((ms) => new Promise((resolve) => setTimeout(resolve, ms)), 100);`,
    `import { setTimeout as sleep } from 'node:timers/promises';`,
    `const woken = harness.waitForEvent('x', undefined, 30_000).then(() => true, () => false);`,
    `const woken = wakeInput(here, home, 'x', 20_000).then(() => true, () => { return false; });`,
    `await harness.waitForEvent('provider:turn_completed').catch(() => undefined);`,
  ],
  allowed: [
    `await expect.poll(() => count()).toBe(1);`,
    `const timer = setTimeout(() => reject(new Error('timed out')), 5_000);`,
    `await new Promise((resolve, reject) => { const t = setTimeout(() => reject(new Error('late')), ms); done.then(resolve); });`,
    `await harness.rpc('StopSession', id).catch(() => undefined);`,
    `await Promise.all(document.getAnimations().map((a) => a.finished.catch(() => undefined)));`,
    `const value = await read().then((v) => v, (error) => { throw error; });`,
  ],
};

/** Fails when a sample is not judged as it says. */
export function checkWaitScanSamples(): string[] {
  const problems: string[] = [];
  for (const [kind, samples] of Object.entries(WAIT_SCAN_SAMPLES)) {
    samples.forEach((sample, i) => {
      const found = scanWaits(parse(`sample-${kind}-${i}.ts`, sample)).length > 0;
      if (found !== (kind === 'flagged')) problems.push(`wait scan ${found ? 'flagged' : 'missed'} the ${kind} sample: ${sample}`);
    });
  }
  return problems;
}
