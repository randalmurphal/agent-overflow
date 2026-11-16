import { SvelteMap } from 'svelte/reactivity';
import type { DiffReviewComment } from '../types/models';

// The comment sets a review reads, one per source, bounded for the drafts
// caches of a thread (diffReviewComments.svelte.ts) and of a PR
// (prDiffReviewComments.svelte.ts). Past `max` sets the least recently
// read or held one goes, never one a surface holds: a surface showing a
// source holds it, so its drafts cannot vanish from the screen while
// SQLite has them. Holders are counted; the cache holds at most `max`
// sets plus the held ones.

export interface DraftSourceCache {
  /** The set as last read (reactive), or undefined before a read. */
  get(key: string): readonly DiffReviewComment[] | undefined;
  has(key: string): boolean;
  /** Stores a read as the most recent use, then evicts past the bound. */
  put(key: string, comments: readonly DiffReviewComment[]): void;
  /** Keeps `key` while held; answers its release (idempotent). */
  hold(key: string): () => void;
  clear(): void;
}

export function createDraftSourceCache(max: number, onEvict: (key: string) => void): DraftSourceCache {
  const sets = new SvelteMap<string, readonly DiffReviewComment[]>();
  // Plain: holding and the use order never wake a reader. Oldest first.
  const used = new Map<string, true>();
  const holders = new Map<string, number>();

  function touch(key: string): void {
    used.delete(key);
    used.set(key, true);
  }

  function evict(): void {
    for (const key of used.keys()) {
      if (sets.size <= max) return;
      if (holders.has(key) || !sets.has(key)) continue;
      sets.delete(key);
      used.delete(key);
      onEvict(key);
    }
  }

  return {
    get: (key) => sets.get(key),
    has: (key) => sets.has(key),
    put(key, comments) {
      sets.set(key, comments);
      touch(key);
      evict();
    },
    hold(key) {
      holders.set(key, (holders.get(key) ?? 0) + 1);
      touch(key);
      let held = true;
      return () => {
        if (!held) return;
        held = false;
        const left = (holders.get(key) ?? 1) - 1;
        if (left > 0) holders.set(key, left);
        else holders.delete(key);
        if (sets.has(key)) touch(key);
        else if (left <= 0) used.delete(key);
      };
    },
    clear() {
      sets.clear();
      used.clear();
      holders.clear();
    },
  };
}
