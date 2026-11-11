// ANSI-coloured text as AnsiText.svelte renders it: the one reading of a
// string's escapes, so what the view shows and what find counts in it are
// the same text. OSC (`ESC ]` … BEL or ST) and APC (`ESC _` … ST) strings
// within a line are hidden, SGR sequences (`ESC [` params `m`) become
// colour spans, and every other character, other escapes included, is
// text. A string never hides across a newline, so a run of lines shows
// each line's text. The backend's search
// (internal/cilog/search.go `hiddenEscapes`) hides the same; the vectors in
// src/test/fixtures/ansiShownVectors.json hold both sides to it.

import { escapeHtml } from './markdownRender';

/** A piece of rendered output: text, or the class a colour span opens
 * (`open`) or a span's close (`close`). */
type Piece = { text: string } | { open: string } | { close: true };

// The body of an OSC or APC string is a negated class that excludes the
// terminator's lead bytes (ESC / BEL) rather than a lazy `[\s\S]*?`: a lazy
// match against an alternation terminator backtracks toward O(n²) on
// pathological input (thousands of unterminated `ESC ]` starts each
// rescanning to the end), while the negated class scans forward once. Real
// OSC payloads (window titles, OSC-8 hyperlinks) never embed a bare ESC,
// so stopping at the first ESC/BEL matches terminal behaviour.
/* eslint-disable no-control-regex -- the escapes these read are control characters */
const OSC = /\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)/g;
const APC = /\x1b_[^\x1b\n]*\x1b\\/g;
const SGR = /\x1b\[([0-9;]*)m/g;
/* eslint-enable no-control-regex */

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

function pieces(input: string): Piece[] {
  const stripped = input.replace(OSC, '').replace(APC, '');
  const out: Piece[] = [];
  let open = false;
  let cursor = 0;
  SGR.lastIndex = 0;
  for (let match = SGR.exec(stripped); match !== null; match = SGR.exec(stripped)) {
    out.push({ text: stripped.slice(cursor, match.index) });
    if (open) {
      out.push({ close: true });
      open = false;
    }
    const className = ansiClass(match[1] ?? '');
    if (className) {
      out.push({ open: className });
      open = true;
    }
    cursor = SGR.lastIndex;
  }
  out.push({ text: stripped.slice(cursor) });
  if (open) out.push({ close: true });
  return out;
}

/** The HTML AnsiText sets: escaped text and colour spans. */
export function ansiHtml(input: string): string {
  return pieces(input)
    .map((piece) => ('text' in piece ? escapeHtml(piece.text) : 'open' in piece ? `<span class="${piece.open}">` : '</span>'))
    .join('');
}

/** The text AnsiText shows for `input`, as its DOM reads: the HTML parser
 * turns each CR LF or lone CR between tags into LF and drops NUL. */
export function ansiShownText(input: string): string {
  let out = '';
  let run = '';
  const flush = () => {
    out += run.replace(/\r\n?/g, '\n');
    run = '';
  };
  for (const piece of pieces(input)) {
    if ('text' in piece) run += piece.text;
    else flush();
  }
  flush();
  return out.replace(/\0/g, '');
}
