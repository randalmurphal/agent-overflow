// Finds `contextmenu` handlers that stop propagation without claiming the
// event first.
//
// The native-menu guard (`lib/utils/browserHistoryGuard.ts`) runs on window
// in the bubble phase, after every app handler. A handler that stops a
// `contextmenu`'s propagation keeps the event from reaching it, so the
// native browser menu opens unless that handler already called
// `preventDefault()` or `suppressNativeContextMenu()`. This scan reads each
// handler body and reports a stop that comes first.
//
// Handlers are found where they are registered: `oncontextmenu={...}` in
// Svelte markup, `addEventListener('contextmenu', handler)`, and, in a file
// that names the `'contextmenu'` type anywhere else (an event list looped
// over), every `addEventListener(type, handler)` whose type is not a
// literal. A handler named by an identifier is resolved to a function or
// arrow declared in the same file; one that cannot be resolved is reported,
// because its body cannot be checked.

import ts from 'typescript';

const STOP_CALLS = new Set(['stopPropagation', 'stopImmediatePropagation']);
const CLAIM_CALLS = new Set(['preventDefault', 'suppressNativeContextMenu']);

interface Script {
  readonly file: ts.SourceFile;
  /** Offset of this script inside the whole source, for line numbers. */
  readonly offset: number;
}

function scriptsOf(path: string, text: string): Script[] {
  if (!path.endsWith('.svelte')) {
    return [{ file: ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true), offset: 0 }];
  }
  const scripts: Script[] = [];
  const block = /<script\b[^>]*>([\s\S]*?)<\/script>/g;
  for (let match = block.exec(text); match; match = block.exec(text)) {
    const offset = match.index + match[0].indexOf('>') + 1;
    scripts.push({
      file: ts.createSourceFile(path, match[1], ts.ScriptTarget.Latest, true, ts.ScriptKind.TS),
      offset,
    });
  }
  return scripts;
}

function functionBodies(scripts: Script[]): Map<string, ts.Node> {
  const bodies = new Map<string, ts.Node>();
  const visit = (node: ts.Node): void => {
    if (ts.isFunctionDeclaration(node) && node.name && node.body) {
      bodies.set(node.name.text, node.body);
    } else if (
      ts.isVariableDeclaration(node)
      && ts.isIdentifier(node.name)
      && node.initializer
      && (ts.isArrowFunction(node.initializer) || ts.isFunctionExpression(node.initializer))
    ) {
      bodies.set(node.name.text, node.initializer.body);
    }
    ts.forEachChild(node, visit);
  };
  for (const script of scripts) visit(script.file);
  return bodies;
}

/** Calls in `node`, by method or function name, in source order. */
function callNames(node: ts.Node): string[] {
  const names: string[] = [];
  const visit = (child: ts.Node): void => {
    if (ts.isCallExpression(child)) {
      const callee = child.expression;
      if (ts.isPropertyAccessExpression(callee)) names.push(callee.name.text);
      else if (ts.isIdentifier(callee)) names.push(callee.text);
    }
    ts.forEachChild(child, visit);
  };
  visit(node);
  return names;
}

/** The body to check for a handler expression, or null when unresolved. */
function handlerBody(expression: ts.Expression, bodies: Map<string, ts.Node>): ts.Node | null {
  if (ts.isIdentifier(expression)) return bodies.get(expression.text) ?? null;
  if (ts.isArrowFunction(expression) || ts.isFunctionExpression(expression)) {
    const body = expression.body;
    // `(e) => openMenu(e, id)`: the work is in the called function.
    if (ts.isCallExpression(body) && ts.isIdentifier(body.expression)) {
      return bodies.get(body.expression.text) ?? body;
    }
    return body;
  }
  return null;
}

function markupHandlers(text: string): Array<{ source: string; at: number }> {
  const handlers: Array<{ source: string; at: number }> = [];
  const attribute = /\boncontextmenu=\{/g;
  for (let match = attribute.exec(text); match; match = attribute.exec(text)) {
    const start = match.index + match[0].length;
    let depth = 1;
    let end = start;
    while (end < text.length && depth > 0) {
      if (text[end] === '{') depth += 1;
      else if (text[end] === '}') depth -= 1;
      end += 1;
    }
    handlers.push({ source: text.slice(start, end - 1), at: match.index });
  }
  return handlers;
}

function lineOf(text: string, at: number): number {
  return text.slice(0, at).split('\n').length;
}

function isContextMenuLiteral(node: ts.Node): boolean {
  return ts.isStringLiteralLike(node) && node.text === 'contextmenu';
}

/**
 * One line per handler in `text` that stops propagation before claiming the
 * event, or whose body cannot be resolved.
 */
export function findContextMenuPropagationFindings(path: string, text: string): string[] {
  if (!text.includes('contextmenu')) return [];
  const scripts = scriptsOf(path, text);
  const bodies = functionBodies(scripts);
  const findings: string[] = [];

  function check(body: ts.Node | null, line: number, what: string): void {
    if (!body) {
      findings.push(`line ${line}: ${what} names a handler this file does not declare`);
      return;
    }
    const calls = callNames(body);
    const stop = calls.findIndex((name) => STOP_CALLS.has(name));
    if (stop < 0) return;
    const claim = calls.findIndex((name) => CLAIM_CALLS.has(name));
    if (claim < 0 || claim > stop) {
      findings.push(`line ${line}: ${what} stops propagation before preventDefault()`);
    }
  }

  // Registrations in script, and whether the type is named indirectly.
  const registrations: Array<{ call: ts.CallExpression; script: Script; literal: boolean }> = [];
  let namedIndirectly = false;
  for (const script of scripts) {
    const visit = (node: ts.Node): void => {
      if (
        ts.isCallExpression(node)
        && ts.isPropertyAccessExpression(node.expression)
        && node.expression.name.text === 'addEventListener'
        && node.arguments.length >= 2
      ) {
        registrations.push({ call: node, script, literal: ts.isStringLiteralLike(node.arguments[0]) });
      } else if (isContextMenuLiteral(node)) {
        const parent = node.parent;
        const registeredDirectly = ts.isCallExpression(parent)
          && parent.arguments[0] === node
          && (
            (ts.isPropertyAccessExpression(parent.expression)
              && /^(add|remove)EventListener$/.test(parent.expression.name.text))
          );
        const constructsEvent = ts.isNewExpression(parent);
        if (!registeredDirectly && !constructsEvent) namedIndirectly = true;
      }
      ts.forEachChild(node, visit);
    };
    visit(script.file);
  }
  for (const { call, script, literal } of registrations) {
    const type = call.arguments[0];
    const handles = literal ? isContextMenuLiteral(type) : namedIndirectly;
    if (!handles) continue;
    const line = lineOf(text, script.offset + call.getStart(script.file));
    check(handlerBody(call.arguments[1], bodies), line, `addEventListener(${type.getText(script.file)}, ...)`);
  }

  if (path.endsWith('.svelte')) {
    for (const handler of markupHandlers(text)) {
      const parsed = ts.createSourceFile('handler.ts', `(${handler.source});`, ts.ScriptTarget.Latest, true);
      const statement = parsed.statements[0];
      let expression: ts.Expression | null = null;
      if (statement && ts.isExpressionStatement(statement)) {
        expression = statement.expression;
        while (ts.isParenthesizedExpression(expression)) expression = expression.expression;
      }
      check(expression ? handlerBody(expression, bodies) : null, lineOf(text, handler.at), 'oncontextmenu');
    }
  }
  return findings;
}
