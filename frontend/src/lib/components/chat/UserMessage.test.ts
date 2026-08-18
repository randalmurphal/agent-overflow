import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  projectTurnCompleted,
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import type { ComposerDraftSnapshot } from '../../stores/composerDraftSnapshots';
import UserMessage from './UserMessage.svelte';
import type { UserMessageActions, UserMessageEditSession } from './userMessageActions';
import { createUserMessageEditUiState } from './userMessageEditUi.svelte';
import { USER_MESSAGE_CLAMP_LINES } from './userMessageClamp';

describe('<UserMessage>', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetBindingMocks();
    resetThreadStatuses();
    // GetAttachmentThumbnail is the inline-grid path (small bytes from the
    // SQLite thumbnail cache); GetAttachmentData is the modal lightbox path
    // (original-resolution refetch). Mock both so tests exercising either
    // path don't blow up on an unstubbed binding.
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    resetThreadStatuses();
  });

  function makeActionsPane(): ThreadPane {
    return {
      threadId: 'thread-1',
      paneId: 'pane-1',
      thread: makeThread({ id: 'thread-1' }),
      attachmentCacheFor: () => undefined,
      setUserMessageExpanded: () => {},
    } as unknown as ThreadPane;
  }

  /**
   * A session for a row that is NOT this one — enough to exercise the
   * one-edit-at-a-time lock without mounting an editor.
   */
  function makeEditSession(itemId: string): UserMessageEditSession {
    const draft = createComposerDraftStore({ persistence: 'none' });
    const seeded: ComposerDraftSnapshot = {
      content: '',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
    };
    draft.seedLocalSnapshot('thread-1', seeded);
    return {
      itemId,
      draft,
      seeded,
      sessionUploadedIds: new Set<string>(),
      ui: createUserMessageEditUiState(),
      stage: 'editing',
      onCancel: () => {},
      onSubmit: () => {},
    };
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

  it('renders the timestamp footer outside the message bubble', () => {
    const { container, getByTestId } = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'hello',
        }),
      },
    });

    const bubble = getByTestId('user-message-bubble');
    const time = container.querySelector('time');
    expect(time).not.toBeNull();
    expect(bubble.contains(time)).toBe(false);
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

  it('shows the fork action for an inactive user message when the handler is available', () => {
    const pane = makeActionsPane();
    const actions: UserMessageActions = {
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
          summary: 'forkable',
        }),
      },
    });

    expect(getByLabelText('Fork from this message')).toBeInTheDocument();
  });

  it('keeps the action toolbar mounted but disabled (grayed out) during an active turn', async () => {
    // Regression for the "footer snaps after the agent responds" glitch:
    // the toolbar used to unmount on getActiveTurn≠null and remount on
    // turn-completed, which toggled the footer geometry as turns started
    // and ended. The buttons now stay rendered but disabled — grayed out
    // via the IconButton disabled styling — so the footer is invariant
    // across turn boundaries and race prevention comes from the native
    // disabled attribute blocking activation.
    const pane = makeActionsPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'commit',
    });
    const onForkMessage = vi.fn(async () => {});
    const actions: UserMessageActions = {
      onForkMessage,
    };

    const { container, getByLabelText } = render(UserMessage, {
      props: { pane, item, actions },
    });

    const forkButton = getByLabelText('Fork from this message');
    expect(forkButton).not.toBeDisabled();

    projectTurnStarted('thread-1', 'turn-1', 1, 1000);
    await tick();

    expect(container.contains(forkButton)).toBe(true);
    expect(forkButton).toBeDisabled();

    await fireEvent.click(forkButton);
    expect(onForkMessage).not.toHaveBeenCalled();

    projectTurnCompleted('thread-1', 'turn-1');
    await tick();

    expect(container.contains(forkButton)).toBe(true);
    expect(forkButton).not.toBeDisabled();
  });

  it('requests message fork through the parent-owned handler', async () => {
    const pane = makeActionsPane();
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

  it('does not show the fork action for wire-only user messages', () => {
    const pane = makeActionsPane();
    const actions: UserMessageActions = {
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

    expect(queryByLabelText('Fork from this message')).toBeNull();
  });

  it('shows the edit action for an inactive user message when the handler is available', () => {
    const pane = makeActionsPane();
    const actions: UserMessageActions = {
      onEditMessage: vi.fn(),
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
          summary: 'editable',
        }),
      },
    });

    expect(getByLabelText('Edit message and resend from here')).toBeInTheDocument();
  });

  it('opens the editor through the parent-owned handler and unclamps the message', async () => {
    const pane = makeActionsPane();
    const setUserMessageExpanded = vi.fn();
    (pane as unknown as { setUserMessageExpanded: unknown }).setUserMessageExpanded =
      setUserMessageExpanded;
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'editable',
    });
    const onEditMessage = vi.fn(async () => {});
    const actions: UserMessageActions = { onEditMessage };

    const { getByLabelText } = render(UserMessage, {
      props: { pane, item, actions },
    });

    await fireEvent.click(getByLabelText('Edit message and resend from here'));
    await waitFor(() => expect(onEditMessage).toHaveBeenCalledWith(item));
    // A clamped message has to open in full to be edited — and stays open
    // if the edit is cancelled.
    expect(setUserMessageExpanded).toHaveBeenCalledWith('user:1', true);
  });

  it('disables the edit action during an active turn and blocks activation', async () => {
    const pane = makeActionsPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'editable',
    });
    const onEditMessage = vi.fn(async () => {});
    const actions: UserMessageActions = { onEditMessage };

    const { getByLabelText } = render(UserMessage, {
      props: { pane, item, actions },
    });

    const editButton = getByLabelText('Edit message and resend from here');
    expect(editButton).not.toBeDisabled();

    projectTurnStarted('thread-1', 'turn-1', 1, 1000);
    await tick();

    expect(editButton).toBeDisabled();
    await fireEvent.click(editButton);
    expect(onEditMessage).not.toHaveBeenCalled();

    projectTurnCompleted('thread-1', 'turn-1');
    await tick();
    expect(editButton).not.toBeDisabled();
  });

  it('disables the edit action while ANOTHER message\'s edit session is active', async () => {
    const pane = makeActionsPane();
    const item = makeItem({
      id: 'user:1',
      threadId: 'thread-1',
      turnIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'editable',
    });
    const onEditMessage = vi.fn(async () => {});
    // The session names a different row: one edit at a time, so this
    // row's button locks too instead of swallowing the click in
    // ChatView's flow guard.
    const actions: UserMessageActions = {
      onEditMessage,
      editSession: makeEditSession('user:other'),
    };

    const { getByLabelText } = render(UserMessage, {
      props: { pane, item, actions },
    });

    const editButton = getByLabelText('Edit message and resend from here');
    expect(editButton).toBeDisabled();
    await fireEvent.click(editButton);
    expect(onEditMessage).not.toHaveBeenCalled();
  });

  it('does not show the edit action for wire-only user messages', () => {
    const pane = makeActionsPane();
    const actions: UserMessageActions = {
      onEditMessage: vi.fn(),
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

    expect(queryByLabelText('Edit message and resend from here')).toBeNull();
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

  it('loads history attachment thumbnails on mount (windowing bufferSize bounds the mount window)', async () => {
    // Pre-rebuild this was gated by an IntersectionObserver inside the
    // row. After the rebuild, the virtualizer's buffer already restricts
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

  // Composer commands (D31): the row shows exactly what was typed, with the
  // command word in the accent colour. Colouring keys off the meta marker the
  // send path wrote, never a live registry match, so history stays truthful
  // about what actually expanded.
  describe('composer command word', () => {
    function renderSummary(summary: string, meta?: string) {
      return render(UserMessage, {
        props: {
          item: makeItem({ kind: 'user_text', role: 'user', summary, meta }),
        },
      });
    }

    it('colours the command word when the meta says it expanded', () => {
      const { getByTestId } = renderSummary(
        '/workflow start the release',
        JSON.stringify({ command: 'workflow' }),
      );
      const word = getByTestId('user-message-command');
      expect(word.textContent).toBe('/workflow');
      expect(word.className).toContain('text-accent');
      // The rest of the message is untouched, and the bubble still reads as
      // one continuous line of text.
      expect(getByTestId('user-message-bubble').textContent).toContain('/workflow start the release');
    });

    it('colours a bare command with no instruction after it', () => {
      const { getByTestId } = renderSummary('/workflow', JSON.stringify({ command: 'workflow' }));
      expect(getByTestId('user-message-command').textContent).toBe('/workflow');
    });

    it('leaves a command-looking message plain when nothing expanded', () => {
      const { queryByTestId } = renderSummary('/workflow start the release');
      expect(queryByTestId('user-message-command')).toBeNull();
    });

    it('colours a mid-sentence command, because that is what invoked it', () => {
      const { getByTestId } = renderSummary(
        'we talked about /workflow yesterday',
        JSON.stringify({ command: 'workflow' }),
      );
      expect(getByTestId('user-message-command').textContent).toBe('/workflow');
      expect(getByTestId('user-message-bubble').textContent).toContain(
        'we talked about /workflow yesterday',
      );
    });

    it('colours every occurrence, though the send expanded once', () => {
      const { getAllByTestId, getByTestId } = renderSummary(
        '/workflow now and /workflow again',
        JSON.stringify({ command: 'workflow' }),
      );
      expect(getAllByTestId('user-message-command').map((el) => el.textContent)).toEqual([
        '/workflow',
        '/workflow',
      ]);
      expect(getByTestId('user-message-bubble').textContent).toContain('/workflow now and /workflow again');
    });

    it('leaves the row plain when the marked command is nowhere in the text', () => {
      const { queryByTestId } = renderSummary(
        'we talked about it yesterday',
        JSON.stringify({ command: 'workflow' }),
      );
      expect(queryByTestId('user-message-command')).toBeNull();
    });

    it('does not colour a longer word that merely starts with the command', () => {
      const { queryByTestId } = renderSummary('/workflows are nice', JSON.stringify({ command: 'workflow' }));
      expect(queryByTestId('user-message-command')).toBeNull();
    });
  });

  // Clamp behavior that needs real geometry (the clip's overflow, the toggle
  // it produces, the expand/collapse round trip) lives in
  // userMessageClamp.browser.test.ts — happy-dom reports zero height, so
  // nothing here can observe an overflow. What this suite pins is the half
  // that is geometry-free: a short message must come out of the feature
  // byte-for-byte unchanged, and the clamp must never reach the attachments.
  describe('clamped long text', () => {
    function renderSummary(summary: string, meta?: string) {
      return render(UserMessage, {
        props: { item: makeItem({ kind: 'user_text', role: 'user', summary, meta }) },
      });
    }

    const longText = Array.from({ length: 40 }, (_, i) => `line ${i}`).join('\n');

    it('leaves a short message with no clip, no fade and no control', () => {
      const { getByTestId, queryByTestId } = renderSummary('ship it');

      const paragraph = getByTestId('user-message-summary');
      expect(paragraph.getAttribute('style')).toBeNull();
      expect(paragraph.className).not.toContain('overflow-hidden');
      expect(paragraph.className).not.toContain('user-message-clamp-fade');
      expect(paragraph.getAttribute('data-clamped')).toBeNull();
      expect(queryByTestId('user-message-clamp-toggle')).toBeNull();
    });

    it('clips a long message to the line threshold', () => {
      const paragraph = renderSummary(longText).getByTestId('user-message-summary');

      expect(paragraph.getAttribute('style')).toContain(`max-height: ${USER_MESSAGE_CLAMP_LINES}lh`);
      expect(paragraph.className).toContain('overflow-hidden');
    });

    it('keeps command colouring inside the clipped text', () => {
      const { getByTestId } = renderSummary(
        `/plan the migration\n${longText}`,
        JSON.stringify({ command: 'plan' }),
      );
      expect(getByTestId('user-message-command').textContent).toBe('/plan');
    });

    it('leaves attachments outside the clipped region', () => {
      const { getByTestId } = renderSummary(longText, JSON.stringify({
        attachments: [{
          id: 'att-1',
          threadId: 'thread-1',
          filename: 'hero.png',
          mimeType: 'image/png',
          size: 128,
          relativePath: 'thread-1/att-1.png',
          createdAt: 1,
        }],
      }));

      const attachments = getByTestId('user-message-attachments');
      const paragraph = getByTestId('user-message-summary');
      expect(paragraph.contains(attachments)).toBe(false);
      expect(getByTestId('user-message-bubble').contains(attachments)).toBe(true);
    });
  });
});
