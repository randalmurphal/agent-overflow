// "Hide resolved threads", a review pane's view option (the toolbar's
// view-options menu, beside whitespace, wrap and split, held in the
// review state as they are). It applies to both homes of a thread: the
// Conversation feed and the thread rows in the diff.
//
// What is hidden is decided when the view is (re)loaded: on the toggle,
// on an explicit refresh or reload, and on each diff load. A thread
// resolved after that moment (the reader's Resolve or a remote one)
// stays on screen, shown as resolved, until the next such moment, so
// nothing vanishes under the cursor; one unresolved since shows again.
// A thread that arrived since the moment shows whatever its state.

import { untrack } from 'svelte';
import type { ReviewThread } from '../types/models';

export interface HideResolvedState {
  readonly on: boolean;
  set(on: boolean): void;
  /** Decide again what is hidden: the threads resolved now. */
  decide(): void;
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

export function createHideResolved(threads: () => readonly ReviewThread[]): HideResolvedState {
  let on = $state(false);
  let decided: ReadonlySet<string> = $state.raw(NONE);

  const hidden = $derived.by<ReadonlySet<string>>(() => {
    if (!on || decided.size === 0) return NONE;
    const out = new Set<string>();
    for (const thread of threads()) if (decided.has(thread.id) && settled(thread)) out.add(thread.id);
    return out.size === 0 ? NONE : out;
  });

  const shownThreads = $derived.by<readonly ReviewThread[]>(() => {
    const all = threads();
    return hidden.size === 0 ? all : all.filter((thread) => !hidden.has(thread.id));
  });

  function decide(): void {
    decided = on ? new Set(untrack(threads).filter(settled).map((thread) => thread.id)) : NONE;
  }

  return {
    get on() { return on; },
    set(next: boolean): void {
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
