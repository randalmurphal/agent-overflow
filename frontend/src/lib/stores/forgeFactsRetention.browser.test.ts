import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cdp } from 'vitest/browser';
import { __forgeFactsHeldForTest, __resetForgeFactsForTest, __setResolveForgeFactsForTest, forgeFactReader, observePRFacts } from './forgeFacts.svelte';
import { prKey, type PRRef } from '../utils/prReference';
import { labelIndex, type ForgeFact } from '../utils/forgeFacts';
import type { PRDetail, ReviewThread } from '../types/models';

// What the facts store keeps alive of a PR's text. Each `pr:updated` frame
// is parsed into new strings; the store compares a body with the one it
// scanned, and must hold the newest frame's string for that, not the one
// from the frame it scanned, or every body of a PR is held twice for as
// long as the PR is open. Measured as the page's live heap after a forced
// collection, the only place two equal strings can be told apart.

const REF: PRRef = { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 9 };
const KEY = prKey(REF);
const BODIES = 100;
const BODY_CHARS = 100_000;
const TEXT_BYTES = BODIES * BODY_CHARS;

type Snapshot = { detail: PRDetail | null; threads: ReviewThread[] };

/** A frame as the wire delivers it: every string its own object. */
function frame(): Snapshot {
  const threads = Array.from({ length: BODIES }, (_, index) => ({
    id: `t${index}`,
    comments: [{ id: `c${index}`, body: `comment ${index} ${'word '.repeat(BODY_CHARS / 5)}` }],
  }));
  return JSON.parse(JSON.stringify({ detail: null, threads })) as Snapshot;
}

async function liveHeap(): Promise<number> {
  const session = cdp();
  await session.send('HeapProfiler.collectGarbage');
  await session.send('HeapProfiler.collectGarbage');
  const usage = (await session.send('Runtime.getHeapUsage')) as { usedSize: number };
  return usage.usedSize;
}

beforeEach(() => {
  __resetForgeFactsForTest();
  __setResolveForgeFactsForTest(async (_backend, _workspace, _pr, _revision, keys) => ({ facts: keys.map((key) => ({ key, status: 'unavailable' as const })) }));
});

afterEach(() => {
  __resetForgeFactsForTest();
});

describe('forge facts retention', () => {
  it('holds one copy of a PR\'s bodies across frames, the newest frame\'s', async () => {
    // Warm the code paths so the measured frames allocate only their text.
    observePRFacts(KEY, REF, frame());
    __resetForgeFactsForTest();
    const before = await liveHeap();
    let held: Snapshot | null = null;
    for (let index = 0; index < 4; index++) {
      held = frame();
      observePRFacts(KEY, REF, held);
    }
    const after = await liveHeap();
    // The PR store's newest snapshot and the facts store share its strings:
    // one copy of the text. A store that kept the strings it first scanned
    // would hold a second.
    expect(held).not.toBeNull();
    expect(after - before).toBeGreaterThan(TEXT_BYTES * 0.8);
    expect(after - before).toBeLessThan(TEXT_BYTES * 1.5);
  });

  it('holds a reference, not an object, per key a body names', async () => {
    // 1,000 comments naming the same 250 issues: 250,000 key references.
    const text = Array.from({ length: 250 }, (_, index) => `#${index + 1}`).join(' ');
    const many = (): Snapshot =>
      JSON.parse(JSON.stringify({
        detail: null,
        threads: Array.from({ length: 1000 }, (_, index) => ({ id: `t${index}`, comments: [{ id: `c${index}`, body: `${index} ${text}` }] })),
      })) as Snapshot;
    observePRFacts(KEY, REF, many());
    __resetForgeFactsForTest();
    const before = await liveHeap();
    const held = many();
    observePRFacts(KEY, REF, held);
    const after = await liveHeap();
    expect(held.threads).toHaveLength(1000);
    expect(__forgeFactsHeldForTest(KEY)).toMatchObject({ facts: 250, bodyKeys: 250_000 });
    // Measured 2.9 MB: the bodies' text is about 1.2 MB and the 250,000
    // references about 1 MB.
    expect(after - before).toBeLessThan(5 * 1024 * 1024);
  });

  it('holds a project\'s 1,000-label list once, however many of its PRs are open', async () => {
    const LABELS = 'labels:group/widget';
    const gitlab = (number: number): PRRef => ({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number });
    // Each answer is the wire's own parse: a new object per call.
    const wire = () => JSON.parse(JSON.stringify({
      key: LABELS, status: 'found', complete: true,
      labels: Array.from({ length: 1000 }, (_, i) => ({ title: `area::component-${i}`, color: `#${(0xd9534f + i).toString(16)}`, textColor: '#ffffff' })),
    })) as ForgeFact;
    __setResolveForgeFactsForTest(async (_backend, _workspace, _pr, _revision, keys) => ({ facts: keys.map((key) => (key === LABELS ? wire() : { key, status: 'unavailable' as const })) }));
    const open = async (number: number) => {
      observePRFacts(prKey(gitlab(number)), gitlab(number), JSON.parse(JSON.stringify({ detail: null, threads: [{ id: 't', comments: [{ id: `c${number}`, body: `PR ${number}: ~bug and ~docs` }] }] })) as Snapshot);
      await new Promise((resolve) => setTimeout(resolve, 0));
    };
    // Warm the code paths.
    await open(1);
    __resetForgeFactsForTest();
    __setResolveForgeFactsForTest(async (_backend, _workspace, _pr, _revision, keys) => ({ facts: keys.map((key) => (key === LABELS ? wire() : { key, status: 'unavailable' as const })) }));
    const before = await liveHeap();
    await open(1);
    const one = (await liveHeap()) - before;
    for (let number = 2; number <= 20; number++) await open(number);
    const twenty = (await liveHeap()) - before;
    console.info(`a 1,000-label list: ${one} bytes held by one PR, ${twenty} by twenty PRs of the project`);
    // Measured: one PR holds about 89 KB, the list and its own
    // bookkeeping; nineteen more add about 3.5 KB each of bookkeeping, not
    // a list each (which would be about 1.6 MB).
    expect(one).toBeGreaterThan(50_000);
    expect(one).toBeLessThan(300_000);
    expect(twenty - one).toBeLessThan(one);
    const held = forgeFactReader(prKey(gitlab(1)))!.get(LABELS);
    expect(held).toMatchObject({ status: 'found', complete: true });
    for (let number = 2; number <= 20; number++) expect(forgeFactReader(prKey(gitlab(number)))!.get(LABELS)).toBe(held);
    // Its title index, built once for every PR's lexes.
    const indexed = await liveHeap();
    expect(labelIndex(held as ForgeFact).size).toBe(1000);
    const index = (await liveHeap()) - indexed;
    console.info(`its title index: ${index} bytes`);
    expect(index).toBeLessThan(one);
  });
});
