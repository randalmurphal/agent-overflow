import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import UsageModal from './UsageModal.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket, type UsageQuery } from '../../stores/bindings';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { resetUsagePeriodForTest } from '../../stores/usagePeriod.svelte';

function bucket(groupKey: string, overrides: Partial<UsageBucket> = {}): UsageBucket {
  return new UsageBucket({
    bucket: groupKey,
    inputTokens: 0,
    outputTokens: 0,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0,
    costUsd: 0,
    turnCount: 0,
    unpricedRows: 0,
    ...overrides,
  });
}

describe('<UsageModal>', () => {
  beforeEach(() => {
    resetUsagePeriodForTest();
    resetProjectsForTest();
    addProjectLocal({
      id: 'proj-known',
      path: '/tmp/known',
      name: 'Known Project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });

    setBindingMock('GetUsageStats', async (query: UsageQuery) => {
      if (query.groupBy === 'day') return [];
      if (query.groupBy === '') {
        return [
          bucket('', {
            inputTokens: 1_500_000,
            outputTokens: 500_000,
            cacheReadInputTokens: 200_000,
            cacheCreationInputTokens: 50_000,
            costUsd: 12.5,
            turnCount: 7,
          }),
        ];
      }
      if (query.groupBy === 'model') {
        return [
          bucket('claude-cheap', { inputTokens: 1000, outputTokens: 200, costUsd: 0.5 }),
          bucket('claude-expensive', { inputTokens: 2000, outputTokens: 400, costUsd: 9.99 }),
        ];
      }
      if (query.groupBy === 'project') {
        return [
          bucket('proj-known', { inputTokens: 100, outputTokens: 50, costUsd: 3 }),
          bucket('', { inputTokens: 10, outputTokens: 5, costUsd: 0.1 }),
          bucket('proj-gone', { inputTokens: 1, outputTokens: 1, costUsd: 0.01 }),
        ];
      }
      return [];
    });
  });

  it('renders the lifetime-of-selection totals row', async () => {
    // The totals tiles render unconditionally (unlike the model table /
    // top-projects sections, which gate on rows.length), so waiting for
    // the testid to exist wouldn't wait for the async fetch to resolve —
    // wait for the actual values instead.
    const { getAllByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    await waitFor(() => {
      const values = getAllByTestId('usage-totals-value').map((el) => el.textContent?.trim());
      expect(values).toEqual(['1.5M', '500.0k', '200.0k', '50.0k', '$12.50', '7']);
    });
  });

  it('renders the per-model table sorted by cost descending', async () => {
    const { findAllByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    const names = (await findAllByTestId('usage-model-row-name')).map((el) => el.textContent?.trim());
    expect(names).toEqual(['claude-expensive', 'claude-cheap']);
    const costs = (await findAllByTestId('usage-model-row-cost')).map((el) => el.textContent?.trim());
    expect(costs).toEqual(['$9.99', '$0.50']);
  });

  it('maps project ids to display names, including "No project" and "(deleted)"', async () => {
    const { findAllByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    const names = (await findAllByTestId('usage-top-project-name')).map((el) => el.textContent?.trim());
    // Ranked by cost desc: proj-known (3) > '' (0.1) > proj-gone (0.01).
    expect(names).toEqual(['Known Project', 'No project', '(deleted)']);
  });
});
