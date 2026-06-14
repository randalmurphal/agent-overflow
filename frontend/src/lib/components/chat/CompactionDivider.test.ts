import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import CompactionDivider from './CompactionDivider.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function compactionItem(overrides = {}) {
  return makeItem({
    kind: 'compaction',
    role: 'system',
    summary: 'Context compacted',
    ...overrides,
  });
}

describe('<CompactionDivider>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders a plain, non-interactive divider when no summary payload is linked', () => {
    // Headless claude / Codex boundaries carry no committed summary, so the row
    // stays the original static divider with no toggle.
    const dataMock = setBindingMock('GetPayloadData', async () => ({ data: '' }));
    const { queryByTestId, container } = render(CompactionDivider, {
      props: { item: compactionItem() },
    });

    expect(queryByTestId('compaction-divider')).not.toBeNull();
    expect(queryByTestId('compaction-toggle')).toBeNull();
    expect(container.textContent).toContain('Context compacted');
    expect(dataMock).not.toHaveBeenCalled();
  });

  it('lazy-loads the committed summary on expand', async () => {
    // Payload data is the raw summary text (same shape as a thinking payload),
    // not a JSON wrapper — the summarizer's reasoning streamed as its own row.
    const summary = 'The user asked to fix compaction grouping under one row.';
    const dataMock = setBindingMock('GetPayloadData', async () => ({ data: summary }));

    const { getByTestId, queryByTestId } = render(CompactionDivider, {
      props: {
        item: compactionItem({
          payloadId: 'compaction-payload',
          payloadKind: 'compaction',
          payloadMeta: JSON.stringify({
            summaryPreview: 'The user asked',
            summaryChars: summary.length,
          }),
        }),
      },
    });

    // Collapsed: a toggle exists but the heavy detail has NOT loaded — the
    // payload must not be fetched until the user expands (memory budget).
    const toggle = getByTestId('compaction-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(queryByTestId('compaction-detail')).toBeNull();
    expect(dataMock).not.toHaveBeenCalled();

    await fireEvent.click(toggle);
    await tick();

    await waitFor(() => {
      expect(getByTestId('compaction-summary').textContent).toContain(
        'fix compaction grouping',
      );
    });
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(dataMock).toHaveBeenCalledTimes(1);
  });

  it('collapses the detail again on a second toggle', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'a committed summary' }));
    const { getByTestId, queryByTestId } = render(CompactionDivider, {
      props: {
        item: compactionItem({
          payloadId: 'compaction-payload',
          payloadKind: 'compaction',
        }),
      },
    });

    const toggle = getByTestId('compaction-toggle');
    await fireEvent.click(toggle);
    await waitFor(() =>
      expect(getByTestId('compaction-summary').textContent).toContain(
        'a committed summary',
      ),
    );

    await fireEvent.click(toggle);
    await tick();
    expect(queryByTestId('compaction-detail')).toBeNull();
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });
});
