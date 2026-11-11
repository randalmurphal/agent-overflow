// The text a settled ChatMarkdown shows, as the runs find matches within
// (utils/findHighlights.ts): one string per run of text under one block
// element, in document order. A find that counts markdown it has not
// mounted counts this, so a match it counts is a match the reader sees
// and the highlighter paints. markdownFindText.browser.test.ts holds it to
// the rendered DOM.
//
// Not shown, so not searched: markup, image alt text, link targets and the
// body of a <details> that is not open. Neither searched nor painted
// (`data-find-skip` on their hosts): Mermaid diagrams, math, and forge
// attachment chips, whose text is drawn or fetched after the render.

import {
  EMBEDDED_HTML_EXTENSIONS,
  lex,
  parseBlocks,
  sanitizeEmbeddedHtmlToken,
  type Extension,
} from '../../../markdown';
import { findTextSegments } from '../../../utils/findHighlights';
import { buildForgeAttachmentExtension } from '../../../utils/forgeAttachmentExtension';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { MarkdownFenceUnwrapper } from './unwrapMarkdownFence';

export interface MarkdownFindTextOptions {
  /** ChatMarkdown's `embeddedHtml`. */
  embeddedHtml: boolean;
  /** The PR whose attachments ChatMarkdown claims (FORGE_ATTACHMENT_SOURCE_CONTEXT). */
  forgeSource?: ForgeAttachmentSource | null;
}

export interface MarkdownFindText {
  readonly runs: readonly string[];
  /** Whether each <details>, in document order, renders open. */
  readonly details: readonly boolean[];
}

// Tokens that render as an element in findHighlights' block list.
const BLOCK_TOKENS = new Set([
  'paragraph', 'heading', 'blockquote', 'list', 'list_item', 'table', 'tr', 'td', 'th',
  'details', 'detailsSummary', 'descriptionList', 'descriptionTerm', 'descriptionDetail', 'hr',
]);

// Tokens whose text never reaches the DOM as searchable text.
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

function extensionsFor(options: MarkdownFindTextOptions): Extension[] {
  const list: Extension[] = [];
  const forge = options.forgeSource;
  if (forge && (forge.pr.forge === 'github' || forge.pr.forge === 'gitlab')) {
    list.push(...buildForgeAttachmentExtension({ ...forge, embeddedHtml: options.embeddedHtml }));
  }
  if (options.embeddedHtml) list.push(...EMBEDDED_HTML_EXTENSIONS);
  return list;
}

/**
 * The runs of text `source` renders as, under ChatMarkdown's options.
 * `open` overrides the open state of each <details> by document order,
 * as the reader left them.
 */
export function markdownFindText(source: string, options: MarkdownFindTextOptions, open?: readonly boolean[]): MarkdownFindText {
  const shown = new MarkdownFenceUnwrapper().render(source, false);
  const extensions = extensionsFor(options);
  const runs: string[] = [];
  const details: boolean[] = [];
  let run = '';
  const flush = () => {
    if (run !== '') runs.push(run);
    run = '';
  };
  // Records a <details> and answers whether it shows its body.
  const opened = (initial: boolean): boolean => {
    details.push(initial);
    return open?.[details.length - 1] ?? initial;
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
    const fragment = doc.body.firstElementChild!;
    for (const element of fragment.querySelectorAll('details')) element.open = opened(element.open);
    runs.push(...findTextSegments(fragment, false));
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
        const show = opened(token.open === true);
        const [summary, ...body] = token.tokens ?? [];
        if (summary?.type === 'detailsSummary') visit(summary);
        else if (summary) body.unshift(summary);
        if (show) walk(body);
        else countDetails(body);
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
  // A closed body still holds <details> the DOM numbers.
  const countDetails = (tokens: readonly TokenLike[] | undefined) => {
    for (const token of tokens ?? []) {
      if (token.type === 'details') opened(token.open === true);
      else if (token.type === 'html' && token.block && options.embeddedHtml) {
        const out = sanitizeEmbeddedHtmlToken({ raw: token.raw ?? '', block: true });
        const doc = new DOMParser().parseFromString(`<div>${out}</div>`, 'text/html');
        for (const element of doc.querySelectorAll('details')) opened(element.open);
        continue;
      }
      countDetails(token.tokens);
    }
  };
  for (const block of parseBlocks(shown, extensions)) {
    walk(lex(block, extensions) as unknown as TokenLike[]);
    flush();
  }
  return { runs, details };
}

/** Holds `markdownFindText` per source while the sources stay the same;
 * a source with <details> the reader toggled is read fresh. */
export function createMarkdownFindTextCache(options: MarkdownFindTextOptions): (source: string, open?: readonly boolean[]) => MarkdownFindText {
  let held = new Map<string, MarkdownFindText>();
  let used = new Map<string, MarkdownFindText>();
  return (source, open) => {
    if (open) return markdownFindText(source, options, open);
    let text = used.get(source) ?? held.get(source);
    if (!text) text = markdownFindText(source, options);
    used.set(source, text);
    if (used.size >= 512) {
      held = used;
      used = new Map();
    }
    return text;
  };
}
