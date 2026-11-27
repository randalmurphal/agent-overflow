// The gate's wait rules over the Node side of the suite, run by
// scripts/check-waits.ts beside the launcher's typecheck. A page's
// `waitForTimeout` throws at run time instead (tests/base-test.ts); this scan
// covers what a fixture cannot reach. Every file of the gate, each spec the
// Playwright config runs and every file of the suite one imports, may not:
//
//   - sleep: `waitForTimeout`, a promise a timer resolves, or a
//     `timers/promises` sleep. A product timer the spec crosses gets a knob
//     (main_harness.go harnessTimers, set by `harnessTiming`) or the page
//     clock (`page.clock`); a settle waits on its condition (`expect.poll`);
//     an absence ends at a barrier (`HarnessApp.watchEvents`).
//   - turn a wait's failure into a value: `.then(onDone, () => false)` or
//     `.catch(() => …)` on a wait, which passes once the wait times out and
//     so costs the whole timeout to say nothing happened.
//   - reach Playwright's own `test`, except tests/base-test.ts: a spec takes
//     `test` from tests/base-test.ts, or from tests/fixtures.ts, which extends
//     it. Its fixtures make a page sleep throw, end each event watch with its
//     test, and capture a stalled page when a test fails.
//
// The scan reads syntax, not types, so it resolves these forms and no others:
//
//   - A timer is `setTimeout` or `setInterval` reached by name, member or
//     string element (`window.setTimeout`, `globalThis['setTimeout']`), or a
//     name the file binds to one: `const t = setTimeout`, a `.bind` of one,
//     `const { setTimeout: t } = globalThis`, `import { setTimeout as t }`.
//     It sleeps when it sits in a `new Promise` executor and is handed the
//     executor's `resolve`, `resolve.bind(…)`, or a function whose own body
//     calls `resolve` outside any branch.
//   - A wait is a call named like one (`waitFor…`, `poll`, `toPass`,
//     `expect…`, `…wait…`), a call handed a timeout, a call chained from a
//     wait, or a name the file assigns a wait to without awaiting it. A call
//     is handed a timeout when it passes an object literal with a property
//     named like a timeout, or passes an argument at a parameter position a
//     scanned file declares with a name like a timeout (`timeoutMs`,
//     `deadlineMs`) under the name the call reaches it by. A number alone is
//     not a timeout.
//   - `then` and `catch` are reached by member or string element, and the
//     handler that turns a failure into a value is a function that returns a
//     literal, `undefined`, `void …`, or nothing.
//   - Playwright's `test` is reached by an import that is not type-only (the
//     named `test`, a namespace or the default), an `export … from` that
//     re-exports it or everything, `import … = require(…)`, `require(…)`, or
//     a dynamic `import(…)`.
//
// Not resolved: names by scope (a name counts file-wide, wherever it is
// bound), a timer or `resolve` passed through a parameter, a property or
// another file, a wait returned from a function, a timeout parameter
// declared outside the scanned files, and `test` handed on as a value by a
// file that imports it from tests/base-test.ts.
//
// WAIT_ALLOWED names the sites that must hold for a time, each with its
// reason. Each file is parsed once and read in one walk; what the walk
// leaves for the timeout parameters of later files is plain data, and no
// tree outlives its file.
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
    file: 'tests/tracker-activity-flow.ts',
    function: 'holdForHalfAMinuteOfWork',
    reason:
      'time_read rounds the input the backend measured to the minute, in wall time on its own clock, which no ' +
      'harness timer moves; the half minute the person spends on a thread has no event to await.',
  },
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
  {
    file: 'src/page-stall.ts',
    function: 'probe',
    reason:
      'Runs only once a test has failed: a page that does not answer within the bound is the stall the capture ' +
      'reports, so the timer gives the verdict and a passing test never reaches it.',
  },
];

const TIMER_PROMISES = new Set(['node:timers/promises', 'timers/promises']);
const TIMER_MODULES = new Set(['node:timers', 'timers']);
const TIMERS = new Set(['setTimeout', 'setInterval']);
const PLAYWRIGHT = '@playwright/test';
const TIMEOUT_NAME = /timeout|deadline/i;
const WAIT_NAME = /^(waitFor|poll$|toPass$|expect)|wait/i;

/** The file that may reach Playwright's own `test`, relative to the e2e root. */
const BASE_TEST = 'tests/base-test.ts';

/** Whether the suite file at `relative` (to the e2e root) is held to the bare-test rule: every file but BASE_TEST. */
function heldToBareTest(relative: string): boolean {
  return relative !== BASE_TEST;
}

/** Strips the wrappers that do not change which value an expression is. */
function strip(node: ts.Expression): ts.Expression {
  while (ts.isParenthesizedExpression(node) || ts.isAsExpression(node) || ts.isNonNullExpression(node) || ts.isSatisfiesExpression(node)) {
    node = node.expression;
  }
  return node;
}

/** `strip`, and the `await` of a promise as the promise. */
function unwrap(node: ts.Expression): ts.Expression {
  for (node = strip(node); ts.isAwaitExpression(node); node = strip(node.expression));
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

/** The object a member call is made on: `a` of `a.f()` or `a['f']()`. */
function receiverOf(call: ts.CallExpression): ts.Expression | undefined {
  const callee = strip(call.expression);
  return ts.isPropertyAccessExpression(callee) || ts.isElementAccessExpression(callee) ? callee.expression : undefined;
}

/** The literal-valued handlers a rejection is turned into a value with. */
function answersALiteral(node: ts.Node | undefined): boolean {
  if (!node || !(ts.isArrowFunction(node) || ts.isFunctionExpression(node))) return false;
  let value: ts.Expression;
  if (ts.isBlock(node.body)) {
    if (node.body.statements.length === 0) return true;
    if (node.body.statements.length !== 1) return false;
    const only = node.body.statements[0];
    if (!ts.isReturnStatement(only)) return false;
    if (!only.expression) return true;
    value = unwrap(only.expression);
  } else {
    value = unwrap(node.body);
  }
  return (
    value.kind === ts.SyntaxKind.TrueKeyword ||
    value.kind === ts.SyntaxKind.FalseKeyword ||
    value.kind === ts.SyntaxKind.NullKeyword ||
    ts.isNumericLiteral(value) ||
    ts.isStringLiteralLike(value) ||
    (ts.isIdentifier(value) && value.text === 'undefined') ||
    ts.isVoidExpression(value)
  );
}

/**
 * The parameter positions the scanned files declare with a name like a
 * timeout, by the name a call reaches the function by. A name declared more
 * than once takes every declaration's positions.
 */
type TimeoutParameters = Map<string, Set<number>>;

/** The function a declaration's value or type is, if it is one. */
function signatureOf(node: ts.Node | undefined): ts.SignatureDeclaration | undefined {
  if (!node) return undefined;
  if (ts.isParenthesizedTypeNode(node)) return signatureOf(node.type);
  if (ts.isExpression(node)) node = strip(node);
  if (ts.isArrowFunction(node) || ts.isFunctionExpression(node) || ts.isFunctionTypeNode(node)) return node;
  return undefined;
}

/** The function `node` declares by name, if it declares one. */
function declaredFunction(node: ts.Node): { name: string; signature: ts.SignatureDeclaration } | undefined {
  let name: ts.Node | undefined;
  let signature: ts.SignatureDeclaration | undefined;
  if (ts.isFunctionDeclaration(node) || ts.isMethodDeclaration(node) || ts.isMethodSignature(node)) [name, signature] = [node.name, node];
  else if (ts.isVariableDeclaration(node) || ts.isPropertyDeclaration(node) || ts.isPropertyAssignment(node)) [name, signature] = [node.name, signatureOf(node.initializer)];
  else if (ts.isPropertySignature(node)) [name, signature] = [node.name, signatureOf(node.type)];
  if (!name || !signature || !(ts.isIdentifier(name) || ts.isPrivateIdentifier(name) || ts.isStringLiteralLike(name))) return undefined;
  return { name: name.text, signature };
}

/** A call in a chain, as much of it as the wait rule needs once every timeout parameter is known. */
interface ChainCall {
  name: string;
  args: number;
  /** Passes an object literal with a property named like a timeout. */
  timeoutOption: boolean;
}

/** An expression as the calls it is chained from, outermost first, and the name the chain starts from. */
interface Chain {
  calls: ChainCall[];
  root: string;
}

function chainOf(expr: ts.Expression): Chain {
  const calls: ChainCall[] = [];
  for (let at = unwrap(expr); ; ) {
    if (ts.isCallExpression(at)) {
      calls.push({
        name: calleeName(at.expression),
        args: at.arguments.length,
        timeoutOption: at.arguments.some((arg) => {
          arg = strip(arg);
          return ts.isObjectLiteralExpression(arg) && arg.properties.some((p) =>
            p.name !== undefined && (ts.isIdentifier(p.name) || ts.isStringLiteralLike(p.name)) && TIMEOUT_NAME.test(p.name.text));
        }),
      });
      const receiver = receiverOf(at);
      if (!receiver) return { calls, root: '' };
      at = unwrap(receiver);
    } else if (ts.isPropertyAccessExpression(at) || ts.isElementAccessExpression(at)) {
      at = unwrap(at.expression);
    } else {
      return { calls, root: ts.isIdentifier(at) ? at.text : '' };
    }
  }
}

/** Whether a chain is a wait, given every timeout parameter and the file's names that hold a wait. */
function isWait(chain: Chain, timeouts: TimeoutParameters, waits: Set<string>): boolean {
  return waits.has(chain.root) || chain.calls.some((call) => {
    if (WAIT_NAME.test(call.name) || call.timeoutOption) return true;
    const positions = timeouts.get(call.name);
    return positions !== undefined && [...positions].some((i) => i < call.args);
  });
}

/** The timer name `expr` refers to as a function, not a call of one: `setTimeout`, `a.setTimeout`, `t.bind(…)`; '' otherwise. */
function timerReference(expr: ts.Expression): string {
  expr = strip(expr);
  if (ts.isIdentifier(expr) || ts.isPropertyAccessExpression(expr) || ts.isElementAccessExpression(expr)) return calleeName(expr);
  if (ts.isCallExpression(expr) && calleeName(expr.expression) === 'bind') {
    const target = receiverOf(expr);
    return target ? timerReference(target) : '';
  }
  return '';
}

/** The resolve parameter's name when `node` is a `new Promise` executor. */
function executorResolve(node: ts.Node): string | undefined {
  if (
    (ts.isArrowFunction(node) || ts.isFunctionExpression(node)) &&
    ts.isNewExpression(node.parent) &&
    calleeName(node.parent.expression) === 'Promise' &&
    node.parameters.length > 0 &&
    ts.isIdentifier(node.parameters[0].name)
  ) {
    return node.parameters[0].name.text;
  }
  return undefined;
}

/**
 * Whether a timer callback settles its promise by resolving it whenever it
 * runs: `resolve` itself, `resolve.bind(…)`, or a function whose own body
 * calls `resolve` outside any branch or nested function. A callback that
 * resolves only on a condition, or rejects, is a check or a deadline rather
 * than a sleep.
 */
function alwaysResolves(callback: ts.Expression, resolve: string): boolean {
  const isResolve = (expr: ts.Expression | undefined): boolean => {
    if (!expr) return false;
    expr = unwrap(expr);
    return ts.isIdentifier(expr) && expr.text === resolve;
  };
  const callsResolve = (expr: ts.Expression | undefined): boolean => {
    if (!expr) return false;
    expr = unwrap(expr);
    if (ts.isVoidExpression(expr)) return callsResolve(expr.expression);
    return ts.isCallExpression(expr) && isResolve(expr.expression);
  };
  callback = unwrap(callback);
  if (ts.isIdentifier(callback)) return isResolve(callback);
  if (ts.isCallExpression(callback) && calleeName(callback.expression) === 'bind') return isResolve(receiverOf(callback));
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

/** Whether `node` names Playwright's package. */
function isPlaywright(node: ts.Node | undefined): boolean {
  return node !== undefined && ts.isStringLiteralLike(node) && node.text === PLAYWRIGHT;
}

/**
 * Whether `node` can reach Playwright's own `test` value: an import of the
 * named `test`, a namespace or the default; a re-export of `test` or of
 * everything; `import … = require(…)`; `require(…)`; or a dynamic `import(…)`.
 */
function reachesPlaywrightTest(node: ts.Node): boolean {
  const namesTest = (elements: ts.NodeArray<ts.ImportSpecifier | ts.ExportSpecifier>) =>
    elements.some((element) => !element.isTypeOnly && (element.propertyName ?? element.name).text === 'test');
  if (ts.isImportDeclaration(node)) {
    const clause = node.importClause;
    const bindings = clause?.namedBindings;
    return isPlaywright(node.moduleSpecifier) && clause !== undefined && !clause.isTypeOnly &&
      (clause.name !== undefined || (bindings !== undefined && (ts.isNamespaceImport(bindings) || namesTest(bindings.elements))));
  }
  if (ts.isExportDeclaration(node)) {
    const clause = node.exportClause;
    return isPlaywright(node.moduleSpecifier) && !node.isTypeOnly && (!clause || ts.isNamespaceExport(clause) || namesTest(clause.elements));
  }
  if (ts.isImportEqualsDeclaration(node)) {
    return !node.isTypeOnly && ts.isExternalModuleReference(node.moduleReference) && isPlaywright(node.moduleReference.expression);
  }
  return ts.isCallExpression(node) &&
    (node.expression.kind === ts.SyntaxKind.ImportKeyword || calleeName(node.expression) === 'require') &&
    isPlaywright(node.arguments[0]);
}

/**
 * What one file's walk leaves once its tree is gone: the findings it
 * decided, and the candidates the expiring-wait rule decides once every
 * file's timeout parameters are known.
 */
interface FileReading {
  findings: WaitFinding[];
  /** Names the file assigns an unawaited value to, with the value's chain. */
  assignedWaits: Array<{ name: string; chain: Chain }>;
  /** `then`/`catch` calls that turn a failure into a literal, with their receiver's chain. */
  expiring: Array<{ chain: Chain; finding: WaitFinding }>;
}

/**
 * Reads `source` in one walk: adds the timeout parameters it declares to
 * `timeouts`, decides its sleeps (and, when `bareTest` is set, its reaches
 * for Playwright's `test`), and keeps the expiring-wait candidates.
 */
function readFile(source: ts.SourceFile, timeouts: TimeoutParameters, bareTest: boolean): FileReading {
  const reading: FileReading = { findings: [], assignedWaits: [], expiring: [] };
  const finding = (node: ts.Node, rule: WaitFinding['rule'], text = node.getText(source).split('\n')[0].slice(0, 120)): WaitFinding => ({
    file: source.fileName,
    line: source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1,
    rule,
    within: rule === 'bare test' ? '' : enclosingFunctionName(node),
    text,
  });
  const timers = new Set(TIMERS);
  const timerAliases: Array<{ name: string; timer: string }> = [];
  const timerCalls: Array<{ timer: string; finding: WaitFinding }> = [];
  // The resolve parameters of the `new Promise` executors the walk is inside, innermost last.
  const executors: string[] = [];
  const assigned = (name: string, value: ts.Expression) => {
    const timer = timerReference(value);
    if (timer) timerAliases.push({ name, timer });
    // An awaited wait holds its result, not the wait.
    if (!ts.isAwaitExpression(strip(value))) reading.assignedWaits.push({ name, chain: chainOf(value) });
  };
  const visit = (node: ts.Node): void => {
    const declared = declaredFunction(node);
    declared?.signature.parameters.forEach((parameter, i) => {
      if (!ts.isIdentifier(parameter.name) || !TIMEOUT_NAME.test(parameter.name.text)) return;
      timeouts.set(declared.name, (timeouts.get(declared.name) ?? new Set<number>()).add(i));
    });
    if (bareTest && reachesPlaywrightTest(node)) {
      reading.findings.push(finding(node, 'bare test', `${node.getText(source).replace(/\s+/g, ' ')} (use ${BASE_TEST})`));
    }
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier)) {
      if (TIMER_PROMISES.has(node.moduleSpecifier.text)) reading.findings.push(finding(node, 'sleep'));
      const named = node.importClause?.namedBindings;
      if (TIMER_MODULES.has(node.moduleSpecifier.text) && named && ts.isNamedImports(named)) {
        for (const element of named.elements) timerAliases.push({ name: element.name.text, timer: (element.propertyName ?? element.name).text });
      }
    } else if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      assigned(node.name.text, node.initializer);
    } else if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken && ts.isIdentifier(node.left)) {
      assigned(node.left.text, node.right);
    } else if (ts.isBindingElement(node) && ts.isIdentifier(node.name) && ts.isObjectBindingPattern(node.parent)) {
      const property = node.propertyName ?? node.name;
      if (ts.isIdentifier(property)) timerAliases.push({ name: node.name.text, timer: property.text });
    } else if (ts.isCallExpression(node)) {
      const name = calleeName(node.expression);
      const receiver = receiverOf(node);
      if (name === 'waitForTimeout') {
        reading.findings.push(finding(node, 'sleep'));
      } else if (name && executors.length > 0 && node.arguments.length > 0 && alwaysResolves(node.arguments[0], executors[executors.length - 1])) {
        // A timer whose firing resolves a promise is a sleep; whether `name`
        // is a timer is known once the file's aliases are.
        timerCalls.push({ timer: name, finding: finding(node, 'sleep') });
      }
      if (receiver && ((name === 'then' && answersALiteral(node.arguments[1])) || (name === 'catch' && answersALiteral(node.arguments[0])))) {
        reading.expiring.push({ chain: chainOf(receiver), finding: finding(node, 'expiring wait') });
      }
    }
    const resolve = executorResolve(node);
    if (resolve !== undefined) executors.push(resolve);
    ts.forEachChild(node, visit);
    if (resolve !== undefined) executors.pop();
  };
  visit(source);
  for (let before = -1; before !== timers.size; ) {
    before = timers.size;
    for (const alias of timerAliases) if (timers.has(alias.timer)) timers.add(alias.name);
  }
  for (const call of timerCalls) if (timers.has(call.timer)) reading.findings.push(call.finding);
  return reading;
}

/** The findings of a reading once `timeouts` holds every scanned file's timeout parameters. */
function settle(reading: FileReading, timeouts: TimeoutParameters): WaitFinding[] {
  const waits = new Set<string>();
  for (let before = -1; before !== waits.size; ) {
    before = waits.size;
    for (const { name, chain } of reading.assignedWaits) if (isWait(chain, timeouts, waits)) waits.add(name);
  }
  const expiring = reading.expiring.filter(({ chain }) => isWait(chain, timeouts, waits)).map(({ finding }) => finding);
  return [...reading.findings, ...expiring];
}

/**
 * Every wait rule `source` breaks, against the timeout parameters it
 * declares itself.
 */
export function scanWaits(source: ts.SourceFile): WaitFinding[] {
  const timeouts: TimeoutParameters = new Map();
  return settle(readFile(source, timeouts, false), timeouts);
}

/** Each place in `source` that can reach Playwright's own `test` value. */
export function bareTestImports(source: ts.SourceFile): WaitFinding[] {
  return readFile(source, new Map(), true).findings.filter((finding) => finding.rule === 'bare test');
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

/** The suite files under `root` that `source`, read from `file`, imports or re-exports. */
function suiteImports(root: string, file: string, source: ts.SourceFile): string[] {
  const targets: string[] = [];
  for (const statement of source.statements) {
    if (!(ts.isImportDeclaration(statement) || ts.isExportDeclaration(statement))) continue;
    const specifier = statement.moduleSpecifier;
    if (!specifier || !ts.isStringLiteral(specifier) || !specifier.text.startsWith('.')) continue;
    const target = path.resolve(path.dirname(file), specifier.text).replace(/\.js$/, '.ts');
    if (!target.startsWith(root + path.sep) || target.includes(`${path.sep}node_modules${path.sep}`)) continue;
    if (target.endsWith('.ts')) targets.push(target);
  }
  return targets;
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
  const readings = new Map<string, FileReading>();
  const timeouts: TimeoutParameters = new Map();
  const pending = [...specs];
  for (let file = pending.pop(); file !== undefined; file = pending.pop()) {
    if (readings.has(file)) continue;
    const source = parse(file);
    const relative = path.relative(root, file).split(path.sep).join('/');
    readings.set(file, readFile(source, timeouts, heldToBareTest(relative)));
    pending.push(...suiteImports(root, file, source));
  }
  const findings: WaitFinding[] = [];
  const used = new Set<WaitAllowance>();
  for (const [file, reading] of readings) {
    const relative = path.relative(root, file).split(path.sep).join('/');
    for (const finding of settle(reading, timeouts)) {
      const allowance = finding.rule === 'bare test' ? undefined : allowed.find((a) => a.file === relative && a.function === finding.within);
      if (allowance) used.add(allowance);
      else findings.push(finding);
    }
  }
  return { findings, unused: allowed.filter((a) => !used.has(a)), files: readings.size };
}

/**
 * Samples each rule must flag and must leave alone, scanned before the
 * suite so a rule that stopped matching fails here rather than letting
 * every later sleep through. A wait sample is judged against the timeout
 * parameters it declares itself.
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
    `await new Promise((resolve) => setTimeout(resolve['bind'](null, 1), 100));`,
    `await new Promise((resolve) => setTimeout(() => void resolve(), 100));`,
    `await page.evaluate((ms) => new Promise((resolve) => setTimeout(resolve, ms)), 100);`,
    `import { setTimeout as sleep } from 'node:timers/promises';`,
    `const later = setTimeout;\nawait new Promise((resolve) => later(resolve, 100));`,
    `await new Promise((resolve) => again(resolve, 100));\nconst later = globalThis.setTimeout.bind(globalThis);\nconst again = later;`,
    `const { setTimeout: later } = globalThis;\nawait new Promise((resolve) => later(resolve, 100));`,
    `import { setTimeout as later } from 'node:timers';\nawait new Promise((resolve) => later(resolve, 100));`,
    `const woken = harness.waitForEvent('x', undefined, 30_000).then(() => true, () => false);`,
    `async function wakeInput(h: Harness, home: string, text: string, timeoutMs = 20_000) {}\n` +
      `const woken = wakeInput(here, home, 'x', 20_000).then(() => true, () => { return false; });`,
    `const came = await rig.arrival('x', LONG).catch(() => false);\nclass Rig { arrival(id: string, deadlineMs: number) {} }`,
    `await harness.waitForEvent('provider:turn_completed').catch(() => undefined);`,
    `const ready = await expect.poll(read).toBe('ready').catch(() => false);`,
    `const ready = await expect.poll(read).toBe('ready')['catch'](() => false);`,
    `const woken = await harness.waitForEvent('x')['then'](() => true, () => false);`,
    `const shown = await locator.isVisible({ timeout: 5_000 }).catch(() => false);`,
    `const sent = await rpc('Send', { deadlineMs: 5_000 }).catch(() => false);`,
    `const arrived = harness.waitForEvent('x');\nawait act();\nconst came = await arrived.catch(() => false);`,
    `let shown;\nshown = locator.isVisible({ timeout: 5_000 });\nconst seen = await shown.then(() => true, () => false);`,
  ],
  allowed: [
    `await expect.poll(() => count()).toBe(1);`,
    `const timer = setTimeout(() => reject(new Error('timed out')), 5_000);`,
    `await new Promise((resolve, reject) => { const t = setTimeout(() => reject(new Error('late')), ms); done.then(resolve); });`,
    `await harness.stopSession(id).catch(() => undefined);`,
    `await Promise.all(document.getAnimations().map((a) => a.finished.catch(() => undefined)));`,
    `const value = await read().then((v) => v, (error) => { throw error; });`,
    `const value = await read().then(use, () => false);`,
    `setTimeout(() => controller.abort(), 1_000);`,
    `await new Promise((resolve) => { const t = setTimeout(() => { kill(); child.once('close', () => resolve(undefined)); }, ms); });`,
    `await new Promise((resolve) => { const tick = () => setTimeout(() => { if (done()) resolve(1); else tick(); }, 0); tick(); });`,
    `await new Promise((resolve) => setTimeout(bind(resolve), 500));`,
    `await new Promise((resolve) => queue.push(resolve));`,
    `const read = await readChunk(4096).catch(() => 0);`,
    `function readChunk(bytes: number) {}\nconst read = await readChunk(4096).catch(() => 0);`,
    `async function stop(id: string, timeoutMs = 5_000) {}\nawait stop('x').catch(() => undefined);`,
    `const event = await harness.waitForEvent('x');\nconst size = await event.load().catch(() => 0);`,
  ],
  bareTest: [
    `import { test, expect } from '@playwright/test';`,
    `import { expect, test as pwTest, type Page } from '@playwright/test';`,
    `import * as pw from '@playwright/test';`,
    `import pw from '@playwright/test';`,
    `export { test } from '@playwright/test';`,
    `export { expect, test as it } from '@playwright/test';`,
    `export * from '@playwright/test';`,
    `export * as pw from '@playwright/test';`,
    `import pw = require('@playwright/test');`,
    `const { test } = require('@playwright/test');`,
    `const pw = await import('@playwright/test');`,
  ],
  fixtureTest: [
    `import { devices, type Page } from '@playwright/test';\nimport { test, expect } from './base-test.js';`,
    `import type * as pw from '@playwright/test';`,
    `import type { test } from '@playwright/test';`,
    `import { type test, expect } from '@playwright/test';`,
    `export { expect, devices } from '@playwright/test';`,
    `export type { Page } from '@playwright/test';`,
    `export { type test } from '@playwright/test';`,
    `export { test } from './base-test.js';`,
    `function at(page: import('@playwright/test').Page) {}`,
  ],
};

/** Suite files, relative to the e2e root, and whether the bare-test rule holds them. */
const BARE_TEST_SCOPE: ReadonlyArray<[string, boolean]> = [
  ['tests/base-test.ts', false],
  ['tests/fixtures.ts', true],
  ['tests/some-flow.spec.ts', true],
  ['src/harness-app.ts', true],
  ['src/any-helper.ts', true],
];

/** Fails when a sample is not judged as it says. */
export function checkWaitScanSamples(): string[] {
  const problems: string[] = [];
  for (const [relative, held] of BARE_TEST_SCOPE) {
    if (heldToBareTest(relative) !== held) problems.push(`wait scan ${held ? 'exempts' : 'holds'} ${relative} ${held ? 'from' : 'to'} the bare-test rule`);
  }
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
