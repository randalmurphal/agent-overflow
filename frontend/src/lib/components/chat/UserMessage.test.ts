import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentDownload } from '../../../test/mocks/attachmentTransfer';
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
    // SQLite thumbnail cache, still an RPC); the modal lightbox path is a
    // ticketed HTTP transfer of the original bytes. Stub both so tests
    // exercising either don't blow up on an unstubbed call.
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    mockAttachmentDownload();
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

  // A message that reached this thread from outside Agent Overflow (Codex's
  // own `codex queue --thread ...`) keeps the user bubble — it IS user-role
  // content the model answered — but says where it came from. Attributing a
  // stranger's write to the reader is a transcript that lies about who asked.
  it('marks a message queued from outside, and leaves a locally typed one unmarked', () => {
    const external = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'run the tests',
          meta: JSON.stringify({ origin: 'external-queue' }),
        }),
      },
    });
    expect(external.getByTestId('user-message-external-origin').textContent).toContain(
      'Queued from outside Agent Overflow',
    );
    external.unmount();

    const local = render(UserMessage, {
      props: { item: makeItem({ kind: 'user_text', role: 'user', summary: 'run the tests' }) },
    });
    expect(local.queryByTestId('user-message-external-origin')).toBeNull();
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

  it('keeps the toolbar mounted during an active turn: edit disabled, fork live', async () => {
    // Two contracts in one mount. (1) Regression for the "footer snaps
    // after the agent responds" glitch: the toolbar used to unmount on
    // getActiveTurn≠null and remount on turn-completed, which toggled the
    // footer geometry as turns started and ended — both buttons stay
    // rendered across the whole turn. (2) The turn lock covers EDIT only:
    // edit-and-resend reverts the source thread's history and cannot race
    // the active turn, while fork snapshots the running thread
    // as-if-interrupted without touching it, so the fork button stays
    // enabled and clickable mid-turn.
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
    const onEditMessage = vi.fn(async () => {});
    const actions: UserMessageActions = {
      onForkMessage,
      onEditMessage,
    };

    const { container, getByLabelText } = render(UserMessage, {
      props: { pane, item, actions },
    });

    const forkButton = getByLabelText('Fork from this message');
    const editButton = getByLabelText('Edit message and resend from here');
    expect(forkButton).not.toBeDisabled();
    expect(editButton).not.toBeDisabled();

    projectTurnStarted('thread-1', 'turn-1', 1, 1000);
    await tick();

    expect(container.contains(editButton)).toBe(true);
    expect(editButton).toBeDisabled();
    await fireEvent.click(editButton);
    expect(onEditMessage).not.toHaveBeenCalled();

    expect(container.contains(forkButton)).toBe(true);
    expect(forkButton).not.toBeDisabled();
    await fireEvent.click(forkButton);
    expect(onForkMessage).toHaveBeenCalledTimes(1);

    projectTurnCompleted('thread-1', 'turn-1');
    await tick();

    expect(container.contains(forkButton)).toBe(true);
    expect(forkButton).not.toBeDisabled();
    expect(editButton).not.toBeDisabled();
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

  it('renders a file attachment as an inert chip and numbers the images around it', async () => {
    const thumbnail = setBindingMock('GetAttachmentThumbnail', async () => ({
      data: 'iVBORw0KGgo=',
      mimeType: 'image/png',
    }));
    const { getByTestId, getByText, getAllByLabelText, queryByLabelText } = render(UserMessage, {
      props: {
        item: makeItem({
          kind: 'user_text',
          role: 'user',
          summary: 'look here [Image #1] and [Image #2]',
          meta: JSON.stringify({
            attachments: [
              {
                id: 'att-1',
                threadId: 'thread-1',
                filename: 'one.png',
                mimeType: 'image/png',
                size: 128,
              },
              {
                id: 'att-2',
                threadId: 'thread-1',
                filename: 'report.pdf',
                mimeType: 'application/pdf',
                size: 2048,
                kind: 'file',
              },
              {
                id: 'att-3',
                threadId: 'thread-1',
                filename: 'two.png',
                mimeType: 'image/png',
                size: 128,
                kind: 'image',
              },
            ],
          }),
        }),
      },
    });

    // `#2` is the SECOND IMAGE, matching `[Image #2]` in the text — not the
    // second attachment, which is the file.
    expect(getAllByLabelText(/^Image \d+$/).map((node) => node.textContent?.trim()))
      .toEqual(['#1', '#2']);
    expect(getByTestId('user-message-file-attachments')).toBeInTheDocument();
    expect(getByText('report.pdf')).toBeInTheDocument();
    expect(getByText('2.0 KB')).toBeInTheDocument();
    // Not a button, and its bytes are never requested.
    expect(queryByLabelText('Preview report.pdf')).toBeNull();
    await waitFor(() => expect(thumbnail).toHaveBeenCalledTimes(2));
    expect(thumbnail.mock.calls.map((call) => call[1])).toEqual(['att-1', 'att-3']);
  });

  it('loads history attachment thumbnails on mount (windowing bufferSize bounds the mount window)', async () => {
    // Pre-rebuild this was gated by an IntersectionObserver inside the
    // row. After the rebuild, the virtualizer's buffer already restricts
    // mount to rows near the viewport, and the per-pane attachment cache
    // de-dupes across remounts — so a separate IO observer was redundant
    // and got removed. Loading happens immediately on mount, and goes
    // through GetAttachmentThumbnail (not the full-size HTTP transfer,
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
