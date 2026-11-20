import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createBrowserPaneRectSender, type SequencedPaneRect } from './browserPaneRectSender';

// The report's sequence is its x unless a test says otherwise: one number
// per distinct report.
function rect(x: number, visible = true, seq = x): SequencedPaneRect {
  return {
    seq, x, y: 20, width: 800, height: 600,
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

function harness() {
  const calls: Call[] = [];
  const failures: Array<{ mount: string; error: unknown }> = [];
  const sender = createBrowserPaneRectSender({
    send: (mount, r) =>
      new Promise<void>((resolve, reject) => {
        calls.push({ mount, x: r.x, resolve, reject });
      }),
    isMountGone: (error) => String(error).includes('pane mount not found'),
    onFailure: (mount, error) => failures.push({ mount, error }),
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

  it('sends a rect the pane returned to under its newer sequence', async () => {
    const { calls, sender } = harness();
    sender.report('m', rect(1, true, 1));
    calls[0].resolve();
    await flush();
    await flush();
    sender.report('m', rect(1, true, 3));
    expect(calls).toHaveLength(2);
  });

  it('lands the last report when sends fail, retrying with the newest', async () => {
    const { calls, failures, sender } = harness();
    sender.report('m', rect(1));
    sender.report('m', rect(2));
    calls[0].reject(new Error('transport busy'));
    await flush();
    await flush();
    // Not marked delivered: the retry carries the newest report.
    await vi.advanceTimersByTimeAsync(100);
    expect(calls.map((c) => c.x)).toEqual([1, 2]);
    calls[1].reject(new Error('transport busy'));
    await flush();
    await vi.advanceTimersByTimeAsync(200);
    expect(calls.map((c) => c.x)).toEqual([1, 2, 2]);
    calls[2].resolve();
    await flush();
    await flush();
    expect(failures).toEqual([]);
    // Delivered now: the same report is not sent again.
    sender.report('m', rect(2));
    expect(calls).toHaveLength(3);
  });

  it('retries three times, each after twice the wait before it', async () => {
    const { calls, failures, sender } = harness();
    sender.report('m', rect(1));
    for (const delay of [100, 200, 400]) {
      calls.at(-1)!.reject(new Error('backend restarting'));
      await flush();
      await flush();
      const before = calls.length;
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(calls).toHaveLength(before);
      await vi.advanceTimersByTimeAsync(1);
      expect(calls).toHaveLength(before + 1);
    }
    expect(failures).toEqual([]);
    calls[3].reject(new Error('backend restarting'));
    await flush();
    await flush();
    expect(failures).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(4);
  });

  it('gives up after the bound, says so, and sends the same rect again later', async () => {
    const { calls, failures, sender } = harness();
    sender.report('m', rect(1));
    for (let attempt = 0; attempt < 4; attempt++) {
      calls[attempt].reject(new Error('backend restarting'));
      await flush();
      await flush();
      await vi.advanceTimersByTimeAsync(400);
    }
    expect(calls).toHaveLength(4);
    expect(failures).toHaveLength(1);
    expect(String(failures[0].error)).toContain('backend restarting');
    sender.report('m', rect(1));
    expect(calls).toHaveLength(5);
  });

  it('treats a failed send as unknown backend state, so the newest report still goes out', async () => {
    // The transport can reject after the backend applied the call (a lost
    // reply): each failed send here applies first, then rejects.
    const { calls, failures, sender } = harness();
    let backend = 0;
    const accept = async (i: number) => {
      backend = calls[i].x;
      calls[i].resolve();
      await flush();
      await flush();
    };
    const applyThenReject = async (i: number) => {
      backend = calls[i].x;
      calls[i].reject(new Error('rpc timeout'));
      await flush();
      await flush();
    };
    sender.report('m', rect(1));
    await accept(0);
    sender.report('m', rect(2));
    await applyThenReject(1);
    await vi.advanceTimersByTimeAsync(100);
    expect(calls.map((c) => c.x)).toEqual([1, 2, 2]);
    // Back to the first rect while the retry is out: the retry carries it.
    sender.report('m', rect(1));
    await applyThenReject(2);
    await vi.advanceTimersByTimeAsync(200);
    expect(calls.map((c) => c.x)).toEqual([1, 2, 2, 1]);
    await accept(3);
    expect(backend).toBe(1);

    // A failure after the backend accepted rect 1 makes rect 1 unknown too:
    // once the sender gives up on rect 2, rect 1 is sent again rather than
    // skipped.
    sender.report('m', rect(2));
    for (const [i, delay] of [[4, 100], [5, 200], [6, 400]]) {
      await applyThenReject(i);
      await vi.advanceTimersByTimeAsync(delay);
    }
    await applyThenReject(7);
    expect(failures).toHaveLength(1);
    expect(backend).toBe(2);
    sender.report('m', rect(1));
    expect(calls.map((c) => c.x)).toEqual([1, 2, 2, 1, 2, 2, 2, 2, 1]);
    await accept(8);
    expect(backend).toBe(1);
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
