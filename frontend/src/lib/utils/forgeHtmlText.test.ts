import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Token } from '../markdown';
import { chatMarkdownForgeOptions } from '../components/chat/markdown/chatMarkdownOptions';
import { __resetForgeEmojiForTest, loadForgeEmoji } from './forgeEmoji.svelte';
import * as extensionModule from './forgeEmojiExtension';
import type { ForgeAttachmentSource } from './forgeAttachments';

// A forge surface's `text` hook over one HTML text run: what each forge
// links and converts there, read with no Markdown rule.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

/** The run as `[kind, shown, href?]` per token, text runs merged. */
function read(source: ForgeAttachmentSource, run: string, linkable = true): (string | undefined)[][] | null {
  const tokens = chatMarkdownForgeOptions(source, true).html!.text!(run, linkable);
  if (tokens === null) return null;
  const shown = (token: Token): string => ('tokens' in token && token.tokens ? token.tokens.map(shown).join('') : 'text' in token ? token.text : '');
  return tokens.map((token) => (token.type === 'link' ? ['link', shown(token), token.href] : [token.type, shown(token)]));
}

beforeEach(async () => {
  __resetForgeEmojiForTest();
  await loadForgeEmoji();
});
afterEach(() => __resetForgeEmojiForTest());

describe('the forge text hook', () => {
  const MARKDOWN = 'a@b.example mailto:a@b.example [l](docs/a.md) <https://github.com/acme/widget/issues/7> **b**';

  it('reads no Markdown, email or bare URL on GitHub', () => {
    expect(read(GITHUB, MARKDOWN)).toBeNull();
    // A run that opens with Markdown syntax.
    expect(read(GITHUB, '<https://github.com/acme/widget/issues/7> x')).toBeNull();
    expect(read(GITHUB, '[l](docs/a.md)')).toBeNull();
    expect(read(GITHUB, 'https://github.com/acme/widget/issues/7 www.example.invalid')).toBeNull();
  });

  it('reads no Markdown or email on GitLab, and links a URL of any scheme', () => {
    // The URL inside `<…>` is linked as bare text is, up to the `>`.
    expect(read(GITLAB, MARKDOWN.replace('github.com/acme/widget/issues/7', 'gitlab.com/group/widget/-/issues/7'))).toEqual([
      ['text', 'a@b.example mailto:a@b.example [l](docs/a.md) <'],
      ['link', '#7', 'https://gitlab.com/group/widget/-/issues/7'],
      ['text', '> **b**'],
    ]);
    expect(read(GITLAB, '[l](docs/a.md) <a@b.example>')).toBeNull();
    expect(read(GITLAB, 'see https://gitlab.com/group/widget/-/issues/7, https://example.invalid/x. irc://h/c')).toEqual([
      ['text', 'see '],
      ['link', '#7', 'https://gitlab.com/group/widget/-/issues/7'],
      ['text', ', '],
      ['link', 'https://example.invalid/x', 'https://example.invalid/x'],
      ['text', '. '],
      ['link', 'irc://h/c', 'irc://h/c'],
    ]);
  });

  it.each([
    [GITHUB, 'x #1 GH-2 @octo', [['text', 'x '], ['link', '#1', 'https://github.com/acme/widget/issues/1'], ['text', ' '], ['link', 'GH-2', 'https://github.com/acme/widget/issues/2'], ['text', ' '], ['link', '@octo', 'https://github.com/octo']]],
    [GITLAB, '!1 [[Home]] :tada:', [['link', '!1', 'https://gitlab.com/group/widget/-/merge_requests/1'], ['text', ' '], ['link', 'Home', 'https://gitlab.com/group/widget/-/wikis/Home'], ['text', ' '], ['escape', '🎉']]],
  ] as const)('links references and converts emoji (%#)', (source, run, expected) => {
    expect(read(source, run)).toEqual(expected);
  });

  it('converts emoji but links nothing where no link may start', () => {
    expect(read(GITHUB, '#1 :tada: @octo', false)).toEqual([['text', '#1 '], ['escape', '🎉'], ['text', ' @octo']]);
    expect(read(GITHUB, '#1 @octo', false)).toBeNull();
  });

  it('converts emoji under options built before the table arrived', async () => {
    let finish!: () => void;
    __resetForgeEmojiForTest(() => new Promise((resolve) => {
      finish = () => resolve(extensionModule);
    }));
    const text = chatMarkdownForgeOptions(GITHUB, true).html!.text!;
    expect(text(':tada:', true)).toBeNull();
    finish();
    await loadForgeEmoji();
    expect(text(':tada:', true)).toMatchObject([{ type: 'escape', text: '🎉' }]);
  });

  it('builds nothing without a forge', () => {
    expect(chatMarkdownForgeOptions(null, false).html).toBeUndefined();
    expect(chatMarkdownForgeOptions(null, true).html?.text).toBeUndefined();
  });
});
