import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';

import SendQueuePreview from './SendQueuePreview.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  applyFlushedLifecycle,
  markItemsFlushed,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
  type QueueItem,
} from '../../stores/sendQueue.svelte';
import { resetForTest as resetThreadStatuses } from '../../stores/threadStatuses.svelte';

function makeQueueItem(overrides: Partial<QueueItem> = {}): QueueItem {
  return {
    id: 'q-1',
    threadId: 'thread-1',
    message: 'queued message body',
    attachmentIds: [],
    sourceProposedPlan: null,
    revisionSourceProposedPlan: null,
    revisionSourceCommentIds: undefined,
    enqueuedAt: 1,
    ...overrides,
  };
}

describe('<SendQueuePreview>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSendQueueForTest();
    resetThreadStatuses();
  });

  afterEach(() => {
    resetSendQueueForTest();
    resetThreadStatuses();
  });

  it('renders nothing when no pending messages exist', async () => {
    const pane = await buildPane();

    const { queryByTestId } = render(SendQueuePreview, { props: { pane } });

    expect(queryByTestId('send-queue-preview')).toBeNull();
  });

  it('renders queued messages as pending rows', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-1', message: 'first queued' }),
      makeQueueItem({ id: 'q-2', message: 'second queued' }),
    ]);

    const { getAllByTestId } = render(SendQueuePreview, { props: { pane } });

    const rows = getAllByTestId('send-queue-preview-row');
    expect(rows.map((row) => row.getAttribute('data-state'))).toEqual(['queued', 'queued']);
    expect(rows.map((row) => row.getAttribute('data-queue-id'))).toEqual(['q-1', 'q-2']);
    expect(rows[0].textContent).toContain('first queued');
    expect(rows[1].textContent).toContain('second queued');
  });

  it('renders flushed messages before still-queued messages', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: 'sent to provider' },
    ]);
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-new', message: 'still queued' }),
    ]);

    const { getAllByTestId } = render(SendQueuePreview, { props: { pane } });

    const rows = getAllByTestId('send-queue-preview-row');
    expect(rows.map((row) => row.getAttribute('data-state'))).toEqual(['flushed', 'queued']);
    expect(rows[0].getAttribute('data-user-item-id')).toBe('u-1');
    expect(rows[1].getAttribute('data-queue-id')).toBe('q-new');
  });

  it('keeps provider-flushed rows in motion while they await confirmation', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: 'sent to provider' },
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const prefix = getByTestId('send-queue-preview-row').querySelector(
      'span[aria-hidden="true"]',
    );
    expect(prefix?.className).toContain('animate-pulse');
  });

  // Provider delivery acks (Claude `command_lifecycle`) are additive: a
  // row with no lifecycle renders exactly as it did before the channel
  // existed, which is what a Codex thread or an older Claude CLI gets.
  describe('provider delivery acks', () => {
    async function renderFlushed(lifecycle?: Parameters<typeof applyFlushedLifecycle>[2]) {
      const pane = await buildPane();
      markItemsFlushed('thread-1', [
        { queueItemId: 'q-1', userItemId: 'u-1', message: 'steer me' },
      ]);
      if (lifecycle) applyFlushedLifecycle('thread-1', 'u-1', lifecycle);
      return render(SendQueuePreview, { props: { pane } });
    }

    it('leaves an unacked row exactly as before', async () => {
      const { getByTestId } = await renderFlushed();
      const row = getByTestId('send-queue-preview-row');
      expect(row.getAttribute('data-lifecycle')).toBeNull();
      expect(row.getAttribute('title')).toBe(
        'Sent to the agent, waiting for it to enter context',
      );
    });

    it('names a mid-turn delivery as steering', async () => {
      const { getByTestId } = await renderFlushed({ state: 'started', delivery: 'mid_turn' });
      const row = getByTestId('send-queue-preview-row');
      expect(row.getAttribute('data-lifecycle')).toBe('started');
      expect(row.getAttribute('data-delivery')).toBe('mid_turn');
      expect(row.textContent).toContain('steering');
      expect(row.getAttribute('title')).toBe('Delivered into the running turn');
    });

    it('does not call a new-turn delivery steering', async () => {
      const { getByTestId } = await renderFlushed({ state: 'started', delivery: 'new_turn' });
      const row = getByTestId('send-queue-preview-row');
      expect(row.textContent).not.toContain('steering');
      expect(row.getAttribute('title')).toBe('Started as its own turn');
    });

    // A cancelled message never arrives. Without this the row would sit
    // above the composer pulsing forever.
    it('stops the pulse and marks a cancelled message as undelivered', async () => {
      const { getByTestId } = await renderFlushed({ state: 'cancelled' });
      const row = getByTestId('send-queue-preview-row');
      expect(row.getAttribute('data-lifecycle')).toBe('cancelled');
      expect(row.textContent).toContain('not delivered');
      expect(
        row.querySelector('span[aria-hidden="true"]')?.className,
      ).not.toContain('animate-pulse');
    });

    it('stays quiet for a plain queued ack', async () => {
      const { getByTestId } = await renderFlushed({ state: 'queued' });
      const row = getByTestId('send-queue-preview-row');
      expect(row.getAttribute('data-lifecycle')).toBe('queued');
      expect(row.getAttribute('title')).toBe('The agent has this message queued');
      expect(row.textContent).toContain('steer me');
      expect(row.textContent).not.toContain('steering');
    });
  });

  it('exposes an aria-label naming pending messages', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    expect(getByTestId('send-queue-preview').getAttribute('aria-label')).toBe(
      'Pending user messages',
    );
  });
});
