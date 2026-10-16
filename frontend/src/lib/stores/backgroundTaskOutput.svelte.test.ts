import { beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import {
  __resetBackgroundTaskOutputForTest,
  applyBackgroundTaskOutputFrame,
  attachBackgroundTaskOutput,
  backgroundTaskOutputKeys,
  boundTail,
  TAIL_BYTES,
} from './backgroundTaskOutput.svelte';
import { applyBackgroundTaskOutputEvent } from './eventsBackgroundTaskOutput';
import { applyTransportGap } from './eventsTransportGap';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __resetEntityIndexForTest, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import type { BackgroundTaskOutputEvent } from '../types/events';

const THREAD = 'thread-1';
const LAUNCH = 'tu-bg';

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function snapshot(overrides: Record<string, unknown> = {}) {
  return {
    watchId: 'w-1',
    seq: 0,
    data: 'line 1\n',
    truncated: false,
    size: 7,
    ended: false,
    ...overrides,
  };
}

function frame(overrides: Partial<BackgroundTaskOutputEvent>): BackgroundTaskOutputEvent {
  return { threadId: THREAD, launchId: LAUNCH, seq: 1, size: 0, ...overrides };
}

beforeEach(() => {
  __resetBackgroundTaskOutputForTest();
  __resetEntityIndexForTest();
  noteThread(THREAD, HOME_BACKEND);
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
});

describe('boundTail', () => {
  it('keeps text within the bound whole', () => {
    expect(boundTail('abc\n', 10)).toEqual({ text: 'abc\n', cut: false });
  });
  it('cuts to the bound and on to the next line start', () => {
    expect(boundTail('first line\nsecond\nthird\n', 12)).toEqual({ text: 'third\n', cut: true });
  });
  it('keeps a cut inside the last line at the bound', () => {
    expect(boundTail('abcdefghij', 4)).toEqual({ text: 'ghij', cut: true });
  });
  it('never splits a surrogate pair at the bound', () => {
    // The bound lands on the low half of the emoji: the cut moves past it.
    expect(boundTail('a\u{1F600}cd', 3)).toEqual({ text: 'cd', cut: true });
    expect(boundTail('a\u{1F600}cd', 4)).toEqual({ text: '\u{1F600}cd', cut: true });
  });
});

describe('backgroundTaskOutput store', () => {
  it('watches on attach, applies the snapshot, and streams appends by seq', async () => {
    const watch = setBindingMock('WatchBackgroundTaskOutput', async () => snapshot());
    const unwatch = setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    expect(watch).toHaveBeenCalledTimes(1);
    expect(watch).toHaveBeenCalledWith(THREAD, LAUNCH);
    expect(attachment.current?.text).toBe('line 1\n');
    expect(attachment.current?.seq).toBe(0);
    expect(attachment.current?.ended).toBe(false);

    applyBackgroundTaskOutputEvent(frame({ seq: 1, append: 'line 2\n', size: 14 }), { backendId: '' });
    expect(attachment.current?.text).toBe('line 1\nline 2\n');
    expect(attachment.current?.seq).toBe(1);
    expect(attachment.current?.size).toBe(14);
    expect(attachment.current?.lastAppendAt).toBeGreaterThan(0);

    // A stale or repeated frame changes nothing.
    applyBackgroundTaskOutputEvent(frame({ seq: 1, append: 'again\n' }), { backendId: '' });
    expect(attachment.current?.text).toBe('line 1\nline 2\n');

    // A reset frame replaces the tail.
    applyBackgroundTaskOutputEvent(frame({ seq: 2, append: 'fresh\n', reset: true, truncated: true }), { backendId: '' });
    expect(attachment.current?.text).toBe('fresh\n');
    expect(attachment.current?.truncated).toBe(true);

    attachment.release();
    await flush();
    expect(unwatch).toHaveBeenCalledWith(THREAD, 'w-1');
    expect(backgroundTaskOutputKeys()).toEqual([]);
  });

  it('re-watches when a frame skips the expected seq', async () => {
    let calls = 0;
    const watch = setBindingMock('WatchBackgroundTaskOutput', async () => {
      calls += 1;
      return snapshot({ watchId: `w-${calls}`, seq: calls === 1 ? 0 : 5, data: calls === 1 ? 'a\n' : 'a\nb\nc\n' });
    });
    setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    expect(attachment.current?.text).toBe('a\n');

    applyBackgroundTaskOutputEvent(frame({ seq: 3, append: 'c\n' }), { backendId: '' });
    await flush();
    expect(watch).toHaveBeenCalledTimes(2);
    expect(attachment.current?.text).toBe('a\nb\nc\n');
    expect(attachment.current?.seq).toBe(5);
    attachment.release();
    await flush();
  });

  it('shares one watch between two attachers and releases it once', async () => {
    const watch = setBindingMock('WatchBackgroundTaskOutput', async () => snapshot());
    const unwatch = setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const first = attachBackgroundTaskOutput(THREAD, LAUNCH);
    const second = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    expect(watch).toHaveBeenCalledTimes(1);
    expect(second.current?.text).toBe('line 1\n');
    first.release();
    await flush();
    expect(unwatch).not.toHaveBeenCalled();
    second.release();
    await flush();
    expect(unwatch).toHaveBeenCalledTimes(1);
  });

  it('ends with the ended frame and still releases the watch it was given', async () => {
    setBindingMock('WatchBackgroundTaskOutput', async () => snapshot());
    const unwatch = setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    applyBackgroundTaskOutputEvent(frame({ seq: 1, append: 'done\n', ended: true, size: 12 }), { backendId: '' });
    expect(attachment.current?.ended).toBe(true);
    expect(attachment.current?.text).toBe('line 1\ndone\n');
    // Nothing follows an end.
    applyBackgroundTaskOutputEvent(frame({ seq: 2, append: 'late\n' }), { backendId: '' });
    expect(attachment.current?.text).toBe('line 1\ndone\n');
    attachment.release();
    await flush();
    // The id the reply named is released whether or not the task ended,
    // so a failed or ended watch never stays counted on the backend.
    expect(unwatch).toHaveBeenCalledWith(THREAD, 'w-1');
  });

  it('releases the watch of a tail whose read failed after it was acquired', async () => {
    setBindingMock('WatchBackgroundTaskOutput', async () => snapshot());
    const unwatch = setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    applyBackgroundTaskOutputEvent(frame({ seq: 1, ended: true, error: 'read task output: gone' }), { backendId: '' });
    expect(attachment.current?.error).toBe('read task output: gone');
    attachment.release();
    await flush();
    expect(unwatch).toHaveBeenCalledWith(THREAD, 'w-1');
  });

  it('replays an append that landed while the watch reply was in flight', async () => {
    let resolveWatch: (value: unknown) => void = () => {};
    const watch = setBindingMock('WatchBackgroundTaskOutput', () => new Promise((resolve) => { resolveWatch = resolve; }));
    setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    expect(watch).toHaveBeenCalledTimes(1);
    // The backend's seed read saw seq 0; its next poll sent seq 1 before
    // the reply reached this page, and a stale seq 0 frame came too.
    applyBackgroundTaskOutputEvent(frame({ seq: 0, append: 'stale\n' }), { backendId: '' });
    applyBackgroundTaskOutputEvent(frame({ seq: 1, append: 'early\n', size: 13 }), { backendId: '' });
    resolveWatch(snapshot());
    await flush();
    expect(attachment.current?.text).toBe('line 1\nearly\n');
    expect(attachment.current?.seq).toBe(1);
    // The stream continues without a re-watch.
    applyBackgroundTaskOutputEvent(frame({ seq: 2, append: 'next\n' }), { backendId: '' });
    expect(attachment.current?.text).toBe('line 1\nearly\nnext\n');
    await flush();
    expect(watch).toHaveBeenCalledTimes(1);
    attachment.release();
    await flush();
  });

  it('re-watches every held tail of a thread after a transport gap on the channel', async () => {
    let calls = 0;
    const watch = setBindingMock('WatchBackgroundTaskOutput', async () => {
      calls += 1;
      return snapshot({ watchId: `w-${calls}`, seq: 4, data: 'a\nb\n' });
    });
    const unwatch = setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    const other = attachBackgroundTaskOutput('thread-2', LAUNCH);
    await flush();
    expect(watch).toHaveBeenCalledTimes(2);

    applyTransportGap({ channel: 'provider:background_task_output', seq: 9, threads: [THREAD] }, { backendId: '' });
    await flush();
    expect(unwatch).toHaveBeenCalledTimes(1);
    expect(unwatch).toHaveBeenCalledWith(THREAD, 'w-1');
    expect(watch).toHaveBeenCalledTimes(3);
    expect(attachment.current?.text).toBe('a\nb\n');

    // A gap naming no thread re-watches every held tail.
    applyTransportGap({ channel: 'provider:background_task_output', seq: 10 }, { backendId: '' });
    await flush();
    expect(watch).toHaveBeenCalledTimes(5);
    attachment.release();
    other.release();
    await flush();
  });

  it('carries a read error from the frame and from the snapshot', async () => {
    setBindingMock('WatchBackgroundTaskOutput', async () => snapshot({ watchId: '', ended: true, error: 'read task output: gone', data: '' }));
    setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    expect(attachment.current?.ended).toBe(true);
    expect(attachment.current?.error).toBe('read task output: gone');
    attachment.release();
    await flush();
  });

  it('bounds the held tail at a line boundary', async () => {
    setBindingMock('WatchBackgroundTaskOutput', async () => snapshot({ data: '' }));
    setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    const line = `${'x'.repeat(99)}\n`;
    const big = line.repeat(Math.ceil(TAIL_BYTES / line.length) + 5);
    applyBackgroundTaskOutputEvent(frame({ seq: 1, append: big, size: big.length }), { backendId: '' });
    expect(attachment.current?.text.length).toBeLessThanOrEqual(TAIL_BYTES);
    expect(attachment.current?.text.startsWith('x')).toBe(true);
    expect((attachment.current?.text.length ?? 0) % line.length).toBe(0);
    expect(attachment.current?.truncated).toBe(true);
    attachment.release();
    await flush();
  });

  it('ignores a frame from a computer other than the one the watch was minted on', async () => {
    setBindingMock('WatchBackgroundTaskOutput', async () => snapshot());
    setBindingMock('UnwatchBackgroundTaskOutput', async () => {});
    const attachment = attachBackgroundTaskOutput(THREAD, LAUNCH);
    await flush();
    applyBackgroundTaskOutputFrame(frame({ seq: 1, append: 'elsewhere\n' }), 'other-computer' as BackendKey);
    expect(attachment.current?.text).toBe('line 1\n');
    applyBackgroundTaskOutputFrame(frame({ seq: 1, append: 'here\n' }), HOME_BACKEND);
    expect(attachment.current?.text).toBe('line 1\nhere\n');
    attachment.release();
    await flush();
  });

  it('drops a frame for a launch nobody holds', () => {
    expect(() =>
      applyBackgroundTaskOutputEvent(frame({ threadId: 'nobody', seq: 1, append: 'x' }), { backendId: '' }),
    ).not.toThrow();
  });
});
