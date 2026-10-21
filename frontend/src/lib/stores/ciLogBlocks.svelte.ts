// The lines of one followed CI job log, as the log view shows them: read
// from the backend's log cache (GetPRCILogLines) a block at a time, when
// a row of the block enters the view's render window, and kept in a
// least-recently-used cache bounded by block count and text size.
//
// A block is the fixed line range [index * CI_LOG_BLOCK_LINES, + that
// many). Each holds the lines of one version of the log. The index the
// pump sends (prReviewCI.svelte.ts) says which version is current and how
// many leading lines of the version before it still hold: when the
// version moves by one, a block wholly inside those lines is still
// current, and the last block of a growing log reads only its new lines.
// Any other block is stale. A stale block keeps showing its lines while
// its reload is in flight, so a followed log never flashes placeholders
// over text the reader is looking at. A block that failed to load holds
// its error, which its row shows in place, and loads again the next time
// a row acquires it or the log moves to another version.
//
// Blocks a mounted row holds are never evicted: the cache holds at most
// CI_LOG_BLOCK_CACHE_MAX blocks and CI_LOG_CACHE_CHARS_MAX of text, or
// the render window's blocks when those alone pass a bound.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { GetPRCILogLines } from './bindings';
import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import type { CILogLines } from '../types/models';
import type { RepoReferenceWire } from '../utils/prReference';
import { errString } from '../utils/errors';

/** Lines per block: a row of the view shows at most one block's lines. */
export const CI_LOG_BLOCK_LINES = 200;
/** Blocks kept: a few viewports of lines on either side of the render
 * window, which a 200-line block spans several of. */
export const CI_LOG_BLOCK_CACHE_MAX = 24;
/** UTF-16 units of text kept. Ordinary blocks are tens of KB, so the
 * block count binds first; a block of long lines (up to the backend's
 * 64 KB line bound each) binds this one. */
export const CI_LOG_CACHE_CHARS_MAX = 8 * 1024 * 1024;
/** The most lines one GetPRCILogLines call asks for: its cap. */
export const CI_LOG_READ_LINES_MAX = 2000;

/** Reads lines [from, from + count) of the log, as GetPRCILogLines. */
export type CILogFetch = (from: number, count: number) => Promise<CILogLines>;

/** GetPRCILogLines for one job of a repository, on the computer that
 * follows it: a log is its repository's, whichever PR or pipeline showed it. */
export function ciLogFetch(backend: BackendKey, repo: RepoReferenceWire, jobId: string): CILogFetch {
  return (from, count) => withBackendTarget(backend, () => GetPRCILogLines(repo, jobId, from, count));
}

/** Reads lines [from, to) of the log whole, all from one version, a read
 * at a time: for a section's copy or send, never the view's cache. */
export async function readCILogLines(fetch: CILogFetch, from: number, to: number): Promise<string[]> {
  const out: string[] = [];
  let version: number | null = null;
  // A version that moves mid-read starts the read again; a log moves at
  // the follow cadence, far slower than a read of 64 KB.
  for (let attempt = 0; attempt < 3; attempt += 1) {
    out.length = 0;
    version = null;
    let moved = false;
    while (from + out.length < to) {
      const reply = await fetch(from + out.length, Math.min(CI_LOG_READ_LINES_MAX, to - from - out.length));
      if (version === null) version = reply.version;
      if (reply.version !== version) {
        moved = true;
        break;
      }
      if (reply.lines.length === 0) break;
      out.push(...reply.lines);
    }
    if (!moved) return out;
  }
  throw new Error('The log kept changing while it was read; try again.');
}

/** What a row reads of a block. */
export interface CILogBlockView {
  /** The block's lines from its first, of the version they were read
   * at; null until a load lands. A stale block's lines stay until its
   * reload replaces them. */
  readonly lines: readonly string[] | null;
  /** The lines (absolute numbers) shown clipped at the line bound. */
  readonly clipped: ReadonlySet<number>;
  readonly loading: boolean;
  /** The last load's failure, shown in place of the lines it lacks. */
  readonly error: string | null;
}

const NO_CLIPPED: ReadonlySet<number> = new Set();

class Block implements CILogBlockView {
  lines = $state.raw<readonly string[] | null>(null);
  clipped = $state.raw<ReadonlySet<number>>(NO_CLIPPED);
  loading = $state(false);
  error = $state<string | null>(null);
  /** The version lines were read at. */
  version = 0;
  chars = 0;
  /** Mounted rows holding the block. */
  uses = 0;
  lastUse = 0;
  /** Bumped by every load and by eviction; a load whose token is stale
   * drops its result. */
  token = 0;
}

export class CILogBlockCache {
  readonly #fetch: CILogFetch;
  readonly #blocks = new SvelteMap<number, Block>();
  #version = 0;
  #totalLines = 0;
  #clock = 0;
  #disposed = false;

  constructor(fetch: CILogFetch) {
    this.#fetch = fetch;
  }

  /** Reactive: the blocks held, in use or not. */
  get size(): number {
    return this.#blocks.size;
  }

  /** Reactive: a block's state, once a row acquired it. */
  block(index: number): CILogBlockView | undefined {
    return this.#blocks.get(index);
  }

  /** Applies the log's index: its version, size and the leading lines of
   * the version before it that this one keeps. */
  setIndex(version: number, totalLines: number, stableLines: number): void {
    untrack(() => {
      const moved = version !== this.#version;
      if (moved) {
        const next = version === this.#version + 1;
        for (const [index, block] of this.#blocks) {
          const kept = block.lines !== null && block.version === this.#version
            && index * CI_LOG_BLOCK_LINES + block.lines.length <= stableLines;
          if (next && kept) block.version = version;
        }
        this.#version = version;
      }
      this.#totalLines = totalLines;
      for (const [index, block] of this.#blocks) {
        if (index * CI_LOG_BLOCK_LINES >= totalLines && block.uses === 0) this.#drop(index, block);
        // A failed block reads again on a new version (the backend
        // downloaded the log again, or a refresh did) or when a row
        // acquires it, so a failure that persists is not read again at
        // the follow cadence.
        else if (block.uses > 0 && (moved || block.error === null)) this.#refresh(index, block);
      }
    });
  }

  /** A mounted row holds the block: it loads now if it is not current
   * (a failed one tries again), and stays while held. Returns the
   * release. */
  acquire(index: number): () => void {
    return untrack(() => {
      let block = this.#blocks.get(index);
      if (!block) {
        block = new Block();
        this.#blocks.set(index, block);
      }
      block.uses += 1;
      block.lastUse = ++this.#clock;
      this.#refresh(index, block);
      this.#evict();
      const held = block;
      let released = false;
      return () => {
        if (released) return;
        released = true;
        held.uses -= 1;
        held.lastUse = ++this.#clock;
        this.#evict();
      };
    });
  }

  dispose(): void {
    this.#disposed = true;
    for (const block of this.#blocks.values()) block.token += 1;
    this.#blocks.clear();
  }

  #want(index: number): number {
    return Math.max(0, Math.min(CI_LOG_BLOCK_LINES, this.#totalLines - index * CI_LOG_BLOCK_LINES));
  }

  #current(index: number, block: Block): boolean {
    return block.lines !== null && block.version === this.#version && block.lines.length >= this.#want(index);
  }

  #refresh(index: number, block: Block): void {
    if (this.#disposed || block.loading) return;
    if (this.#current(index, block) || this.#want(index) === 0) return;
    void this.#load(index, block);
  }

  async #load(index: number, block: Block): Promise<void> {
    const token = ++block.token;
    block.loading = true;
    block.error = null;
    const start = index * CI_LOG_BLOCK_LINES;
    const want = this.#want(index);
    // A current prefix (the block of a growing log) reads only the rest.
    const keep = block.lines !== null && block.version === this.#version;
    let lines: string[] = keep ? [...(block.lines ?? [])] : [];
    let clipped = new Set(keep ? block.clipped : []);
    let version = keep ? block.version : 0;
    try {
      while (lines.length < want) {
        const reply = await this.#fetch(start + lines.length, Math.min(CI_LOG_READ_LINES_MAX, want - lines.length));
        if (token !== block.token) return;
        if (version !== 0 && reply.version !== version) {
          // The log moved mid-block: read the block again from its first
          // line, of the version the backend holds now.
          lines = [];
          clipped = new Set();
          version = 0;
          continue;
        }
        version = reply.version;
        if (reply.lines.length === 0) break;
        lines.push(...reply.lines);
        for (const line of reply.clipped ?? []) clipped.add(line);
        if (start + lines.length >= reply.totalLines) break;
      }
    } catch (err) {
      if (token === block.token) {
        block.error = errString(err);
        block.loading = false;
      }
      return;
    }
    block.chars = lines.reduce((sum, line) => sum + line.length, 0);
    block.lines = lines;
    block.clipped = clipped.size > 0 ? clipped : NO_CLIPPED;
    block.version = version;
    block.loading = false;
    // The index moved on while the load ran: read the block again. A
    // reply ahead of the index waits for the index to reach it.
    if (block.uses > 0 && version < this.#version) this.#refresh(index, block);
    this.#evict();
  }

  #drop(index: number, block: Block): void {
    block.token += 1;
    this.#blocks.delete(index);
  }

  /** Drops the least recently used blocks no row holds while the cache is
   * over either bound. */
  #evict(): void {
    for (;;) {
      let chars = 0;
      let oldest: [number, Block] | null = null;
      for (const entry of this.#blocks) {
        const block = entry[1];
        chars += block.chars;
        if (block.uses === 0 && (oldest === null || block.lastUse < oldest[1].lastUse)) oldest = entry;
      }
      if (oldest === null || (this.#blocks.size <= CI_LOG_BLOCK_CACHE_MAX && chars <= CI_LOG_CACHE_CHARS_MAX)) return;
      this.#drop(oldest[0], oldest[1]);
    }
  }
}
