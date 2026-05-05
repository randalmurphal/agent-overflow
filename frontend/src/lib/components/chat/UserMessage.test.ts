import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { createDiffPanelState } from '../../stores/diffPanel.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import UserMessage from './UserMessage.svelte';
import type { UserMessageActions } from './userMessageActions';

describe('<UserMessage>', () => {
  beforeEach(() => {
    vi.clearAllMocks();
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

  function makeCheckpointedPane(userItemId = 'user:1'): ThreadPane {
    const diffPanel = createDiffPanelState();
    diffPanel.setCheckpoints([{
      id: 'checkpoint-1',
      threadId: 'thread-1',
      userItemId,
      turnIndex: 1,
      status: 'ready',
      files: [],
      capturedAt: 1,
    }]);
    return {
      threadId: 'thread-1',
      thread: makeThread({ id: 'thread-1' }),
      diffPanel,
      attachmentCacheFor: () => undefined,
    } as unknown as ThreadPane;
  }

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

  it('shows revert and fork actions for a checkpointed inactive user message when handlers are available', () => {
    const pane = makeCheckpointedPane();
    const actions: UserMessageActions = {
      onRevertMessage: vi.fn(),
      onForkMessage: vi.fn(),
    };

    const { getByLabelText } = render(UserMessage, {
      props: {
        pane,
        actions,
        item: makeItem({
          id: 'user:1',
          threadId: 'thread-1',
          turnIndex: 1,
          kind: 'user_text',
          role: 'user',
          summary: 'revertable',
        }),
      },
    });

    expect(getByLabelText('Revert to this message')).toBeInTheDocument();
    expect(getByLabelText('Fork from this message')).toBeInTheDocument();
  });

  it('requests message revert through the parent-owned handler', async () => {
    const pane = makeCheckpointedPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'revertable',
    });
    const onRevertMessage = vi.fn(async () => {});
    const actions: UserMessageActions = { onRevertMessage };

    const { getByLabelText, queryByTestId } = render(UserMessage, {
      props: {
        pane,
        item,
        actions,
      },
    });

    await fireEvent.click(getByLabelText('Revert to this message'));
    await waitFor(() => expect(onRevertMessage).toHaveBeenCalledWith(item));
    expect(queryByTestId('user-message-revert-popover')).toBeNull();
  });

  it('renders the revert choice popover with file totals and confirms the selected mode', async () => {
    const pane = makeCheckpointedPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'revertable',
    });
    const onConfirmRevertMessage = vi.fn(async () => {});
    const onCancelRevertMessage = vi.fn();
    const actions: UserMessageActions = {
      onRevertMessage: vi.fn(),
      onConfirmRevertMessage,
      onCancelRevertMessage,
      revertTargetItemId: item.id,
      revertAffectedFiles: [
        { path: 'notes.txt', kind: 'modified', additions: 3, deletions: 1, lines: [] },
        { path: 'scratch.txt', kind: 'modified', additions: 2, deletions: 4, lines: [] },
      ],
    };

    const { getByTestId, getByText } = render(UserMessage, {
      props: {
        pane,
        item,
        actions,
      },
    });

    const trigger = getByText('Conversation & files').closest('[role="menu"]')
      ?? getByTestId('user-message-revert-popover').parentElement;
    expect(getByTestId('user-message-revert-popover')).toBeInTheDocument();
    expect(trigger).toHaveAttribute('role', 'menu');
    expect(document.querySelector('[aria-label="Revert to this message"]')).toHaveAttribute('aria-expanded', 'true');
    expect(getByText('+5')).toBeInTheDocument();
    expect(getByText('-5')).toBeInTheDocument();

    await fireEvent.click(getByTestId('revert-conversation-only'));
    expect(onConfirmRevertMessage).toHaveBeenCalledWith('conversation-only');
  });

  it('closes the revert choice popover on Escape and outside mousedown', async () => {
    const pane = makeCheckpointedPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'revertable',
    });
    const onCancelRevertMessage = vi.fn();
    const actions: UserMessageActions = {
      onRevertMessage: vi.fn(),
      onConfirmRevertMessage: vi.fn(),
      onCancelRevertMessage,
      revertTargetItemId: item.id,
      revertAffectedFiles: [],
    };

    const { getByTestId } = render(UserMessage, {
      props: {
        pane,
        item,
        actions,
      },
    });

    expect(getByTestId('user-message-revert-popover')).toBeInTheDocument();
    await fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancelRevertMessage).toHaveBeenCalledTimes(1);

    const outside = document.createElement('button');
    document.body.appendChild(outside);
    await fireEvent.mouseDown(outside);
    expect(onCancelRevertMessage).toHaveBeenCalledTimes(2);
    outside.remove();
  });

  it('requests message fork through the parent-owned handler', async () => {
    const pane = makeCheckpointedPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'forkable',
    });
    const onForkMessage = vi.fn(async () => {});
    const actions: UserMessageActions = { onForkMessage };

    const { getByLabelText } = render(UserMessage, {
      props: {
        pane,
        item,
        actions,
      },
    });

    await fireEvent.click(getByLabelText('Fork from this message'));
    await waitFor(() => expect(onForkMessage).toHaveBeenCalledWith(item));
  });

  it('does not show revert and fork actions for wire-only user messages', () => {
    const pane = makeCheckpointedPane();
    const actions: UserMessageActions = {
      onRevertMessage: vi.fn(),
      onForkMessage: vi.fn(),
    };

    const { queryByLabelText } = render(UserMessage, {
      props: {
        pane,
        actions,
        item: makeItem({
          id: 'user:1',
          threadId: 'thread-1',
          turnIndex: 1,
          kind: 'user_text',
          role: 'user',
          summary: 'wire only',
          meta: JSON.stringify({ wire_only: true }),
        }),
      },
    });

    expect(queryByLabelText('Revert to this message')).toBeNull();
    expect(queryByLabelText('Fork from this message')).toBeNull();
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
