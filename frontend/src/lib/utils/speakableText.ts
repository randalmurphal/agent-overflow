// Markdown → the pieces read-aloud speaks, in order.
//
// Parsed with the app's own lexer, like the clipboard serializer
// (`markdownHtmlSerialize.ts`), so what is read is what was rendered.
// What a listener gains nothing from is skipped silently: code blocks,
// display math, tables, raw HTML, images, bare URLs, footnote markers and
// emoji. Link text and inline code are kept as words; emphasis is
// flattened.
//
// Each piece is one sentence, so playback can resume at the start of the
// sentence it stopped in, and no utterance is long enough for an engine to
// stall on. A sentence past MAX_PIECE_CHARS is cut at clause punctuation,
// then at whitespace.

import { lex } from '../markdown';

/** The longest piece handed to a speech engine. */
export const MAX_PIECE_CHARS = 280;

type MdToken = {
  type: string;
  text?: string;
  href?: string;
  tokens?: MdToken[];
  isInline?: boolean;
};

/** The sentences `markdown` is read aloud as; empty when nothing is. */
export function speakablePieces(markdown: string): string[] {
  if (!markdown.trim()) return [];
  const blocks: string[] = [];
  collectBlocks(lex(markdown) as unknown as MdToken[], blocks);
  return blocks.flatMap((block) => splitSentences(block)).flatMap((sentence) => capPiece(sentence));
}

// Block types read as their own run of text. Anything else that carries
// children (blockquote, alert, list, align, a future extension) recurses.
const SILENT_BLOCKS = new Set(['code', 'table', 'html', 'def', 'space', 'hr']);

function collectBlocks(tokens: MdToken[], out: string[]): void {
  let inline: MdToken[] = [];
  const flush = (): void => {
    if (inline.length === 0) return;
    push(out, inlineText(inline));
    inline = [];
  };
  for (const token of tokens) {
    if (!isBlock(token)) {
      inline.push(token);
      continue;
    }
    flush();
    if (SILENT_BLOCKS.has(token.type) || token.type === 'math') continue;
    if (token.type === 'paragraph' || token.type === 'heading') {
      push(out, inlineText(token.tokens ?? []));
      continue;
    }
    collectBlocks(token.tokens ?? [], out);
  }
  flush();
}

const BLOCK_TYPES = new Set([
  'paragraph', 'heading', 'code', 'blockquote', 'alert', 'list', 'list_item', 'table', 'hr',
  'descriptionList', 'description', 'descriptionTerm', 'descriptionDetail', 'align', 'html', 'def', 'space',
]);

function isBlock(token: MdToken): boolean {
  if (token.type === 'math') return token.isInline === false;
  return BLOCK_TYPES.has(token.type);
}

function inlineText(tokens: MdToken[]): string {
  let out = '';
  for (const token of tokens) out += inlineTokenText(token);
  return out;
}

function inlineTokenText(token: MdToken): string {
  switch (token.type) {
    case 'text':
      return token.tokens ? inlineText(token.tokens) : token.text ?? '';
    case 'escape':
    case 'codespan':
    case 'math':
      return token.text ?? '';
    case 'br':
      return ' ';
    case 'link': {
      const text = token.tokens ? inlineText(token.tokens) : token.text ?? '';
      // A bare URL says nothing worth hearing; its words are the address.
      return text.trim() === (token.href ?? '').trim() || /^[a-z][a-z0-9+.-]*:\/\//i.test(text.trim()) ? '' : text;
    }
    case 'image':
    case 'footnoteRef':
    case 'inline-citations':
    case 'html':
      return '';
    default:
      // strong, em, del, sub, sup and any future inline extension: the
      // words without the markup.
      return token.tokens ? inlineText(token.tokens) : token.text ?? '';
  }
}

const EMOJI = /[\p{Extended_Pictographic}\u{FE0F}\u{200D}]/gu;
const BARE_URL = /\b[a-z][a-z0-9+.-]*:\/\/\S+/gi;

function push(out: string[], text: string): void {
  const spoken = text
    .replace(EMOJI, '')
    .replace(BARE_URL, '')
    .replace(/\s+/g, ' ')
    // What was dropped can leave a space before punctuation.
    .replace(/ ([.,;:!?…])/g, '$1')
    .trim();
  if (/[\p{L}\p{N}]/u.test(spoken)) out.push(spoken);
}

const sentenceSegmenter = typeof Intl !== 'undefined' && 'Segmenter' in Intl
  ? new Intl.Segmenter(undefined, { granularity: 'sentence' })
  : null;

/** The sentences of one block of text. */
export function splitSentences(text: string): string[] {
  const sentences = sentenceSegmenter
    ? Array.from(sentenceSegmenter.segment(text), (s) => s.segment)
    : text.split(/(?<=[.!?…])\s+/);
  return sentences.map((s) => s.trim()).filter((s) => s !== '');
}

/** One sentence as pieces no longer than MAX_PIECE_CHARS: cut after
 * clause punctuation where it can be, otherwise at whitespace, otherwise
 * anywhere. */
export function capPiece(sentence: string): string[] {
  const pieces: string[] = [];
  let rest = sentence;
  while (rest.length > MAX_PIECE_CHARS) {
    const window = rest.slice(0, MAX_PIECE_CHARS + 1);
    const clause = Math.max(window.lastIndexOf('; '), window.lastIndexOf(': '), window.lastIndexOf(', '));
    const space = window.lastIndexOf(' ');
    const cut = clause > 0 ? clause + 1 : space > 0 ? space : MAX_PIECE_CHARS;
    pieces.push(rest.slice(0, cut).trim());
    rest = rest.slice(cut).trim();
  }
  if (rest) pieces.push(rest);
  return pieces;
}
