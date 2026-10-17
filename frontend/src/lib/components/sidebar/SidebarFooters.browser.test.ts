import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import '../../../app.css';
import SystemStatsFooter from './SystemStatsFooter.svelte';
import UsageFooter from './UsageFooter.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';
import { setSystemStats, resetForTest as resetSystemStatsForTest } from '../../stores/systemStats.svelte';
import { resetTelemetryForTest, setTelemetrySelection } from '../../stores/telemetryComputers.svelte';
import { resetUsagePeriodForTest } from '../../stores/usagePeriod.svelte';
import { resetUsageRefreshForTest } from '../../stores/usageRefresh.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { getTelemetryFooterShape, recordTelemetryFooterShape, telemetryFooterShapeKey } from '../../stores/telemetryFooterShape.svelte';
import { REMOTE_BACKEND_UUID, stageBackend, resetStagedBackends } from '../../../test/helpers/backends';

// The footers sit under the sidebar's project list, so a footer that mounts
// or grows after first paint pushes every row above it. Each one holds its
// populated height from mount.

beforeEach(() => {
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  resetSystemStatsForTest();
  resetTelemetryForTest();
  resetUsagePeriodForTest();
  resetUsageRefreshForTest();
  setBindingMock('UpdateSettings', async () => null);
});

afterEach(() => {
  setCompactLayoutForTest(false);
  resetStagedBackends();
  resetSystemStatsForTest();
  resetTelemetryForTest();
});

function deferredUsage() {
  let resolve!: (rows: UsageBucket[]) => void;
  const reply = new Promise<UsageBucket[]>((done) => { resolve = done; });
  setBindingMock('GetUsageStats', () => reply);
  return resolve;
}

const usageKey = telemetryFooterShapeKey('usage', null, 'month');
function recordUsageShape(lines: number): void {
  recordTelemetryFooterShape(usageKey, { lines, picker: false });
}

const sample = { isWsl: false, cpuPercent: 20, memUsedBytes: 4 * 1024 ** 3, memTotalBytes: 16 * 1024 ** 3 };

function claudeBucket(bucket = 'claude'): UsageBucket {
  return new UsageBucket({
    bucket, inputTokens: 1_000, outputTokens: 40_000, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
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

it('both footers wait rather than report offline while the first connect is in flight', async () => {
  __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
  const resolve = deferredUsage();
  const stats = render(SystemStatsFooter);
  const usage = render(UsageFooter);
  const statsFooter = stats.getByTestId('sidebar-system-stats');
  const usageFooter = usage.getByTestId('sidebar-usage-footer');
  expect(statsFooter.textContent).toContain('Waiting for system stats');
  expect(usage.getByTestId('usage-footer-empty').textContent).toContain('Loading usage');
  const heights = [statsFooter.getBoundingClientRect().height, usageFooter.getBoundingClientRect().height];

  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  setSystemStats({ isWsl: false, cpuPercent: 20, memUsedBytes: 4 * 1024 ** 3, memTotalBytes: 16 * 1024 ** 3 });
  await waitFor(() => expect(statsFooter.textContent).toContain('CPU 20%'));
  resolve([claudeBucket()]);
  await waitFor(() => expect(usage.getAllByTestId('usage-footer-row')).toHaveLength(1));
  expect([statsFooter.getBoundingClientRect().height, usageFooter.getBoundingClientRect().height]).toEqual(heights);
});

/** Fails the test if the footer is ever removed from the document. */
function watchFooterRemoval(container: HTMLElement): { removed: () => boolean; stop: () => void } {
  let removed = false;
  const observer = new MutationObserver((records) => {
    for (const record of records) {
      for (const node of record.removedNodes) {
        if (node instanceof HTMLElement && node.dataset.testid === 'sidebar-usage-footer') removed = true;
      }
    }
  });
  observer.observe(container, { childList: true, subtree: true });
  return { removed: () => removed, stop: () => observer.disconnect() };
}

it.each([false, true])('usage footer paints a recorded two-provider shape before the reply (compact %s)', async (compact) => {
  setCompactLayoutForTest(compact);
  recordUsageShape(2);
  const resolve = deferredUsage();
  const view = render(UsageFooter);
  view.container.style.width = '260px';
  const footer = view.getByTestId('sidebar-usage-footer');
  expect(view.getAllByTestId('usage-footer-placeholder-row')).toHaveLength(1);
  const before = footer.getBoundingClientRect().height;

  resolve([claudeBucket('claude'), claudeBucket('codex')]);
  await waitFor(() => expect(view.getAllByTestId('usage-footer-row')).toHaveLength(2));
  expect(footer.getBoundingClientRect().height).toBe(before);
});

it('usage footer paints nothing before the reply when the last reply had no usage', async () => {
  recordUsageShape(0);
  const resolve = deferredUsage();
  const view = render(UsageFooter);
  expect(view.queryByTestId('sidebar-usage-footer')).toBeNull();
  let mounted = false;
  const watch = new MutationObserver(() => { mounted ||= view.queryByTestId('sidebar-usage-footer') !== null; });
  watch.observe(view.container, { childList: true, subtree: true });
  resolve([]);
  await new Promise((done) => setTimeout(done, 50));
  watch.disconnect();
  expect(mounted).toBe(false);
  expect(view.queryByTestId('sidebar-usage-footer')).toBeNull();
});

it('a reply that differs from the recorded shape updates the record', async () => {
  recordUsageShape(1);
  setBindingMock('GetUsageStats', async () => [claudeBucket('claude'), claudeBucket('codex')]);
  const first = render(UsageFooter);
  await waitFor(() => expect(first.getAllByTestId('usage-footer-row')).toHaveLength(2));
  first.unmount();
  deferredUsage();
  const second = render(UsageFooter);
  expect(second.queryAllByTestId('usage-footer-placeholder-row')).toHaveLength(1);
  expect(second.getByTestId('usage-footer-empty').textContent).toContain('Loading usage');
});

it('a selected computer whose first connect fails keeps the usage footer mounted', async () => {
  const remote = stageBackend({ id: 'remote', name: 'Remote computer', status: 'disconnected' });
  setBindingMock('GetUsageStats', async () => []);
  const view = render(UsageFooter);
  const removal = watchFooterRemoval(view.container);
  await waitFor(() => expect(view.getByTestId('usage-footer-empty').textContent).toContain('Loading usage'));
  // Home's empty reply has landed; the remote computer is still pending.
  await new Promise((done) => setTimeout(done, 50));
  expect(view.queryByTestId('sidebar-usage-footer')).not.toBeNull();
  remote.setStatus('reconnecting');
  await waitFor(() => expect(view.getByTestId('sidebar-usage-footer').textContent).toContain('Usage unavailable'));
  removal.stop();
  expect(removal.removed()).toBe(false);
});

it('usage footer still reads as loading after a recovery cancels its first read', async () => {
  const remote = stageBackend({ id: 'remote', name: 'Remote computer' });
  deferredUsage();
  const view = render(UsageFooter);
  await waitFor(() => expect(view.getByTestId('usage-footer-empty').textContent).toContain('Loading usage'));
  // The read in flight is abandoned and nothing has answered yet.
  remote.replay('cancel');
  await new Promise((done) => setTimeout(done, 0));
  expect(view.getByTestId('usage-footer-empty').textContent).toContain('Loading usage');
});

it.each([false, true])('usage footer with a paired computer failing its first connect keeps its height on the next launch (compact %s)', async (compact) => {
  setCompactLayoutForTest(compact);
  setBindingMock('GetUsageStats', async () => [claudeBucket()]);
  const remote = stageBackend({ id: 'remote', name: 'Remote computer', status: 'disconnected' });
  const first = render(UsageFooter);
  first.container.style.width = '260px';
  await waitFor(() => expect(first.getAllByTestId('usage-footer-row')).toHaveLength(1));
  remote.setStatus('reconnecting');
  await waitFor(() => expect(first.getByTestId('sidebar-usage-footer').textContent).toContain('Partial usage'));
  const settledHeight = first.getByTestId('sidebar-usage-footer').getBoundingClientRect().height;
  expect(getTelemetryFooterShape(usageKey)?.lines).toBe(2);
  first.unmount();

  // The next launch: the remote computer is connecting again.
  remote.setStatus('disconnected');
  const second = render(UsageFooter);
  second.container.style.width = '260px';
  const footer = second.getByTestId('sidebar-usage-footer');
  expect(footer.getBoundingClientRect().height).toBe(settledHeight);
  await waitFor(() => expect(second.getAllByTestId('usage-footer-row')).toHaveLength(1));
  expect(footer.getBoundingClientRect().height).toBe(settledHeight);
  remote.setStatus('reconnecting');
  await waitFor(() => expect(footer.textContent).toContain('Partial usage'));
  expect(footer.getBoundingClientRect().height).toBe(settledHeight);
});

it('usage footer records an unavailable answer as one line', async () => {
  setBindingMock('GetUsageStats', async () => []);
  stageBackend({ id: 'remote', name: 'Remote computer', status: 'reconnecting' });
  const view = render(UsageFooter);
  await waitFor(() => expect(getTelemetryFooterShape(usageKey)?.lines).toBe(1));
  expect(view.getByTestId('sidebar-usage-footer').textContent).toContain('Usage unavailable');
});

it.each([false, true])('system stats footer paints its recorded picker and rows before the attached list arrives (compact %s)', async (compact) => {
  setCompactLayoutForTest(compact);
  setTelemetrySelection('system', ['@local', REMOTE_BACKEND_UUID]);
  const key = telemetryFooterShapeKey('system', ['@local', REMOTE_BACKEND_UUID]);
  recordTelemetryFooterShape(key, { lines: 2, picker: true });
  const view = render(SystemStatsFooter);
  view.container.style.width = '260px';
  const footer = view.getByTestId('sidebar-system-stats');
  expect(view.getAllByTestId('system-stats-computer')).toHaveLength(1);
  expect(view.getAllByTestId('system-stats-placeholder')).toHaveLength(1);
  const before = footer.getBoundingClientRect().height;

  stageBackend({ id: 'remote', name: 'Remote computer' });
  await waitFor(() => expect(view.getAllByTestId('system-stats-computer')).toHaveLength(2));
  expect(footer.getBoundingClientRect().height).toBe(before);
  setSystemStats(sample);
  setSystemStats({ ...sample, cpuPercent: 90 }, 'remote');
  await waitFor(() => expect(footer.textContent).toContain('CPU 90%'));
  expect(footer.getBoundingClientRect().height).toBe(before);
  expect(getTelemetryFooterShape(key)).toEqual({ lines: 2, picker: true });
});

it('system stats footer records its settled shape for the next launch', async () => {
  stageBackend({ id: 'remote', name: 'Remote computer' });
  const view = render(SystemStatsFooter);
  expect(getTelemetryFooterShape(telemetryFooterShapeKey('system', null))).toBeNull();
  setSystemStats(sample);
  await waitFor(() => expect(view.getByTestId('sidebar-system-stats').textContent).toContain('CPU 20%'));
  expect(getTelemetryFooterShape(telemetryFooterShapeKey('system', null))).toEqual({ lines: 1, picker: true });
});
