import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import EmbeddedHtmlHost from './EmbeddedHtmlHost.svelte';
import { MEDIA_CLAIM_ATTR } from '../../../markdown';
import {
  buildForgeAttachmentHref,
  type ForgeAttachmentSource,
} from '../../../utils/forgeAttachments';

// The bytes never arrive: a mounted host is the claim this file is about.
const acquired: string[] = [];
const released: string[] = [];
vi.mock('../../../utils/forgeAttachmentCache', () => ({
  acquireForgeAttachment: (_backend: string, _pr: unknown, href: string) => {
    acquired.push(href);
    return {
      value: new Promise(() => {}),
      release: () => released.push(href),
    };
  },
}));

const HEX = '0123456789abcdef0123456789abcdef';
const SOURCE: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'gpu',
  webBase: 'https://gitlab.example.test/group/widget/-/merge_requests/3',
};

const claimFor = (name: string) =>
  buildForgeAttachmentHref({ href: `/uploads/${HEX}/${name}`, ...SOURCE });

const token = { type: 'html', raw: '', pre: false, text: '', block: true } as const;

const marked = (name: string, alt = 'a shot') =>
  `<p align="center"><img ${MEDIA_CLAIM_ATTR}="${claimFor(name)}" alt="${alt}"></p>`;

// Reset before, not after: testing-library's auto-cleanup is itself an
// afterEach, and an unmount it performs would otherwise land in the next
// test's arrays.
beforeEach(() => {
  acquired.length = 0;
  released.length = 0;
});

describe('EmbeddedHtmlHost', () => {
  it('swaps a claimed marker for a live attachment host', async () => {
    const { container } = render(EmbeddedHtmlHost, {
      props: { token, content: marked('shot.png') },
    });

    await waitFor(() => {
      expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    });
    // The marker is replaced, not decorated: no <img> is left pointing
    // anywhere, and the wrapper the host owns keeps the fragment's own
    // element around it.
    expect(container.querySelector(`[${MEDIA_CLAIM_ATTR}]`)).toBeNull();
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('p[align="center"]')).not.toBeNull();
    expect(acquired).toEqual([`/uploads/${HEX}/shot.png`]);
  });

  it('releases every host it mounted when it is destroyed', async () => {
    const { container, unmount } = render(EmbeddedHtmlHost, {
      props: {
        token,
        content: `${marked('one.png')}${marked('two.png')}`,
      },
    });

    await waitFor(() => {
      expect(container.querySelectorAll('[data-forge-attachment-loading]')).toHaveLength(2);
    });
    expect(released).toEqual([]);

    unmount();
    await waitFor(() => {
      expect(released).toEqual([`/uploads/${HEX}/one.png`, `/uploads/${HEX}/two.png`]);
    });
  });

  it('re-hydrates when the fragment changes, releasing the previous mounts', async () => {
    const { container, rerender } = render(EmbeddedHtmlHost, {
      props: { token, content: marked('one.png') },
    });
    await waitFor(() => {
      expect(container.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    });

    await rerender({ token, content: marked('two.png') });

    await waitFor(() => {
      expect(acquired).toEqual([`/uploads/${HEX}/one.png`, `/uploads/${HEX}/two.png`]);
    });
    await waitFor(() => expect(released).toEqual([`/uploads/${HEX}/one.png`]));
    expect(container.querySelectorAll('[data-forge-attachment-loading]')).toHaveLength(1);
  });

  it('leaves an unclaimed fragment as the bare injection it has always been', async () => {
    const { container } = render(EmbeddedHtmlHost, {
      props: {
        token,
        content: '<p align="center"><img src="https://example.test/x.png" alt="b"></p>',
      },
    });

    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.querySelector('img')!.getAttribute('src')).toBe(
      'https://example.test/x.png',
    );
    // No wrapper element: the fragment's blocks stay direct siblings of
    // whatever the renderer put around them.
    expect(container.querySelector('[data-markdown-embedded-html]')).toBeNull();
    expect(acquired).toEqual([]);
  });
});
