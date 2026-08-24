import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import APIRetryRow from './APIRetryRow.svelte';
import type { Item } from '../../types/models';

function makeRetryItem(overrides: Partial<Item> = {}): Item {
  const meta = { kind: 'api_retry', attempt: 4, max_retries: 10, error: 'rate_limit' };
  return {
    id: 'retry:0',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'api_retry',
    role: 'system',
    status: 'running',
    summary: 'Retrying (4/10, rate_limit)',
    meta: JSON.stringify(meta),
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  } as Item;
}

describe('<APIRetryRow>', () => {
  it('renders summary copy and applies the live (pulsing) styling while running', () => {
    const { getByTestId } = render(APIRetryRow, { props: { item: makeRetryItem() } });
    const row = getByTestId('api-retry-row');
    expect(row.textContent).toContain('Retrying (4/10, rate_limit)');
    expect(row.dataset.status).toBe('running');
    // Pulsing state lives on the inner svg via animate-pulse — assert
    // it's present for the running row.
    const icon = row.querySelector('.lucide-icon');
    expect(icon?.getAttribute('class') ?? '').toContain('animate-pulse');
  });

  it('drops the pulse and the live aria-attributes once the row is completed', () => {
    const { getByTestId } = render(APIRetryRow, {
      props: { item: makeRetryItem({ status: 'completed' }) },
    });
    const row = getByTestId('api-retry-row');
    expect(row.dataset.status).toBe('completed');
    expect(row.getAttribute('role')).toBeNull();
    expect(row.getAttribute('aria-live')).toBeNull();
    const icon = row.querySelector('.lucide-icon');
    expect(icon?.getAttribute('class') ?? '').not.toContain('animate-pulse');
  });

  it('falls back to a generic summary when the wire summary is empty', () => {
    const { getByTestId } = render(APIRetryRow, {
      props: { item: makeRetryItem({ summary: '' }) },
    });
    expect(getByTestId('api-retry-row').textContent).toContain('Retrying provider request');
  });

  it('encodes attempt/max in the tooltip when the upstream summary omits them', () => {
    const { getByTestId } = render(APIRetryRow, {
      props: {
        item: makeRetryItem({
          summary: 'Retrying',
          meta: JSON.stringify({ attempt: 4, max_retries: 10, error: 'server_error' }),
        }),
      },
    });
    const row = getByTestId('api-retry-row');
    expect(row.getAttribute('title')).toBe('Attempt 4 of 10: server_error');
  });
});
