import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';

import SendQueuePreview from './SendQueuePreview.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  markItemsFlushed,
  replaceQueueForThread,
  resetSendQueueForTest,
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

  it('renders nothing when both zones are empty', async () => {
    const pane = await buildPane();

    const { queryByTestId } = render(SendQueuePreview, { props: { pane } });
    expect(queryByTestId('send-queue-preview')).toBeNull();
    expect(queryByTestId('send-queue-zone-queued')).toBeNull();
    expect(queryByTestId('send-queue-zone-flushed')).toBeNull();
  });

  it('renders only the queued zone (Zone 1) when only queued items exist', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-1', message: 'first queued' }),
      makeQueueItem({ id: 'q-2', message: 'second queued' }),
    ]);

    const { getByTestId, queryByTestId, getAllByTestId } = render(SendQueuePreview, {
      props: { pane },
    });

    expect(getByTestId('send-queue-zone-queued')).toBeTruthy();
    expect(queryByTestId('send-queue-zone-flushed')).toBeNull();
    expect(queryByTestId('send-queue-zone-divider')).toBeNull();

    const rows = getAllByTestId('send-queue-preview-row');
    expect(rows.length).toBe(2);
    expect(rows[0].textContent).toContain('first queued');
    expect(rows[1].textContent).toContain('second queued');
  });

  it('renders only the flushed zone (Zone 2) when only flushed items exist', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-1', userItemId: 'u-1', message: 'flushed message' },
    ]);

    const { getByTestId, queryByTestId, getAllByTestId } = render(SendQueuePreview, {
      props: { pane },
    });

    expect(getByTestId('send-queue-zone-flushed')).toBeTruthy();
    expect(queryByTestId('send-queue-zone-queued')).toBeNull();
    expect(queryByTestId('send-queue-zone-divider')).toBeNull();

    const rows = getAllByTestId('send-queue-preview-flushed-row');
    expect(rows.length).toBe(1);
    expect(rows[0].textContent).toContain('flushed message');
  });

  it('renders both zones with a divider when populated together', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-0', userItemId: 'u-0', message: 'in flight' },
    ]);
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-1', message: 'still queued' }),
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    expect(getByTestId('send-queue-zone-flushed')).toBeTruthy();
    expect(getByTestId('send-queue-zone-queued')).toBeTruthy();
    expect(getByTestId('send-queue-zone-divider')).toBeTruthy();
  });

  it('renders the flushed zone above the queued zone', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-flushed', userItemId: 'u-flushed', message: 'in flight' },
    ]);
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-queued', message: 'still queued' }),
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const root = getByTestId('send-queue-preview');
    const flushed = getByTestId('send-queue-zone-flushed');
    const queued = getByTestId('send-queue-zone-queued');

    // Flushed zone comes before the queued zone in DOM order. Items
    // already on the way render ahead of items still waiting behind
    // them — like a launching queue.
    const flushedIndex = Array.from(root.children).indexOf(flushed);
    const queuedIndex = Array.from(root.children).indexOf(queued);
    expect(flushedIndex).toBeGreaterThanOrEqual(0);
    expect(queuedIndex).toBeGreaterThanOrEqual(0);
    expect(flushedIndex).toBeLessThan(queuedIndex);
  });

  it('keys queued rows by item id and flushed rows by userItemId', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [
      makeQueueItem({ id: 'q-alpha' }),
      makeQueueItem({ id: 'q-beta' }),
    ]);
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-gamma', message: 'gamma' },
    ]);

    const { getAllByTestId } = render(SendQueuePreview, { props: { pane } });

    const queuedRows = getAllByTestId('send-queue-preview-row');
    expect(queuedRows.map((row) => row.getAttribute('data-queue-id'))).toEqual([
      'q-alpha',
      'q-beta',
    ]);

    const flushedRows = getAllByTestId('send-queue-preview-flushed-row');
    expect(flushedRows.map((row) => row.getAttribute('data-user-item-id'))).toEqual([
      'u-gamma',
    ]);
  });

  it('marks rows with a data-zone attribute that distinguishes the two zones', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1', message: 'queued' })]);
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: 'flushed' },
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    expect(getByTestId('send-queue-preview-row').getAttribute('data-zone')).toBe('queued');
    expect(getByTestId('send-queue-preview-flushed-row').getAttribute('data-zone')).toBe(
      'flushed',
    );
  });

  it('uses italic + line-clamp on both zones', async () => {
    // Long message text exercises the clamp behaviour. We assert on the
    // semantic Tailwind classes rather than measuring rendered pixels —
    // happy-dom doesn't compute layout and pixel asserts would be brittle.
    const pane = await buildPane();
    const long = 'a very long queued message '.repeat(40);
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1', message: long })]);
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: long },
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const queuedBody = getByTestId('send-queue-preview-row').querySelector('span:last-child');
    const flushedBody = getByTestId('send-queue-preview-flushed-row').querySelector(
      'span:last-child',
    );
    expect(queuedBody?.className).toContain('italic');
    expect(queuedBody?.className).toContain('line-clamp-3');
    expect(flushedBody?.className).toContain('italic');
    expect(flushedBody?.className).toContain('line-clamp-3');
  });

  it('applies a more muted opacity to flushed rows than queued rows', async () => {
    // Opacity hierarchy is the key visual signal between zones — if a
    // future change ever flattens them, this test should fail loudly so
    // the "Zone 2 reads as in flight" intent is preserved.
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: 'flushed' },
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const queuedBody = getByTestId('send-queue-preview-row').querySelector('span:last-child');
    const flushedBody = getByTestId('send-queue-preview-flushed-row').querySelector(
      'span:last-child',
    );
    // Queued reads at fg-muted (~80% on text-primary); flushed reads at
    // fg-hint (~30% on text-primary). The two zones MUST resolve to
    // different foreground tokens or the visual separation collapses.
    expect(queuedBody?.className).toContain('text-fg-muted');
    expect(flushedBody?.className).toContain('text-fg-hint');
    expect(queuedBody?.className).not.toBe(flushedBody?.className);
  });

  it('flushed-zone prefix is animated to read as in motion', async () => {
    const pane = await buildPane();
    markItemsFlushed('thread-1', [
      { queueItemId: 'q-old', userItemId: 'u-1', message: 'flushed' },
    ]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const prefix = getByTestId('send-queue-preview-flushed-row').querySelector(
      'span[aria-hidden="true"]',
    );
    expect(prefix?.className).toContain('animate-pulse');
  });

  it('queued-zone prefix is static (no animation)', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });

    const prefix = getByTestId('send-queue-preview-row').querySelector(
      'span[aria-hidden="true"]',
    );
    expect(prefix?.className).not.toContain('animate-pulse');
  });

  it('exposes an aria-label naming the queue', async () => {
    const pane = await buildPane();
    replaceQueueForThread('thread-1', [makeQueueItem({ id: 'q-1' })]);

    const { getByTestId } = render(SendQueuePreview, { props: { pane } });
    expect(getByTestId('send-queue-preview').getAttribute('aria-label')).toBeTruthy();
  });
});
