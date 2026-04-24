import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import UserMessage from './UserMessage.svelte';

describe('<UserMessage>', () => {
  beforeEach(() => {
    resetBindingMocks();
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

  it('defers history attachment preview loading until the message is near the viewport', async () => {
    const observers: Array<{ trigger: () => void }> = [];
    vi.stubGlobal('IntersectionObserver', class {
      private callback: IntersectionObserverCallback;

      constructor(callback: IntersectionObserverCallback) {
        this.callback = callback;
        observers.push({
          trigger: () => this.callback(
            [{ isIntersecting: true } as IntersectionObserverEntry],
            this as unknown as IntersectionObserver,
          ),
        });
      }

      observe = vi.fn();
      disconnect = vi.fn();
      unobserve = vi.fn();
      takeRecords = vi.fn(() => []);
      root = null;
      rootMargin = '';
      thresholds = [];
    });
    const getAttachmentData = setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');

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
    await tick();

    expect(getAttachmentData).not.toHaveBeenCalled();
    observers[0]?.trigger();

    await waitFor(() => {
      expect(getAttachmentData).toHaveBeenCalledWith('thread-1', 'att-1');
    });
  });
});
