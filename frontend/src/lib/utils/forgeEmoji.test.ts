import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { lex, type Extension } from '../markdown';
import { reportFrontendDiagnostic } from './frontendErrorCapture';
import { GITHUB_IMAGES, SHORTCODES } from './forgeEmojiTable';
import * as extensionModule from './forgeEmojiExtension';
import { decodeForgeEmojiTables, forgeEmojiExtension, type EmojiForge } from './forgeEmojiExtension';
import {
  __resetForgeEmojiForTest,
  forgeEmojiGeneration,
  forgeEmojiSlot,
  loadForgeEmoji,
  resolveForgeEmoji,
} from './forgeEmoji.svelte';

vi.mock('./frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

type GenericToken = { type: string; raw: string; text?: string; tokens?: GenericToken[] } & Record<string, unknown>;

const TABLES = decodeForgeEmojiTables(SHORTCODES, GITHUB_IMAGES);
const SLOT: Extension = { name: 'forgeEmoji', level: 'inline', tokenizer: () => undefined };
const EXTENSIONS: Record<EmojiForge, Extension[]> = {
  github: [forgeEmojiExtension('github', TABLES.github, SLOT)],
  gitlab: [forgeEmojiExtension('gitlab', TABLES.gitlab, SLOT)],
};

// The text inline tokens show; an image as `[img alt]`.
const shown = (tokens: GenericToken[]): string =>
  tokens
    .map((token) => {
      if (token.type === 'image') return `[img ${token.text}]`;
      if (token.tokens && token.type !== 'codespan') return shown(token.tokens as GenericToken[]);
      return token.text ?? '';
    })
    .join('');

const render = (forge: EmojiForge, src: string): string =>
  (lex(src, EXTENSIONS[forge]) as GenericToken[]).map((block) => shown((block.tokens ?? [block]) as GenericToken[])).join('\n');

const inline = (forge: EmojiForge, src: string): GenericToken[] => {
  const [paragraph] = lex(src, EXTENSIONS[forge]) as GenericToken[];
  expect(paragraph.type).toBe('paragraph');
  return paragraph.tokens as GenericToken[];
};

describe('forge emoji shortcodes', () => {
  it.each(['github', 'gitlab'] as const)('render known names on %s and leave unknown or uppercase ones as text', (forge) => {
    expect(render(forge, 'Ship it :rocket: :white_check_mark: :+1: :-1: :not_a_real_emoji: :ROCKET: and :heart:'))
      .toBe('Ship it 🚀 ✅ 👍 👎 :not_a_real_emoji: :ROCKET: and ❤️');
  });

  it.each(['github', 'gitlab'] as const)('ride escape tokens whose raw is the shortcode on %s', (forge) => {
    const tokens = inline(forge, 'a :tada: b');
    expect(tokens.map((token) => [token.type, token.raw])).toEqual([['text', 'a '], ['escape', ':tada:'], ['text', ' b']]);
    expect(tokens.map((token) => token.raw).join('')).toBe('a :tada: b');
  });

  it.each(['github', 'gitlab'] as const)('stay text in code, URLs, words and times on %s', (forge) => {
    expect(render(forge, '`:rocket:` a:b:c 10:30:00 x:rocket: :rocket:x https://example.invalid/:rocket: <https://example.invalid/:x:>'))
      .toBe(':rocket: a:b:c 10:30:00 x:rocket: :rocket:x https://example.invalid/:rocket: https://example.invalid/:x:');
    expect(render(forge, '```\n:rocket:\n```')).toBe(':rocket:');
  });

  it.each(['github', 'gitlab'] as const)('treat element edges and a following scheme URL as no neighbour on %s', (forge) => {
    expect(render(forge, 'a **b**:rocket: and [:tada:](https://example.invalid) and `c`:x: and :rocket:https://example.invalid/e'))
      .toBe('a b🚀 and 🎉 and c❌ and 🚀https://example.invalid/e');
  });

  it('render adjacent shortcodes and escaped colons on GitHub only', () => {
    expect(render('github', 'a :tada::tada: \\:rocket\\: :white\\_check\\_mark:')).toBe('a 🎉🎉 🚀 ✅');
    expect(render('gitlab', 'a :tada::tada: \\:rocket\\: :white\\_check\\_mark:')).toBe('a :tada::tada: :rocket: :white_check_mark:');
    expect(render('gitlab', 'a::rocket: :rocket:: b')).toBe('a::rocket: :rocket:: b');
  });

  it("follow each forge's own names", () => {
    // GitLab only: skin tones and long-form aliases.
    expect(render('gitlab', 'a :thumbsup_tone3: :grinning_face: :family_mwg:')).toBe('a 👍🏽 😀 👨‍👩‍👧');
    expect(render('github', 'a :thumbsup_tone3: :grinning_face:')).toBe('a :thumbsup_tone3: :grinning_face:');
    // A name the forges map to different characters.
    expect(render('github', 'a :beetle:')).toBe('a 🪲');
    expect(render('gitlab', 'a :beetle:')).toBe('a 🐞');
  });

  it("render GitHub's image emoji as a 20px image of GitHub's icon, and leave them as text on GitLab", () => {
    const [, image] = inline('github', 'ok :shipit:');
    expect(image).toMatchObject({
      type: 'image',
      raw: ':shipit:',
      href: 'https://github.githubassets.com/images/icons/emoji/shipit.png',
      text: ':shipit:',
      title: ':shipit:',
      width: '20',
      height: '20',
    });
    expect(render('gitlab', 'a :shipit: :octocat:')).toBe('a :shipit: :octocat:');
  });

  it('render inside headings, tables and list items', () => {
    expect(render('github', '## Heading with :sparkles: emoji')).toBe('Heading with ✨ emoji');
    const [table] = lex('| a |\n| --- |\n| :x: |', EXTENSIONS.gitlab) as GenericToken[];
    expect(JSON.stringify(table)).toContain('"text":"❌"');
    const [list] = lex('- item :bug: here', EXTENSIONS.gitlab) as GenericToken[];
    expect(JSON.stringify(list)).toContain('"text":"🐛"');
  });

  it('keeps the name scan bounded by the longest name', () => {
    const long = `x :${'a'.repeat(TABLES.gitlab.longest + 1)}:`;
    expect(render('gitlab', long)).toBe(long);
  });
});

describe('forge emoji loading', () => {
  afterEach(() => __resetForgeEmojiForTest());

  it('holds a claim-nothing slot until the table loads, then resolves it, once', async () => {
    let finish!: () => void;
    const load = vi.fn(() => new Promise<typeof extensionModule>((resolve) => {
      finish = () => resolve(extensionModule);
    }));
    __resetForgeEmojiForTest(load);
    const slot = forgeEmojiSlot('gitlab');
    expect(forgeEmojiSlot('gitlab')).toBe(slot);
    expect(resolveForgeEmoji(slot)).toBe(slot);
    expect(forgeEmojiGeneration()).toBe(0);
    expect(shown((lex('a :rocket: x', [slot]) as GenericToken[])[0].tokens as GenericToken[])).toBe('a :rocket: x');
    finish();
    await loadForgeEmoji();
    flushSync();
    const loaded = resolveForgeEmoji(slot);
    expect(loaded).not.toBe(slot);
    expect(resolveForgeEmoji(slot)).toBe(loaded);
    expect(forgeEmojiGeneration()).toBe(1);
    expect(shown((lex('a :rocket: x', [loaded]) as GenericToken[])[0].tokens as GenericToken[])).toBe('a 🚀 x');
    forgeEmojiSlot('github');
    expect(load).toHaveBeenCalledTimes(1);
  });

  it('keeps blocks without a known shortcode when the slot resolves', async () => {
    __resetForgeEmojiForTest();
    const slot = forgeEmojiSlot('github');
    await loadForgeEmoji();
    const loaded = resolveForgeEmoji(slot);
    expect(loaded.tokenizesAlike!(slot, 'plain 10:30:00 :unknown_name: text')).toBe(true);
    expect(loaded.tokenizesAlike!(slot, 'with :rocket: here')).toBe(false);
    expect(loaded.tokenizesAlike!(slot, 'with \\:rocket\\: here')).toBe(false);
    expect(loaded.tokenizesAlike!(slot, 'with :shipit: here')).toBe(false);
    expect(loaded.tokenizesAlike!(forgeEmojiSlot('gitlab'), 'plain')).toBe(false);
  });

  it('reports a failed load and leaves the slot in place', async () => {
    __resetForgeEmojiForTest(() => Promise.reject(new Error('chunk gone')));
    const slot = forgeEmojiSlot('github');
    await loadForgeEmoji();
    expect(reportFrontendDiagnostic).toHaveBeenCalledWith('Forge emoji table failed to load', 'chunk gone');
    expect(resolveForgeEmoji(slot)).toBe(slot);
    expect(forgeEmojiGeneration()).toBe(0);
  });
});
