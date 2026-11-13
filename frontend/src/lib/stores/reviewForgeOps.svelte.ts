import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { toastLostWrite, writeFailure, type LostWriteText } from './lostWrite';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { PRRef } from '../utils/prReference';

// One review pane's small forge writes on a comment: a reaction's toggle
// and a suggestion's apply. Each is one operation on one key (a chip, a
// suggestion), and nothing is optimistic: the key reads as pending from
// the call until the pane has applied the PR frame the write answered
// with (`PRActionResult.seq`, the pump's sequence after the poll it
// asked for), so the control shows the forge's answer and never its own
// guess. `seq` 0 has nothing to wait for. The wait ends with the
// backend's launch (a restarted pump numbers from one) and while the pump
// is failing (its next answer is unknown, and the pane says so). One
// operation per key at a time: a second press while one is pending does
// nothing.
//
// Everything is keyed by the PR it started on (its computer-scoped key),
// so the controls of the PR on screen read only their own. A failure's
// sentence belongs to a surface (a comment's chip row, a suggestion), and
// the newest operation on a surface owns it: an older answer never
// replaces or clears a newer one's. Where a failure goes is chosen when
// it arrives: its surface while the review state lives, whether or not
// its PR is on screen that moment; a toast once the state was disposed
// (the pane closed), through the lost-write rule; nothing once the pane's
// PR became another, which also forgets every surface's sentence.

export interface ForgeOpsHost {
  readonly backend: BackendKey;
  /** The PR the pane shows, or null outside the PR scope. */
  ref(): PRRef | null;
  /** The PR's computer-scoped key (computerPRKey of ref), whatever the
   * scope; null without a PR. */
  identity(): string | null;
  /** The PR's applied seq (peekPRSeq of its entity key), which settles a
   * write. */
  appliedSeq(): number;
  launch(): string;
  updateFailing(): boolean;
  /** Reads the PR's threads again, for an answer with no pump (seq 0);
   * the read shows its own failure on the pane. */
  refreshThreads(): Promise<void>;
}

/** What a failure after the pane closed says (lostWrite.ts). */
export type ForgeOpLost = Omit<LostWriteText, 'body'>;

export interface ForgeOpOptions {
  /** A refusal reads the PR again (a suggestion refused because the head
   * or the suggestion moved): the pane then shows what the forge holds. */
  refreshOnFailure?: boolean;
}

export interface ForgeOps {
  /** Runs `call` as the operation on `key` of the PR on screen, its
   * failure shown on `surface`. Answers false when it did not start (no
   * PR, or `key` is pending) or failed. */
  run(key: string, surface: string, call: (ref: PRRef) => Promise<{ seq: number } | null | undefined>, lost: ForgeOpLost, options?: ForgeOpOptions): Promise<boolean>;
  /** The operation on `key` of the PR on screen is in flight or waiting
   * for its frame. */
  pending(key: string): boolean;
  /** The failure shown on `surface` of the PR on screen. */
  error(surface: string): string | null;
  /** Bumps when the pane's PR becomes another: open popovers close. */
  readonly epoch: number;
  dispose(): void;
}

interface Op {
  readonly id: number;
  readonly launch: string;
  /** The answer's seq; undefined while the call is in flight. */
  readonly seq?: number;
}

const SEP = '\u0000';

export function createForgeOps(host: ForgeOpsHost): ForgeOps {
  const ops = new SvelteMap<string, Op>();
  const errors = new SvelteMap<string, string>();
  // The newest operation on each surface, so an older one's answer never
  // replaces or clears a newer one's sentence.
  const surfaceOwner = new Map<string, number>();
  let counter = 0;
  let epoch = $state(0);
  let disposed = false;

  function opPending(op: Op | undefined): boolean {
    if (!op) return false;
    if (op.seq === undefined) return true;
    return op.launch === host.launch() && !host.updateFailing() && host.appliedSeq() < op.seq;
  }

  function scoped(key: string): string | null {
    const identity = host.identity();
    return identity === null ? null : `${identity}${SEP}${key}`;
  }

  // A moment without a PR (git status settling) is not another PR.
  let identity: string | null = untrack(() => host.identity());
  const stopWatch = $effect.root(() => {
    $effect(() => {
      const next = host.identity();
      if (next === null || next === identity) return;
      const was = identity;
      identity = next;
      if (was === null) return;
      untrack(() => {
        epoch += 1;
        errors.clear();
        surfaceOwner.clear();
      });
    });
  });

  return {
    async run(key, surface, call, lost, options = {}) {
      const ref = host.ref();
      const opKey = scoped(key);
      const surfaceKey = scoped(surface);
      if (!ref || opKey === null || surfaceKey === null || opPending(ops.get(opKey))) return false;
      const started = epoch;
      const op: Op = { id: ++counter, launch: host.launch() };
      ops.set(opKey, op);
      surfaceOwner.set(surfaceKey, op.id);
      errors.delete(surfaceKey);
      let seq: number;
      try {
        // An older backend's reply answers nothing: read as no pump.
        seq = (await withBackendTarget(host.backend, () => call(ref)))?.seq ?? 0;
      } catch (err) {
        if (ops.get(opKey) === op) ops.delete(opKey);
        if (disposed) {
          toastLostWrite(lost, err);
          return false;
        }
        if (started !== epoch) return false;
        if (surfaceOwner.get(surfaceKey) === op.id) errors.set(surfaceKey, writeFailure(err).sentence);
        if (options.refreshOnFailure) void host.refreshThreads();
        return false;
      }
      if (ops.get(opKey) !== op) return true;
      if (seq <= 0) {
        ops.delete(opKey);
        if (!disposed) void host.refreshThreads();
        return true;
      }
      ops.set(opKey, { ...op, seq });
      return true;
    },
    pending(key) {
      const opKey = scoped(key);
      return opKey !== null && opPending(ops.get(opKey));
    },
    error(surface) {
      const surfaceKey = scoped(surface);
      return surfaceKey === null ? null : (errors.get(surfaceKey) ?? null);
    },
    get epoch() { return epoch; },
    dispose() {
      disposed = true;
      stopWatch();
    },
  };
}
