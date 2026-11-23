import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import '../../../../app.css';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { findTextSegments } from '../../../utils/findHighlights';
import { foldFindText } from '../../../utils/findMatch';
import { createMarkdownFindTextCache, markdownFindText } from './markdownFindText';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { __resetForgeEmojiForTest, loadForgeEmoji } from '../../../utils/forgeEmoji.svelte';
import * as extensionModule from '../../../utils/forgeEmojiExtension';

// Find's projection against a review comment's DOM for emoji shortcodes,
// before and after the name table arrives, and GitHub's image emoji in
// the line of text it sits in.

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

const SOURCE = 'Ship :rocket: and :tada::tada: then :shipit: done `:x:`\n\n## Head :sparkles:\n\n- [ ] task :bug: x\n\n| a |\n| --- |\n| b :x: |\n';

let finish: (() => void) | null = null;
beforeEach(() => {
  document.body.innerHTML = '';
  __resetForgeEmojiForTest(() => new Promise((resolve) => {
    finish = () => resolve(extensionModule);
  }));
});
afterEach(() => __resetForgeEmojiForTest());

async function mounted(source: string, forge: ForgeAttachmentSource): Promise<Element> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('td')).not.toBeNull());
  return container.querySelector('.markdown-body')!;
}

const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
const runs = (list: readonly string[]) => list.map(text).filter(Boolean);

describe('forge emoji under find', () => {
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('projects what a %s comment shows before and after the table loads', async (_name, forge) => {
    const root = await mounted(SOURCE, forge);
    const cache = createMarkdownFindTextCache({ embeddedHtml: true, forgeSource: forge });
    const before = runs(findTextSegments(root));
    expect(before.join(' ')).toContain(':rocket:');
    expect(runs(markdownFindText(SOURCE, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(before);
    expect(runs(cache(SOURCE).runs)).toEqual(before);

    finish!();
    await loadForgeEmoji();
    flushSync();
    await waitFor(() => expect(root.textContent).toContain('🚀'));
    const after = runs(findTextSegments(root));
    expect(after).not.toEqual(before);
    expect(runs(markdownFindText(SOURCE, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(after);
    // A cache filled before the load reads the source again.
    expect(runs(cache(SOURCE).runs)).toEqual(after);
  });

  it("paints GitHub's image emoji at 20px in its line of text", async () => {
    finish = null;
    __resetForgeEmojiForTest();
    await loadForgeEmoji();
    const root = await mounted('Looks good to me, merge it :shipit: now please\n\n| a |\n| --- |\n| b |\n', GITHUB);
    await waitFor(() => expect(root.querySelector('img')).not.toBeNull());
    const img = root.querySelector('img')!;
    const box = img.getBoundingClientRect();
    expect(box.width).toBe(20);
    expect(box.height).toBe(20);
    const paragraph = root.querySelector('p')!;
    // One line: the words before and after the image share it.
    const words = [...paragraph.childNodes].filter((node) => node.nodeType === Node.TEXT_NODE && node.textContent!.trim() !== '');
    const tops = words.map((node) => {
      const r = document.createRange();
      r.selectNodeContents(node);
      return Math.round(r.getBoundingClientRect().top);
    });
    expect(tops).toHaveLength(2);
    expect(new Set(tops).size).toBe(1);
    expect(box.top).toBeGreaterThanOrEqual(paragraph.getBoundingClientRect().top - 1);
    expect(box.bottom).toBeLessThanOrEqual(paragraph.getBoundingClientRect().bottom + 1);
  });
});
