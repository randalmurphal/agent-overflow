// ANSI-coloured text as AnsiText.svelte renders it: the one reading of a
// string's escapes, so what the view shows and what find counts in it are
// the same text. OSC (`ESC ]` … BEL or ST) and APC (`ESC _` … ST) strings
// within a line are hidden, SGR sequences (`ESC [` params `m`) become
// colour spans, and every other character, other escapes included, is
// text. A string never hides across a newline, so a run of lines shows
// each line's text.
//
// One left-to-right scan reads the escapes: at each ESC the next character
// names the only form that can start there, and the form is read forward
// to its end or to the first character it cannot hold. A form that does
// not end leaves its ESC as text and the scan goes on after the ESC, so an
// escape inside another is read where it starts and what it leaves is not
// read again. Every character is read at most twice. The backend's search
// (internal/cilog/search.go `stripHidden`) is the same scan; the vectors in
// src/test/fixtures/ansiShownVectors.json hold both sides to it.

import { escapeHtml } from './markdownRender';

const ESC = 0x1b;
const BEL = 0x07;
const LF = 0x0a;
const CR = 0x0d;
const BACKSLASH = 0x5c;
const SEMICOLON = 0x3b;
const OPEN_BRACKET = 0x5b;
const CLOSE_BRACKET = 0x5d;
const UNDERSCORE = 0x5f;
const LOWER_M = 0x6d;

const ANSI_CLASS_CACHE_MAX = 256;
const ansiClassCache = new Map<string, string>();

function ansiClass(codeText: string): string {
  const cached = ansiClassCache.get(codeText);
  if (cached !== undefined) return cached;
  let bold = false;
  let italic = false;
  let underline = false;
  let fg = 0;
  let reset = false;
  let i = 0;
  const len = codeText.length;
  while (true) {
    let val = 0;
    let valid = true;
    let hasDigits = false;
    while (i < len) {
      const c = codeText.charCodeAt(i);
      if (c === SEMICOLON) break;
      if (c >= 0x30 && c <= 0x39) {
        val = val * 10 + (c - 0x30);
        hasDigits = true;
      } else {
        valid = false;
      }
      i += 1;
    }
    if (valid) {
      const code = hasDigits ? val : 0;
      if (code === 0) {
        reset = true;
        break;
      }
      if (code === 1) bold = true;
      else if (code === 3) italic = true;
      else if (code === 4) underline = true;
      else if (fg === 0 && ((code >= 30 && code <= 37) || (code >= 90 && code <= 97))) fg = code;
    }
    if (i >= len) break;
    i += 1;
  }
  let out = '';
  if (!reset) {
    if (bold) out = 'ansi-bold';
    if (italic) out = out ? `${out} ansi-italic` : 'ansi-italic';
    if (underline) out = out ? `${out} ansi-underline` : 'ansi-underline';
    if (fg !== 0) out = out ? `${out} ansi-fg-${fg}` : `ansi-fg-${fg}`;
  }
  if (ansiClassCache.size >= ANSI_CLASS_CACHE_MAX) ansiClassCache.clear();
  ansiClassCache.set(codeText, out);
  return out;
}

// What a scan reports: text between forms, and each SGR's parameters.
interface ScanVisitor {
  text(from: number, to: number): void;
  sgr(params: string): void;
  incompleteEscape?(): void;
}

// Whether an unrecognized ESC at `at` reached EOF in a state that further
// appended bytes could complete into a valid OSC, APC or SGR sequence.
function isIncompleteEscapeAt(input: string, at: number): boolean {
  const len = input.length;
  if (at + 1 >= len) return true;
  const kind = input.charCodeAt(at + 1);
  if (kind === CLOSE_BRACKET || kind === UNDERSCORE) {
    for (let i = at + 2; i < len; i += 1) {
      const c = input.charCodeAt(i);
      if (c === LF) return false;
      if (c === ESC) return i + 1 === len;
      if (kind === CLOSE_BRACKET && c === BEL) return false;
    }
    return true;
  }
  if (kind === OPEN_BRACKET) {
    for (let i = at + 2; i < len; i += 1) {
      const c = input.charCodeAt(i);
      if ((c >= 0x30 && c <= 0x39) || c === SEMICOLON) continue;
      return false;
    }
    return true;
  }
  return false;
}

// The end (exclusive) of the OSC, APC or SGR starting at the ESC at `at`,
// else -1. `steps` counts the characters it read.
function formEnd(input: string, at: number, steps: { n: number }): number {
  const kind = input.charAt(at + 1);
  let i = at + 2;
  if (kind === ']') {
    // OSC: to BEL or ST, never past a newline or another ESC.
    for (; i < input.length; i += 1) {
      steps.n += 1;
      const c = input.charCodeAt(i);
      if (c === BEL) return i + 1;
      if (c === ESC) return input.charCodeAt(i + 1) === BACKSLASH ? i + 2 : -1;
      if (c === LF) return -1;
    }
    return -1;
  }
  if (kind === '_') {
    // APC: to ST, never past a newline or another ESC.
    for (; i < input.length; i += 1) {
      steps.n += 1;
      const c = input.charCodeAt(i);
      if (c === ESC) return input.charCodeAt(i + 1) === BACKSLASH ? i + 2 : -1;
      if (c === LF) return -1;
    }
    return -1;
  }
  if (kind === '[') {
    // SGR: digits and semicolons, then `m`.
    for (; i < input.length; i += 1) {
      steps.n += 1;
      const c = input.charCodeAt(i);
      if ((c >= 0x30 && c <= 0x39) || c === SEMICOLON) continue;
      return c === LOWER_M ? i + 1 : -1;
    }
    return -1;
  }
  return -1;
}

// The scan. Returns the characters it read.
function scanAnsi(input: string, visit: ScanVisitor): number {
  const steps = { n: 0 };
  let text = 0;
  for (let i = input.indexOf('\x1b'); i >= 0; i = input.indexOf('\x1b', i)) {
    steps.n += 1;
    const end = formEnd(input, i, steps);
    if (end < 0) {
      if (visit.incompleteEscape && isIncompleteEscapeAt(input, i)) {
        visit.incompleteEscape();
      }
      i += 1;
      continue;
    }
    if (i > text) visit.text(text, i);
    text = end;
    if (input.charCodeAt(i + 1) === OPEN_BRACKET) visit.sgr(input.slice(i + 2, end - 1));
    i = end;
  }
  if (text < input.length) visit.text(text, input.length);
  return steps.n;
}

/** Tail state after rendering an ANSI string, used by AnsiText for O(delta) appends. */
export interface AnsiTailState {
  openClass: string;
  incompleteTail: boolean;
  endsWithCR: boolean;
}

function normalizeHtmlParserText(text: string): string {
  let out = text.indexOf('\r') < 0 ? text : text.replace(/\r\n?/g, '\n');
  if (out.indexOf('\0') >= 0) out = out.replace(/\0/g, '');
  return out;
}

/** The HTML AnsiText sets alongside its tail state for incremental appends. */
export function ansiHtmlWithState(input: string): { html: string; state: AnsiTailState } {
  let out = '';
  let openClass = '';
  let incompleteTail = false;
  let endsWithCR = false;
  scanAnsi(input, {
    text: (from, to) => {
      if (to > from) {
        out += escapeHtml(input.slice(from, to));
        endsWithCR = input.charCodeAt(to - 1) === CR;
      }
    },
    sgr: (params) => {
      const wasOpen = openClass !== '';
      if (wasOpen) out += '</span>';
      openClass = ansiClass(params);
      const isOpen = openClass !== '';
      if (isOpen) out += `<span class="${openClass}">`;
      if (wasOpen || isOpen) endsWithCR = false;
    },
    incompleteEscape: () => {
      incompleteTail = true;
    },
  });
  return {
    html: openClass !== '' ? `${out}</span>` : out,
    state: {
      openClass,
      incompleteTail,
      endsWithCR,
    },
  };
}

/**
 * Appends `delta` directly to `root` when the prior tail ended cleanly.
 * Returns the next tail state on success, or null when the caller must fall
 * back to full morphing. Preserves existing Text and Span node identity so
 * active selections never break.
 */
export function appendAnsiDeltaToDom(
  root: HTMLElement,
  delta: string,
  prevState: AnsiTailState,
): AnsiTailState | null {
  if (delta.length === 0) return prevState;
  if (prevState.incompleteTail) return null;
  if (prevState.endsWithCR && delta.charCodeAt(0) === LF) return null;

  const doc = root.ownerDocument;
  let openClass = prevState.openClass;
  let activeSpan: HTMLSpanElement | null = null;
  if (openClass !== '') {
    const last = root.lastChild;
    if (!(last instanceof HTMLSpanElement) || last.className !== openClass) {
      return null;
    }
    activeSpan = last;
  }

  let incompleteTail = false;
  const appendText = (raw: string): void => {
    const text = normalizeHtmlParserText(raw);
    if (text.length === 0) return;
    const container: Node = activeSpan ?? root;
    const last = container.lastChild;
    if (last && last.nodeType === 3 /* Node.TEXT_NODE */) {
      (last as Text).appendData(text);
    } else {
      container.appendChild(doc.createTextNode(text));
    }
  };

  if (delta.indexOf('\x1b') < 0) {
    appendText(delta);
    return {
      openClass,
      incompleteTail,
      endsWithCR: delta.charCodeAt(delta.length - 1) === CR,
    };
  }

  // Buffer contiguous text across non-SGR escapes (OSC/APC) and no-op SGRs
  // so `\r` + OSC + `\n` collapses to one `\n` just like `ansiHtml`/`ansiShownText`.
  let pendingCR = prevState.endsWithCR;
  let pendingText = '';
  let crLfSplit = false;
  const flushPendingText = (): void => {
    if (pendingText.length === 0) return;
    if (pendingCR && pendingText.charCodeAt(0) === LF) {
      crLfSplit = true;
      pendingText = '';
      return;
    }
    pendingCR = pendingText.charCodeAt(pendingText.length - 1) === CR;
    appendText(pendingText);
    pendingText = '';
  };

  scanAnsi(delta, {
    text: (from, to) => {
      if (crLfSplit || to <= from) return;
      if (pendingCR && pendingText.length === 0 && delta.charCodeAt(from) === LF) {
        crLfSplit = true;
        return;
      }
      pendingText += delta.slice(from, to);
    },
    sgr: (params) => {
      if (crLfSplit) return;
      const nextClass = ansiClass(params);
      if (openClass === '' && nextClass === '') return;
      flushPendingText();
      if (crLfSplit) return;
      pendingCR = false;
      openClass = nextClass;
      if (openClass !== '') {
        const span = doc.createElement('span');
        span.className = openClass;
        root.appendChild(span);
        activeSpan = span;
      } else {
        activeSpan = null;
      }
    },
    incompleteEscape: () => {
      incompleteTail = true;
    },
  });
  if (crLfSplit) return null;
  flushPendingText();
  if (crLfSplit) return null;

  return {
    openClass,
    incompleteTail,
    endsWithCR: pendingCR,
  };
}

/** The HTML AnsiText sets: escaped text and colour spans. */
export function ansiHtml(input: string): string {
  return ansiHtmlWithState(input).html;
}

/** The text AnsiText shows for `input`, as its DOM reads: the HTML parser
 * turns each CR LF or lone CR between tags into LF and drops NUL. */
export function ansiShownText(input: string): string {
  if (input.indexOf('\x1b') < 0 && input.indexOf('\r') < 0 && input.indexOf('\0') < 0) return input;
  let out = '';
  let run = '';
  const flush = () => {
    out += run.indexOf('\r') < 0 ? run : run.replace(/\r\n?/g, '\n');
    run = '';
  };
  let open = false;
  scanAnsi(input, {
    text: (from, to) => {
      run += input.slice(from, to);
    },
    sgr: (params) => {
      // A span's close or open is a tag, which ends the parser's run.
      const opens = ansiClass(params) !== '';
      if (open || opens) flush();
      open = opens;
    },
  });
  flush();
  return out.indexOf('\0') < 0 ? out : out.replace(/\0/g, '');
}

/** The characters the scan reads for `input`: at most twice its length. */
export function ansiScanStepsForTest(input: string): number {
  return scanAnsi(input, { text: () => {}, sgr: () => {} });
}
