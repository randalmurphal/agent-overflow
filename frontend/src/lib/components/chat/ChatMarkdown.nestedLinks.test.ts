import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// A link whose text holds a link is not a link, as CommonMark and both
// forges have it: the inner link is the one anchor, on every surface and
// both render paths. An image label's link shows as its alt text.

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

const NESTED = 'Nested: [outer [inner](https://example.invalid/i) text](https://example.invalid/o)';
const SOURCE = `${NESTED}\n\nlast line`;

async function settled(root: HTMLElement): Promise<void> {
  await waitFor(() => expect(root.textContent).toContain('last line'));
}

function expectInnerLinkWins(root: Element): void {
  const paragraph = [...root.querySelectorAll('p')].find((p) => p.textContent!.startsWith('Nested'))!;
  // The inner link, then the outer destination as a bare URL, as the forges show it.
  expect([...paragraph.querySelectorAll('a')].map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
    ['inner', 'https://example.invalid/i'],
    ['https://example.invalid/o', 'https://example.invalid/o'],
  ]);
  expect(paragraph.textContent).toBe('Nested: [outer inner text](https://example.invalid/o)');
  expect(root.querySelector('a a')).toBeNull();
}

describe('a link around a link', () => {
  it('links the inner one on both render paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, { source: SOURCE });
    const componentPath = container.querySelector<HTMLElement>('[data-full-token-tree]')!;
    const staticPath = container.querySelector<HTMLElement>('[data-compact-static]')!;
    await settled(componentPath);
    await settled(staticPath);
    for (const root of [componentPath, staticPath]) expectInnerLinkWins(root);
  });

  it('links the inner one in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOURCE } });
    await settled(container);
    expectInnerLinkWins(container);
  });

  it.each([
    ['GitHub', false, GITHUB],
    ['GitHub', true, GITHUB],
    ['GitLab', false, GITLAB],
    ['GitLab', true, GITLAB],
  ] as const)('links the inner one on a %s surface (streaming %s), Markdown or HTML around it', async (_name, streaming, forgeSource) => {
    const source = `${NESTED}\n\nAnchor: <a href="https://example.invalid/out">[md](https://example.invalid/in)</a> after\n\nlast line`;
    const { container } = render(ForgeAttachmentChatHarness, { props: { source, forgeSource, streaming } });
    await settled(container);
    expectInnerLinkWins(container);
    const anchor = [...container.querySelectorAll('p')].find((p) => p.textContent!.startsWith('Anchor'))!;
    expect([...anchor.querySelectorAll('a')].map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['md', 'https://example.invalid/in'],
    ]);
    expect(anchor.textContent).toBe('Anchor: md after');
  });
});

describe('a link in an image label', () => {
  it('shows as the alt text on both render paths', async () => {
    const { container } = render(CompactStaticMarkdownHarness, {
      source: '![alt with [link](https://example.invalid/x)](https://example.invalid/y.png)\n\nlast line',
    });
    const componentPath = container.querySelector<HTMLElement>('[data-full-token-tree]')!;
    const staticPath = container.querySelector<HTMLElement>('[data-compact-static]')!;
    await settled(componentPath);
    await settled(staticPath);
    for (const root of [componentPath, staticPath]) {
      expect(root.querySelector('img')!.getAttribute('alt')).toBe('alt with link');
      expect(root.querySelector('a')).toBeNull();
    }
  });
});
