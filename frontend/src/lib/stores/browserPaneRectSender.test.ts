import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createBrowserPaneRectSender, type BrowserPaneRectReport } from './browserPaneRectSender';

function rect(x: number, visible = true): BrowserPaneRectReport {
  return {
    x, y: 20, width: 800, height: 600,
    clipX: x, clipY: 20, clipWidth: 800, clipHeight: 600,
    viewportWidth: 1600, viewportHeight: 900, devicePixelRatio: 1.25,
    visible, background: '#000000',
  };
}

interface Call {
  mount: string;
  x: number;
  resolve: () => void;
  reject: (error: unknown) => void;
}

function harness(options: { maxRetries?: number } = {}) {
  const calls: Call[] = [];
  const failures: Array<{ mount: string; error: unknown }> = [];
  const sender = createBrowserPaneRectSender({
    send: (mount, r) =>
      new Promise<void>((resolve, reject) => {
        calls.push({ mount, x: r.x, resolve, reject });
      }),
    isMountGone: (error) => String(error).includes('pane mount not found'),
    onFailure: (mount, error) => failures.push({ mount, error }),
    retryDelayMs: 10,
    ...options,
  });
  return { calls, failures, sender };
}

const flush = () => Promise.resolve().then(() => undefined);

describe('browser pane rect sender', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('keeps one report in flight and sends only the newest behind it', async () => {
    const { calls, sender } = harness();
    sender.report('m', rect(1));
    sender.report('m', rect(2));
    sender.report('m', rect(3));
    sender.report('m', rect(4));
    expect(calls.map((c) => c.x)).toEqual([1]);
    calls[0].resolve();
    await flush();
    await flush();
    expect(calls.map((c) => c.x)).toEqual([1, 4]);
    calls[1].resolve();
    await flush();
    await flush();
    expect(calls).toHaveLength(2);
  });

  it('skips a report the backend already accepted, and the one already out', async () => {
    const { calls, sender } = harness();
    sender.report('m', rect(1));
    sender.report('m', rect(2));
    // Back to the one in flight: nothing newer is left to send.
    sender.report('m', rect(1));
    calls[0].resolve();
    await flush();
    await flush();
    expect(calls).toHaveLength(1);
    sender.report('m', rect(1));
    expect(calls).toHaveLength(1);
    sender.report('m', rect(5));
    expect(calls.map((c) => c.x)).toEqual([1, 5]);
  });

  it('lands the last report when sends fail, retrying with the newest', async () => {
    const { calls, failures, sender } = harness();
    sender.report('m', rect(1));
    sender.report('m', rect(2));
    calls[0].reject(new Error('transport busy'));
    await flush();
    await flush();
    // Not marked delivered: the retry carries the newest report.
    await vi.advanceTimersByTimeAsync(10);
    expect(calls.map((c) => c.x)).toEqual([1, 2]);
    calls[1].reject(new Error('transport busy'));
    await flush();
    await vi.advanceTimersByTimeAsync(20);
    expect(calls.map((c) => c.x)).toEqual([1, 2, 2]);
    calls[2].resolve();
    await flush();
    await flush();
    expect(failures).toEqual([]);
    // Delivered now: the same report is not sent again.
    sender.report('m', rect(2));
    expect(calls).toHaveLength(3);
  });

  it('gives up after the bound, says so, and sends the same rect again later', async () => {
    const { calls, failures, sender } = harness({ maxRetries: 2 });
    sender.report('m', rect(1));
    for (let attempt = 0; attempt < 3; attempt++) {
      calls[attempt].reject(new Error('backend restarting'));
      await flush();
      await flush();
      await vi.advanceTimersByTimeAsync(100);
    }
    expect(calls).toHaveLength(3);
    expect(failures).toHaveLength(1);
    expect(String(failures[0].error)).toContain('backend restarting');
    sender.report('m', rect(1));
    expect(calls).toHaveLength(4);
  });

  it('stops reporting a mount the backend no longer has', async () => {
    const { calls, failures, sender } = harness();
    sender.report('m', rect(1));
    sender.report('m', rect(2));
    calls[0].reject(new Error('browser: pane mount not found'));
    await flush();
    await flush();
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toHaveLength(1);
    expect(failures).toEqual([]);
  });

  it('ignores answers for a forgotten mount and keeps mounts independent', async () => {
    const { calls, sender } = harness();
    sender.report('a', rect(1));
    sender.report('b', rect(1));
    expect(calls.map((c) => c.mount)).toEqual(['a', 'b']);
    sender.report('a', rect(2));
    sender.forget('a');
    calls[0].resolve();
    await flush();
    await flush();
    expect(calls).toHaveLength(2);
    // A new report on the same mount id starts afresh.
    sender.report('a', rect(1));
    expect(calls).toHaveLength(3);
  });
});
