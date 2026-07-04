import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import UsageFooter from '../UsageFooter.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { UsageBucket } from '../../../stores/bindings';
import { getUsagePeriod, resetUsagePeriodForTest } from '../../../stores/usagePeriod.svelte';
import { resetUsageRefreshForTest } from '../../../stores/usageRefresh.svelte';

const STORAGE_KEY = 'agent-overflow:usage:period';

function providerBucket(provider: string, overrides: Partial<UsageBucket> = {}): UsageBucket {
  return new UsageBucket({
    bucket: provider,
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

describe('<UsageFooter>', () => {
  beforeEach(() => {
    resetUsagePeriodForTest();
    resetUsageRefreshForTest();
  });

  it('renders one row per provider present, skipping zero rows', async () => {
    setBindingMock('GetUsageStats', async () => [
      providerBucket('claude', { inputTokens: 3_000_000, outputTokens: 1_100_000, costUsd: 42.104 }),
      providerBucket('codex', { inputTokens: 0, outputTokens: 0, costUsd: 0 }),
    ]);
    const { findAllByTestId } = render(UsageFooter, { props: {} });
    const rows = await findAllByTestId('usage-footer-row');
    expect(rows).toHaveLength(1);
    // The label is rendered lowercase with a CSS `uppercase` transform
    // (visual-only), so textContent stays lowercase.
    expect(rows[0].textContent).toContain('claude');
    // Cents always show (a bare "$42" or "$118" reading as exact was a
    // bug), with real spaces around the separator (template whitespace
    // collapsing once glued it: "4.1M· $42.10").
    expect(rows[0].textContent).toContain('4.1M · $42.10');
  });

  it('is hidden entirely when there is no usage', async () => {
    setBindingMock('GetUsageStats', async () => []);
    const { queryByTestId } = render(UsageFooter, { props: {} });
    await waitFor(() => {
      expect(queryByTestId('sidebar-usage-footer')).toBeNull();
    });
  });

  it('clicking the period label cycles + persists the period without opening the modal', async () => {
    setBindingMock('GetUsageStats', async () => [
      providerBucket('claude', { inputTokens: 100, costUsd: 1 }),
    ]);
    const { findByTestId, queryByRole } = render(UsageFooter, { props: {} });
    const periodBtn = await findByTestId('usage-footer-period');

    expect(getUsagePeriod()).toBe('30d');
    await fireEvent.click(periodBtn);
    expect(getUsagePeriod()).toBe('all');
    expect(localStorage.getItem(STORAGE_KEY)).toBe('all');
    expect(queryByRole('dialog')).toBeNull();
  });

  it('clicking the row (outside the period label) opens the usage modal', async () => {
    setBindingMock('GetUsageStats', async () => [
      providerBucket('claude', { inputTokens: 100, costUsd: 1 }),
    ]);
    const { findByTestId, findByRole } = render(UsageFooter, { props: {} });
    const row = await findByTestId('sidebar-usage-footer');
    await fireEvent.click(row);
    expect(await findByRole('dialog')).toBeInTheDocument();
  });
});
