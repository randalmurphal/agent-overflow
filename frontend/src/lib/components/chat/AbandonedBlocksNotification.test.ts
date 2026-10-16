import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import AbandonedBlocksNotification from './AbandonedBlocksNotification.svelte';
import { makeItem } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';

function record(fromBlockIndex: number, itemIds: string[] = ['text:0:1']): Item {
  return makeItem({
    id: 'notification:0:abandoned:msg_01',
    kind: 'notification',
    role: 'system',
    summary: 'Discarded by Claude after a stalled response',
    meta: JSON.stringify({ kind: 'abandoned_blocks', api_message_id: 'msg_01', from_block_index: fromBlockIndex, item_ids: itemIds }),
  });
}

describe('<AbandonedBlocksNotification>', () => {
  it('shows the summary and nothing about what followed while nothing has', () => {
    const rec = record(0);
    const { getByTestId } = render(AbandonedBlocksNotification, {
      props: { item: rec, pane: { items: [makeItem({ id: 'text:0:1' }), rec] } },
    });
    const row = getByTestId('abandoned-blocks-notification');
    expect(row.textContent?.trim()).toBe('Discarded by Claude after a stalled response');
    expect(row.dataset.followed).toBe('false');
    expect(row.dataset.wholeResponse).toBe('true');
    expect(row.dataset.discardedCount).toBe('1');
  });

  it('says the reply started over when a whole-response discard is followed by a new row', () => {
    const rec = record(0);
    const { getByTestId } = render(AbandonedBlocksNotification, {
      props: { item: rec, pane: { items: [makeItem({ id: 'text:0:1' }), rec, makeItem({ id: 'text:0:2' })] } },
    });
    const row = getByTestId('abandoned-blocks-notification');
    expect(row.textContent).toContain('response. Claude started the reply over below.');
    expect(row.dataset.followed).toBe('true');
  });

  it('says the reply was picked up when a tail discard is followed by a new row', () => {
    const rec = record(1, ['text:0:2']);
    const { getByTestId } = render(AbandonedBlocksNotification, {
      props: {
        item: rec,
        pane: { items: [makeItem({ id: 'text:0:1' }), makeItem({ id: 'text:0:2' }), rec, makeItem({ id: 'text:0:3' })] },
      },
    });
    const row = getByTestId('abandoned-blocks-notification');
    expect(row.textContent).toContain('Claude picked the reply up below.');
    expect(row.dataset.wholeResponse).toBe('false');
  });

  it('renders without a pane', () => {
    const { getByTestId } = render(AbandonedBlocksNotification, { props: { item: record(0) } });
    expect(getByTestId('abandoned-blocks-notification').dataset.followed).toBe('false');
  });
});
