import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  clearAllThreadSizePriorsForTest,
  getThreadSizePriors,
  peekThreadSizePriorsForTest,
  setThreadSizePriors,
  type SizePriorsEntry,
} from './priors';
import { __resetSizePriorsStorageForTest, installSizePriorsPersistence } from './priorsStorage';

const V1_PREFIX = 'agent-overflow.sizePriors.v1.';

const entry = (overrides: Partial<SizePriorsEntry> = {}): SizePriorsEntry => ({
  width: 800,
  expansionSig: '',
  rows: new Map([
    ['L:a:completed:2:1', 42],
    ['L:b:completed:1:1', 30],
  ]),
  ...overrides,
});

beforeEach(() => {
  vi.useFakeTimers();
  clearAllThreadSizePriorsForTest();
  __resetSizePriorsStorageForTest();
  installSizePriorsPersistence();
});

afterEach(() => {
  // Restore before reset: a test that failed mid-body leaves its throwing
  // setItem mock installed, which would poison every later test.
  vi.restoreAllMocks();
  __resetSizePriorsStorageForTest();
  vi.useRealTimers();
});

describe('persistence round-trip', () => {
  it('debounces the write, then hydrates from storage after an in-memory clear (restart simulation)', () => {
    setThreadSizePriors('t1', entry());
    // The write is debounced — nothing durable yet.
    expect(localStorage.getItem(`${V1_PREFIX}t1`)).toBeNull();

    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V1_PREFIX}t1`)).not.toBeNull();

    // Simulate an app restart: wipe the in-memory LRU, keep localStorage.
    clearAllThreadSizePriorsForTest();
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();

    const hydrated = getThreadSizePriors('t1');
    expect(hydrated?.width).toBe(800);
    expect(hydrated?.expansionSig).toBe('');
    expect(hydrated?.rows.get('L:a:completed:2:1')).toBe(42);
    expect(hydrated?.rows.get('L:b:completed:1:1')).toBe(30);
    // Hydration installs the entry back into the in-memory LRU.
    expect(peekThreadSizePriorsForTest('t1')).toEqual(hydrated);
  });

  it('coalesces repeated persists before the debounce fires into one write of the latest entry', () => {
    setThreadSizePriors('t1', entry({ width: 100 }));
    vi.advanceTimersByTime(500);
    setThreadSizePriors('t1', entry({ width: 200 }));
    vi.advanceTimersByTime(999);
    expect(localStorage.getItem(`${V1_PREFIX}t1`)).toBeNull(); // debounce kept resetting
    vi.advanceTimersByTime(1);
    const stored = JSON.parse(localStorage.getItem(`${V1_PREFIX}t1`) as string);
    expect(stored.width).toBe(200);
  });
});

describe('malformed storage', () => {
  it('drops malformed JSON, warns once, and removes the key', () => {
    localStorage.setItem(`${V1_PREFIX}bad`, '{not json');
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V1_PREFIX}bad`)).toBeNull();
    warn.mockRestore();
  });

  it('drops an entry that fails shape validation', () => {
    localStorage.setItem(
      `${V1_PREFIX}bad2`,
      JSON.stringify({ width: 'not-a-number', expansionSig: '', rows: [] }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad2')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V1_PREFIX}bad2`)).toBeNull();
    warn.mockRestore();
  });

  it('drops a row pair with a non-finite height', () => {
    // NaN doesn't survive JSON.stringify (becomes null), so seed the raw
    // string directly to exercise the finite-number guard.
    localStorage.setItem(`${V1_PREFIX}bad3`, '{"width":800,"expansionSig":"","rows":[["sig",null]]}');
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad3')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });

  it('drops a row pair with a negative height (corrupt prior)', () => {
    // Capture filters UNMEASURED/negatives before persisting, so a
    // negative height can only come from corrupt/hand-edited storage — a
    // negative estimate would poison the size store's offsets.
    localStorage.setItem(`${V1_PREFIX}bad4`, '{"width":800,"expansionSig":"","rows":[["sig",-5]]}');
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad4')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V1_PREFIX}bad4`)).toBeNull();
    warn.mockRestore();
  });
});

describe('stale-version sweep', () => {
  it('removes keys under an older schema version on install', () => {
    localStorage.setItem('agent-overflow.sizePriors.v0.t1', 'stale');
    installSizePriorsPersistence();
    expect(localStorage.getItem('agent-overflow.sizePriors.v0.t1')).toBeNull();
  });

  it('leaves current-version keys alone', () => {
    setThreadSizePriors('t1', entry());
    vi.advanceTimersByTime(1000);
    installSizePriorsPersistence();
    expect(localStorage.getItem(`${V1_PREFIX}t1`)).not.toBeNull();
  });
});

describe('index LRU cap', () => {
  it('evicts the oldest stored thread past the 50-thread cap', () => {
    for (let i = 0; i < 51; i++) {
      setThreadSizePriors(`t${i}`, entry());
      vi.advanceTimersByTime(1000);
    }
    expect(localStorage.getItem(`${V1_PREFIX}t0`)).toBeNull();
    expect(localStorage.getItem(`${V1_PREFIX}t50`)).not.toBeNull();
  });

  it('a load bumps recency, folded into the next flush rather than a synchronous write', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    vi.advanceTimersByTime(1000);

    // Restart simulation: hydrate t0 through the adapter, bumping its recency.
    clearAllThreadSizePriorsForTest();
    getThreadSizePriors('t0');
    // No synchronous index write from the load alone.
    const indexBefore = JSON.parse(
      localStorage.getItem('agent-overflow.sizePriors.v1.index') as string,
    );
    expect(indexBefore[indexBefore.length - 1]).not.toBe('t0');

    vi.advanceTimersByTime(1000);
    const indexAfter = JSON.parse(
      localStorage.getItem('agent-overflow.sizePriors.v1.index') as string,
    );
    expect(indexAfter[indexAfter.length - 1]).toBe('t0');

    // t0 is now most-recent, so the next new thread evicts t1 instead.
    setThreadSizePriors('t51', entry());
    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V1_PREFIX}t0`)).not.toBeNull();
    expect(localStorage.getItem(`${V1_PREFIX}t1`)).toBeNull();
  });
});

describe('quota exceeded', () => {
  const quotaError = () => new DOMException('quota exceeded', 'QuotaExceededError');

  it('persists the post-eviction index when the index write itself hits quota', () => {
    setThreadSizePriors('t1', entry());
    vi.advanceTimersByTime(1000);
    setThreadSizePriors('t2', entry());
    vi.advanceTimersByTime(1000);
    // Stored index is now [t1, t2].

    // Next flush: t3's entry write succeeds, the index write hits quota
    // once (evicting t1), and the retried index write must reflect the
    // eviction — not the pre-eviction serialization.
    const originalSetItem = localStorage.setItem.bind(localStorage);
    let threw = false;
    vi.spyOn(localStorage, 'setItem')
      .mockImplementation((key: string, value: string) => {
        if (key === `${V1_PREFIX}index` && !threw) {
          threw = true;
          throw quotaError();
        }
        originalSetItem(key, value);
      });

    setThreadSizePriors('t3', entry());
    vi.advanceTimersByTime(1000);

    expect(localStorage.getItem(`${V1_PREFIX}t1`)).toBeNull();
    const index = JSON.parse(localStorage.getItem(`${V1_PREFIX}index`) as string);
    expect(index).toEqual(['t2', 't3']);
  });

  it('keeps the write-target thread indexed when quota eviction lands on itself', () => {
    setThreadSizePriors('t1', entry());
    vi.advanceTimersByTime(1000);
    // Stored index is now [t1] — the only evictable thread is t1 itself.

    const originalSetItem = localStorage.setItem.bind(localStorage);
    let threw = false;
    vi.spyOn(localStorage, 'setItem')
      .mockImplementation((key: string, value: string) => {
        if (key === `${V1_PREFIX}t1` && !threw) {
          threw = true;
          throw quotaError();
        }
        originalSetItem(key, value);
      });

    setThreadSizePriors('t1', entry({ width: 900 }));
    vi.advanceTimersByTime(1000);

    // The retried entry write succeeded, so the thread must stay indexed —
    // an unindexed entry escapes the LRU cap forever.
    const stored = JSON.parse(localStorage.getItem(`${V1_PREFIX}t1`) as string);
    expect(stored.width).toBe(900);
    const index = JSON.parse(localStorage.getItem(`${V1_PREFIX}index`) as string);
    expect(index).toEqual(['t1']);
  });

  it('evicts the oldest thread and retries once; disables persistence if it still fails', () => {
    setThreadSizePriors('t1', entry());
    vi.advanceTimersByTime(1000);

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const setItemSpy = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('quota exceeded', 'QuotaExceededError');
    });

    setThreadSizePriors('t2', entry());
    vi.advanceTimersByTime(1000);
    expect(warn).toHaveBeenCalledTimes(1);

    // Persistence is now disabled for the session: a further persist
    // doesn't even attempt a write.
    setItemSpy.mockClear();
    setThreadSizePriors('t3', entry());
    vi.advanceTimersByTime(1000);
    expect(setItemSpy).not.toHaveBeenCalled();

    setItemSpy.mockRestore();
    warn.mockRestore();
  });
});
