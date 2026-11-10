import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createSnapshotStream, type SnapshotStream } from './snapshotStream';

// A minimal owning store: a transcript per key, with the sequence folded in.
interface Frame {
  seq: number;
  chunk?: string;
  finished?: boolean;
  /** The fold throws on this frame. */
  poison?: boolean;
}
interface Snapshot {
  text: string;
  seq: number;
}
interface State extends Snapshot {
  finished: boolean;
}

const KEY = 'k1';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

let states: Map<string, State>;
let read: ReturnType<typeof vi.fn<(key: string) => Promise<Snapshot>>>;
/** Every store callback, in call order. */
let calls: string[];
let stream: SnapshotStream<Frame>;
let warn: ReturnType<typeof vi.spyOn>;

function text(key = KEY): string | undefined {
  return states.get(key)?.text;
}

beforeEach(() => {
  states = new Map();
  calls = [];
  read = vi.fn<(key: string) => Promise<Snapshot>>();
  warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
  stream = createSnapshotStream<Frame, Snapshot>({
    name: 'test',
    read: (key) => read(key),
    applySnapshot(key, snapshot) {
      calls.push(`snapshot ${key} ${snapshot.seq}`);
      states.set(key, { ...snapshot, finished: false });
    },
    applyFrame(key, frame) {
      calls.push(`frame ${key} ${frame.seq}`);
      if (frame.poison) throw new Error('fold failed');
      const state = states.get(key) ?? { text: '', seq: 0, finished: false };
      // Already in the snapshot.
      if (frame.seq <= state.seq) return;
      // A hole: ask for the snapshot, whether the frame is live or replayed.
      if (frame.seq > state.seq + 1) void stream.hydrate(key);
      states.set(key, {
        text: state.text + (frame.chunk ?? ''),
        seq: frame.seq,
        finished: state.finished || Boolean(frame.finished),
      });
    },
  });
});

afterEach(() => {
  warn.mockRestore();
});

describe('live frames', () => {
  it('folds a frame at once when no read is in flight', () => {
    stream.push(KEY, { seq: 1, chunk: 'a' });
    stream.push(KEY, { seq: 2, chunk: 'b' });
    expect(text()).toBe('ab');
    expect(calls).toEqual([`frame ${KEY} 1`, `frame ${KEY} 2`]);
    expect(read).not.toHaveBeenCalled();
  });

  it('folds a frame of another key while one key is being read', () => {
    read.mockReturnValue(deferred<Snapshot>().promise);
    void stream.hydrate(KEY);
    stream.push('k2', { seq: 1, chunk: 'x' });
    stream.push(KEY, { seq: 1, chunk: 'a' });
    expect(text('k2')).toBe('x');
    expect(text()).toBeUndefined();
  });
});

describe('a read with frames in flight', () => {
  it('replays the buffered frames once each, in arrival order, over the snapshot', async () => {
    const pending = deferred<Snapshot>();
    read.mockReturnValue(pending.promise);
    const hydration = stream.hydrate(KEY);
    // Frame 2 is in the snapshot, frames 3 and 4 are not.
    stream.push(KEY, { seq: 2, chunk: 'b' });
    stream.push(KEY, { seq: 3, chunk: 'c' });
    stream.push(KEY, { seq: 4, chunk: 'd' });
    expect(calls).toEqual([]);

    pending.resolve({ text: 'ab', seq: 2 });
    await hydration;
    expect(calls).toEqual([`snapshot ${KEY} 2`, `frame ${KEY} 2`, `frame ${KEY} 3`, `frame ${KEY} 4`]);
    expect(text()).toBe('abcd');
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('applies a finish that lands during the read after the snapshot', async () => {
    const pending = deferred<Snapshot>();
    read.mockReturnValue(pending.promise);
    const hydration = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, finished: true });
    pending.resolve({ text: 'a', seq: 1 });
    await hydration;
    expect(states.get(KEY)).toEqual({ text: 'a', seq: 2, finished: true });
  });

  it('folds frames live again once the read has settled', async () => {
    read.mockResolvedValue({ text: 'a', seq: 1 });
    await stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    expect(calls).toEqual([`snapshot ${KEY} 1`, `frame ${KEY} 2`]);
    expect(text()).toBe('ab');
  });
});

describe('overlapping reads', () => {
  it('discards the older answer and leaves the buffer to the newest read', async () => {
    const first = deferred<Snapshot>();
    const second = deferred<Snapshot>();
    read.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const a = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    const b = stream.hydrate(KEY);
    stream.push(KEY, { seq: 3, chunk: 'c' });

    first.resolve({ text: 'old', seq: 9 });
    await a;
    // Nothing applied, and the key is still buffering.
    expect(calls).toEqual([]);
    stream.push(KEY, { seq: 4, chunk: 'd' });
    expect(calls).toEqual([]);

    second.resolve({ text: 'a', seq: 1 });
    await b;
    expect(calls).toEqual([`snapshot ${KEY} 1`, `frame ${KEY} 2`, `frame ${KEY} 3`, `frame ${KEY} 4`]);
    expect(text()).toBe('abcd');
  });

  it('ignores an older answer that lands after the newest has settled', async () => {
    const first = deferred<Snapshot>();
    const second = deferred<Snapshot>();
    read.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const a = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    const b = stream.hydrate(KEY);
    second.resolve({ text: 'a', seq: 1 });
    await b;
    expect(text()).toBe('ab');

    first.resolve({ text: 'old', seq: 9 });
    await a;
    expect(text()).toBe('ab');
    expect(calls).toEqual([`snapshot ${KEY} 1`, `frame ${KEY} 2`]);
    // The settled key is not buffering behind the stale read.
    stream.push(KEY, { seq: 3, chunk: 'c' });
    expect(text()).toBe('abc');
  });

  it('leaves the buffer to the newest read when the older one fails', async () => {
    const first = deferred<Snapshot>();
    const second = deferred<Snapshot>();
    read.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
    const a = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    const b = stream.hydrate(KEY);
    first.reject(new Error('offline'));
    await a;
    expect(calls).toEqual([]);

    second.resolve({ text: 'a', seq: 1 });
    await b;
    expect(text()).toBe('ab');
  });
});

describe('a failed read', () => {
  it('applies no snapshot, replays the buffer and resumes live frames', async () => {
    stream.push(KEY, { seq: 1, chunk: 'a' });
    const pending = deferred<Snapshot>();
    read.mockReturnValue(pending.promise);
    const hydration = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    pending.reject(new Error('offline'));
    await expect(hydration).resolves.toBeUndefined();

    expect(calls).toEqual([`frame ${KEY} 1`, `frame ${KEY} 2`]);
    expect(text()).toBe('ab');
    expect(warn).toHaveBeenCalledWith(`test: hydrate ${KEY}: Error: offline`);
    stream.push(KEY, { seq: 3, chunk: 'c' });
    expect(text()).toBe('abc');
  });

  it('does not leave the key buffering when the read throws before it returns a promise', async () => {
    read.mockImplementation(() => { throw new Error('no route'); });
    await stream.hydrate(KEY);
    stream.push(KEY, { seq: 1, chunk: 'a' });
    expect(text()).toBe('a');
    expect(warn).toHaveBeenCalledTimes(1);
  });
});

describe('repeated reads', () => {
  it('reads again on each call and replays only what arrived during that read', async () => {
    const first = deferred<Snapshot>();
    read.mockReturnValueOnce(first.promise);
    const a = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, chunk: 'b' });
    first.resolve({ text: 'a', seq: 1 });
    await a;

    const second = deferred<Snapshot>();
    read.mockReturnValueOnce(second.promise);
    const b = stream.hydrate(KEY);
    stream.push(KEY, { seq: 4, chunk: 'd' });
    second.resolve({ text: 'abc', seq: 3 });
    await b;

    expect(read).toHaveBeenCalledTimes(2);
    expect(calls).toEqual([`snapshot ${KEY} 1`, `frame ${KEY} 2`, `snapshot ${KEY} 3`, `frame ${KEY} 4`]);
    expect(text()).toBe('abcd');
  });
});

describe('replay', () => {
  it('starts no read for a replayed frame that is still ahead of the snapshot', async () => {
    const pending = deferred<Snapshot>();
    read.mockReturnValue(pending.promise);
    const hydration = stream.hydrate(KEY);
    // Frame 2 is missing: the fold asks for a read.
    stream.push(KEY, { seq: 3, chunk: 'c' });
    pending.resolve({ text: 'a', seq: 1 });
    await hydration;
    expect(read).toHaveBeenCalledTimes(1);
    expect(text()).toBe('ac');
    // Not buffering: no read was started.
    stream.push(KEY, { seq: 4, chunk: 'd' });
    expect(text()).toBe('acd');
  });

  it('starts a read for a live frame after a replay', async () => {
    read.mockResolvedValueOnce({ text: 'a', seq: 1 });
    const hydration = stream.hydrate(KEY);
    stream.push(KEY, { seq: 3, chunk: 'c' });
    await hydration;
    expect(read).toHaveBeenCalledTimes(1);

    read.mockReturnValueOnce(deferred<Snapshot>().promise);
    stream.push(KEY, { seq: 9, chunk: 'z' });
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('starts a read of another key from a replayed frame', async () => {
    const other = createSnapshotStream<Frame, Snapshot>({
      name: 'test',
      read: (key) => read(key),
      applySnapshot: () => undefined,
      applyFrame(key) {
        if (key === KEY) void other.hydrate('k2');
      },
    });
    const pending = deferred<Snapshot>();
    read.mockReturnValueOnce(pending.promise).mockReturnValueOnce(deferred<Snapshot>().promise);
    const hydration = other.hydrate(KEY);
    other.push(KEY, { seq: 1 });
    pending.resolve({ text: '', seq: 0 });
    await hydration;
    expect(read.mock.calls).toEqual([[KEY], ['k2']]);
  });

  it('reads again after a fold threw during a replay', async () => {
    const pending = deferred<Snapshot>();
    read.mockReturnValueOnce(pending.promise);
    const hydration = stream.hydrate(KEY);
    stream.push(KEY, { seq: 2, poison: true });
    pending.resolve({ text: 'a', seq: 1 });
    await expect(hydration).rejects.toThrow('fold failed');

    read.mockResolvedValueOnce({ text: 'ab', seq: 2 });
    await stream.hydrate(KEY);
    expect(read).toHaveBeenCalledTimes(2);
    expect(text()).toBe('ab');
  });
});

describe('reset', () => {
  it('abandons every read in flight on reset', async () => {
    const one = deferred<Snapshot>();
    const two = deferred<Snapshot>();
    read.mockReturnValueOnce(one.promise).mockReturnValueOnce(two.promise);
    const a = stream.hydrate(KEY);
    const b = stream.hydrate('k2');
    stream.push(KEY, { seq: 1, chunk: 'a' });
    stream.reset();

    one.resolve({ text: 'stale', seq: 5 });
    two.reject(new Error('offline'));
    await Promise.all([a, b]);
    expect(calls).toEqual([]);
    stream.push(KEY, { seq: 1, chunk: 'live' });
    stream.push('k2', { seq: 1, chunk: 'live' });
    expect(calls).toEqual([`frame ${KEY} 1`, `frame k2 1`]);
  });
});
