import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import CompactionDivider from './CompactionDivider.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('compaction summary disclosure', () => {
  beforeEach(resetBindingMocks);
  afterEach(cleanup);

  it('bounds a long provider summary and scrolls it in place', async () => {
    const summary = Array.from({ length: 120 }, (_, index) => `summary line ${index}`).join('\n');
    setBindingMock('GetPayloadData', async () => ({ data: summary }));
    const item = makeItem({
      kind: 'compaction',
      role: 'system',
      status: 'completed',
      summary: 'Context compacted',
      payloadId: 'compaction-payload',
      payloadKind: 'compaction',
    });

    const { getByTestId } = render(CompactionDivider, { props: { item } });
    await fireEvent.click(getByTestId('compaction-toggle'));
    await waitFor(() => expect(getByTestId('compaction-summary').textContent).toContain('summary line 119'));

    const detail = getByTestId('compaction-detail');
    expect(detail.clientHeight).toBeLessThanOrEqual(240);
    expect(detail.scrollHeight).toBeGreaterThan(detail.clientHeight);
    expect(getComputedStyle(detail).overflowY).toBe('auto');
  });
});
