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
// shows, this asks the same function (`tableCellRenders`, `linkTagText`),
// and it parses and sanitizes with the options ChatMarkdown builds
// (chatMarkdownOptions.ts): the same extensions, and the sanitizer's media
// claims, which drop a claimed video's fallback children. A review comment
// has no thread, workspace or path list, so no preview or path links.
// One walk yields both the text and every <details> in render order: the
// body of a closed <details> is walked for the elements it renders, with
// its text held back, so a token the renderer drops hides both. `all`
// keeps the held-back runs too, marked not shown, so each run has the same
// number in walk order whichever <details> are open.
//
// Not shown, so not searched: markup, image alt text, link targets and
// titles, a table cell merged into the one above, an `[align]` block (no
// renderer handles it), and the body of a <details> that is not open.
// Neither searched nor painted (`data-find-skip` on their hosts): Mermaid
// diagrams, math, and forge attachment chips, whose text is drawn or
// fetched after the render.

import {
  lex,
  linkTagText,
  parseBlocks,
  sanitizeEmbeddedHtmlToken,
  tableCellRenders,
  type StreamdownToken,
} from '../../../markdown';
import { CHAT_LINK_PREFIXES } from './chatLinkPrefixes';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from './chatMarkdownOptions';
import { findTextRuns, type FindRun } from '../../../utils/findHighlights';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { MarkdownFenceUnwrapper } from './unwrapMarkdownFence';

export interface MarkdownFindTextOptions {
  /** ChatMarkdown's `embeddedHtml`. */
  embeddedHtml: boolean;
  /** The PR whose attachments ChatMarkdown claims (FORGE_ATTACHMENT_SOURCE_CONTEXT). */
  forgeSource?: ForgeAttachmentSource | null;
}

export interface MarkdownFindText {
  /** The runs shown. */
  readonly runs: readonly string[];
  /** Every run in walk order, shown or held back in a closed <details>. */
  readonly all: readonly FindRun[];
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

/**
 * The runs of text `source` renders as, under ChatMarkdown's options.
 * `open` overrides the open state of each <details> by document order,
 * as the reader left them.
 */
export function markdownFindText(source: string, options: MarkdownFindTextOptions, open?: readonly boolean[]): MarkdownFindText {
  const shown = new MarkdownFenceUnwrapper().render(source, false);
  const forge = chatMarkdownForgeOptions(options.forgeSource, options.embeddedHtml);
  const extensions = chatMarkdownExtensions({ forge: forge.extensions, embeddedHtml: options.embeddedHtml });
  const all: FindRun[] = [];
  const details: boolean[] = [];
  let run = '';
  // How many closed <details> the walk is inside: rendered, not shown. A
  // run never straddles one: the walk flushes at each <details> edge.
  let hidden = 0;
  const flush = () => {
    if (run !== '') all.push({ text: run, shown: hidden === 0 });
    run = '';
  };
  const add = (text: string) => {
    run += text;
  };
  const push = (...texts: string[]) => {
    for (const text of texts) all.push({ text, shown: hidden === 0 });
  };
  // Records a <details> and answers whether it shows its body.
  const opened = (initial: boolean): boolean => {
    details.push(initial);
    return open?.[details.length - 1] ?? initial;
  };
  const html = (token: TokenLike) => {
    if (!options.embeddedHtml) return;
    const out = sanitizeEmbeddedHtmlToken({ raw: token.raw ?? '', block: token.block }, forge.html);
    if (out === '') return;
    if (!token.block) {
      // A stray inline tag renders as its own literal text.
      add(token.raw ?? '');
      return;
    }
    flush();
    const doc = new DOMParser().parseFromString(`<div>${out}</div>`, 'text/html');
    const fragment = doc.body.firstElementChild!;
    for (const element of fragment.querySelectorAll('details')) element.open = opened(element.open);
    for (const each of findTextRuns(fragment)) all.push({ text: each.text, shown: each.shown && hidden === 0 });
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
    else if (!block && token.text !== undefined) add(token.text);
    if (block) flush();
  };
  const own = (token: TokenLike) => {
    switch (token.type) {
      case 'code':
        flush();
        if (token.lang !== 'mermaid') push(token.text ?? '');
        return;
      case 'html':
        html(token);
        return;
      case 'codespan':
      case 'escape':
        add(token.text ?? '');
        return;
      case 'footnoteRef':
        if (token.label && token.label !== 'streamdown:footnote') add(token.label.replace('^', ''));
        return;
      case 'inline-citations':
        // ChatMarkdown's inlineCitation snippet shows the token's text.
        add(token.text ?? token.raw ?? '');
        return;
      case 'details': {
        flush();
        const show = opened(token.open === true);
        const [summary, ...body] = token.tokens ?? [];
        if (summary?.type === 'detailsSummary') visit(summary);
        else if (summary) body.unshift(summary);
        flush();
        if (!show) hidden += 1;
        walk(body);
        flush();
        if (!show) hidden -= 1;
        return;
      }
      case 'link':
        walk(token.tokens);
        add(linkTagText(token.href, [...CHAT_LINK_PREFIXES]));
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
        push(String(token.variant ?? ''));
        walk(token.tokens);
        flush();
        return;
      case 'text':
        if (token.tokens && token.tokens.length > 0) walk(token.tokens);
        else add(token.text ?? '');
        return;
    }
    throw new Error(`markdownFindText: no rule for ${token.type}`);
  };
  for (const block of parseBlocks(shown, extensions)) {
    walk(lex(block, extensions) as unknown as TokenLike[]);
    flush();
  }
  return { runs: all.filter((each) => each.shown).map((each) => each.text), all, details };
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
