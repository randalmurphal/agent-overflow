import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../../app.css';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { findTextSegments } from '../../../utils/findHighlights';
import { foldFindText } from '../../../utils/findMatch';
import { markdownFindText } from './markdownFindText';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';

// Find counts a forge reference as the review pane paints it: a pasted URL
// by its short form, a commit by its short SHA.

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

beforeEach(() => {
  document.body.innerHTML = '';
});

async function mounted(source: string, forge: ForgeAttachmentSource): Promise<Element> {
  const { container } = render(ChatMarkdown, {
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('end'));
  return container.querySelector('.markdown-body')!;
}

const text = (runs: readonly string[]) => runs.map((run) => foldFindText(run).replace(/\s+/g, ' ').trim()).filter(Boolean);

describe('find over forge references', () => {
  it.each([
    ['GitHub', GITHUB, `See https://github.com/acme/widget/pull/12#issuecomment-1 and ${SHA}, @octo and a@b.example end`, ['#12 (comment)', '0123456']],
    ['GitLab', GITLAB, `See https://gitlab.com/group/widget/-/merge_requests/12/diffs and ${SHA}, [[Two Words]] end`, ['!12 (diffs)', '01234567']],
  ] as const)('projects what a %s comment paints', async (_name, forge, source, shown) => {
    const root = await mounted(source, forge);
    const dom = text(findTextSegments(root));
    expect(text(markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs)).toEqual(dom);
    for (const each of shown) expect(dom.join(' ')).toContain(each);
    expect(dom.join(' ')).not.toContain(SHA);
  });
});
