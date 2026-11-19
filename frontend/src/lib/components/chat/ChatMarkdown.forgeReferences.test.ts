import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ChatMarkdown from './ChatMarkdown.svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from './markdown/chatMarkdownOptions';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Forge references render as ordinary external links on a review surface,
// the same anchor on the component and compact static paths, and agent
// chat keeps them as text.

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

const SOURCE = 'Fixes #12 in 0123abcd by @octo, see [notes](docs/notes.md) and a@b.example. !5 last';

interface Anchor {
  text: string;
  href: string | null;
  html: string;
  target: string | null;
}

function anchors(root: Element): Anchor[] {
  return [...root.querySelectorAll('a')].map((a) => ({
    text: a.textContent ?? '',
    href: a.getAttribute('href'),
    html: a.innerHTML.replace(/<!---->/g, '').replace(/ (?:class|data-[\w-]+)="[^"]*"/g, ''),
    target: a.getAttribute('target'),
  }));
}

const GITHUB_ANCHORS: Anchor[] = [
  { text: '#12', href: 'https://github.com/acme/widget/issues/12', html: '#12', target: '_blank' },
  { text: '0123abc', href: 'https://github.com/acme/widget/commit/0123abcd', html: '<code>0123abc</code>', target: '_blank' },
  { text: '@octo', href: 'https://github.com/octo', html: '@octo', target: '_blank' },
  { text: 'notes', href: 'https://github.com/acme/widget/pull/docs/notes.md', html: 'notes', target: '_blank' },
  { text: 'a@b.example', href: 'mailto:a@b.example', html: 'a@b.example', target: '_blank' },
];
const GITLAB_ANCHORS: Anchor[] = [
  { text: '#12', href: 'https://gitlab.com/group/widget/-/issues/12', html: '#12', target: '_blank' },
  { text: '0123abcd', href: 'https://gitlab.com/group/widget/-/commit/0123abcd', html: '0123abcd', target: '_blank' },
  { text: '@octo', href: 'https://gitlab.com/octo', html: '@octo', target: '_blank' },
  { text: 'notes', href: 'https://gitlab.com/group/widget/-/blob/HEAD/docs/notes.md', html: 'notes', target: '_blank' },
  { text: 'a@b.example', href: 'mailto:a@b.example', html: 'a@b.example', target: '_blank' },
  { text: '!5', href: 'https://gitlab.com/group/widget/-/merge_requests/5', html: '!5', target: '_blank' },
];

describe('forge references in ChatMarkdown', () => {
  it.each([
    ['GitHub', false, GITHUB, GITHUB_ANCHORS],
    ['GitHub', true, GITHUB, GITHUB_ANCHORS],
    ['GitLab', false, GITLAB, GITLAB_ANCHORS],
    ['GitLab', true, GITLAB, GITLAB_ANCHORS],
  ] as const)('render as external links on a %s surface (streaming %s)', async (_name, streaming, forgeSource, expected) => {
    const { container } = render(ForgeAttachmentChatHarness, { props: { source: SOURCE, forgeSource, streaming } });
    await waitFor(() => expect(container.textContent).toContain('last'));
    await waitFor(() => expect(anchors(container)).toEqual(expected));
  });

  it.each([
    ['GitHub', GITHUB, GITHUB_ANCHORS],
    ['GitLab', GITLAB, GITLAB_ANCHORS],
  ] as const)('render the same anchors on the component and compact static paths (%s)', async (_name, forgeSource, expected) => {
    const extensions = chatMarkdownExtensions({ forge: chatMarkdownForgeOptions(forgeSource, false).extensions, embeddedHtml: false });
    const { container } = render(CompactStaticMarkdownHarness, { source: SOURCE, extensions });
    const componentPath = container.querySelector('[data-full-token-tree]')!;
    const staticPath = container.querySelector('[data-compact-static]')!;
    await waitFor(() => {
      expect(componentPath.textContent).toContain('last');
      expect(staticPath.textContent).toContain('last');
    });
    expect(anchors(componentPath)).toEqual(expected);
    expect(anchors(staticPath)).toEqual(expected);
  });

  it('keeps them as text in agent chat', async () => {
    const { container } = render(ChatMarkdown, { props: { source: SOURCE } });
    await waitFor(() => expect(container.textContent).toContain('last'));
    expect(container.querySelectorAll('a')).toHaveLength(0);
    expect(container.textContent).toContain('#12 in 0123abcd by @octo');
  });
});
