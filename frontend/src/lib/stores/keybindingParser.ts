// Keybindings parser: chord strings + VS Code-style `when` expressions.
//
// Chord syntax
//   mod+j         → `mod` = cmd on macOS, ctrl elsewhere
//   cmd+shift+p   → explicit cmd/meta
//   ctrl+alt+/    → explicit ctrl + alt + slash
//   shift++       → "shift + +" (the plus key with a trailing separator)
//   space, esc    → translated to " " and "escape" respectively
//
// when expression syntax
//   identifier    → string of ident chars ([A-Za-z_][A-Za-z0-9_.-]*)
//   !expr         → logical NOT
//   a && b        → logical AND
//   a || b        → logical OR
//   (expr)        → grouping (parens)
// Operator precedence: NOT > AND > OR.
//
// All functions are pure and have no side effects. Errors are returned as
// `null` from low-level helpers and as thrown ParseError from parseWhen /
// parseChord when used directly from settings — the keybindings store
// catches and routes them into the issue list surfaced to the user.

export interface Chord {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
  modKey: boolean;
}

type ChordKeyboardEvent = {
  key: string;
  code?: string;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
};

export type WhenNode =
  | { type: 'identifier'; name: string }
  | { type: 'not'; node: WhenNode }
  | { type: 'and'; left: WhenNode; right: WhenNode }
  | { type: 'or'; left: WhenNode; right: WhenNode };

export class ParseError extends Error {
  constructor(public readonly source: string, message: string) {
    super(`${message} — in "${source}"`);
    this.name = 'ParseError';
  }
}

const MAX_WHEN_DEPTH = 64;

function normalizeKeyToken(token: string): string {
  if (token === 'space') return ' ';
  if (token === 'esc') return 'escape';
  return token;
}

/**
 * Parse a shortcut spec (e.g. "mod+shift+p"). Returns null when the spec is
 * unparseable so the caller can surface a structured error. Callers that want
 * to throw should use parseChord().
 */
export function tryParseChord(value: string): Chord | null {
  if (value.trim().length === 0) return null;
  const rawTokens = value
    .toLowerCase()
    .split('+')
    .map((t) => t.trim());
  const tokens = [...rawTokens];
  let trailingEmptyCount = 0;
  while (tokens[tokens.length - 1] === '') {
    trailingEmptyCount += 1;
    tokens.pop();
  }
  if (trailingEmptyCount > 0) {
    tokens.push('+');
  }
  if (tokens.some((t) => t.length === 0)) return null;
  if (tokens.length === 0) return null;

  let key: string | null = null;
  let metaKey = false;
  let ctrlKey = false;
  let shiftKey = false;
  let altKey = false;
  let modKey = false;

  for (const token of tokens) {
    switch (token) {
      case 'cmd':
      case 'meta':
        metaKey = true;
        break;
      case 'ctrl':
      case 'control':
        ctrlKey = true;
        break;
      case 'shift':
        shiftKey = true;
        break;
      case 'alt':
      case 'option':
        altKey = true;
        break;
      case 'mod':
        modKey = true;
        break;
      default: {
        if (key !== null) return null;
        // Reject tokens with internal whitespace — valid keys are single
        // tokens after the "+" split.
        if (/\s/.test(token)) return null;
        key = normalizeKeyToken(token);
      }
    }
  }
  if (key === null) return null;
  return { key, metaKey, ctrlKey, shiftKey, altKey, modKey };
}

export function parseChord(value: string): Chord {
  const parsed = tryParseChord(value);
  if (!parsed) {
    throw new ParseError(value, 'invalid keybinding shortcut');
  }
  return parsed;
}

export function encodeChord(chord: Chord): string {
  const parts: string[] = [];
  if (chord.modKey) parts.push('mod');
  if (chord.metaKey) parts.push('cmd');
  if (chord.ctrlKey) parts.push('ctrl');
  if (chord.altKey) parts.push('alt');
  if (chord.shiftKey) parts.push('shift');
  parts.push(chord.key === ' ' ? 'space' : chord.key);
  return parts.join('+');
}

/**
 * Check whether a given KeyboardEvent matches a chord. Uses `modKey` to map
 * to the platform-native modifier (cmd on macOS, ctrl elsewhere).
 */
export function chordMatches(
  chord: Chord,
  event: ChordKeyboardEvent,
  isMac: boolean,
): boolean {
  // Resolve the `mod` alias.
  const wantMeta = chord.metaKey || (chord.modKey && isMac);
  const wantCtrl = chord.ctrlKey || (chord.modKey && !isMac);

  if (event.metaKey !== wantMeta) return false;
  if (event.ctrlKey !== wantCtrl) return false;
  if (event.shiftKey !== chord.shiftKey) return false;
  if (event.altKey !== chord.altKey) return false;
  return eventKeyMatchesChord(chord, event, isMac);
}

function eventKeyMatchesChord(
  chord: Chord,
  event: Pick<ChordKeyboardEvent, 'key' | 'code' | 'altKey'>,
  isMac: boolean,
): boolean {
  const wantedKey = chord.key.toLowerCase();
  if (event.key.toLowerCase() === wantedKey) return true;
  if (!isMac || !event.altKey) return false;
  return macOptionLetterFromCode(event.code) === wantedKey;
}

export function macOptionLetterFromCode(code: string | undefined): string | null {
  if (!code?.startsWith('Key') || code.length !== 4) return null;
  const codeUnit = code.charCodeAt(3);
  if (codeUnit >= 65 && codeUnit <= 90) {
    return String.fromCharCode(codeUnit + 32);
  }
  return null;
}

// ---- when expression ----

type WhenToken =
  | { type: 'identifier'; value: string }
  | { type: 'not' }
  | { type: 'and' }
  | { type: 'or' }
  | { type: 'lparen' }
  | { type: 'rparen' };

function tokenizeWhen(expression: string): WhenToken[] | null {
  const tokens: WhenToken[] = [];
  let i = 0;
  while (i < expression.length) {
    const c = expression[i];
    if (/\s/.test(c)) {
      i += 1;
      continue;
    }
    if (expression.startsWith('&&', i)) {
      tokens.push({ type: 'and' });
      i += 2;
      continue;
    }
    if (expression.startsWith('||', i)) {
      tokens.push({ type: 'or' });
      i += 2;
      continue;
    }
    if (c === '!') {
      tokens.push({ type: 'not' });
      i += 1;
      continue;
    }
    if (c === '(') {
      tokens.push({ type: 'lparen' });
      i += 1;
      continue;
    }
    if (c === ')') {
      tokens.push({ type: 'rparen' });
      i += 1;
      continue;
    }
    const match = /^[A-Za-z_][A-Za-z0-9_.-]*/.exec(expression.slice(i));
    if (!match) return null;
    tokens.push({ type: 'identifier', value: match[0] });
    i += match[0].length;
  }
  return tokens;
}

export function tryParseWhen(expression: string): WhenNode | null {
  const tokens = tokenizeWhen(expression);
  if (!tokens || tokens.length === 0) return null;
  let idx = 0;

  const parsePrimary = (depth: number): WhenNode | null => {
    if (depth > MAX_WHEN_DEPTH) return null;
    const tok = tokens[idx];
    if (!tok) return null;
    if (tok.type === 'identifier') {
      idx += 1;
      return { type: 'identifier', name: tok.value };
    }
    if (tok.type === 'lparen') {
      idx += 1;
      const expr = parseOr(depth + 1);
      const close = tokens[idx];
      if (!expr || !close || close.type !== 'rparen') return null;
      idx += 1;
      return expr;
    }
    return null;
  };

  const parseUnary = (depth: number): WhenNode | null => {
    let notCount = 0;
    while (tokens[idx]?.type === 'not') {
      idx += 1;
      notCount += 1;
      if (notCount > MAX_WHEN_DEPTH) return null;
    }
    let node = parsePrimary(depth);
    if (!node) return null;
    while (notCount > 0) {
      node = { type: 'not', node };
      notCount -= 1;
    }
    return node;
  };

  const parseAnd = (depth: number): WhenNode | null => {
    let left = parseUnary(depth);
    if (!left) return null;
    while (tokens[idx]?.type === 'and') {
      idx += 1;
      const right = parseUnary(depth);
      if (!right) return null;
      left = { type: 'and', left, right };
    }
    return left;
  };

  const parseOr = (depth: number): WhenNode | null => {
    let left = parseAnd(depth);
    if (!left) return null;
    while (tokens[idx]?.type === 'or') {
      idx += 1;
      const right = parseAnd(depth);
      if (!right) return null;
      left = { type: 'or', left, right };
    }
    return left;
  };

  const ast = parseOr(0);
  if (!ast || idx !== tokens.length) return null;
  return ast;
}

export function parseWhen(expression: string): WhenNode {
  const parsed = tryParseWhen(expression);
  if (!parsed) {
    throw new ParseError(expression, 'invalid when expression');
  }
  return parsed;
}

/**
 * Evaluate a `when` AST against a context where missing identifiers are false.
 */
export function evaluateWhen(node: WhenNode, context: Record<string, boolean>): boolean {
  switch (node.type) {
    case 'identifier':
      return context[node.name] === true;
    case 'not':
      return !evaluateWhen(node.node, context);
    case 'and':
      return evaluateWhen(node.left, context) && evaluateWhen(node.right, context);
    case 'or':
      return evaluateWhen(node.left, context) || evaluateWhen(node.right, context);
  }
}

export function encodeWhen(node: WhenNode): string {
  switch (node.type) {
    case 'identifier':
      return node.name;
    case 'not':
      return `!(${encodeWhen(node.node)})`;
    case 'and':
      return `(${encodeWhen(node.left)} && ${encodeWhen(node.right)})`;
    case 'or':
      return `(${encodeWhen(node.left)} || ${encodeWhen(node.right)})`;
  }
}
