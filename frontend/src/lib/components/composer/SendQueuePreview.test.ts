import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';

import SendQueuePreview from './SendQueuePreview.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
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

  it('exposes an aria-label naming pending messages', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    expect(getByTestId('send-queue-preview').getAttribute('aria-label')).toBe(
      'Pending user messages',
    );
  });
});
