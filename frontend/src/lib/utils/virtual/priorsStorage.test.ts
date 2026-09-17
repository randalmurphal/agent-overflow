import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  clearAllThreadSizePriorsForTest,
  getThreadSizePriors,
  peekThreadSizePriorsForTest,
  setThreadSizePriors,
  sizePriorsAtGeometry,
  sizePriorsGeometryKey,
  MAX_ROWS_PER_BUCKET,
  type SizePriorsBucket,
  type SizePriorsGeometry,
} from './priors';
import { __resetSizePriorsStorageForTest, installSizePriorsPersistence } from './priorsStorage';

const V3_PREFIX = 'agent-overflow.sizePriors.v3.';

/** Stand-in for `typographySignature()`; storage only round-trips it. */
const TYPO = 'f15/sgeist/mgeist/c1/w1';

const geom = (width = 800, typography = TYPO): SizePriorsGeometry => ({ width, typography });
const key = (width = 800, typography = TYPO): string =>
  sizePriorsGeometryKey(geom(width, typography));

const bucket = (overrides: Partial<SizePriorsBucket> = {}): SizePriorsBucket => ({
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
    setThreadSizePriors('t1', geom(800), bucket());
    // The write is debounced — nothing durable yet.
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).toBeNull();

    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).not.toBeNull();

    // Simulate an app restart: wipe the in-memory LRU, keep localStorage.
    clearAllThreadSizePriorsForTest();
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();

    const hydrated = getThreadSizePriors('t1');
    const restored = sizePriorsAtGeometry(hydrated!, geom(800));
    expect(restored?.expansionSig).toBe('');
    expect(restored?.rows.get('L:a:completed:2:1')).toBe(42);
    expect(restored?.rows.get('L:b:completed:1:1')).toBe(30);
    // Hydration installs the entry back into the in-memory LRU.
    expect(peekThreadSizePriorsForTest('t1')).toEqual(hydrated);
  });

  it('round-trips every geometry bucket, in LRU order, after an in-memory clear', () => {
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['sig', 42]]) }));
    setThreadSizePriors('t1', geom(600), bucket({ expansionSig: 'x', rows: new Map([['sig', 61]]) }));
    vi.advanceTimersByTime(1000);

    clearAllThreadSizePriorsForTest();
    const hydrated = getThreadSizePriors('t1')!;
    expect([...hydrated.byGeometry.keys()]).toEqual([key(800), key(600)]); // most recent LAST
    expect(sizePriorsAtGeometry(hydrated, geom(800))?.rows.get('sig')).toBe(42);
    expect(sizePriorsAtGeometry(hydrated, geom(600))?.rows.get('sig')).toBe(61);
    expect(sizePriorsAtGeometry(hydrated, geom(600))?.expansionSig).toBe('x');
  });

  it('round-trips the composite key, so typography survives a restart', () => {
    // Two buckets at the SAME width, separated only by typography: the
    // stored key has to carry both halves or they collapse into one on
    // load and the wrong heights replay.
    const small = geom(800, 'f13/sgeist/mgeist/c1/w1');
    const large = geom(800, 'f18/shack-nerd/mgeist/c0/w1');
    setThreadSizePriors('t1', small, bucket({ rows: new Map([['sig', 42]]) }));
    setThreadSizePriors('t1', large, bucket({ rows: new Map([['sig', 61]]) }));
    vi.advanceTimersByTime(1000);

    clearAllThreadSizePriorsForTest();
    const hydrated = getThreadSizePriors('t1')!;
    expect([...hydrated.byGeometry.keys()]).toEqual([
      '800|f13/sgeist/mgeist/c1/w1',
      '800|f18/shack-nerd/mgeist/c0/w1',
    ]);
    expect(sizePriorsAtGeometry(hydrated, small)?.rows.get('sig')).toBe(42);
    expect(sizePriorsAtGeometry(hydrated, large)?.rows.get('sig')).toBe(61);
  });

  it('coalesces repeated persists before the debounce fires into one write of the latest entry', () => {
    setThreadSizePriors('t1', geom(100), bucket());
    vi.advanceTimersByTime(500);
    setThreadSizePriors('t1', geom(200), bucket());
    vi.advanceTimersByTime(999);
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).toBeNull(); // debounce kept resetting
    vi.advanceTimersByTime(1);
    const stored = JSON.parse(localStorage.getItem(`${V3_PREFIX}t1`) as string);
    expect(stored.buckets.map((pair: [string, unknown]) => pair[0])).toEqual([key(100), key(200)]);
  });
});

describe('malformed storage', () => {
  it('drops malformed JSON, warns once, and removes the key', () => {
    localStorage.setItem(`${V3_PREFIX}bad`, '{not json');
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}bad`)).toBeNull();
    warn.mockRestore();
  });

  it('drops an entry that fails shape validation', () => {
    localStorage.setItem(
      `${V3_PREFIX}bad2`,
      JSON.stringify({ buckets: [['not-a-geometry-key', { expansionSig: '', rows: [] }]] }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad2')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}bad2`)).toBeNull();
    warn.mockRestore();
  });

  it('drops a row pair with a non-finite height', () => {
    // NaN doesn't survive JSON.stringify (becomes null), so seed the raw
    // string directly to exercise the finite-number guard.
    localStorage.setItem(
      `${V3_PREFIX}bad3`,
      '{"buckets":[["800|f15",{"expansionSig":"","rows":[["sig",null]]}]]}',
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad3')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });

  it('drops a row pair with a negative height (corrupt prior)', () => {
    // Capture filters UNMEASURED/negatives before persisting, so a
    // negative height can only come from corrupt/hand-edited storage — a
    // negative estimate would poison the size store's offsets.
    localStorage.setItem(
      `${V3_PREFIX}bad4`,
      '{"buckets":[["800|f15",{"expansionSig":"","rows":[["sig",-5]]}]]}',
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad4')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}bad4`)).toBeNull();
    warn.mockRestore();
  });

  it('drops the WHOLE thread entry when one geometry bucket is malformed', () => {
    // A bucket is only ever written as a unit, so a bucket that fails
    // validation means the value was corrupted or hand-edited; the buckets
    // around it are no more trustworthy than the one that failed.
    localStorage.setItem(
      `${V3_PREFIX}bad5`,
      JSON.stringify({
        buckets: [
          [key(800), { expansionSig: '', rows: [['sig', 42]] }],
          [key(600), { expansionSig: 7, rows: [] }],
        ],
      }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad5')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}bad5`)).toBeNull();
    warn.mockRestore();
  });

  it('drops a negative width in the geometry key', () => {
    localStorage.setItem(
      `${V3_PREFIX}bad6`,
      JSON.stringify({ buckets: [['-800|f15', { expansionSig: '', rows: [['sig', 42]] }]] }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('bad6')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });

  it('caps a stored entry that carries more geometry buckets than the store keeps', () => {
    localStorage.setItem(
      `${V3_PREFIX}wide`,
      JSON.stringify({
        buckets: [900, 800, 700, 600].map((width) => [
          key(width),
          { expansionSig: '', rows: [['sig', width]] },
        ]),
      }),
    );

    // The tail is the most recently captured, so that is what survives.
    expect([...getThreadSizePriors('wide')!.byGeometry.keys()]).toEqual([
      key(800), key(700), key(600),
    ]);
  });

  it('drops a bucket holding more rows than a pane can retain', () => {
    // The cap is the pane's own retention ceiling, so a bucket past it
    // cannot have come from a capture in this app — there is nothing to
    // salvage, the way a negative height above has nothing to salvage.
    const rows = Array.from({ length: MAX_ROWS_PER_BUCKET + 1 }, (_, i) => [`sig${i}`, 10]);
    localStorage.setItem(
      `${V3_PREFIX}wide-bucket`,
      JSON.stringify({ buckets: [[key(800), { expansionSig: '', rows }]] }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('wide-bucket')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}wide-bucket`)).toBeNull();
    warn.mockRestore();
  });

  it('keeps a bucket holding exactly the cap', () => {
    const rows = Array.from({ length: MAX_ROWS_PER_BUCKET }, (_, i) => [`sig${i}`, 10]);
    localStorage.setItem(
      `${V3_PREFIX}at-cap`,
      JSON.stringify({ buckets: [[key(800), { expansionSig: '', rows }]] }),
    );

    expect(sizePriorsAtGeometry(getThreadSizePriors('at-cap')!, geom(800))?.rows.size).toBe(
      MAX_ROWS_PER_BUCKET,
    );
  });

  it('drops a v1-shaped bucket key that escaped the version sweep', () => {
    // v1 keyed a thread's measurements by a bare numeric width. A
    // hand-copied or half-swept profile can still hold one; it names no
    // geometry this build could ever look up, so the entry goes rather
    // than sitting in the cap as a permanent miss.
    localStorage.setItem(
      `${V3_PREFIX}v1shape`,
      JSON.stringify({ buckets: [[800, { expansionSig: '', rows: [['sig', 42]] }]] }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors('v1shape')).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}v1shape`)).toBeNull();
    warn.mockRestore();
  });
});

describe('stale-version sweep', () => {
  it('removes keys under an older schema version on install', () => {
    // v1 held one width per thread; each shape change ships as a version
    // bump with no data migration, so the sweep is the migration.
    localStorage.setItem(
      'agent-overflow.sizePriors.v2.t1',
      JSON.stringify({ widths: [[800, { expansionSig: '', rows: [['sig', 42]] }]] }),
    );
    localStorage.setItem('agent-overflow.sizePriors.v2.index', '["t1"]');
    localStorage.setItem('agent-overflow.sizePriors.v1.t1', 'stale');
    localStorage.setItem('agent-overflow.sizePriors.v1.index', '["t1"]');
    localStorage.setItem('agent-overflow.sizePriors.v0.t1', 'stale');
    installSizePriorsPersistence();
    expect(localStorage.getItem('agent-overflow.sizePriors.v2.t1')).toBeNull();
    expect(localStorage.getItem('agent-overflow.sizePriors.v2.index')).toBeNull();
    expect(localStorage.getItem('agent-overflow.sizePriors.v1.t1')).toBeNull();
    expect(localStorage.getItem('agent-overflow.sizePriors.v1.index')).toBeNull();
    expect(localStorage.getItem('agent-overflow.sizePriors.v0.t1')).toBeNull();
    expect(getThreadSizePriors('t1')).toBeUndefined();
  });

  it('leaves current-version keys alone', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    installSizePriorsPersistence();
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).not.toBeNull();
  });
});

describe('index LRU cap', () => {
  it('evicts the oldest stored thread past the 50-thread cap', () => {
    for (let i = 0; i < 51; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
      vi.advanceTimersByTime(1000);
    }
    expect(localStorage.getItem(`${V3_PREFIX}t0`)).toBeNull();
    expect(localStorage.getItem(`${V3_PREFIX}t50`)).not.toBeNull();
  });

  it('a load bumps recency, folded into the next flush rather than a synchronous write', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
    }
    vi.advanceTimersByTime(1000);

    // Restart simulation: hydrate t0 through the adapter, bumping its recency.
    clearAllThreadSizePriorsForTest();
    getThreadSizePriors('t0');
    // No synchronous index write from the load alone.
    const indexBefore = JSON.parse(
      localStorage.getItem('agent-overflow.sizePriors.v3.index') as string,
    );
    expect(indexBefore[indexBefore.length - 1]).not.toBe('t0');

    vi.advanceTimersByTime(1000);
    const indexAfter = JSON.parse(
      localStorage.getItem('agent-overflow.sizePriors.v3.index') as string,
    );
    expect(indexAfter[indexAfter.length - 1]).toBe('t0');

    // t0 is now most-recent, so the next new thread evicts t1 instead.
    setThreadSizePriors('t51', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V3_PREFIX}t0`)).not.toBeNull();
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).toBeNull();
  });
});

describe('quota exceeded', () => {
  const quotaError = () => new DOMException('quota exceeded', 'QuotaExceededError');

  it('persists the post-eviction index when the index write itself hits quota', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    setThreadSizePriors('t2', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    // Stored index is now [t1, t2].

    // Next flush: t3's entry write succeeds, the index write hits quota
    // once (evicting t1), and the retried index write must reflect the
    // eviction — not the pre-eviction serialization.
    const originalSetItem = localStorage.setItem.bind(localStorage);
    let threw = false;
    vi.spyOn(localStorage, 'setItem')
      .mockImplementation((key: string, value: string) => {
        if (key === `${V3_PREFIX}index` && !threw) {
          threw = true;
          throw quotaError();
        }
        originalSetItem(key, value);
      });

    setThreadSizePriors('t3', geom(800), bucket());
    vi.advanceTimersByTime(1000);

    expect(localStorage.getItem(`${V3_PREFIX}t1`)).toBeNull();
    const index = JSON.parse(localStorage.getItem(`${V3_PREFIX}index`) as string);
    expect(index).toEqual(['t2', 't3']);
  });

  it('keeps the write-target thread indexed when quota eviction lands on itself', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    // Stored index is now [t1] — the only evictable thread is t1 itself.

    const originalSetItem = localStorage.setItem.bind(localStorage);
    let threw = false;
    vi.spyOn(localStorage, 'setItem')
      .mockImplementation((key: string, value: string) => {
        if (key === `${V3_PREFIX}t1` && !threw) {
          threw = true;
          throw quotaError();
        }
        originalSetItem(key, value);
      });

    setThreadSizePriors('t1', geom(900), bucket());
    vi.advanceTimersByTime(1000);

    // The retried entry write succeeded, so the thread must stay indexed —
    // an unindexed entry escapes the LRU cap forever.
    const stored = JSON.parse(localStorage.getItem(`${V3_PREFIX}t1`) as string);
    expect(stored.buckets.map((pair: [string, unknown]) => pair[0])).toEqual([key(800), key(900)]);
    const index = JSON.parse(localStorage.getItem(`${V3_PREFIX}index`) as string);
    expect(index).toEqual(['t1']);
  });

  it('keeps evicting until the write fits rather than giving up after one', () => {
    // One eviction is not a bound on how much room a write needs: a
    // single large entry can outweigh several small ones. Stopping after
    // one retry disabled persistence for the whole session while the
    // profile still held evictable priors.
    for (const id of ['t1', 't2', 't3']) {
      setThreadSizePriors(id, geom(800), bucket());
      vi.advanceTimersByTime(1000);
    }
    // Stored index is now [t1, t2, t3].

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const originalSetItem = localStorage.setItem.bind(localStorage);
    let refusals = 0;
    vi.spyOn(localStorage, 'setItem')
      .mockImplementation((k: string, value: string) => {
        if (k === `${V3_PREFIX}t4` && refusals < 2) {
          refusals += 1;
          throw quotaError();
        }
        originalSetItem(k, value);
      });

    setThreadSizePriors('t4', geom(800), bucket());
    vi.advanceTimersByTime(1000);

    // Two evictions, then the write landed — and persistence is intact.
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).toBeNull();
    expect(localStorage.getItem(`${V3_PREFIX}t2`)).toBeNull();
    expect(localStorage.getItem(`${V3_PREFIX}t3`)).not.toBeNull();
    expect(localStorage.getItem(`${V3_PREFIX}t4`)).not.toBeNull();
    expect(JSON.parse(localStorage.getItem(`${V3_PREFIX}index`) as string)).toEqual(['t3', 't4']);
    expect(warn).not.toHaveBeenCalled();

    setThreadSizePriors('t5', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V3_PREFIX}t5`)).not.toBeNull();
    warn.mockRestore();
  });

  it('evicts every stored thread, then disables persistence when the write still fails', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    vi.advanceTimersByTime(1000);

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const setItemSpy = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('quota exceeded', 'QuotaExceededError');
    });

    setThreadSizePriors('t2', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(warn).toHaveBeenCalledTimes(1);

    // Persistence is now disabled for the session: a further persist
    // doesn't even attempt a write.
    setItemSpy.mockClear();
    setThreadSizePriors('t3', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(setItemSpy).not.toHaveBeenCalled();

    setItemSpy.mockRestore();
    warn.mockRestore();
  });

  it('disables persistence without evicting when the failure is not a quota error', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).not.toBeNull();

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const setItemSpy = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('denied', 'SecurityError');
    });

    setThreadSizePriors('t2', geom(800), bucket());
    vi.advanceTimersByTime(1000);
    expect(warn).toHaveBeenCalledTimes(1);
    setItemSpy.mockRestore();

    // Only a full store earns an eviction: the stored thread survives a
    // failure that was not about room.
    expect(localStorage.getItem(`${V3_PREFIX}t1`)).not.toBeNull();
    warn.mockRestore();
  });
});
