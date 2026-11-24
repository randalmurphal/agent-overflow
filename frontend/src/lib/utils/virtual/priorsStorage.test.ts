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
import { installSizePriorsPersistence } from './priorsStorage';

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
  installSizePriorsPersistence();
});

afterEach(() => {
  // Restore before reset: a test that failed mid-body leaves its throwing
  // setItem mock installed, which would poison every later test.
  vi.restoreAllMocks();
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

  // Two buckets on one thread survive an in-memory clear, most recent LAST.
  it.each([
    ['round-trips every geometry bucket, in LRU order, after an in-memory clear',
      geom(800), geom(600), 'x', [key(800), key(600)]],
    // Two buckets at the SAME width, separated only by typography: the
    // stored key has to carry both halves or they collapse into one on
    // load and the wrong heights replay.
    ['round-trips the composite key, so typography survives a restart',
      geom(800, 'f13/sgeist/mgeist/c1/w1'), geom(800, 'f18/shack-nerd/mgeist/c0/w1'), '',
      ['800|f13/sgeist/mgeist/c1/w1', '800|f18/shack-nerd/mgeist/c0/w1']],
  ])('%s', (_, first, second, secondSig, keys) => {
    setThreadSizePriors('t1', first, bucket({ rows: new Map([['sig', 42]]) }));
    setThreadSizePriors('t1', second, bucket({ expansionSig: secondSig, rows: new Map([['sig', 61]]) }));
    vi.advanceTimersByTime(1000);

    clearAllThreadSizePriorsForTest();
    const hydrated = getThreadSizePriors('t1')!;
    expect([...hydrated.byGeometry.keys()]).toEqual(keys);
    expect(sizePriorsAtGeometry(hydrated, first)?.rows.get('sig')).toBe(42);
    expect(sizePriorsAtGeometry(hydrated, second)?.rows.get('sig')).toBe(61);
    expect(sizePriorsAtGeometry(hydrated, second)?.expansionSig).toBe(secondSig);
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
  // Every malformed payload drops the whole thread entry: warn once, return
  // nothing, and remove the key.
  it.each([
    ['drops malformed JSON, warns once, and removes the key', 'bad', '{not json'],
    ['drops an entry that fails shape validation', 'bad2',
      JSON.stringify({ buckets: [['not-a-geometry-key', { expansionSig: '', rows: [] }]] })],
    // NaN doesn't survive JSON.stringify (becomes null), so seed the raw
    // string directly to exercise the finite-number guard.
    ['drops a row pair with a non-finite height', 'bad3',
      '{"buckets":[["800|f15",{"expansionSig":"","rows":[["sig",null]]}]]}'],
    // Capture filters UNMEASURED/negatives before persisting, so a negative
    // height can only come from corrupt/hand-edited storage; a negative
    // estimate would poison the size store's offsets.
    ['drops a row pair with a negative height (corrupt prior)', 'bad4',
      '{"buckets":[["800|f15",{"expansionSig":"","rows":[["sig",-5]]}]]}'],
    // A bucket is only ever written as a unit, so a bucket that fails
    // validation means the value was corrupted or hand-edited; the buckets
    // around it are no more trustworthy than the one that failed.
    ['drops the WHOLE thread entry when one geometry bucket is malformed', 'bad5',
      JSON.stringify({
        buckets: [
          [key(800), { expansionSig: '', rows: [['sig', 42]] }],
          [key(600), { expansionSig: 7, rows: [] }],
        ],
      })],
    ['drops a negative width in the geometry key', 'bad6',
      JSON.stringify({ buckets: [['-800|f15', { expansionSig: '', rows: [['sig', 42]] }]] })],
    // The cap is the pane's own retention ceiling, so a bucket past it
    // cannot have come from a capture in this app; there is nothing to
    // salvage, the way a negative height has nothing to salvage.
    ['drops a bucket holding more rows than a pane can retain', 'wide-bucket',
      JSON.stringify({
        buckets: [[key(800), {
          expansionSig: '',
          rows: Array.from({ length: MAX_ROWS_PER_BUCKET + 1 }, (_, i) => [`sig${i}`, 10]),
        }]],
      })],
    // v1 keyed a thread's measurements by a bare numeric width. A
    // hand-copied or half-swept profile can still hold one; it names no
    // geometry this build could ever look up, so the entry goes rather
    // than sitting in the cap as a permanent miss.
    ['drops a v1-shaped bucket key that escaped the version sweep', 'v1shape',
      JSON.stringify({ buckets: [[800, { expansionSig: '', rows: [['sig', 42]] }]] })],
  ])('%s', (_, threadId, raw) => {
    localStorage.setItem(`${V3_PREFIX}${threadId}`, raw);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    expect(getThreadSizePriors(threadId)).toBeUndefined();

    expect(warn).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(`${V3_PREFIX}${threadId}`)).toBeNull();
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
