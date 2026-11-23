import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { markedSoftBreak } from '../../markdown';
import { markedHtmlEntities } from '../../markdown/parser/extensions/htmlEntities';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Forge text the way the forge renders it: GitHub shows a soft line break
// in a PR body or comment as <br>, GitLab and agent chat as a space, and
// entity references decode on both renderer paths.

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

const SOFT = 'line one\nline two\n\n> quoted one\n> quoted two\n\n- item one\n  item more\n\nlast line\n';

async function paragraphs(container: HTMLElement): Promise<Element[]> {
  await waitFor(() => expect(container.textContent).toContain('last line'));
  return [...container.querySelectorAll('p')];
}

describe('soft line breaks in forge content', () => {
  it.each([false, true])('render as <br> for a GitHub source with embedded HTML (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: SOFT, forgeSource: GITHUB, embeddedHtml: true, streaming },
    });
    const [first] = await paragraphs(container);
    expect(first.innerHTML).toMatch(/line one<br[^>]*>line two/);
    // Paragraph, blockquote and list item; none after the last line.
    expect(container.querySelectorAll('br')).toHaveLength(3);
    expect(container.querySelector('blockquote br')).not.toBeNull();
    expect(container.querySelector('li br')).not.toBeNull();
  });

  it.each([
    ['a GitLab source', GITLAB, true],
    ['no forge source', null, true],
    ['a GitHub source without embedded HTML', GITHUB, false],
  ] as const)('render as a space for %s', async (_name, forgeSource, embeddedHtml) => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: SOFT, forgeSource, embeddedHtml },
    });
    const [first] = await paragraphs(container);
    expect(container.querySelector('br')).toBeNull();
    expect(first.textContent).toBe('line one\nline two');
  });

  it('render as a space in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOFT } });
    await paragraphs(container);
    expect(container.querySelector('br')).toBeNull();
  });

  it('render the same <br> on the component and compact static paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: 'one\ntwo  \nthree',
      extensions: [markedSoftBreak],
    });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('three');
      expect(staticPath.textContent).toContain('three');
    });
    for (const root of [componentPath, staticPath]) {
      expect(root.querySelectorAll('p br')).toHaveLength(2);
      expect(root.querySelector('p')!.textContent).toBe('onetwothree');
    }
  });
});

describe('entity references', () => {
  it('decode to the same text on the component and compact static paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: 'A&nbsp;B &lt;tag&gt; &amp; &copy; &#35; &#x1F600; &bogus; `&amp;`',
      extensions: [markedHtmlEntities],
    });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('😀');
      expect(staticPath.textContent).toContain('😀');
    });
    for (const root of [componentPath, staticPath]) {
      const paragraph = root.querySelector('p')!;
      expect(paragraph.textContent).toBe('A B <tag> & © # 😀 &bogus; &amp;');
      // Decoded text, never markup.
      expect(paragraph.querySelector('tag')).toBeNull();
      expect(paragraph.querySelector('code')!.textContent).toBe('&amp;');
    }
  });
});

// GitLab's rich text editor writes `{width=…}` after an image it resized.
describe('image attribute lists', () => {
  const SIZED = 'See ![badge](https://example.com/badge.png){width=30} here.';

  it.each([false, true])('size the image and show no list for a GitLab source (streaming %s)', async (streaming) => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: SIZED, forgeSource: GITLAB, embeddedHtml: true, streaming },
    });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.querySelector('img')!.style.width).toBe('30px');
    expect(container.textContent).not.toContain('{');
    expect(container.querySelector('p')!.textContent).toBe('See  here.');
  });

  it.each([
    ['a GitHub source', GITHUB],
    ['no forge source', null],
  ] as const)('show as text for %s', async (_name, forgeSource) => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: SIZED, forgeSource, embeddedHtml: true },
    });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.querySelector('img')!.style.width).toBe('');
    expect(container.textContent).toContain('{width=30}');
  });

  it('show as text in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SIZED } });
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.textContent).toContain('{width=30}');
  });
});
