import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import UserMessage from './UserMessage.svelte';

describe('<UserMessage>', () => {
  beforeEach(() => {
    resetBindingMocks();
    // GetAttachmentThumbnail is the inline-grid path (small bytes from the
    // SQLite thumbnail cache); GetAttachmentData is the modal lightbox path
    // (original-resolution refetch). Mock both so tests exercising either
    // path don't blow up on an unstubbed binding.
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('shows its timestamp without requiring row hover', () => {
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const { container } = render(UserMessage, {
      props: {
        item: makeItem({
          createdAt,
          kind: 'user_text',
          role: 'user',
          summary: 'hello',
        }),
      },
    });

    const time = container.querySelector('time');
    expect(time).not.toBeNull();
    expect(time?.getAttribute('datetime')).toBe(new Date(createdAt).toISOString());
    expect(time?.className).not.toContain('opacity-0');
    expect(time?.className).not.toContain('group-hover:opacity-100');
  });

  it('renders a copy button when there is visible text', () => {
    const { getByLabelText } = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'copy me',
        }),
      },
    });
    expect(getByLabelText('Copy message')).toBeInTheDocument();
  });

  it('does not render a copy button when summary is only a stripped attachment marker', () => {
    const { container } = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: '\n\n![](attachment://thread-1/att-1.png)',
        }),
      },
    });
    expect(container.querySelector('[aria-label="Copy message"]')).toBeNull();
  });

  it('writes the visible summary to the clipboard on click', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const { getByLabelText } = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'visible body\n\n![](attachment://thread-1/att-1.png)',
        }),
      },
    });

    await fireEvent.click(getByLabelText('Copy message'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('visible body'));
  });

  it('renders image attachments from item metadata and expands them', async () => {
    const onImageExpand = vi.fn();
    const { getByLabelText, getByText } = render(UserMessage, {
      props: {
        onImageExpand,
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'look here',
          meta: JSON.stringify({
            attachments: [{
              id: 'att-1',
              threadId: 'thread-1',
              filename: 'hero.png',
              mimeType: 'image/png',
              size: 128,
              relativePath: 'thread-1/att-1.png',
              createdAt: 1,
            }],
          }),
        }),
      },
    });

    const previewButton = getByLabelText('Preview hero.png');
    expect(getByText('#1')).toBeInTheDocument();
    await fireEvent.click(previewButton);
    await waitFor(() => expect(onImageExpand).toHaveBeenCalledTimes(1));

    expect(onImageExpand.mock.calls[0]?.[0]).toMatchObject({
      images: [{
        id: 'att-1',
        filename: 'hero.png',
        mimeType: 'image/png',
        size: 128,
      }],
      index: 0,
    });
    expect(onImageExpand.mock.calls[0]?.[0].images[0]?.url).toMatch(/^(blob:|data:image\/png;base64,)/);
  });

  it('loads history attachment thumbnails on mount (virtua bufferSize bounds the mount window)', async () => {
    // Pre-rebuild this was gated by an IntersectionObserver inside the
    // row. After the rebuild, virtua's `bufferSize=900` already restricts
    // mount to rows near the viewport, and the per-pane attachment cache
    // de-dupes across remounts — so a separate IO observer was redundant
    // and got removed. Loading happens immediately on mount, and goes
    // through GetAttachmentThumbnail (not the full-size GetAttachmentData
    // which is reserved for the lightbox modal).
    const getAttachmentThumbnail = setBindingMock(
      'GetAttachmentThumbnail',
      async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }),
    );

    render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'look here [Image #1]',
          meta: JSON.stringify({
            attachments: [{
              id: 'att-1',
              threadId: 'thread-1',
              filename: 'hero.png',
              mimeType: 'image/png',
              size: 128,
              relativePath: 'thread-1/att-1.png',
              createdAt: 1,
            }],
          }),
        }),
      },
    });

    await waitFor(() => {
      expect(getAttachmentThumbnail).toHaveBeenCalledWith('thread-1', 'att-1');
    });
  });
});
