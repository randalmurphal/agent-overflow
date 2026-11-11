// The text a settled ChatMarkdown shows, as the runs find matches within
// (utils/findHighlights.ts): one string per run of text under one block
// element, in document order. A find that counts markdown it has not
// mounted counts this, so a match it counts is a match the reader sees
// and the highlighter paints. markdownFindText.browser.test.ts holds it to
// the rendered DOM.
//
// It reads the lexer's tokens, not a render: the static renderer gives up
// (to Svelte components) on every token a review comment leans on (HTML,
// code, images, alerts, details, footnotes, citations, math, MDX), and
// mounting the components offscreen would run their highlight and
// attachment reads. So each token kind says what it shows in SHOWS, a
// record the type check holds to the lexer's whole token union; a kind it
// does not know renders nothing. Where the renderers decide what a token
// shows, this asks the same function (`tableCellRenders`, `linkTagText`).
//
// Not shown, so not searched: markup, image alt text, link targets and
// titles, a table cell merged into the one above, an `[align]` block (no
// renderer handles it), and the body of a <details> that is not open.
// Neither searched nor painted (`data-find-skip` on their hosts): Mermaid
// diagrams, math, and forge attachment chips, whose text is drawn or
// fetched after the render.

import {
  EMBEDDED_HTML_EXTENSIONS,
  lex,
  linkTagText,
  parseBlocks,
  sanitizeEmbeddedHtmlToken,
  tableCellRenders,
  type Extension,
  type StreamdownToken,
} from '../../../markdown';
import { CHAT_LINK_PREFIXES } from './chatLinkPrefixes';
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

interface TokenLike {
  type: string;
  raw?: string;
  text?: string;
  block?: boolean;
  open?: boolean;
  lang?: string;
  label?: string;
  href?: string;
  rowspan?: number;
  variant?: string;
  tokens?: TokenLike[];
}

/** How a token kind shows: no text; its children (or its own text) in
 * the run it sits in; its children in a run of their own, as an element
 * of findHighlights' block list; or by its own rule. */
type Shows = 'nothing' | 'inline' | 'block' | 'own';

/** Every kind the lexer can produce, as ChatMarkdown renders it. */
export const SHOWS: Record<StreamdownToken['type'], Shows> = {
  // Drawn or fetched after the render, or not text at all.
  image: 'nothing', br: 'nothing', space: 'nothing', def: 'nothing', footnote: 'nothing', math: 'nothing', hr: 'block',
  // Element.svelte hands an `[align]` block to a children handler
  // ChatMarkdown does not give, and the static path gives up on it.
  align: 'nothing',
  paragraph: 'block', heading: 'block', blockquote: 'block', list: 'block', list_item: 'block',
  table: 'block', thead: 'inline', tbody: 'inline', tfoot: 'inline', tr: 'block',
  descriptionList: 'block', description: 'inline', descriptionTerm: 'block', descriptionDetail: 'block',
  detailsSummary: 'block',
  strong: 'inline', em: 'inline', del: 'inline', sub: 'inline', sup: 'inline', mdx: 'inline',
  code: 'own', html: 'own', codespan: 'own', escape: 'own', footnoteRef: 'own', 'inline-citations': 'own',
  details: 'own', link: 'own', td: 'own', th: 'own', alert: 'own', text: 'own',
};

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
    const shows: Shows = SHOWS[token.type as StreamdownToken['type']] ?? 'nothing';
    if (shows === 'nothing') return;
    if (shows === 'own') {
      own(token);
      return;
    }
    const block = shows === 'block';
    if (block) flush();
    if (token.tokens && token.tokens.length > 0) walk(token.tokens);
    else if (!block && token.text !== undefined) run += token.text;
    if (block) flush();
  };
  const own = (token: TokenLike) => {
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
        // ChatMarkdown's inlineCitation snippet shows the token's text.
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
      case 'link':
        walk(token.tokens);
        run += linkTagText(token.href, [...CHAT_LINK_PREFIXES]);
        return;
      case 'td':
      case 'th':
        if (!tableCellRenders({ rowspan: token.rowspan ?? 1 })) return;
        flush();
        walk(token.tokens);
        flush();
        return;
      case 'alert':
        flush();
        // The title shows the variant (ChatMarkdown passes no translations).
        runs.push(String(token.variant ?? ''));
        walk(token.tokens);
        flush();
        return;
      case 'text':
        if (token.tokens && token.tokens.length > 0) walk(token.tokens);
        else run += token.text ?? '';
        return;
    }
    throw new Error(`markdownFindText: no rule for ${token.type}`);
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
