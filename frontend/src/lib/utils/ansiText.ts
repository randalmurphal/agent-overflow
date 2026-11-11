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
const BACKSLASH = 0x5c;
const SEMICOLON = 0x3b;
const OPEN_BRACKET = 0x5b;
const LOWER_M = 0x6d;

function ansiClass(codeText: string): string {
  const codes = codeText
    .split(';')
    .map((code) => Number(code || 0))
    .filter((code) => Number.isFinite(code));
  if (codes.includes(0)) return '';
  const classes: string[] = [];
  if (codes.includes(1)) classes.push('ansi-bold');
  if (codes.includes(3)) classes.push('ansi-italic');
  if (codes.includes(4)) classes.push('ansi-underline');
  const foreground = codes.find((code) => (code >= 30 && code <= 37) || (code >= 90 && code <= 97));
  if (foreground) classes.push(`ansi-fg-${foreground}`);
  return classes.join(' ');
}

// What a scan reports: text between forms, and each SGR's parameters.
interface ScanVisitor {
  text(from: number, to: number): void;
  sgr(params: string): void;
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

/** The HTML AnsiText sets: escaped text and colour spans. */
export function ansiHtml(input: string): string {
  let out = '';
  let open = false;
  scanAnsi(input, {
    text: (from, to) => {
      out += escapeHtml(input.slice(from, to));
    },
    sgr: (params) => {
      if (open) out += '</span>';
      const className = ansiClass(params);
      open = className !== '';
      if (open) out += `<span class="${className}">`;
    },
  });
  return open ? `${out}</span>` : out;
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
