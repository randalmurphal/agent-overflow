import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import UsageModal from './UsageModal.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { CodexAccountUsage, UsageBucket, type UsageQuery } from '../../stores/bindings';
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
    sessionCount: 0,
    unpricedRows: 0,
    ...overrides,
  });
}

describe('<UsageModal>', () => {
  let seenQueries: UsageQuery[] = [];

  beforeEach(() => {
    resetUsagePeriodForTest();
    resetProjectsForTest();
    seenQueries = [];
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
      seenQueries.push(query);
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
            sessionCount: 3,
          }),
        ];
      }
      if (query.groupBy === 'model') {
        return [
          bucket('claude-haiku-4-5-20251001', { inputTokens: 1000, outputTokens: 200, costUsd: 0.5 }),
          bucket('claude-fable-5', { inputTokens: 2000, outputTokens: 400, costUsd: 9.99 }),
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

  it('renders the totals row with a session count (threads, not turns)', async () => {
    // The totals tiles render unconditionally (unlike the model table,
    // which gates on rows.length), so waiting for the testid to exist
    // wouldn't wait for the async fetch to resolve — wait for the actual
    // values instead. The last tile is sessionCount=3, NOT turnCount=7.
    const { getAllByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    await waitFor(() => {
      const values = getAllByTestId('usage-totals-value').map((el) => el.textContent?.trim());
      expect(values).toEqual(['1.5M', '500.0k', '200.0k', '50.0k', '$12.50', '3']);
    });
  });

  it('renders the per-model table with picker-style display names, sorted by cost descending', async () => {
    const { findAllByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    const rows = await findAllByTestId('usage-model-row-name');
    // Friendly names in the cell; the raw ledger slug survives as the
    // hover tooltip.
    expect(rows.map((el) => el.textContent?.trim())).toEqual(['Fable 5', 'Haiku 4.5']);
    expect(rows.map((el) => el.getAttribute('title'))).toEqual([
      'claude-fable-5',
      'claude-haiku-4-5-20251001',
    ]);
    const costs = (await findAllByTestId('usage-model-row-cost')).map((el) => el.textContent?.trim());
    expect(costs).toEqual(['$9.99', '$0.50']);
  });

  it('lists projects with usage in the filter dropdown, excluding the no-project bucket', async () => {
    const { findByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    const select = (await findByTestId('usage-project-select')) as HTMLSelectElement;
    await waitFor(() => {
      const labels = Array.from(select.options).map((o) => o.textContent?.trim());
      // "All Projects" first, then usage-bearing projects sorted by
      // label. The empty-id bucket is NOT listed — an empty value is
      // both the all-projects option and the backend's no-filter
      // sentinel, so it can't double as a selectable bucket.
      expect(labels).toEqual(['All Projects', '(deleted)', 'Known Project']);
    });
  });

  it('selecting a project refetches the stats sections filtered to that project', async () => {
    const { findByTestId } = render(UsageModal, {
      props: { open: true, onClose: () => {} },
    });
    const select = (await findByTestId('usage-project-select')) as HTMLSelectElement;
    await waitFor(() => {
      expect(Array.from(select.options).some((o) => o.value === 'proj-known')).toBe(true);
    });

    seenQueries = [];
    await fireEvent.change(select, { target: { value: 'proj-known' } });

    await waitFor(() => {
      // Totals (groupBy ''), model table, and the heatmap (groupBy
      // 'day') all refetch with the project filter applied.
      const filtered = seenQueries.filter((q) => q.projectId === 'proj-known');
      expect(filtered.map((q) => q.groupBy).sort()).toEqual(['', 'day', 'model']);
    });
  });

  // Codex's own account-level report. It is a different population from
  // every other section (the whole login, including usage outside AO),
  // so it renders only under the Codex filter, and "nothing to report"
  // must render nothing rather than zeros.
  describe('Codex account section', () => {
    function todayKey(): string {
      const now = new Date();
      const month = String(now.getMonth() + 1).padStart(2, '0');
      const day = String(now.getDate()).padStart(2, '0');
      return `${now.getFullYear()}-${month}-${day}`;
    }

    async function selectCodex(getByRole: (role: string, opts: object) => HTMLElement) {
      await fireEvent.click(getByRole('radio', { name: 'Codex' }));
    }

    it('is not rendered — or even fetched — until the Codex filter is selected', async () => {
      setBindingMock('GetCodexAccountUsage', async () =>
        new CodexAccountUsage({ lifetimeTokens: 11_776_335_004, dailyBuckets: [] }),
      );
      const { queryByTestId, findByTestId, getByRole } = render(UsageModal, {
        props: { open: true, onClose: () => {} },
      });

      // "All" is the default filter: the section answers for one login,
      // not for a selection, so it must stay out of the combined view.
      await waitFor(() => expect(getBindingMock('GetUsageStats')).toBeDefined());
      expect(queryByTestId('usage-codex-account')).toBeNull();
      expect(getBindingMock('GetCodexAccountUsage')).not.toHaveBeenCalled();

      await selectCodex(getByRole);
      await findByTestId('usage-codex-account');
      expect(getBindingMock('GetCodexAccountUsage')).toHaveBeenCalledTimes(1);
    });

    it('renders only the stats Codex actually reported, and the day grid', async () => {
      setBindingMock('GetCodexAccountUsage', async () =>
        new CodexAccountUsage({
          lifetimeTokens: 11_776_335_004,
          currentStreakDays: 8,
          longestStreakDays: 17,
          // peakDailyTokens / longestRunningTurnSec deliberately absent.
          dailyBuckets: [{ startDate: todayKey(), tokens: 725_458_670 }],
          accountEmail: 'someone@example.com',
        }),
      );
      const { findAllByTestId, findByText, getByRole } = render(UsageModal, {
        props: { open: true, onClose: () => {} },
      });
      await selectCodex(getByRole);

      const values = await findAllByTestId('usage-codex-account-value');
      // An unreported stat produces no tile at all — never a zero.
      expect(values.map((el) => el.textContent?.trim())).toEqual(['11.8B', '8d (best 17d)']);
      await findByText('someone@example.com');

      const cells = await findAllByTestId('usage-codex-account-cell');
      expect(cells).toHaveLength(26 * 7);
      const today = cells.find((el) => el.getAttribute('data-date') === todayKey());
      expect(today?.getAttribute('data-level')).not.toBe('0');
    });

    it('leaves and re-enters cleanly as the filter moves', async () => {
      // Transition coverage, not just state coverage: switching away has to
      // drop the section AND the report behind it, so a later re-entry can
      // never paint one account's figures while another's are loading.
      setBindingMock('GetCodexAccountUsage', async () =>
        new CodexAccountUsage({ lifetimeTokens: 5_000, dailyBuckets: [] }),
      );
      const { queryByTestId, findByTestId, getByRole } = render(UsageModal, {
        props: { open: true, onClose: () => {} },
      });

      await selectCodex(getByRole);
      await findByTestId('usage-codex-account');

      await fireEvent.click(getByRole('radio', { name: 'All' }));
      await waitFor(() => expect(queryByTestId('usage-codex-account')).toBeNull());

      await selectCodex(getByRole);
      await findByTestId('usage-codex-account');
      expect(getBindingMock('GetCodexAccountUsage')).toHaveBeenCalledTimes(2);
    });

    it('renders nothing when Codex has nothing to report', async () => {
      // Older codex, an API-key login, or a brand-new account: the
      // backend answers null and absence must stay absence.
      setBindingMock('GetCodexAccountUsage', async () => null);
      const { queryByTestId, getByRole, findAllByTestId } = render(UsageModal, {
        props: { open: true, onClose: () => {} },
      });
      await selectCodex(getByRole);

      await waitFor(() => expect(getBindingMock('GetCodexAccountUsage')).toHaveBeenCalled());
      // The ledger heatmap still renders; only the account section is gone.
      await findAllByTestId('usage-heatmap-cell');
      expect(queryByTestId('usage-codex-account')).toBeNull();
    });

    it('surfaces a real read failure instead of hiding it', async () => {
      const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
      setBindingMock('GetCodexAccountUsage', async () => {
        throw new Error('codex account usage: codex binary not configured');
      });
      const { findByTestId, getByRole } = render(UsageModal, {
        props: { open: true, onClose: () => {} },
      });
      await selectCodex(getByRole);

      const error = await findByTestId('usage-codex-account-error');
      expect(error.textContent).toContain('codex binary not configured');
      consoleError.mockRestore();
    });
  });
});
