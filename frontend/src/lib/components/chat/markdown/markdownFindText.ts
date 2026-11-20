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
// An html region renders as one sanitized fragment with its Markdown islands
// in place: its runs are the fragment's, each island's runs where its
// placeholder sits, and its <details>, authored or island tokens, recorded
// in the fragment's document order.
//
// `[center]`/`[right]` markers show nothing and the blocks between them
// show as they would unaligned; a block string is lexed alone, so markers
// are the ones the block parse found, as Streamdown reads them. A forge's
// grammar has no markers.
//
// Not shown, so not searched: markup, image alt text, link targets and
// titles, a table cell merged into the one above, and the body of a
// <details> that is not open.
// Neither searched nor painted (`data-find-skip` on their hosts): Mermaid
// diagrams, math, and forge attachment chips, whose text is drawn or
// fetched after the render, and a blocked link's tag, the link policy's
// verdict rather than the message's text. Each ends the run it sits in, as
// an image and a line break do, so a match never spans one
// (findHighlights.ts).

import {
  blockAlignment,
  createDocumentContextCache,
  createParseBlocksCache,
  documentContextOf,
  fenceLanguage,
  lex,
  linkTagText,
  MEDIA_CLAIM_ATTR,
  parseBlocks,
  parseHtmlRegion,
  sanitizeEmbeddedHtmlToken,
  tableCellRenders,
  type DocumentContext,
  type DocumentReading,
  type ForgeFactReader,
  type ParseBlocksCache,
  type StreamdownToken,
} from '../../../markdown';
import { CHAT_LINK_PREFIXES } from './chatLinkPrefixes';
import { chatMarkdownExtensions, chatMarkdownForgeOptions, chatMarkdownOptionsGeneration } from './chatMarkdownOptions';
import { findRegionRuns, findTextRuns, type FindRun } from '../../../utils/findHighlights';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { MarkdownFenceUnwrapper } from './unwrapMarkdownFence';

export interface MarkdownFindTextOptions {
  /** ChatMarkdown's `embeddedHtml`. */
  embeddedHtml: boolean;
  /** The PR whose attachments ChatMarkdown claims (FORGE_ATTACHMENT_SOURCE_CONTEXT). */
  forgeSource?: ForgeAttachmentSource | null;
  /** Fenced blocks ChatMarkdown's `codeClaim` renders as something find
   * skips (codeClaim.ts), by their info string. */
  claimsCode?: (lang: string) => boolean;
  /** The PR's facts as of now, as ChatMarkdown reads them from the same
   * source (`ForgeAttachmentSource.facts`). */
  facts?: () => ForgeFactReader | null;
}

export interface MarkdownFindText {
  /** The runs shown. */
  readonly runs: readonly string[];
  /** Every run in walk order, shown or held back in a closed <details>. */
  readonly all: readonly FindRun[];
  /** Whether each <details>, in document order, renders open. */
  readonly details: readonly boolean[];
  /** Each fact the projection read, with its signature then: the text
   * stands while every one still signs the same. */
  readonly factReads: readonly (readonly [string, string])[];
}

interface TokenLike {
  type: string;
  raw?: string;
  text?: string;
  block?: boolean;
  open?: boolean;
  lang?: string;
  label?: string;
  number?: number;
  href?: string;
  rowspan?: number;
  variant?: string;
  title?: string;
  scope?: string | null;
  name?: string;
  tokens?: TokenLike[];
  parts?: (string | TokenLike)[];
}

/** How a token kind shows: no text; its children (or its own text) in
 * the run it sits in; its children in a run of their own, as an element
 * of findHighlights' block list; or by its own rule. */
type Shows = 'nothing' | 'inline' | 'block' | 'own';

/** Every kind the lexer can produce, as ChatMarkdown renders it. */
export const SHOWS: Record<StreamdownToken['type'], Shows> = {
  // Drawn or fetched after the render, or not text at all.
  space: 'nothing', def: 'nothing', footnote: 'nothing', hr: 'block',
  // Skipped, and the end of the run around them; a line break ends it too.
  image: 'own', math: 'own', br: 'own',
  align: 'nothing', paragraph: 'block', heading: 'block', blockquote: 'block', list: 'block', list_item: 'block',
  table: 'block', caption: 'block', checkbox: 'nothing', thead: 'inline', tbody: 'inline', tfoot: 'inline', tr: 'block',
  descriptionList: 'block', description: 'inline', descriptionTerm: 'block', descriptionDetail: 'block',
  // A file excerpt: its header paragraph and its code, as themselves.
  codeSnippet: 'block',
  detailsSummary: 'block',
  strong: 'inline', em: 'inline', del: 'inline', sub: 'inline', sup: 'inline', mdx: 'inline', htmlInline: 'inline', inlineDiff: 'inline',
  code: 'own', html: 'own', codespan: 'own', escape: 'own', footnoteRef: 'own', 'inline-citations': 'own',
  details: 'own', link: 'own', labelChip: 'own', td: 'own', th: 'own', alert: 'own', text: 'own', htmlRegion: 'own',
};

/**
 * The runs of text `source` renders as, under ChatMarkdown's options.
 * `open` overrides the open state of each <details> by document order,
 * as the reader left them.
 */
export function markdownFindText(source: string, options: MarkdownFindTextOptions, open?: readonly boolean[]): MarkdownFindText {
  const shown = new MarkdownFenceUnwrapper().render(source, false);
  const forge = chatMarkdownForgeOptions(options.forgeSource, options.embeddedHtml);
  const extensions = chatMarkdownExtensions({ forge });
  let all: FindRun[] = [];
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
  // The block being walked, whose scope its HTML's text reads under, as
  // the renderer's (render/elements/Element.svelte).
  let reading: DocumentReading | null = null;
  const html = (token: TokenLike) => {
    if (!options.embeddedHtml) return;
    const out = sanitizeEmbeddedHtmlToken({ raw: token.raw ?? '', block: token.block }, forge.html, reading);
    if (out === '') return;
    if (!token.block) {
      // A stray inline tag shows a void element, which ends the run as a
      // line break does, or its tag-filtered source text.
      const text = new DOMParser().parseFromString(out, 'text/html').body.textContent ?? '';
      if (text === '') flush();
      else add(text);
      return;
    }
    flush();
    const doc = new DOMParser().parseFromString(`<div>${out}</div>`, 'text/html');
    const fragment = doc.body.firstElementChild!;
    endRunsAtClaims(fragment);
    for (const element of fragment.querySelectorAll('details')) element.open = opened(element.open);
    for (const each of findTextRuns(fragment)) all.push({ text: each.text, shown: each.shown && hidden === 0 });
  };
  const region = (token: TokenLike) => {
    const islands = token.tokens ?? [];
    // Without embedded HTML a region shows its islands and no HTML.
    if (!options.embeddedHtml) {
      walk(islands);
      return;
    }
    flush();
    const parsed = parseHtmlRegion(token.parts ?? [], (each, at) => sanitizeEmbeddedHtmlToken(each, forge.html, at), reading);
    endRunsAtClaims(parsed.root);
    // Each island's runs, walked where it sits among the fragment's
    // <details> so every <details> is numbered in document order.
    const islandRuns = new Map<number, FindRun[]>();
    for (const element of parsed.root.querySelectorAll('details, div[data-markdown-island]')) {
      const index = parsed.islands.get(element);
      if (index === undefined) {
        if (element instanceof HTMLDetailsElement) element.open = opened(element.open);
        continue;
      }
      const outer = all;
      const outerHidden = hidden;
      all = [];
      hidden = 0;
      visit(islands[index]);
      flush();
      islandRuns.set(index, all);
      all = outer;
      hidden = outerHidden;
    }
    // Each placeholder, by identity, expands to its island's runs.
    for (const each of findRegionRuns(parsed.root, parsed.islands)) {
      const runs = 'island' in each ? (islandRuns.get(each.island) ?? []) : [each];
      for (const run of runs) all.push({ text: run.text, shown: run.shown && each.shown && hidden === 0 });
    }
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
      case 'image':
      case 'math':
      case 'br':
        flush();
        return;
      case 'code':
        flush();
        if (fenceLanguage(token.lang) !== 'mermaid' && !options.claimsCode?.(token.lang ?? '')) push(token.text ?? '');
        return;
      case 'html':
        html(token);
        return;
      case 'htmlRegion':
        region(token);
        return;
      case 'codespan':
      case 'escape':
        add(token.text ?? '');
        return;
      case 'footnoteRef':
        if (token.label && token.label !== 'streamdown:footnote') add(token.number !== undefined ? String(token.number) : token.label.replace('^', ''));
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
      case 'labelChip':
        add(`${token.scope ?? ''}${token.name ?? ''}`);
        return;
      case 'link':
        walk(token.tokens);
        if (linkTagText(token.href, [...CHAT_LINK_PREFIXES])) flush();
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
        // The title shows its own text or the variant (ChatMarkdown passes
        // no translations).
        push(String(token.title ?? token.variant ?? ''));
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
  // Each block lexes under its document scope, and a forge's footnotes
  // section follows the body, as ChatMarkdown renders them.
  // Alignment markers are the parse's, as Streamdown reads them.
  const blocksOf = (source: string): { blocks: readonly string[]; cache: ParseBlocksCache } => {
    const cache = createParseBlocksCache();
    return { blocks: parseBlocks(source, extensions, cache), cache };
  };
  const body = blocksOf(shown);
  const forgeName = options.forgeSource?.pr.forge;
  const documentForge = forgeName === 'github' || forgeName === 'gitlab' ? forgeName : null;
  // The facts a lex reads, through a reader that records each key.
  const facts = documentForge ? (options.facts?.() ?? null) : null;
  const read = new Set<string>();
  const recording: ForgeFactReader | null = facts && {
    get: (key) => {
      read.add(key);
      return facts.get(key);
    },
    signature: (key) => facts.signature(key),
  };
  const document = documentContextOf(body.cache, extensions, createDocumentContextCache(), documentForge, recording);
  const walkBlocks = ({ blocks, cache }: ReturnType<typeof blocksOf>, context: DocumentContext | null) => {
    for (const [index, block] of blocks.entries()) {
      if (blockAlignment(cache, index) === 'marker') continue;
      const scope = context?.scope(index) ?? null;
      reading = scope && { scope, reads: new Set() };
      walk(lex(block, extensions, scope) as unknown as TokenLike[]);
      flush();
    }
  };
  walkBlocks(body, document);
  if (document?.footnoteSection) walkBlocks(blocksOf(document.footnoteSection), document.after);
  const factReads = facts && read.size > 0 ? [...read].map((key) => [key, facts.signature(key)] as const) : NO_FACT_READS;
  return { runs: all.filter((each) => each.shown).map((each) => each.text), all, details, factReads };
}

const NO_FACT_READS: readonly (readonly [string, string])[] = Object.freeze([]);

/** Whether every fact `text` read still signs as it did. */
function factsHold(text: MarkdownFindText, facts: ForgeFactReader | null): boolean {
  for (const [key, signature] of text.factReads) if ((facts?.signature(key) ?? '') !== signature) return false;
  return true;
}

/** A claimed medium's host (ForgeAttachmentHost) is an image, a player or
 * a chip, each of which ends a run, whatever the bytes turn out to be. */
function endRunsAtClaims(root: Element): void {
  for (const marker of root.querySelectorAll(`[${MEDIA_CLAIM_ATTR}]`)) marker.setAttribute('data-find-skip', '');
}

/** Holds `markdownFindText` per source while the sources, the options'
 * generation and the facts each read stay the same; a source with
 * <details> the reader toggled is read fresh. */
export function createMarkdownFindTextCache(options: MarkdownFindTextOptions): (source: string, open?: readonly boolean[]) => MarkdownFindText {
  let held = new Map<string, MarkdownFindText>();
  let used = new Map<string, MarkdownFindText>();
  let generation = chatMarkdownOptionsGeneration();
  return (source, open) => {
    if (generation !== chatMarkdownOptionsGeneration()) {
      generation = chatMarkdownOptionsGeneration();
      held = new Map();
      used = new Map();
    }
    if (open) return markdownFindText(source, options, open);
    let text = used.get(source) ?? held.get(source);
    // A text whose facts changed is read again; one that read none stands.
    if (text && text.factReads.length > 0 && !factsHold(text, options.facts?.() ?? null)) text = undefined;
    if (!text) text = markdownFindText(source, options);
    used.set(source, text);
    if (used.size >= 512) {
      held = used;
      used = new Map();
    }
    return text;
  };
}
