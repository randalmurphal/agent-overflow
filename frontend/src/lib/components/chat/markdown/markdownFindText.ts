// The text a settled ChatMarkdown shows, as the runs find matches within
// (utils/findHighlights.ts): one string per run of text under one block
// element, in document order. A find that counts markdown it has not
// mounted counts this, so a match it counts is a match the reader sees
// and the highlighter paints. markdownFindText.browser.test.ts holds it to
// the rendered DOM.
//
// Not shown, so not searched: markup, image alt text, link targets, the
// body of a <details> that is not open, and the drawn text of a Mermaid
// diagram or a math formula.

import {
  EMBEDDED_HTML_EXTENSIONS,
  lex,
  parseBlocks,
  sanitizeEmbeddedHtmlToken,
} from '../../../markdown';
import { findTextSegments } from '../../../utils/findHighlights';
import { MarkdownFenceUnwrapper } from './unwrapMarkdownFence';

export interface MarkdownFindTextOptions {
  /** ChatMarkdown's `embeddedHtml`. */
  embeddedHtml: boolean;
}

// Tokens that render as an element in findHighlights' block list.
const BLOCK_TOKENS = new Set([
  'paragraph', 'heading', 'blockquote', 'list', 'list_item', 'table', 'tr', 'td', 'th',
  'details', 'detailsSummary', 'descriptionList', 'descriptionTerm', 'descriptionDetail', 'hr',
]);

// Tokens whose text never reaches the DOM as text.
const SILENT_TOKENS = new Set(['image', 'br', 'space', 'def', 'footnote', 'math']);

interface TokenLike {
  type: string;
  raw?: string;
  text?: string;
  block?: boolean;
  open?: boolean;
  lang?: string;
  label?: string;
  tokens?: TokenLike[];
}

/** The runs of text `source` renders as, under ChatMarkdown's options. */
export function markdownFindText(source: string, options: MarkdownFindTextOptions): string[] {
  const shown = new MarkdownFenceUnwrapper().render(source, false);
  const extensions = options.embeddedHtml ? EMBEDDED_HTML_EXTENSIONS : [];
  const runs: string[] = [];
  let run = '';
  const flush = () => {
    if (run !== '') runs.push(run);
    run = '';
  };
  const html = (token: TokenLike) => {
    if (!options.embeddedHtml) return;
    const out = sanitizeEmbeddedHtmlToken({ raw: token.raw ?? '', block: token.block });
    if (out === '') return;
    if (!token.block) {
      // A stray inline tag renders as its own literal text.
      run += token.raw ?? '';
      return;
    }
    flush();
    const doc = new DOMParser().parseFromString(`<div>${out}</div>`, 'text/html');
    runs.push(...findTextSegments(doc.body.firstElementChild!, false));
  };
  const walk = (tokens: readonly TokenLike[] | undefined) => {
    for (const token of tokens ?? []) visit(token);
  };
  const visit = (token: TokenLike) => {
    if (SILENT_TOKENS.has(token.type)) return;
    switch (token.type) {
      case 'code':
        flush();
        if (token.lang !== 'mermaid') runs.push(token.text ?? '');
        return;
      case 'html':
        html(token);
        return;
      case 'codespan':
      case 'escape':
        run += token.text ?? '';
        return;
      case 'footnoteRef':
        if (token.label && token.label !== 'streamdown:footnote') run += token.label.replace('^', '');
        return;
      case 'inline-citations':
        run += token.text ?? token.raw ?? '';
        return;
      case 'details': {
        flush();
        const [summary, ...body] = token.tokens ?? [];
        if (summary?.type === 'detailsSummary') visit(summary);
        else if (summary) body.unshift(summary);
        if (token.open) walk(body);
        flush();
        return;
      }
      case 'alert':
        flush();
        // The title shows the variant (ChatMarkdown passes no translations).
        runs.push(String((token as { variant?: string }).variant ?? ''));
        walk(token.tokens);
        flush();
        return;
      case 'text':
        if (token.tokens && token.tokens.length > 0) walk(token.tokens);
        else run += token.text ?? '';
        return;
    }
    const block = BLOCK_TOKENS.has(token.type);
    if (block) flush();
    if (token.tokens && token.tokens.length > 0) walk(token.tokens);
    else if (!block && token.text !== undefined && token.type !== 'hr') run += token.text;
    if (block) flush();
  };
  for (const block of parseBlocks(shown, extensions)) {
    walk(lex(block, extensions) as unknown as TokenLike[]);
    flush();
  }
  return runs;
}

/** Holds `markdownFindText` per source while the sources stay the same. */
export function createMarkdownFindTextCache(options: MarkdownFindTextOptions): (source: string) => readonly string[] {
  let held = new Map<string, readonly string[]>();
  let used = new Map<string, readonly string[]>();
  return (source) => {
    let runs = used.get(source) ?? held.get(source);
    if (!runs) runs = markdownFindText(source, options);
    used.set(source, runs);
    if (used.size >= 512) {
      held = used;
      used = new Map();
    }
    return runs;
  };
}
