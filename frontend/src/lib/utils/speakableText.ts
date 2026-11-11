// Markdown → what read-aloud speaks, in sections.
//
// Parsed with the app's own lexer, like the clipboard serializer
// (`markdownHtmlSerialize.ts`), so what is read is what was rendered.
// What a listener gains nothing from is skipped silently: code blocks,
// display math, tables, raw HTML, images, bare URLs, footnote markers and
// emoji. Link text and inline code are kept as words; emphasis is
// flattened.
//
// A reply is read in sections: runs of its top-level blocks of about
// SECTION_CHARS. An OpenAI voice reads one section per request, so its
// pace and intonation carry across the sentences inside it. A device voice
// reads the same section as its sentences, each at most MAX_PIECE_CHARS,
// because those engines stall on long utterances and resume only at an
// utterance start. A sentence past a limit is cut at clause punctuation,
// then at whitespace.
//
// Every limit is a spoken length (`spokenLength`), not a character count:
// a request is capped by its audio's duration, and a digit or a Chinese
// character takes several times as long to say as a letter.
//
// Packing: a section takes following blocks while its text stays within
// SECTION_CHARS. A heading always starts a section. A section under
// MIN_SECTION_CHARS takes the next block up to MAX_SECTION_CHARS, so a
// one-line opener is not followed by silence while a long paragraph is
// fetched. A block with nothing to say (code, a table, a rule, an image)
// ends the section, so a section never covers a block that is not read. A
// block past MAX_SECTION_CHARS is split at sentences into parts of about
// equal length, each its own section.
//
// A section names the blocks it covers as indexes into `renderedBlocks`:
// the elements a settled reply renders as the children of its Markdown
// wrapper, in order.

import { lex } from '../markdown';

/** The longest piece handed to a device voice, as a spoken length. */
export const MAX_PIECE_CHARS = 280;
/** The spoken length a section fills up to with whole blocks. */
export const SECTION_CHARS = 400;
/** A section shorter than this takes the next block past SECTION_CHARS. */
export const MIN_SECTION_CHARS = 80;
/** The longest section; a longer block is split. About 50 seconds of
 *  audio, against the 90 a request may run (`openai.MaxSpeechSeconds`). */
export const MAX_SECTION_CHARS = 800;

/**
 * How long `text` takes to say, in letters of English prose (about 0.064 s
 * each in an OpenAI voice). A decimal digit of any script counts 8:
 * "123,456" is said as fifty-odd letters. A Chinese character counts 4, a
 * Hangul syllable 3 and a kana 2, from the syllables a second those
 * languages are spoken at. Never less than `text.length`.
 */
export function spokenLength(text: string): number {
  let length = 0;
  for (let i = 0; i < text.length; i += 1) {
    const point = text.codePointAt(i)!;
    length += pointLength(point);
    if (point > 0xffff) i += 1;
  }
  return length;
}

const DECIMAL_DIGIT = /\p{Nd}/u;

function pointLength(point: number): number {
  if (point < 0x30) return 1;
  if (point <= 0x39) return 8;
  // Below the Arabic-Indic digits there is no other digit or dense script.
  if (point < 0x660) return 1;
  if (point >= 0x3040 && point <= 0x30ff) return 2;
  if (point >= 0xac00 && point <= 0xd7a3) return 3;
  if ((point >= 0x3400 && point <= 0x9fff) || (point >= 0xf900 && point <= 0xfaff)) return 4;
  if (point >= 0x20000 && point <= 0x3ffff) return 4;
  if (DECIMAL_DIGIT.test(String.fromCodePoint(point))) return 8;
  return point > 0xffff ? 2 : 1;
}

// Where the longest run of whole characters from `start` within `max`
// spoken length ends; at least one character.
function prefixEnd(text: string, start: number, max: number): number {
  let length = 0;
  let end = start;
  while (end < text.length) {
    const point = text.codePointAt(end)!;
    length += pointLength(point);
    if (length > max && end > start) break;
    end += point > 0xffff ? 2 : 1;
  }
  return end;
}

export interface SpeakableSection {
  /** What an OpenAI voice reads in one request. */
  text: string;
  /** What a device voice reads it as: its sentences, each at most MAX_PIECE_CHARS. */
  sentences: string[];
  /** The reply's blocks it covers, first to last inclusive, as indexes
   *  into `renderedBlocks(lex(markdown))`. */
  firstBlock: number;
  lastBlock: number;
}

type MdToken = {
  type: string;
  text?: string;
  href?: string;
  tokens?: MdToken[];
  isInline?: boolean;
};

// Top-level tokens that render exactly one element in chat
// (`markdown/render/Element.svelte`, `staticHtml.ts`).
const ELEMENT_BLOCKS = new Set([
  'paragraph', 'heading', 'code', 'blockquote', 'alert', 'list', 'table', 'hr', 'math', 'descriptionList', 'details', 'align',
]);

/**
 * The top-level tokens of a lexed reply that render one element each, in
 * order: index i is the i-th element child of the reply's Markdown wrapper.
 * An MDX container renders its blocks in place (chat registers no MDX
 * component), so it contributes its blocks. Everything else renders no
 * element: `space`, `def` and footnote definitions, raw HTML (chat renders
 * none), and unknown extensions.
 */
export function renderedBlocks<T extends { type: string; tokens?: unknown }>(tokens: readonly T[]): T[] {
  const out: T[] = [];
  const collect = (from: readonly T[]): void => {
    for (const token of from) {
      if (ELEMENT_BLOCKS.has(token.type)) out.push(token);
      else if (token.type === 'mdx' && Array.isArray(token.tokens)) collect(token.tokens as T[]);
    }
  };
  collect(tokens);
  return out;
}

/** The sections `markdown` is read aloud in; empty when nothing is said. */
export function speakableSections(markdown: string): SpeakableSection[] {
  if (!markdown.trim()) return [];
  const blocks = renderedBlocks(lex(markdown) as unknown as MdToken[]);
  const sections: SpeakableSection[] = [];
  let open: SpeakableSection | null = null;
  // The open section's spoken length.
  let openLength = 0;
  blocks.forEach((token, index) => {
    const parts: string[] = [];
    collectBlocks([token], parts);
    const text = parts.join('\n');
    if (text === '') {
      open = null;
      return;
    }
    const length = spokenLength(text);
    if (length > MAX_SECTION_CHARS) {
      open = null;
      for (const part of splitBlock(parts)) sections.push({ ...part, firstBlock: index, lastBlock: index });
      return;
    }
    if (open && token.type !== 'heading') {
      const joined = openLength + 2 + length;
      if (joined <= SECTION_CHARS || (openLength < MIN_SECTION_CHARS && joined <= MAX_SECTION_CHARS)) {
        open.text += `\n\n${text}`;
        open.sentences.push(...sentencesOf(parts));
        open.lastBlock = index;
        openLength = joined;
        return;
      }
    }
    open = { text, sentences: sentencesOf(parts), firstBlock: index, lastBlock: index };
    openLength = length;
    sections.push(open);
  });
  return sections;
}

function sentencesOf(parts: string[]): string[] {
  return parts.flatMap((part) => splitSentences(part)).flatMap((sentence) => capPiece(sentence));
}

// One block past MAX_SECTION_CHARS as parts of about equal length, cut
// between sentences (or inside one longer than the cap).
function splitBlock(parts: string[]): Array<{ text: string; sentences: string[] }> {
  const units: Array<{ text: string; length: number; separator: string }> = [];
  parts.forEach((part, at) => {
    splitSentences(part).flatMap((sentence) => capPiece(sentence, MAX_SECTION_CHARS)).forEach((text, i) => {
      units.push({ text, length: spokenLength(text), separator: at > 0 && i === 0 ? '\n' : ' ' });
    });
  });
  const total = spokenLength(parts.join('\n'));
  const target = total / Math.ceil(total / MAX_SECTION_CHARS);
  const out: Array<{ text: string; sentences: string[] }> = [];
  let text = '';
  let length = 0;
  let sentences: string[] = [];
  for (const unit of units) {
    if (text !== '') {
      // Cut where the unit's middle would pass the target, so parts come
      // out near it on either side.
      if (length + 1 + unit.length > MAX_SECTION_CHARS || length + 1 + unit.length / 2 > target) {
        out.push({ text, sentences });
        text = '';
        length = 0;
        sentences = [];
      }
    }
    length = text === '' ? unit.length : length + 1 + unit.length;
    text = text === '' ? unit.text : `${text}${unit.separator}${unit.text}`;
    sentences.push(...capPiece(unit.text));
  }
  if (text !== '') out.push({ text, sentences });
  return out;
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
  // A container this list does not name (an MDX element, a future
  // extension) is a block when it holds blocks, so its code and tables
  // stay silent too.
  return BLOCK_TYPES.has(token.type) || (token.tokens?.some(isBlock) ?? false);
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

// Clause punctuation: written before a space, or wide and without one.
const SPACED_CLAUSE = /[;:,]/;
const WIDE_CLAUSE = /[，、；：]/;

/** One sentence as pieces of spoken length at most `max`: cut after clause
 * punctuation where it can be, otherwise at a space, otherwise anywhere.
 * Each cut reads only the piece it makes, so the work is linear in the
 * sentence. */
export function capPiece(sentence: string, max = MAX_PIECE_CHARS): string[] {
  const pieces: string[] = [];
  let start = 0;
  for (;;) {
    // A piece starts at a character, so none is empty.
    while (start < sentence.length && /\s/.test(sentence[start])) start += 1;
    const limit = prefixEnd(sentence, start, max);
    if (limit >= sentence.length) break;
    // The last clause end before the limit and the last space up to it: a
    // word that ends exactly at the limit stays in this piece.
    let clause = -1;
    let space = sentence[limit] === ' ' ? limit : -1;
    for (let i = limit - 1; i > start && clause < 0; i -= 1) {
      const char = sentence[i];
      if (char === ' ') {
        if (space < 0) space = i;
      } else if (WIDE_CLAUSE.test(char) || (SPACED_CLAUSE.test(char) && sentence[i + 1] === ' ')) {
        clause = i;
      }
    }
    const cut = clause >= 0 ? clause + 1 : space >= 0 ? space : limit;
    pieces.push(sentence.slice(start, cut).trim());
    start = cut;
  }
  const rest = sentence.slice(start).trim();
  if (rest) pieces.push(rest);
  return pieces;
}
