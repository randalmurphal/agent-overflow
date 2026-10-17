import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import '../../../app.css';
import SystemStatsFooter from './SystemStatsFooter.svelte';
import UsageFooter from './UsageFooter.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { UsageBucket } from '../../stores/bindings';
import { setSystemStats, resetForTest as resetSystemStatsForTest } from '../../stores/systemStats.svelte';
import { resetTelemetryForTest, setTelemetrySelection } from '../../stores/telemetryComputers.svelte';
import { cycleUsagePeriod, resetUsagePeriodForTest } from '../../stores/usagePeriod.svelte';
import { resetUsageRefreshForTest } from '../../stores/usageRefresh.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { __resetScopesForTest, setPageGrantsFromBootstrap } from '../../transport/scopes';
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
  // First paint precedes the manifest.
  __resetScopesForTest();
  const view = render(SystemStatsFooter);
  view.container.style.width = '260px';
  const footer = view.getByTestId('sidebar-system-stats');
  expect(view.getAllByTestId('system-stats-computer')).toHaveLength(1);
  expect(view.getAllByTestId('system-stats-placeholder')).toHaveLength(1);
  const before = footer.getBoundingClientRect().height;

  // The manifest settles the page and publishes the attached list in one step.
  flushSync(() => {
    setPageGrantsFromBootstrap(false);
    stageBackend({ id: 'remote', name: 'Remote computer' });
  });
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

it('system stats footer drops a selected computer that is no longer attached', async () => {
  const detached = 'detached-computer';
  setTelemetrySelection('system', ['@local', REMOTE_BACKEND_UUID, detached]);
  const key = telemetryFooterShapeKey('system', ['@local', REMOTE_BACKEND_UUID, detached]);
  recordTelemetryFooterShape(key, { lines: 3, picker: true });
  const remote = stageBackend({ id: 'remote', name: 'Remote computer' });
  const view = render(SystemStatsFooter);
  setSystemStats(sample);
  await tick();
  expect(view.getAllByTestId('system-stats-computer')).toHaveLength(2);
  expect(view.queryAllByTestId('system-stats-placeholder')).toHaveLength(0);
  // The remote computer has not sampled yet, so nothing is re-recorded.
  expect(getTelemetryFooterShape(key)?.lines).toBe(3);
  remote.setStatus('reconnecting');
  await waitFor(() => expect(getTelemetryFooterShape(key)).toEqual({ lines: 2, picker: true }));
  expect(view.getAllByTestId('system-stats-computer')).toHaveLength(2);
});

it('system stats footer counts an offline computer as settled', async () => {
  setTelemetrySelection('system', ['@local', REMOTE_BACKEND_UUID]);
  const key = telemetryFooterShapeKey('system', ['@local', REMOTE_BACKEND_UUID]);
  stageBackend({ id: 'remote', name: 'Remote computer', status: 'reconnecting' });
  const view = render(SystemStatsFooter);
  setSystemStats(sample);
  await waitFor(() => expect(view.getByTestId('sidebar-system-stats').textContent).toContain('Offline'));
  await waitFor(() => expect(getTelemetryFooterShape(key)).toEqual({ lines: 2, picker: true }));
});

it('usage footer keeps a pending selection visible when the last answer was empty', async () => {
  recordUsageShape(0);
  setBindingMock('GetUsageStats', async () => [claudeBucket()]);
  stageBackend({ id: 'remote', name: 'Remote computer', status: 'disconnected' });
  const view = render(UsageFooter);
  await waitFor(() => expect(view.getAllByTestId('usage-footer-row')).toHaveLength(1));
  expect(view.queryByTestId('sidebar-usage-footer')).not.toBeNull();
});

it('usage footer records only a settled answer', async () => {
  recordUsageShape(0);
  setBindingMock('GetUsageStats', async () => [claudeBucket()]);
  stageBackend({ id: 'remote', name: 'Remote computer', status: 'disconnected' });
  const view = render(UsageFooter);
  await waitFor(() => expect(view.getAllByTestId('usage-footer-row')).toHaveLength(1));
  await new Promise((done) => setTimeout(done, 50));
  // Home has answered; the remote computer is still pending.
  expect(getTelemetryFooterShape(usageKey)?.lines).toBe(0);
});

/** Counts writes of the recorded shapes to storage. */
function countShapeWrites(): { count: () => number; stop: () => void } {
  const original = Storage.prototype.setItem;
  let writes = 0;
  Storage.prototype.setItem = function (this: Storage, key: string, value: string) {
    if (key.endsWith('telemetry-footer-shapes')) writes += 1;
    return original.call(this, key, value);
  };
  return { count: () => writes, stop: () => { Storage.prototype.setItem = original; } };
}

it('both footers settle together without re-recording each other', async () => {
  const errors: unknown[] = [];
  const onError = (e: ErrorEvent) => errors.push(e.error ?? e.message);
  const onRejection = (e: PromiseRejectionEvent) => errors.push(e.reason);
  window.addEventListener('error', onError);
  window.addEventListener('unhandledrejection', onRejection);
  const replies: Array<(rows: UsageBucket[]) => void> = [];
  setBindingMock('GetUsageStats', () => new Promise<UsageBucket[]>((done) => { replies.push(done); }));
  const writes = countShapeWrites();
  try {
    const usage = render(UsageFooter);
    const stats = render(SystemStatsFooter);
    await tick();
    const remote = stageBackend({ id: 'remote', name: 'Remote computer', status: 'disconnected' });
    await tick();
    cycleUsagePeriod();
    await tick();
    cycleUsagePeriod();
    await tick();
    remote.setStatus('connected');
    await tick();
    // Both footers settle in one synchronous batch.
    flushSync(() => {
      for (const done of replies.splice(0)) done([claudeBucket('claude'), claudeBucket('codex')]);
      setSystemStats(sample);
      setSystemStats(sample, 'remote');
    });
    await waitFor(() => expect(usage.queryAllByTestId('usage-footer-row').length).toBeGreaterThan(0));
    await new Promise((done) => setTimeout(done, 300));
    for (const done of replies.splice(0)) done([claudeBucket('claude'), claudeBucket('codex')]);
    await new Promise((done) => setTimeout(done, 300));
    const before = writes.count();
    for (let i = 0; i < 10; i += 1) {
      setSystemStats({ ...sample, cpuPercent: i });
      setSystemStats({ ...sample, cpuPercent: i }, 'remote');
      await tick();
    }
    await new Promise((done) => setTimeout(done, 100));
    expect(writes.count() - before).toBeLessThanOrEqual(1);
    expect(errors).toEqual([]);
    expect(stats.getByTestId('sidebar-system-stats').textContent).toContain('CPU');
  } finally {
    writes.stop();
    window.removeEventListener('error', onError);
    window.removeEventListener('unhandledrejection', onRejection);
  }
});

it('system stats footer drops a recorded computer the manifest no longer lists', async () => {
  setTelemetrySelection('system', ['@local', REMOTE_BACKEND_UUID]);
  recordTelemetryFooterShape(telemetryFooterShapeKey('system', ['@local', REMOTE_BACKEND_UUID]), { lines: 2, picker: true });
  __resetScopesForTest();
  const view = render(SystemStatsFooter);
  expect(view.getAllByTestId('system-stats-placeholder')).toHaveLength(1);
  // An unpaired remote page settles to the placeholder's own grants, and
  // its manifest lists no other computer.
  flushSync(() => setPageGrantsFromBootstrap(true));
  expect(view.queryAllByTestId('system-stats-placeholder')).toHaveLength(0);
  expect(view.getAllByTestId('system-stats-computer')).toHaveLength(1);
});
