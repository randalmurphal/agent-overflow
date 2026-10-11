import { describe, expect, it } from 'vitest';
import {
  CI_LOG_BLOCK_CACHE_MAX,
  CI_LOG_BLOCK_LINES,
  CI_LOG_CACHE_CHARS_MAX,
  CI_LOG_READ_LINES_MAX,
  CILogBlockCache,
  readCILogLines,
} from './ciLogBlocks.svelte';
import type { CILogLines } from '../types/models';

const B = CI_LOG_BLOCK_LINES;

/** A log the test grows and answers reads of by hand. */
class Backend {
  lines: string[] = [];
  version = 0;
  readonly reads: [number, number][] = [];
  readonly #pending: { resolve: () => void; reject: (err: Error) => void }[] = [];
  /** When true, reads answer at once. */
  auto = true;

  set(version: number, lines: string[]): void {
    this.version = version;
    this.lines = lines;
  }

  readonly fetch = (from: number, count: number): Promise<CILogLines> => {
    this.reads.push([from, count]);
    return new Promise((resolve, reject) => {
      const answer = () => resolve({
        from,
        lines: this.lines.slice(from, from + count),
        totalLines: this.lines.length,
        version: this.version,
      });
      if (this.auto) answer();
      else this.#pending.push({ resolve: answer, reject });
    });
  };

  /** Answers the oldest waiting read with the log as it is now. */
  async answer(): Promise<void> {
    this.#pending.shift()?.resolve();
    await settle();
  }

  async fail(message: string): Promise<void> {
    this.#pending.shift()?.reject(new Error(message));
    await settle();
  }
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
}

function lines(count: number, tag = 'v1', first = 0): string[] {
  return Array.from({ length: count }, (_, index) => `${tag} ${first + index}`);
}

function open(total: number) {
  const backend = new Backend();
  backend.set(1, lines(total));
  const cache = new CILogBlockCache(backend.fetch);
  cache.setIndex(1, total, 0);
  return { backend, cache };
}

describe('CILogBlockCache', () => {
  it('reads a block when a row acquires it, and only that block', async () => {
    const { backend, cache } = open(3 * B);
    expect(backend.reads).toEqual([]);
    cache.acquire(1);
    await settle();
    expect(backend.reads).toEqual([[B, B]]);
    expect(cache.block(1)?.lines?.[0]).toBe(`v1 ${B}`);
    expect(cache.block(1)?.lines).toHaveLength(B);
    expect(cache.block(0)).toBeUndefined();
    // A second row of the same block reads nothing.
    cache.acquire(1);
    await settle();
    expect(backend.reads).toHaveLength(1);
  });

  it('keeps blocks inside the next version\'s stable lines and reads only the tail of a grown last block', async () => {
    const { backend, cache } = open(B + 50);
    cache.acquire(0);
    cache.acquire(1);
    await settle();
    expect(backend.reads).toEqual([[0, B], [B, 50]]);

    backend.set(2, [...lines(B + 50), ...lines(30, 'v1', B + 50)]);
    cache.setIndex(2, B + 80, B + 50);
    await settle();
    expect(backend.reads.slice(2)).toEqual([[B + 50, 30]]);
    expect(cache.block(1)?.lines).toHaveLength(80);
    expect(cache.block(1)?.lines?.at(-1)).toBe(`v1 ${B + 79}`);
  });

  it('reads a block again whole when the next version changed lines inside it, showing the old lines meanwhile', async () => {
    const { backend, cache } = open(B + 50);
    cache.acquire(0);
    cache.acquire(1);
    await settle();
    backend.auto = false;
    // A GitLab trace rewrote its last lines: only B + 10 lines hold.
    backend.set(2, [...lines(B + 10), ...lines(60, 'v2', B + 10)]);
    cache.setIndex(2, B + 70, B + 10);
    await settle();
    expect(backend.reads.slice(2)).toEqual([[B, 70]]);
    // Stale while revalidating: the old lines stay shown.
    expect(cache.block(1)?.loading).toBe(true);
    expect(cache.block(1)?.lines?.at(-1)).toBe(`v1 ${B + 49}`);
    await backend.answer();
    expect(cache.block(1)?.lines?.at(-1)).toBe(`v2 ${B + 69}`);
    expect(cache.block(1)?.lines?.[0]).toBe(`v1 ${B}`);
    expect(cache.block(1)?.loading).toBe(false);
  });

  it('reads every held block again on a version it did not see the step to', async () => {
    const { backend, cache } = open(B + 50);
    cache.acquire(0);
    cache.acquire(1);
    await settle();
    // Version 3 after 1: its stable lines are 2's, not 1's.
    backend.set(3, lines(B + 50, 'v3'));
    cache.setIndex(3, B + 50, B + 50);
    await settle();
    expect(backend.reads.slice(2)).toEqual([[0, B], [B, 50]]);
    expect(cache.block(0)?.lines?.[0]).toBe('v3 0');
  });

  it('marks a block it does not hold the rows of as stale, and reads it when a row acquires it again', async () => {
    const { backend, cache } = open(2 * B);
    const release = cache.acquire(0);
    await settle();
    release();
    backend.set(2, lines(2 * B, 'v2'));
    cache.setIndex(2, 2 * B, 0);
    await settle();
    // Unheld: nothing reads it until a row wants it.
    expect(backend.reads).toEqual([[0, B]]);
    cache.acquire(0);
    await settle();
    expect(backend.reads).toEqual([[0, B], [0, B]]);
    expect(cache.block(0)?.lines?.[0]).toBe('v2 0');
  });

  it('reads a block again from its first line when the log moved between its reads', async () => {
    // A block bigger than one read only happens above the read cap.
    expect(CI_LOG_READ_LINES_MAX).toBeGreaterThanOrEqual(B);
    const { backend, cache } = open(B + 50);
    cache.acquire(1);
    await settle();
    backend.auto = false;
    backend.set(2, [...lines(B + 50), ...lines(10, 'v1', B + 50)]);
    cache.setIndex(2, B + 60, B + 50);
    await settle();
    expect(backend.reads.at(-1)).toEqual([B + 50, 10]);
    // The answer is of version 3, which rewrote the block.
    backend.set(3, lines(B + 60, 'v3'));
    await backend.answer();
    expect(backend.reads.at(-1)).toEqual([B, 60]);
    await backend.answer();
    expect(cache.block(1)?.lines?.[0]).toBe(`v3 ${B}`);
    expect(cache.block(1)?.lines).toHaveLength(60);
  });

  it('holds a failed read\'s error in place and reads again on the next acquire', async () => {
    const { backend, cache } = open(B);
    backend.auto = false;
    const release = cache.acquire(0);
    await backend.fail('the downloaded log is gone');
    expect(cache.block(0)?.error).toBe('the downloaded log is gone');
    expect(cache.block(0)?.loading).toBe(false);
    expect(cache.block(0)?.lines).toBeNull();
    // An index frame does not retry a failed block on its own.
    backend.set(2, lines(B + 10));
    cache.setIndex(2, B + 10, B);
    expect(backend.reads).toHaveLength(1);

    release();
    cache.acquire(0);
    expect(backend.reads).toHaveLength(2);
    expect(cache.block(0)?.error).toBeNull();
    await backend.answer();
    expect(cache.block(0)?.lines).toHaveLength(B);
  });

  it('holds at most its block bound of blocks no row holds, dropping the least recently used', async () => {
    const total = (CI_LOG_BLOCK_CACHE_MAX + 6) * B;
    const { cache } = open(total);
    for (let index = 0; index < CI_LOG_BLOCK_CACHE_MAX + 6; index += 1) {
      const release = cache.acquire(index);
      await settle();
      release();
    }
    expect(cache.size).toBe(CI_LOG_BLOCK_CACHE_MAX);
    expect(cache.block(5)).toBeUndefined();
    expect(cache.block(6)?.lines).toHaveLength(B);
    // Touching a block makes it the newest.
    cache.acquire(6)();
    cache.acquire(CI_LOG_BLOCK_CACHE_MAX + 6)();
    await settle();
    expect(cache.block(6)).toBeDefined();
    expect(cache.block(7)).toBeUndefined();
  });

  it('never drops a block a row holds, whatever the bound', async () => {
    const count = CI_LOG_BLOCK_CACHE_MAX + 6;
    const { cache } = open(count * B);
    const releases = Array.from({ length: count }, (_, index) => cache.acquire(index));
    await settle();
    expect(cache.size).toBe(count);
    for (let index = 0; index < count; index += 1) expect(cache.block(index)?.lines).toHaveLength(B);
    for (const release of releases) release();
    expect(cache.size).toBe(CI_LOG_BLOCK_CACHE_MAX);
  });

  it('holds at most its text bound of blocks no row holds', async () => {
    // Long lines: each block is a quarter of the text bound and a bit.
    const long = 'x'.repeat(Math.ceil(CI_LOG_CACHE_CHARS_MAX / 4 / B) + 1);
    const backend = new Backend();
    backend.set(1, Array.from({ length: 6 * B }, () => long));
    const cache = new CILogBlockCache(backend.fetch);
    cache.setIndex(1, 6 * B, 0);
    for (let index = 0; index < 6; index += 1) {
      const release = cache.acquire(index);
      await settle();
      release();
    }
    expect(cache.size).toBe(3);
    expect([0, 1, 2].map((index) => cache.block(index))).toEqual([undefined, undefined, undefined]);
  });

  it('drops blocks past the end of a log that shrank, and a read that lands after it', async () => {
    const { backend, cache } = open(2 * B);
    cache.acquire(0)();
    await settle();
    backend.auto = false;
    const release = cache.acquire(1);
    backend.set(2, lines(50, 'v2'));
    release();
    cache.setIndex(2, 50, 0);
    expect(cache.block(1)).toBeUndefined();
    await backend.answer();
    expect(cache.block(1)).toBeUndefined();
    expect(cache.size).toBe(1);
  });

  it('drops a read that lands after dispose, answered or failed', async () => {
    const { backend, cache } = open(2 * B);
    backend.auto = false;
    cache.acquire(0);
    cache.acquire(1);
    const views = [cache.block(0), cache.block(1)];
    cache.dispose();
    await backend.answer();
    await backend.fail('connection lost');
    expect(cache.size).toBe(0);
    expect(views[0]?.lines).toBeNull();
    expect(views[1]?.error).toBeNull();
    expect(views[1]?.loading).toBe(true);
    // A disposed cache reads nothing more.
    cache.acquire(0);
    expect(backend.reads).toHaveLength(2);
  });

  it('reads a block again when the index moved on while its read ran', async () => {
    const { backend, cache } = open(B);
    backend.auto = false;
    cache.acquire(0);
    // The reply is of version 1; version 2 lands before it does.
    const answered = backend.answer();
    backend.set(2, lines(B, 'v2'));
    cache.setIndex(2, B, 0);
    await answered;
    expect(backend.reads).toEqual([[0, B], [0, B]]);
    await backend.answer();
    expect(cache.block(0)?.lines?.[0]).toBe('v2 0');
  });

  it('reads nothing before the log has a version', async () => {
    const backend = new Backend();
    const cache = new CILogBlockCache(backend.fetch);
    cache.acquire(0);
    cache.setIndex(0, 0, 0);
    await settle();
    expect(backend.reads).toEqual([]);
    backend.set(1, lines(10));
    cache.setIndex(1, 10, 0);
    await settle();
    expect(backend.reads).toEqual([[0, 10]]);
  });
});

describe('readCILogLines', () => {
  it('reads a range in reads of at most the cap, all of one version', async () => {
    const backend = new Backend();
    backend.set(4, lines(CI_LOG_READ_LINES_MAX + 10));
    const out = await readCILogLines(backend.fetch, 5, CI_LOG_READ_LINES_MAX + 10);
    expect(backend.reads).toEqual([[5, CI_LOG_READ_LINES_MAX], [CI_LOG_READ_LINES_MAX + 5, 5]]);
    expect(out).toHaveLength(CI_LOG_READ_LINES_MAX + 5);
    expect(out[0]).toBe('v1 5');
  });

  it('starts again when the version moves mid-read, and gives up on a log that keeps moving', async () => {
    const backend = new Backend();
    backend.set(1, lines(CI_LOG_READ_LINES_MAX + 10));
    let calls = 0;
    const moving = (from: number, count: number) => {
      calls += 1;
      if (calls === 2) backend.set(2, lines(CI_LOG_READ_LINES_MAX + 10, 'v2'));
      return backend.fetch(from, count);
    };
    const out = await readCILogLines(moving, 0, CI_LOG_READ_LINES_MAX + 10);
    expect(out[0]).toBe('v2 0');
    expect(out).toHaveLength(CI_LOG_READ_LINES_MAX + 10);

    let version = 10;
    const always = async (from: number, count: number): Promise<CILogLines> => {
      version += 1;
      return { from, lines: lines(count, 'x', from), totalLines: 1e6, version };
    };
    await expect(readCILogLines(always, 0, CI_LOG_READ_LINES_MAX + 1)).rejects.toThrow('The log kept changing while it was read; try again.');
  });
});
