import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';

// Which PR one review pane's per-PR state belongs to. The pane is keyed by
// thread and workspace, and the PR under it follows git status: a branch
// checkout, or the branch's PR merging and a new one opening, puts another
// PR under live state. This is the one place that says the PR changed;
// every piece of state that belongs to a PR is read through it, either
// kept per PR (`keyed`) or keyed by `key` and dropped on `onChange`.
//
// A moment without a PR (git status settling) is not another PR: `key`
// stays on the PR the pane last had, and only a different PR is a change.
// The first PR the pane has adopts what was there before it.

export interface ReviewPRIdentity {
  /** The PR under the pane now (its computer-scoped key), or null. */
  readonly live: string | null;
  /** The PR the pane's per-PR state is on: `live`, or through a moment
   * without a PR the PR the pane last had. Null before the first one. */
  readonly key: string | null;
  /** Bumps each time `key` becomes another PR. */
  readonly epoch: number;
  /** Runs `change` untracked whenever `key` moves: `from` null for the
   * pane's first PR. Answers its removal. */
  onChange(change: (from: string | null, to: string) => void): () => void;
  /** State kept for each PR: `current` is the value of `key`'s PR, and a
   * PR the pane left keeps its value until the pane is back on it. A value
   * equal to `initial` is not kept. */
  keyed<T extends object>(initial: T): PerPR<T>;
  dispose(): void;
}

export interface PerPR<T extends object> {
  /** The value of the PR the pane's state is on. Reactive. */
  readonly current: T;
  /** The value of PR `key` (null: before the pane's first PR). */
  of(key: string | null): T;
  /** Changes PR `key`'s value. */
  set(key: string | null, patch: Partial<T>): void;
}

// The record of the state before the pane's first PR.
const NO_PR = '';

export function createReviewPRIdentity(live: () => string | null): ReviewPRIdentity {
  // Read synchronously, not after an effect: a reader in the same tick as
  // the change must already see the new PR's state.
  let last: string | null = untrack(live);
  function key(): string | null {
    const now = live();
    if (now !== null) last = now;
    return last;
  }

  let epoch = $state(0);
  const listeners = new Set<(from: string | null, to: string) => void>();
  let notified: string | null = untrack(live);
  const stop = $effect.root(() => {
    $effect(() => {
      const next = live();
      if (next === null || next === notified) return;
      const was = notified;
      notified = next;
      untrack(() => {
        if (was !== null) epoch += 1;
        for (const listener of [...listeners]) listener(was, next);
      });
    });
  });

  const identity: ReviewPRIdentity = {
    get live() { return live(); },
    get key() { return key(); },
    get epoch() { return epoch; },
    onChange(change) {
      listeners.add(change);
      return () => { listeners.delete(change); };
    },
    keyed<T extends object>(initial: T): PerPR<T> {
      const frozen = Object.freeze({ ...initial });
      const values = new SvelteMap<string, T>();
      const of = (pr: string | null): T => values.get(pr ?? NO_PR) ?? frozen;
      function put(slot: string, value: T): void {
        const pristine = (Object.keys(frozen) as (keyof T)[]).every((field) => Object.is(value[field], frozen[field]));
        if (pristine) values.delete(slot);
        else values.set(slot, value);
      }
      identity.onChange((from, to) => {
        if (from !== null || values.has(to)) return;
        const before = values.get(NO_PR);
        if (before === undefined) return;
        values.delete(NO_PR);
        values.set(to, before);
      });
      return {
        get current() { return of(key()); },
        of,
        set(pr, patch) {
          put(pr ?? NO_PR, Object.freeze({ ...of(pr), ...patch }));
        },
      };
    },
    dispose() {
      listeners.clear();
      stop();
    },
  };
  return identity;
}
