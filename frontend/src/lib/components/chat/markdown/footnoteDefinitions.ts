// Document-level footnote lookup for the rendered transcript.
//
// `[^1]` renders as a chip inline with the prose; its `[^1]: body`
// definition renders nowhere — the parser drops the block, and the
// renderer's per-block lexers never connect the two (markdown/AGENTS.md
// § Host seams). So the body cannot be recovered from the DOM,
// and it cannot be recovered from the ref token either. It is recovered
// from the SOURCE, which is canonical anyway.
//
// Two halves:
//   - a registry: each rendered `.markdown-body` root publishes a reader
//     for the markdown it was rendered from. A WeakMap keyed on the root
//     element, so a surface that unmounts (or a virtualizer row that
//     recycles) drops its entry with the node, and nothing here has to
//     be told about a thread switch.
//   - a resolver, run on the CLICK and not before. A full lex is not
//     something to pay per render, and a message with footnotes that
//     nobody clicks pays nothing at all.

import { lexFootnoteDefinitions, type Footnote } from '../../../markdown';

/** The rendered root → the markdown it was rendered from. */
const sources = new WeakMap<HTMLElement, () => string>();

/**
 * Publish a rendered markdown root's source. Returns the unregister
 * function, so the caller can hand it straight back to `{@attach}` /
 * `$effect`. Re-registering the same root replaces the reader.
 */
export function registerFootnoteSource(
  root: HTMLElement,
  read: () => string,
): () => void {
  sources.set(root, read);
  return () => {
    if (sources.get(root) === read) sources.delete(root);
  };
}

// One-entry memo, keyed on the source string's IDENTITY. A reader
// clicking several chips in one message pays one lex; moving to another
// message replaces the entry rather than growing a cache nobody evicts.
// `null` is a real answer (the source declares no footnotes) and is
// memoized like any other.
let memoSource: string | null = null;
let memoDefinitions: Map<string, Footnote> | null = null;

// Cheap veto before the lex: does this source declare a footnote at all?
// Mirrors the renderer's own `FOOTNOTE_DEF_LINE` guard — a textual test
// that can only over-approximate (a `[^x]:` line inside a fence passes
// it), which is exactly what a veto is allowed to do. The lex behind it
// is the authority.
const FOOTNOTE_DEF_LINE = /(^|\n)[ \t]*\[\^[^\]\n]+\]:/;

function definitionsFor(source: string): Map<string, Footnote> | null {
  if (source === memoSource) return memoDefinitions;
  memoSource = source;
  memoDefinitions = FOOTNOTE_DEF_LINE.test(source)
    ? lexFootnoteDefinitions(source)
    : null;
  return memoDefinitions;
}

/**
 * The markdown body of the definition a footnote chip refers to, or null
 * when the document defines no such label — a dangling `[^1]` stays the
 * inert marker it renders as. Every chip carrying the same label resolves
 * to the same definition, because the lookup is by label over the whole
 * document.
 */
export function resolveFootnoteBody(chip: HTMLElement): string | null {
  const label = chip.dataset.footnoteLabel;
  if (!label) return null;
  // `.markdown-body` is `ChatMarkdown`'s own wrapper — the same marker the
  // markdown copy delegate keys on — so the walk stops at the surface that
  // rendered this chip and never crosses into a neighbouring message.
  const root = chip.closest<HTMLElement>('.markdown-body');
  if (!root) return null;
  return resolveFootnoteBodyAt(root, label);
}

/**
 * The same lookup against an explicit rendered root. The popup host uses
 * it for a footnote ref INSIDE a footnote body: that chip's nearest
 * `.markdown-body` is the popup's, whose registered source is just the
 * body being shown, so a chained `[^b]` must resolve against the
 * document root the ORIGINAL chip came from.
 */
export function resolveFootnoteBodyAt(
  root: HTMLElement,
  label: string,
): string | null {
  const read = sources.get(root);
  if (!read) return null;
  const body = definitionsFor(read())?.get(label)?.lines.join('\n').trim();
  return body ? body : null;
}
