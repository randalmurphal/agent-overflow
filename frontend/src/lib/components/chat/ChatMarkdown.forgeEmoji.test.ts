import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import { __resetForgeEmojiForTest, loadForgeEmoji } from '../../utils/forgeEmoji.svelte';
import * as extensionModule from '../../utils/forgeEmojiExtension';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Emoji shortcodes on a forge surface, on the compact static path (settled)
// and the component path (streaming), and their absence in agent chat.

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

const SOURCE = 'Ship it :rocket: :+1: :nope: `:tada:` a :tada::tada: end\n\n## Head :sparkles:\n\n| s |\n| --- |\n| :x: |\n';

async function shown(container: HTMLElement): Promise<string> {
  await waitFor(() => expect(container.querySelector('td')?.textContent).toBeTruthy());
  return [...container.querySelectorAll('p, h2, th, td')].map((element) => element.textContent!.trim()).join(' | ');
}

beforeEach(async () => {
  __resetForgeEmojiForTest();
  await loadForgeEmoji();
});
afterEach(() => __resetForgeEmojiForTest());

describe('forge emoji shortcodes in ChatMarkdown', () => {
  it.each([false, true])('render on a GitHub surface (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITHUB, streaming } });
    expect(await shown(container)).toBe('Ship it 🚀 👍 :nope: :tada: a 🎉🎉 end | Head ✨ | s | ❌');
    expect(container.querySelector('code')!.textContent).toBe(':tada:');
  });

  it.each([false, true])('render by GitLab rules on a GitLab surface (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITLAB, streaming } });
    expect(await shown(container)).toBe('Ship it 🚀 👍 :nope: :tada: a :tada::tada: end | Head ✨ | s | ❌');
  });

  it('stay text in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOURCE } });
    expect(await shown(container)).toBe('Ship it :rocket: :+1: :nope: :tada: a :tada::tada: end | Head :sparkles: | s | :x:');
  });

  it.each([false, true])("paint GitHub's image emoji as an inline 20px image (streaming %s)", async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: 'Merge it :shipit: now', forgeSource: GITHUB, streaming } });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    const img = container.querySelector('img')!;
    expect(img.getAttribute('src')).toBe('https://github.githubassets.com/images/icons/emoji/shipit.png');
    expect(img.getAttribute('alt')).toBe(':shipit:');
    expect(img.getAttribute('title')).toBe(':shipit:');
    expect(img.style.width).toBe('20px');
    expect(img.closest('p')!.textContent!.replace(/\s+/g, ' ')).toBe('Merge it now');
  });

  it('keep :shipit: as text on a GitLab surface', async () => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: 'Merge it :shipit: now', forgeSource: GITLAB } });
    await waitFor(() => expect(container.querySelector('p')?.textContent).toBe('Merge it :shipit: now'));
    expect(container.querySelector('img')).toBeNull();
  });
});

describe('a forge body rendered before the emoji table arrives', () => {
  it('re-renders once with emoji and keeps the blocks that hold none', async () => {
    let finish!: () => void;
    __resetForgeEmojiForTest(() => new Promise((resolve) => {
      finish = () => resolve(extensionModule);
    }));
    const source = 'plain first block 10:30:00\n\nthen :rocket: here\n\nplain last block\n';
    const { container } = render(ForgeAttachmentChatHarness, { props: { source, forgeSource: GITHUB } });
    await waitFor(() => expect(container.querySelectorAll('p')).toHaveLength(3));
    const [first, middle, last] = container.querySelectorAll('p');
    expect(middle.textContent).toBe('then :rocket: here');
    finish();
    await loadForgeEmoji();
    flushSync();
    await waitFor(() => expect(container.querySelectorAll('p')[1].textContent).toBe('then 🚀 here'));
    const after = container.querySelectorAll('p');
    expect(after[0]).toBe(first);
    expect(after[2]).toBe(last);
  });
});
