import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentDownload } from '../../../test/mocks/attachmentTransfer';
import type { ThreadPane } from '../../stores/thread.svelte';
import GeneratedImageMessage from './GeneratedImageMessage.svelte';
import type { Item } from '../../types/models';

const attachment = {
  id: 'att-1',
  threadId: 'thread-1',
  filename: 'render.png',
  mimeType: 'image/png',
  size: 4096,
  kind: 'image',
};

function generatedItem(overrides: Record<string, unknown> = {}, attachments: unknown[] = [attachment]): Item {
  return makeItem({
    id: 'image:img-1',
    summary: 'A quiet dashboard',
    meta: JSON.stringify({
      generatedImage: { sourceItemId: 'img-1', provider: 'codex', prompt: 'A quiet dashboard', ...overrides },
      ...(attachments.length > 0 ? { attachments } : {}),
    }),
  });
}

function makePane(): ThreadPane {
  return {
    threadId: 'thread-1',
    paneId: 'pane-1',
    attachmentCacheFor: () => undefined,
  } as unknown as ThreadPane;
}

describe('<GeneratedImageMessage>', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetBindingMocks();
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    mockAttachmentDownload();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders the picture through the ordinary attachment preview path', async () => {
    const { getByTestId, queryByTestId } = render(GeneratedImageMessage, {
      props: { pane: makePane(), item: generatedItem() },
    });

    expect(getByTestId('generated-image-message')).toBeTruthy();
    expect(getByTestId('generated-image-attachments')).toBeTruthy();
    expect(queryByTestId('generated-image-error')).toBeNull();
    await waitFor(() => {
      expect(getByTestId('generated-image-attachments').querySelector('img')).toBeTruthy();
    });
  });

  it('captions the picture with the revised prompt', () => {
    const { getByTestId } = render(GeneratedImageMessage, {
      props: { pane: makePane(), item: generatedItem() },
    });
    expect(getByTestId('generated-image-caption').textContent?.trim()).toBe('A quiet dashboard');
  });

  it('omits the caption when the model reported no prompt', () => {
    const { queryByTestId } = render(GeneratedImageMessage, {
      props: { pane: makePane(), item: generatedItem({ prompt: undefined }) },
    });
    expect(queryByTestId('generated-image-caption')).toBeNull();
  });

  // A failed import is a visible reason, never a broken tile.
  it('renders the failure reason instead of an image', () => {
    const { getByTestId, queryByTestId } = render(GeneratedImageMessage, {
      props: {
        pane: makePane(),
        item: generatedItem({ error: 'source path is outside the permitted directory' }, []),
      },
    });

    const error = getByTestId('generated-image-error');
    expect(error.textContent).toContain('source path is outside the permitted directory');
    expect(queryByTestId('generated-image-attachments')).toBeNull();
    expect(error.querySelector('img')).toBeNull();
  });

  it('opens the lightbox through the pane-owned preview loader', async () => {
    const onImageExpand = vi.fn();
    const { getByRole } = render(GeneratedImageMessage, {
      props: { pane: makePane(), item: generatedItem(), onImageExpand },
    });

    await fireEvent.click(getByRole('button', { name: 'Preview render.png' }));
    await waitFor(() => {
      expect(onImageExpand).toHaveBeenCalledTimes(1);
    });
    expect(onImageExpand.mock.calls[0][0]).toMatchObject({
      index: 0,
      images: [expect.objectContaining({ id: 'att-1', mimeType: 'image/png' })],
    });
  });

  it('renders nothing for an ordinary assistant row', () => {
    const { queryByTestId } = render(GeneratedImageMessage, {
      props: { pane: makePane(), item: makeItem({ summary: 'just prose' }) },
    });
    expect(queryByTestId('generated-image-message')).toBeNull();
  });

  // The phone and a connected browser mount the same row with no pane-owned
  // cache; the tile must still resolve.
  it('renders without a pane', async () => {
    const { getByTestId } = render(GeneratedImageMessage, {
      props: { item: generatedItem() },
    });
    await waitFor(() => {
      expect(getByTestId('generated-image-attachments').querySelector('img')).toBeTruthy();
    });
  });
});
