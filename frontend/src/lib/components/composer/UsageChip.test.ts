import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';

import UsageChip from './UsageChip.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';
import { bumpUsageRefresh, resetUsageRefreshForTest } from '../../stores/usageRefresh.svelte';

function lifetimeBucket(overrides: Partial<UsageBucket> = {}): UsageBucket {
  return new UsageBucket({
    bucket: '',
    inputTokens: 1000,
    outputTokens: 500,
    cacheReadInputTokens: 200,
    cacheCreationInputTokens: 50,
    reasoningOutputTokens: 0,
    costUsd: 0.32,
    turnCount: 3,
    unpricedRows: 0,
    ...overrides,
  });
}

function modelBucket(overrides: Partial<UsageBucket> = {}): UsageBucket {
  return new UsageBucket({
    bucket: 'claude-sonnet-4-6',
    inputTokens: 1000,
    outputTokens: 500,
    cacheReadInputTokens: 0,
    cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0,
    costUsd: 0.32,
    turnCount: 3,
    unpricedRows: 0,
    ...overrides,
  });
}

describe('<UsageChip>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetUsageRefreshForTest();
  });

  it('renders nothing when the thread has no usage yet', async () => {
    const pane = await buildPane(makeThread());
    // installPaneMocks already defaults GetUsageStats to an empty bucket
    // list; keep it explicit here so the intent of the test reads clearly.
    setBindingMock('GetUsageStats', async () => []);
    const { queryByTestId } = render(UsageChip, { props: { pane } });

    await waitFor(() => {
      expect(getBindingMock('GetUsageStats')).toHaveBeenCalled();
    });
    expect(queryByTestId('usage-chip-trigger')).toBeNull();
  });

  it('renders tokens and cost after the lifetime bucket loads', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async () => [lifetimeBucket()]);
    const { findByTestId } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    // inputTokens (1000) + outputTokens (500) = 1500 -> "1.5k"; costUsd 0.32 -> "$0.32".
    expect(trigger.textContent?.trim()).toBe('1.5k · $0.32');
  });

  it('suppresses the cost when costUsd is 0 and some rows are unpriced', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async () => [
      lifetimeBucket({ costUsd: 0, unpricedRows: 2 }),
    ]);
    const { findByTestId } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    expect(trigger.textContent?.trim()).toBe('1.5k');
  });

  it('shows the ≥ lower-bound marker when cost is nonzero but some rows are unpriced', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async () => [
      lifetimeBucket({ costUsd: 1.2, unpricedRows: 2 }),
    ]);
    const { findByTestId } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    expect(trigger.textContent?.trim()).toBe('1.5k · ≥$1.20');
  });

  it('refetches the lifetime bucket when its own thread usage refresh version bumps', async () => {
    const pane = await buildPane(makeThread());
    const getUsageStats = setBindingMock('GetUsageStats', async () => [lifetimeBucket()]);
    render(UsageChip, { props: { pane } });

    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(1);
    });

    bumpUsageRefresh(pane.threadId!);

    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(2);
    });
  });

  it('does not refetch the lifetime bucket when a DIFFERENT thread bumps', async () => {
    const pane = await buildPane(makeThread());
    const getUsageStats = setBindingMock('GetUsageStats', async () => [lifetimeBucket()]);
    render(UsageChip, { props: { pane } });

    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(1);
    });

    bumpUsageRefresh('some-other-thread');

    // Give any (unwanted) refetch a chance to land before asserting it
    // didn't.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(getUsageStats).toHaveBeenCalledTimes(1);

    bumpUsageRefresh(pane.threadId!);

    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(2);
    });
  });

  it('shows the token split and lazily fetches per-model rows on first open', async () => {
    const pane = await buildPane(makeThread());
    const getUsageStats = setBindingMock('GetUsageStats', async (query: unknown) => {
      const q = query as { groupBy?: string };
      if (q.groupBy === 'model') return [modelBucket()];
      return [lifetimeBucket()];
    });
    const { findByTestId, getByText } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    // Only the lifetime query has run before the popover opens.
    expect(getUsageStats).toHaveBeenCalledTimes(1);

    await fireEvent.click(trigger);

    const popover = await findByTestId('usage-chip-popover');
    expect(popover).toBeInTheDocument();
    // Token split rows, right-aligned via formatTokens.
    expect(getByText('Input')).toBeInTheDocument();
    expect(getByText('1.0k')).toBeInTheDocument(); // Input tokens
    expect(getByText('Output')).toBeInTheDocument();
    expect(getByText('500')).toBeInTheDocument(); // Output tokens
    expect(getByText('Cache read')).toBeInTheDocument();
    expect(getByText('Cache write')).toBeInTheDocument();

    // Per-model row lazily fetched on first open.
    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(2);
    });
    expect(getByText('claude-sonnet-4-6')).toBeInTheDocument();

    // Turn count line.
    expect(getByText('3 turns')).toBeInTheDocument();
  });

  it('does not include a Reasoning row when reasoningOutputTokens is 0', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async (query: unknown) => {
      const q = query as { groupBy?: string };
      if (q.groupBy === 'model') return [];
      return [lifetimeBucket({ reasoningOutputTokens: 0 })];
    });
    const { findByTestId, queryByText } = render(UsageChip, { props: { pane } });

    await fireEvent.click(await findByTestId('usage-chip-trigger'));
    await findByTestId('usage-chip-popover');
    expect(queryByText('Reasoning')).toBeNull();
  });

  it('marks a per-model row cost with the ≥ lower-bound marker when that model has unpriced rows', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async (query: unknown) => {
      const q = query as { groupBy?: string };
      if (q.groupBy === 'model') return [modelBucket({ costUsd: 0.5, unpricedRows: 1 })];
      return [lifetimeBucket()];
    });
    const { findByTestId } = render(UsageChip, { props: { pane } });

    await fireEvent.click(await findByTestId('usage-chip-trigger'));
    const popover = await findByTestId('usage-chip-popover');
    await waitFor(() => {
      expect(popover.textContent).toContain('≥$0.50');
    });
  });
});
