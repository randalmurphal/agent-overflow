/**
 * Emoji shortcodes in forge content: `:rocket:` renders `🚀` on a GitHub or
 * GitLab surface, by that forge's own names and rules. Agent chat keeps
 * shortcodes as text.
 *
 * GitHub (gemoji names, plus image-only emoji such as `:shipit:`): a
 * shortcode not touching a letter or digit on either side; adjacent
 * shortcodes (`:tada::tada:`) both render, and backslash escapes resolve
 * first, so `\:rocket\:` renders too. An image-only name is an image token
 * of GitHub's 20px icon, with `:name:` as its alt text and title.
 * GitLab (tanuki_emoji names and aliases, skin tones as `_tone1`..`_tone5`
 * names): `Banzai::Filter::EmojiFilter`'s rule, no letter, digit or colon
 * on either side, and an escaped colon is never part of one.
 * On both, an unknown or uppercase name stays text, and code and URLs never
 * reach this tokenizer. Text inside HTML does (`Lexer.htmlTextTokens`),
 * where a backslash is itself. A neighbour is the source character
 * beside the shortcode within its text run; the start or end of an
 * element's text, or a scheme URL right after, counts as no neighbour, as
 * the forges match per text node.
 *
 * A character rides an `escape` token, which both renderers and find's
 * projection show as its `text`.
 *
 * This module and the name table (forgeEmojiTable.ts, generated) load on
 * demand through forgeEmoji.svelte.ts.
 */
import type { Extension, Tokens } from '../markdown';
import { GITHUB_IMAGES, SHORTCODES } from './forgeEmojiTable';

export type EmojiForge = 'github' | 'gitlab';

export interface ForgeEmojiTable {
  /** Name to character. */
  readonly chars: ReadonlyMap<string, string>;
  /** Names that render as an image (GitHub's image-only emoji). */
  readonly images: ReadonlySet<string>;
  /** The longest name, which bounds a scan. */
  readonly longest: number;
}

/** The two forges' tables from the generated encoding. */
export function decodeForgeEmojiTables(shortcodes: string, githubImages: string): Record<EmojiForge, ForgeEmojiTable> {
  const github = new Map<string, string>();
  const gitlab = new Map<string, string>();
  for (const line of shortcodes.split('\n')) {
    const [char, ...names] = line.split(' ');
    for (const entry of names) {
      const marker = entry.charCodeAt(0);
      if (marker === 33 /* ! */) github.set(entry.slice(1), char);
      else if (marker === 63 /* ? */) gitlab.set(entry.slice(1), char);
      else {
        github.set(entry, char);
        gitlab.set(entry, char);
      }
    }
  }
  const images = new Set(githubImages.split(' '));
  const longest = (names: Iterable<string>) => {
    let max = 0;
    for (const name of names) max = Math.max(max, name.length);
    return max;
  };
  return {
    github: { chars: github, images, longest: Math.max(longest(github.keys()), longest(images)) },
    gitlab: { chars: gitlab, images: new Set(), longest: longest(gitlab.keys()) },
  };
}

const COLON = 58;
const BACKSLASH = 92;

const isNameCode = (code: number): boolean =>
  (code >= 97 && code <= 122) || (code >= 48 && code <= 57) || code === 95 || code === 43 || code === 45;

const ALNUM = /^[\p{Alphabetic}\p{Nd}]$/u;

/** Whether the code point ending at `end` (exclusive) is a letter or digit. */
const alnumBefore = (text: string, end: number): boolean => {
  const code = text.charCodeAt(end - 1);
  if (code < 128) return (code >= 97 && code <= 122) || (code >= 65 && code <= 90) || (code >= 48 && code <= 57);
  const start = code >= 0xdc00 && code <= 0xdfff && end >= 2 ? end - 2 : end - 1;
  return ALNUM.test(text.slice(start, end));
};

/** Whether the code point starting at `at` is a letter or digit. */
const alnumAt = (text: string, at: number): boolean => {
  const code = text.codePointAt(at)!;
  return alnumBefore(String.fromCodePoint(code), code > 0xffff ? 2 : 1);
};

// A scheme URL right after a shortcode is its own link, so the shortcode's
// text ends there.
const SCHEME_URL = /^(?:https?|ftp):\/\//i;

interface Rules {
  /** Backslash escapes resolve before matching (GitHub). */
  readonly escapes: boolean;
  /** A colon beside a shortcode blocks it (GitLab). */
  readonly colonBlocks: boolean;
}

const RULES: Record<EmojiForge, Rules> = {
  github: { escapes: true, colonBlocks: false },
  gitlab: { escapes: false, colonBlocks: true },
};

/** The name between `src[open - 1]`'s colon and the closing colon, and the
 * index after the closing colon; undefined when no complete name closes. */
const readShortcode = (src: string, open: number, longest: number, escapes: boolean): { name: string; end: number } | undefined => {
  let name = '';
  let at = open;
  while (name.length <= longest) {
    const code = src.charCodeAt(at);
    if (isNameCode(code)) {
      name += src[at];
      at++;
    } else if (code === COLON) {
      return name === '' ? undefined : { name, end: at + 1 };
    } else if (code === BACKSLASH && escapes) {
      const next = src.charCodeAt(at + 1);
      if (next === COLON) return name === '' ? undefined : { name, end: at + 2 };
      if (next !== 95 && next !== 43 && next !== 45) return undefined;
      name += src[at + 1];
      at += 2;
    } else {
      return undefined;
    }
  }
  return undefined;
};

/** The source character before the current position in its text run, or
 * '' at the start of an element's text. */
const charBefore = (tokens: readonly { type: string; raw: string; text?: string }[], escapes: boolean): string => {
  const last = tokens[tokens.length - 1];
  if (!last) return '';
  if (last.type === 'text') return last.raw.slice(-2);
  // GitLab renders a backslash escape as its own element.
  if (last.type === 'escape' && (escapes || last.raw.charCodeAt(0) !== BACKSLASH)) return (last.text ?? '').slice(-2);
  return '';
};

const GITHUB_EMOJI_IMAGES = 'https://github.githubassets.com/images/icons/emoji/';

type EmojiImageToken = Tokens.Image & { width: string; height: string };

/** The emoji extension for `forge` over `table`, standing in for `slot`. */
export function forgeEmojiExtension(forge: EmojiForge, table: ForgeEmojiTable, slot: Extension): Extension {
  const rules = RULES[forge];
  const { escapes, colonBlocks } = rules;
  // The character ending at `end` in `text`; none at 0.
  const blockedBefore = (text: string, end: number): boolean =>
    end > 0 && (alnumBefore(text, end) || (colonBlocks && text.charCodeAt(end - 1) === COLON));
  const blockedAfter = (src: string, end: number): boolean => {
    if (end >= src.length) return false;
    const code = src.charCodeAt(end);
    if (code === COLON) return colonBlocks;
    return alnumAt(src, end) && !SCHEME_URL.test(src.slice(end, end + 8));
  };
  const known = (name: string): boolean => table.chars.has(name) || table.images.has(name);
  const extension: Extension = {
    name: 'forgeEmoji',
    level: 'inline',
    start(src) {
      for (let at = src.indexOf(':'); at !== -1; at = src.indexOf(':', at + 1)) {
        if (blockedBefore(src, at)) continue;
        const next = src.charCodeAt(at + 1);
        if (isNameCode(next) || (escapes && next === BACKSLASH)) return at;
      }
      return undefined;
    },
    tokenizer(src, tokens) {
      // A backslash in HTML text is itself, never an escape.
      const escapes = rules.escapes && !this.lexer.state.inHtmlText;
      const first = src.charCodeAt(0);
      const open = first === COLON ? 1 : first === BACKSLASH && escapes && src.charCodeAt(1) === COLON ? 2 : 0;
      if (open === 0) return undefined;
      const before = charBefore(tokens as readonly { type: string; raw: string; text?: string }[], escapes);
      if (blockedBefore(before, before.length)) return undefined;
      const code = readShortcode(src, open, table.longest, escapes);
      if (!code || blockedAfter(src, code.end)) return undefined;
      const raw = src.slice(0, code.end);
      const char = table.chars.get(code.name);
      if (char !== undefined) {
        const token: Tokens.Escape = { type: 'escape', raw, text: char };
        return token;
      }
      if (!table.images.has(code.name)) return undefined;
      const label = `:${code.name}:`;
      const image: EmojiImageToken = {
        type: 'image',
        raw,
        href: `${GITHUB_EMOJI_IMAGES}${code.name}.png`,
        title: label,
        text: label,
        tokens: [],
        width: '20',
        height: '20',
      };
      return image;
    },
    // Against the forge's slot, a block renders alike when no known name
    // sits between colons anywhere in it.
    tokenizesAlike(previous, source) {
      if (previous !== slot) return false;
      for (let at = source.indexOf(':'); at !== -1; at = source.indexOf(':', at + 1)) {
        const code = readShortcode(source, at + 1, table.longest, escapes);
        if (code && known(code.name)) return false;
      }
      return true;
    },
  };
  return extension;
}

/** Both forges' extensions over the generated table, standing in for `slots`. */
export function buildForgeEmojiExtensions(slots: Record<EmojiForge, Extension>): Record<EmojiForge, Extension> {
  const tables = decodeForgeEmojiTables(SHORTCODES, GITHUB_IMAGES);
  return {
    github: forgeEmojiExtension('github', tables.github, slots.github),
    gitlab: forgeEmojiExtension('gitlab', tables.gitlab, slots.gitlab),
  };
}
