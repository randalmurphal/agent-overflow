import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import SendQueuePreview from './SendQueuePreview.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  createComposerDraftStore,
  resetComposerDraftSnapshotsForTest,
} from '../../stores/composerDraft.svelte';
import {
  enqueue as enqueueQueueItem,
  getQueueForThread,
  resetSendQueueForTest,
} from '../../stores/sendQueue.svelte';

function enqueueSimple(threadId: string, message: string): void {
  enqueueQueueItem(threadId, {
    message,
    attachments: [],
    terminalChips: [],
    sourceProposedPlan: null,
  });
}

async function buildDraftStore(threadId: string) {
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

describe('<SendQueuePreview>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSendQueueForTest();
    resetComposerDraftSnapshotsForTest();
    setBindingMock('GetDraft', async (threadId: string) => ({
      threadId,
      content: '',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 0,
    }));
    setBindingMock('SaveDraft', async () => {});
    setBindingMock('ClearDraft', async () => {});
    setBindingMock('ListAttachments', async () => []);
  });

  afterEach(() => {
    cleanup();
  });

  it('renders nothing when the queue is empty', async () => {
    const pane = await buildPane();
    const draft = await buildDraftStore(pane.threadId ?? 'thread-1');

    const { queryByTestId } = render(SendQueuePreview, { props: { pane, draft } });
    await tick();

    expect(queryByTestId('send-queue-preview')).toBeNull();
  });

  it('renders all queued items in FIFO order with ↳ prefix and dim italic body', async () => {
    const pane = await buildPane();
    const tid = pane.threadId ?? 'thread-1';
    enqueueSimple(tid, 'first');
    enqueueSimple(tid, 'second');
    enqueueSimple(tid, 'third');
    const draft = await buildDraftStore(tid);

    const { getAllByTestId, getByTestId } = render(SendQueuePreview, { props: { pane, draft } });
    await tick();

    const rows = getAllByTestId('send-queue-preview-row');
    expect(rows).toHaveLength(3);

    const editButtons = getAllByTestId('send-queue-preview-edit');
    expect(editButtons.map((btn) => btn.textContent?.trim())).toEqual(['first', 'second', 'third']);
    // Each edit button is italic with the line-clamp utility for
    // 3-line truncation; assert the class survived render.
    for (const button of editButtons) {
      expect(button.className).toMatch(/italic/);
      expect(button.className).toMatch(/line-clamp-3/);
    }

    expect(getByTestId('send-queue-preview').textContent).toContain('↳');
  });

  it('clicking the × button cancels only that item', async () => {
    const pane = await buildPane();
    const tid = pane.threadId ?? 'thread-1';
    enqueueSimple(tid, 'first');
    enqueueSimple(tid, 'second');
    enqueueSimple(tid, 'third');
    const draft = await buildDraftStore(tid);

    const { getAllByTestId } = render(SendQueuePreview, { props: { pane, draft } });
    await tick();

    const cancelButtons = getAllByTestId('send-queue-preview-cancel');
    await fireEvent.click(cancelButtons[1]);
    await tick();

    expect(getQueueForThread(tid).map((item) => item.message)).toEqual(['first', 'third']);
  });

  it('clicking a row pops it and restores it into the composer draft', async () => {
    const pane = await buildPane();
    const tid = pane.threadId ?? 'thread-1';
    enqueueSimple(tid, 'edit me');
    enqueueSimple(tid, 'leave alone');
    const draft = await buildDraftStore(tid);

    const { getAllByTestId } = render(SendQueuePreview, { props: { pane, draft } });
    await tick();

    const editButtons = getAllByTestId('send-queue-preview-edit');
    await fireEvent.click(editButtons[0]);
    // restoreDraftFor is async (saves snapshot, then mirrors into the
    // local draft store). Wait one microtask cycle.
    await Promise.resolve();
    await tick();

    expect(getQueueForThread(tid).map((item) => item.message)).toEqual(['leave alone']);
    expect(draft.content).toBe('edit me');
  });

  it('renders an aria-label on the edit button so screen readers announce intent', async () => {
    const pane = await buildPane();
    const tid = pane.threadId ?? 'thread-1';
    enqueueSimple(tid, 'first');
    const draft = await buildDraftStore(tid);

    const { getByTestId } = render(SendQueuePreview, { props: { pane, draft } });
    await tick();

    const button = getByTestId('send-queue-preview-edit');
    expect(button.getAttribute('aria-label')).toBe('Edit queued message');

    const cancel = getByTestId('send-queue-preview-cancel');
    expect(cancel.getAttribute('aria-label')).toBe('Remove from queue');
  });
});
