import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';

import UsageChip from './UsageChip.svelte';
import { applyUsageEvent } from '../../stores/eventsProvider';
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
    // Output tokens ONLY (500), not in+out (1500) — input re-bills the
    // growing context every turn and drowns out what the thread
    // actually produced. costUsd 0.32 -> "$0.32".
    expect(trigger.textContent?.trim()).toBe('500 · $0.32');
    expect(trigger.title).toContain('Estimated cost');
  });

  it('suppresses the cost when costUsd is 0 and some rows are unpriced', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async () => [
      lifetimeBucket({ costUsd: 0, unpricedRows: 2 }),
    ]);
    const { findByTestId } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    expect(trigger.textContent?.trim()).toBe('500');
  });

  it('shows the ≥ lower-bound marker when cost is nonzero but some rows are unpriced', async () => {
    const pane = await buildPane(makeThread());
    setBindingMock('GetUsageStats', async () => [
      lifetimeBucket({ costUsd: 1.2, unpricedRows: 2 }),
    ]);
    const { findByTestId } = render(UsageChip, { props: { pane } });

    const trigger = await findByTestId('usage-chip-trigger');
    expect(trigger.textContent?.trim()).toBe('500 · ≥$1.20');
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

    // Per-model row lazily fetched on first open, rendered with the
    // picker-style display name (raw slug survives in the title attr).
    await waitFor(() => {
      expect(getUsageStats).toHaveBeenCalledTimes(2);
    });
    const modelRow = getByText('Sonnet 4.6');
    expect(modelRow).toBeInTheDocument();
    expect(modelRow.getAttribute('title')).toBe('claude-sonnet-4-6');

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


it('refreshes the chip and open model breakdown on reported progress without clearing context', async () => {
  const pane = await buildPane(makeThread());
  let output = 500;
  let pending = 1;
  setBindingMock('GetUsageStats', async (query: unknown) => {
    const values = { outputTokens: output, pendingRows: pending, unpricedRows: pending };
    return [(query as { groupBy?: string }).groupBy === 'model' ? modelBucket(values) : lifetimeBucket(values)];
  });
  const { findByTestId, getByTestId, getByText } = render(UsageChip, { props: { pane } });
  const trigger = await findByTestId('usage-chip-trigger');
  await fireEvent.click(trigger);
  await waitFor(() => expect(getByText('Latest reported tokens. Cost accounting is still pending.')).toBeTruthy());
  await waitFor(() => expect(getByText('Sonnet 4.6').parentElement?.textContent).toContain('500'));
  pane.setContextWindow({ usedTokens: 4000, maxTokens: 200000, usedPercentage: 2 });
  const contextBefore = pane.contextWindow;
  output = 900;
  pending = 0;
  applyUsageEvent({ action: 'progress', threadId: pane.threadId! });
  await waitFor(() => {
    expect(trigger.textContent).toContain('900');
    expect(getByText('Sonnet 4.6').parentElement?.textContent).toContain('900');
    expect(getByTestId('usage-chip-popover').textContent).not.toContain('accounting is still pending');
  });
  expect(pane.contextWindow).toEqual(contextBefore);
  const modelCalls = getBindingMock('GetUsageStats')!.mock.calls.filter(([q]) => (q as { groupBy?: string }).groupBy === 'model');
  expect(modelCalls.length).toBeGreaterThanOrEqual(2);
});

it('preserves known totals and displays a failed refresh', async () => {
  const pane = await buildPane(makeThread());
  setBindingMock('GetUsageStats', async () => [lifetimeBucket()]);
  const { findByTestId } = render(UsageChip, { props: { pane } });
  const trigger = await findByTestId('usage-chip-trigger');
  setBindingMock('GetUsageStats', async () => { throw new Error('read failed'); });
  bumpUsageRefresh(pane.threadId!);
  await waitFor(() => expect(trigger.title).toContain('could not be refreshed'));
  expect(trigger.textContent).toContain('500');
  applyUsageEvent({ action: 'progress', threadId: pane.threadId!, error: 'Reported usage could not be saved.' });
  await waitFor(() => expect(trigger.title).toBe('Reported usage could not be saved.'));
});


it('explains why the Codex thread estimate can differ from the model breakdown', async () => {
  resetBindingMocks();
  resetUsageRefreshForTest();
  const pane = await buildPane(makeThread());
  setBindingMock('GetUsageStats', async (query: unknown) => (query as { groupBy?: string }).groupBy === 'model'
    ? [modelBucket()]
    : [lifetimeBucket({ costSource: 'provider-estimate', costUsd: 1.2 })]);
  const { findByTestId, getByTestId } = render(UsageChip, { props: { pane } });
  const trigger = await findByTestId('usage-chip-trigger');
  expect(trigger.title).toContain('Cost estimated by Codex');
  await fireEvent.click(trigger);
  await waitFor(() => {
    expect(getByTestId('usage-chip-popover').textContent).toContain('Model totals below use standard token rates');
  });
});
