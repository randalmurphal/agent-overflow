import { describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ForgeAttachmentChatHarness from './ForgeAttachmentChatHarness.svelte';
import CompactStaticMarkdownHarness from './CompactStaticMarkdownHarness.svelte';
import {
  FORGE_ATTACHMENT_HREF_PREFIX,
  parseForgeAttachmentHref,
  type ForgeAttachmentSource,
} from '../../utils/forgeAttachments';
import { buildForgeAttachmentExtension } from '../../utils/forgeAttachmentExtension';
import { PATH_LINK_HREF_PREFIX } from '../../utils/pathLinkExtension';
import type { Extension } from '../../markdown';

// The bytes never arrive here: the host stays in its loading state, which is
// what these cases assert about. A mount is the claim, not a picture.
vi.mock('../../utils/forgeAttachmentCache', () => ({
  acquireForgeAttachment: () => ({ value: new Promise(() => {}), release: () => {} }),
}));

const HEX = '0123456789abcdef0123456789abcdef';
const SOURCE: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'gpu',
  webBase: 'https://gitlab.example.test/group/widget/-/merge_requests/3',
};

const IMAGE_MD = `![a shot](/uploads/${HEX}/shot.png)`;
const LINK_MD = `[the report](/uploads/${HEX}/report.pdf)`;

describe('forge attachments through ChatMarkdown', () => {
  it('mounts the attachment host for an image, on the compact static path a sealed body uses', async () => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: IMAGE_MD, forgeSource: SOURCE },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    });
    // The static renderer must bail to a component island for this token;
    // a serialized <img> would mean it painted the ticketed URL itself.
    expect(container.querySelector('img')).toBeNull();
  });

  it('renders a file link as a live anchor carrying the forge href', async () => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: LINK_MD, forgeSource: SOURCE },
    });

    await waitFor(() => expect(container.textContent).toContain('the report'));
    const anchor = container.querySelector('a[data-streamdown-link]');
    expect(anchor).not.toBeNull();
    expect(parseForgeAttachmentHref(anchor!.getAttribute('href'))).toMatchObject({
      href: `/uploads/${HEX}/report.pdf`,
      backend: 'gpu',
    });
    expect(anchor!.getAttribute('title')).toBe('Download report.pdf');
  });

  it('changes nothing on a surface with no forge source', async () => {
    const { container } = render(ForgeAttachmentChatHarness, {
      props: { source: `${IMAGE_MD}\n\n${LINK_MD}`, forgeSource: null },
    });

    await waitFor(() => expect(container.textContent).toContain('the report'));
    expect(container.querySelector('[data-forge-attachment-loading]')).toBeNull();
    expect(container.querySelector(`a[href^="${FORGE_ATTACHMENT_HREF_PREFIX}"]`)).toBeNull();
    // A root-relative destination stays the untagged reference it has always
    // been on a surface with no workspace and no PR.
    expect(container.querySelector('span[data-streamdown-link-blocked]')).not.toBeNull();
  });
});

// The link half again, this time against BOTH renderers at once. A forge
// link is an ordinary anchor to `classifyLinkHref`, and the two paths
// deciding that differently is the silent fork this harness exists to catch.
describe('a forge link across both render paths', () => {
  it('renders as an anchor with the forge href on the component and static paths', async () => {
    const extensions = buildForgeAttachmentExtension({
      ...SOURCE,
      embeddedHtml: false,
    }) as unknown as Extension[];
    const { container } = render(CompactStaticMarkdownHarness, {
      source: LINK_MD,
      allowedLinkPrefixes: ['*', PATH_LINK_HREF_PREFIX, FORGE_ATTACHMENT_HREF_PREFIX],
      extensions,
    });

    const componentPath = container.querySelector('[data-full-token-tree] > div');
    const staticPath = container.querySelector('[data-compact-static] > div');
    if (!componentPath || !staticPath) throw new Error('differential roots did not mount');

    await waitFor(() => {
      expect(componentPath.textContent).toContain('the report');
      expect(staticPath.textContent).toContain('the report');
    });

    for (const root of [componentPath, staticPath]) {
      const anchor = root.querySelector('a[data-streamdown-link]');
      expect(anchor, 'forge links render as anchors on both paths').not.toBeNull();
      expect(anchor!.getAttribute('href')).toBe(
        componentPath.querySelector('a[data-streamdown-link]')!.getAttribute('href'),
      );
      expect(parseForgeAttachmentHref(anchor!.getAttribute('href'))?.href)
        .toBe(`/uploads/${HEX}/report.pdf`);
    }
  });
});
