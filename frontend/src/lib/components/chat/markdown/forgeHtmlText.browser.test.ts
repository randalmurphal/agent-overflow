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

// Text and links inside forge HTML in real Chromium: find's projection
// against the painted DOM before and after the emoji table arrives, and a
// jump across the Markdown/HTML boundary in both directions.

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
const SHA = '0123456789abcdef0123456789abcdef01234567';

const SOURCE = [
  `<table><tr><td>:x: See #12, @octo and ${SHA}</td><td><code>:x: #13</code></td><td><kbd>#14 :tada:</kbd></td></tr></table>`,
  '',
  '<p align="center">https://gitlab.com/group/widget/-/issues/9 and :rocket: <a href="docs/a.md">doc :tada:</a></p>',
  '',
  '<div>#15 :sparkles:',
  '',
  '**island** #16',
  '',
  '</div>',
  '',
  'end',
  '',
].join('\n');

let finish: (() => void) | null = null;
beforeEach(() => {
  document.body.innerHTML = '';
  __resetForgeEmojiForTest(() => new Promise((resolve) => {
    finish = () => resolve(extensionModule);
  }));
});
afterEach(() => __resetForgeEmojiForTest());

async function mounted(source: string, forge: ForgeAttachmentSource, target?: HTMLElement): Promise<Element> {
  const { container } = render(ChatMarkdown, {
    target: target ?? document.body.appendChild(document.createElement('div')),
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('end'));
  return container.querySelector('.markdown-body')!;
}

const runs = (list: readonly string[]) => list.map((run) => foldFindText(run).replace(/\s+/g, ' ').trim()).filter(Boolean);

describe('find over text in forge HTML', () => {
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('projects what a %s comment paints before and after the emoji table', async (_name, forge) => {
    const root = await mounted(SOURCE, forge);
    const cache = createMarkdownFindTextCache({ embeddedHtml: true, forgeSource: forge });
    const before = runs(findTextSegments(root));
    expect(before.join(' ')).toContain(':rocket:');
    expect(before.join(' ')).not.toContain(SHA);
    expect(root.querySelector('td a')?.textContent).toBe('#12');
    expect(runs(markdownFindText(SOURCE, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(before);
    expect(runs(cache(SOURCE).runs)).toEqual(before);

    finish!();
    await loadForgeEmoji();
    flushSync();
    await waitFor(() => expect(root.textContent).toContain('🚀'));
    const after = runs(findTextSegments(root));
    expect(after.join(' ')).toContain('❌ see #12');
    expect(after.join(' ')).toContain('✨');
    expect(runs(markdownFindText(SOURCE, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(after);
    expect(runs(cache(SOURCE).runs)).toEqual(after);
  });
});

describe('a task list in an HTML table cell', () => {
  // The island's checkbox is checked by property, so serialized HTML shows
  // no `checked` attribute; Chromium paints the property.
  const TABLE = '<table><tr><td>\n\n- [ ] Task one\n- [x] Task two\n\n</td></tr></table>\n\nend\n';

  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('paints its checked task as checked on %s', async (_name, forge) => {
    const root = await mounted(TABLE, forge);
    const boxes = [...root.querySelectorAll<HTMLInputElement>('td input[type="checkbox"]')];
    expect(boxes.map((box) => [box.checked, box.matches(':checked'), box.closest('li')?.textContent?.trim()])).toEqual([
      [false, false, 'Task one'],
      [true, true, 'Task two'],
    ]);
  });
});

describe('jumps across the Markdown and HTML boundary', () => {
  const filler = Array.from({ length: 30 }, (_, index) => `Filler line ${index}.`).join('\n\n');
  const BODY = [
    '<p><a href="#markdown-target">to Markdown</a></p>',
    '',
    '[to HTML](#html-target)',
    '',
    filler,
    '',
    '## Markdown target',
    '',
    filler,
    '',
    '<div><a name="html-target"></a>HTML target</div>',
    '',
    filler,
    '',
    'end',
  ].join('\n');

  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('jump within the %s body from HTML and to HTML', async (_name, forge) => {
    const scroller = document.body.appendChild(document.createElement('div'));
    scroller.style.cssText = 'height: 200px; overflow-y: auto; width: 500px;';
    const root = await mounted(BODY, forge, scroller);
    const top = (node: Element) => node.getBoundingClientRect().top - scroller.getBoundingClientRect().top;

    root.querySelector<HTMLAnchorElement>('a[href="#markdown-target"]')!.click();
    expect(Math.abs(top(root.querySelector('[data-md-anchor="markdown-target"]')!))).toBeLessThan(1);

    scroller.scrollTop = 0;
    root.querySelector<HTMLAnchorElement>('a[href="#html-target"]')!.click();
    expect(scroller.scrollTop).toBeGreaterThan(0);
    expect(Math.abs(top(root.querySelector('[data-md-anchor="html-target"]')!))).toBeLessThan(1);
    expect(location.hash).toBe('');
  });
});
