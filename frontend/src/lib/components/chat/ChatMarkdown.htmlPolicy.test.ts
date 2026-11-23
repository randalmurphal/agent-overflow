import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { embeddedHtmlFor } from '../../markdown';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// One HTML element policy per forge (parser/extensions/htmlPolicy.ts): an
// element or attribute a forge keeps is kept in a paragraph and in an HTML
// block alike, on both render paths, and one it strips is stripped in both.

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

const INLINE = 'a <small>s</small> <dfn>d</dfn> <bdo dir="rtl">b</bdo> <u>u</u> <span dir="rtl" lang="ar" style="x">r</span> <b title="t">w</b> end';
const SOURCE = `${INLINE}\n\n<p>${INLINE}</p>\n`;

/** Each paragraph's kept elements as `tag[attr=value…]`, in order. */
const kept = (root: Element): string[][] =>
  [...root.querySelectorAll('p')].map((p) =>
    [...p.querySelectorAll('*')].map((element) => {
      const attrs = element.getAttributeNames().filter((name) => !name.startsWith('data-') && name !== 'class');
      return `${element.localName}${attrs.map((name) => `[${name}=${element.getAttribute(name)}]`).join('')}`;
    }),
  );

describe('the HTML policy on a forge surface', () => {
  it.each([false, true])('keeps the same elements and attributes in a paragraph and an HTML block (streaming %s)', async (streaming) => {
    const github = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITHUB, streaming } }).container;
    await waitFor(() => expect(github.querySelectorAll('p')).toHaveLength(2));
    const githubKept = ['span[dir=rtl][lang=ar]', 'b[title=t]'];
    expect(kept(github)).toEqual([githubKept, githubKept]);

    const gitlab = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource: GITLAB, streaming } }).container;
    await waitFor(() => expect(gitlab.querySelectorAll('p')).toHaveLength(2));
    const gitlabKept = ['small', 'dfn', 'bdo[dir=rtl]', 'span[dir=rtl][lang=ar]', 'b[title=t]'];
    expect(kept(gitlab)).toEqual([gitlabKept, gitlabKept]);
    for (const root of [github, gitlab]) for (const p of root.querySelectorAll('p')) expect(p.textContent).toBe('a s d b u r w end');
  });

  it.each(['github', 'gitlab'] as const)('renders inline elements alike on the component and compact paths (%s)', async (forge) => {
    const { container } = render(CompactStaticMarkdownHarness, { source: INLINE, extensions: [...embeddedHtmlFor(forge).extensions] });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('end');
      expect(staticPath.textContent).toContain('end');
    });
    expect(kept(staticPath)).toEqual(kept(componentPath));
    expect(kept(componentPath)[0]).toContain('span[dir=rtl][lang=ar]');
  });
});
