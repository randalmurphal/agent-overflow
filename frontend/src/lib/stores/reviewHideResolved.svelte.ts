// "Hide resolved threads", a review pane's view option in the toolbar's
// view-options menu. It applies to both homes of a thread: the
// Conversation feed and the thread rows in the diff.
//
// The option is a remembered preference of the frontend, per computer
// (appStorage, as the Pull requests page's filters are). A review starts
// from it; the toggle and the "N resolved hidden" line write it. A
// review already open adopts a change made in another one only at its
// own explicit refresh or reload, so nothing vanishes in a pane the
// reader was not touching. A failed save is appStorage's toast; the
// toggle still applies to the review.
//
// What is hidden is decided when the view is (re)loaded: on the toggle,
// on an explicit refresh or reload, and on each diff load. A thread
// resolved after that moment (the reader's Resolve or a remote one)
// stays on screen, shown as resolved, until the next such moment, so
// nothing vanishes under the cursor; one unresolved since shows again.
// A thread that arrived since the moment shows whatever its state, and
// one holding the reader's draft reply always shows, so the draft stays
// where it is written.

import { untrack } from 'svelte';
import type { BackendKey } from '../transport/backendKey';
import type { ReviewThread } from '../types/models';
import { appStorageGet, appStorageSet } from './appStorage';

export const HIDE_RESOLVED_KEY = 'review:hide-resolved';

/** The remembered value on one computer. */
export interface HideResolvedPreference {
  read(): boolean;
  write(on: boolean): void;
}

export function hideResolvedPreference(backend: BackendKey): HideResolvedPreference {
  return {
    read: () => appStorageGet(HIDE_RESOLVED_KEY, backend) === '1',
    write: (on) => appStorageSet(HIDE_RESOLVED_KEY, on ? '1' : '0', backend),
  };
}

export interface HideResolvedState {
  readonly on: boolean;
  /** The reader's choice: applies here and is remembered. */
  set(on: boolean): void;
  /** Decide again what is hidden: the threads resolved now. `adopt`
   * first takes up the remembered value (an explicit refresh or
   * reload). */
  decide(opts?: { adopt?: boolean }): void;
  /** Show a hidden thread until the next decision (a jump to it). */
  reveal(threadId: string): void;
  isHidden(threadId: string): boolean;
  /** Hidden threads among those the PR has now. */
  readonly hiddenCount: number;
  /** The PR's threads less the hidden ones; the same array when none is
   * hidden, so a reader of its identity sees no change. */
  readonly shownThreads: readonly ReviewThread[];
}

const NONE: ReadonlySet<string> = Object.freeze(new Set<string>());

function settled(thread: ReviewThread): boolean {
  return thread.isResolvable && thread.isResolved;
}

export function createHideResolved(
  threads: () => readonly ReviewThread[],
  /** Threads that hold a draft reply: never hidden. */
  heldReplies: () => ReadonlySet<string>,
  preference: HideResolvedPreference,
): HideResolvedState {
  let on = $state(preference.read());
  let decided: ReadonlySet<string> = $state.raw(NONE);

  const hidden = $derived.by<ReadonlySet<string>>(() => {
    if (!on || decided.size === 0) return NONE;
    const out = new Set<string>();
    const held = heldReplies();
    for (const thread of threads()) if (decided.has(thread.id) && settled(thread) && !held.has(thread.id)) out.add(thread.id);
    return out.size === 0 ? NONE : out;
  });

  const shownThreads = $derived.by<readonly ReviewThread[]>(() => {
    const all = threads();
    return hidden.size === 0 ? all : all.filter((thread) => !hidden.has(thread.id));
  });

  function decide(opts?: { adopt?: boolean }): void {
    if (opts?.adopt) on = preference.read();
    if (!on) {
      decided = NONE;
      return;
    }
    const held = untrack(heldReplies);
    decided = new Set(untrack(threads).filter((thread) => settled(thread) && !held.has(thread.id)).map((thread) => thread.id));
  }

  return {
    get on() { return on; },
    set(next: boolean): void {
      preference.write(next);
      if (next === on) return;
      on = next;
      decide();
    },
    decide,
    reveal(threadId: string): void {
      if (!decided.has(threadId)) return;
      const next = new Set(decided);
      next.delete(threadId);
      decided = next;
    },
    isHidden: (threadId) => hidden.has(threadId),
    get hiddenCount() { return hidden.size; },
    get shownThreads() { return shownThreads; },
  };
}
