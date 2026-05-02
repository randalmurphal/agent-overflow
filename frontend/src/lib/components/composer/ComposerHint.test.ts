import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';

import ComposerHint from './ComposerHint.svelte';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
  type ComposerDraftStore,
} from '../../stores/composerDraft.svelte';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
  type QueueItem,
} from '../../stores/sendQueue.svelte';
import type { Attachment } from '../../types/attachment';

function installDraftMocks(): void {
  setBindingMock('GetDraft', async (threadId: unknown) => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
}

async function buildDraft(threadId: string | null = 'thread-1'): Promise<ComposerDraftStore> {
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

function makeQueueItem(overrides: Partial<QueueItem> = {}): QueueItem {
  return {
    id: 'q-1',
    threadId: 'thread-1',
    message: 'queued',
    attachmentIds: [],
    sourceProposedPlan: null,
    revisionSourceProposedPlan: null,
    revisionSourceCommentIds: undefined,
    enqueuedAt: 1,
    ...overrides,
  };
}

function makeAttachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 16,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

describe('<ComposerHint>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetComposerDraftSnapshotsForTest();
    resetSendQueueForTest();
    installDraftMocks();
  });

  afterEach(() => {
    resetSendQueueForTest();
  });

  it('renders the hint when queue has items AND composer is empty', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem()]);

    const { getByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(getByTestId('composer-hint').textContent?.trim()).toBe(
      'Press ↑ to retract queued message',
    );
  });

  it('does not render hint text when no queue items', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { queryByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(queryByTestId('composer-hint')).toBeNull();
  });

  it('does not render hint text when composer has typed content', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem()]);
    draft.setContent('typing a follow-up');

    const { queryByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(queryByTestId('composer-hint')).toBeNull();
  });

  it('does not render hint text when composer has attachments', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem()]);
    draft.addAttachment(makeAttachment('att-1'));

    const { queryByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(queryByTestId('composer-hint')).toBeNull();
  });

  it('does not render hint text when composer has terminal chips', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem()]);
    draft.addTerminalChip({
      id: 'chip-1',
      label: 'pwd',
      preview: '/repo',
      content: '/repo',
      createdAt: 1,
    });

    const { queryByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(queryByTestId('composer-hint')).toBeNull();
  });

  it('does not render hint text when no thread is active', async () => {
    const pane = createThreadPane() as ThreadPane;
    const draft = await buildDraft(null);

    const { queryByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(queryByTestId('composer-hint')).toBeNull();
  });

  it('reserves the slot height even when the hint is hidden', async () => {
    // The composer's bottom edge is anchored by the bottom of the overlay
    // in ChatView; reserving a fixed-height slot under the BelowComposerBar
    // ensures the composer doesn't shift between hint-visible and
    // hint-hidden states. Treat the wrapper element + its min-h class as a
    // structural invariant the layout depends on.
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByTestId } = render(ComposerHint, { props: { pane, draft } });
    const slot = getByTestId('composer-hint-slot');
    expect(slot).toBeTruthy();
    expect(slot.className).toContain('min-h-[1.25rem]');
    expect(slot.getAttribute('aria-hidden')).toBe('true');
  });

  it('keeps the slot mounted when the hint is visible', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem()]);

    const { getByTestId } = render(ComposerHint, { props: { pane, draft } });
    const slot = getByTestId('composer-hint-slot');
    expect(slot.className).toContain('min-h-[1.25rem]');
    expect(slot.getAttribute('aria-hidden')).toBe('false');
  });

  it('uses singular phrasing for one queued message', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);

    const { getByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(getByTestId('composer-hint').textContent?.trim()).toBe(
      'Press ↑ to retract queued message',
    );
  });

  it('uses plural phrasing for multiple queued messages', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-1', message: 'first' }),
      makeQueueItem({ id: 'q-2', message: 'second' }),
    ]);

    const { getByTestId } = render(ComposerHint, { props: { pane, draft } });
    expect(getByTestId('composer-hint').textContent?.trim()).toBe(
      'Press ↑ to retract queued messages',
    );
  });
});
