import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import SystemStatsFooter from './SystemStatsFooter.svelte';
import UsageFooter from './UsageFooter.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';
import { setSystemStats, resetForTest as resetSystemStatsForTest } from '../../stores/systemStats.svelte';
import { resetTelemetryForTest } from '../../stores/telemetryComputers.svelte';
import { resetUsagePeriodForTest } from '../../stores/usagePeriod.svelte';
import { resetUsageRefreshForTest } from '../../stores/usageRefresh.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';

// The footers sit under the sidebar's project list, so a footer that mounts
// or grows after first paint pushes every row above it. Each one holds its
// populated height from mount.

beforeEach(() => {
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  resetSystemStatsForTest();
  resetTelemetryForTest();
  resetUsagePeriodForTest();
  resetUsageRefreshForTest();
});

afterEach(() => {
  setCompactLayoutForTest(false);
  resetSystemStatsForTest();
  resetTelemetryForTest();
});

function deferredUsage() {
  let resolve!: (rows: UsageBucket[]) => void;
  const reply = new Promise<UsageBucket[]>((done) => { resolve = done; });
  setBindingMock('GetUsageStats', () => reply);
  return resolve;
}

function claudeBucket(): UsageBucket {
  return new UsageBucket({
    bucket: 'claude', inputTokens: 1_000, outputTokens: 40_000, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
    reasoningOutputTokens: 0, costUsd: 141.03, turnCount: 1, unpricedRows: 0,
  });
}

it.each([false, true])('system stats footer holds its height from mount (compact %s)', async (compact) => {
  setCompactLayoutForTest(compact);
  const view = render(SystemStatsFooter);
  view.container.style.width = '260px';
  const footer = view.getByTestId('sidebar-system-stats');
  expect(footer.textContent).toContain('Waiting for system stats');
  const before = footer.getBoundingClientRect().height;

  setSystemStats({ isWsl: false, cpuPercent: 20, memUsedBytes: 4 * 1024 ** 3, memTotalBytes: 16 * 1024 ** 3 });
  await tick();
  expect(footer.textContent).toContain('CPU 20%');
  expect(before).toBeGreaterThan(0);
  expect(footer.getBoundingClientRect().height).toBe(before);
});

it.each([false, true])('usage footer holds its height while the first reply is pending (compact %s)', async (compact) => {
  setCompactLayoutForTest(compact);
  const resolve = deferredUsage();
  const view = render(UsageFooter);
  view.container.style.width = '260px';
  const footer = view.getByTestId('sidebar-usage-footer');
  expect(view.getByTestId('usage-footer-empty').textContent).toContain('Loading usage');
  const before = footer.getBoundingClientRect().height;

  resolve([claudeBucket()]);
  await waitFor(() => expect(view.getAllByTestId('usage-footer-row')).toHaveLength(1));
  expect(before).toBeGreaterThan(0);
  expect(footer.getBoundingClientRect().height).toBe(before);
});

it('usage footer collapses once the first reply has no usage', async () => {
  const resolve = deferredUsage();
  const view = render(UsageFooter);
  expect(view.queryByTestId('sidebar-usage-footer')).not.toBeNull();
  resolve([]);
  await waitFor(() => expect(view.queryByTestId('sidebar-usage-footer')).toBeNull());
});
