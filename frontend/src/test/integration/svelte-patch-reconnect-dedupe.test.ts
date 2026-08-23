// Regression suite for the "reconnect-dedupe" hunk of
// frontend/patches/svelte@5.56.8.patch.
//
// Pristine svelte (5.56.8 and main as of 2026-08-23): `get()` on a derived
// that was DISCONNECTED (lost its last reaction earlier), has run before,
// and is dirty does two registrations in one call. `update_derived` runs
// with CONNECTED pre-set, so `update_reaction` pushes the derived into the
// `reactions` of every dep beyond `skipped_deps` — the deps that are new
// or re-ordered this run. Then `reconnect()` pushes it into EVERY dep's
// `reactions`, including those. A dep that was new that run now holds the
// derived twice. When the derived later loses its last reader,
// `remove_reaction` pops one copy and the other keeps the dep connected —
// and, transitively, everything upstream — for the life of the app.
//
// Live-app shape (2026-08-23 heap snapshot): ComposerToolbar's
// `sessionUsesSelectedAccount` short-circuits before reading
// `selectedAccount` until the session account connects, so its dep list
// grows exactly when the rate-limit popover is re-hovered after connect.
// The leftover duplicate pinned `selectedAccount` in the global `accounts`
// signal's reactions after the pane closed, retaining the pane's whole
// detached DOM (3.4k nodes) through the derived's closure context.
//
// Drop the hunk when this suite passes on an unpatched release.
import { describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import * as internal from 'svelte/internal/client';
import { get, set, state, type ValueLike } from 'svelte/internal/client';

type Reactions = { reactions: unknown[] | null };

// `derived`, `effect` and `effect_root` are exported by the runtime
// module but absent from svelte's internal type surface.
const { derived, effect, effect_root } = internal as unknown as {
  derived: <V>(fn: () => V) => ValueLike<V> & Reactions;
  effect: (fn: () => void) => void;
  effect_root: (fn: () => void) => () => void;
};

function readThrough(signal: ValueLike<unknown>): () => void {
  return effect_root(() => {
    effect(() => {
      get(signal);
    });
  });
}

describe('svelte patch: reconnecting a dirty derived registers it once per dependency', () => {
  it('a dep that became new on the reconnect run holds the derived exactly once', () => {
    const src = state(1);
    const gate = state(false);
    const inner = derived(() => get(src) * 2);
    const outer = derived(() => (get(gate) ? get(inner) : -1));

    // Connect (deps: [gate]), then disconnect.
    let dispose = readThrough(outer);
    flushSync();
    dispose();
    flushSync();
    expect((gate as unknown as Reactions).reactions).toBeNull();

    // Dirty the disconnected derived so its next run grows its deps to
    // [gate, inner]; `inner` is new on that run.
    set(gate, true);
    dispose = readThrough(outer);
    flushSync();
    expect(get(outer)).toBe(2);

    const innerReactions = (inner as unknown as Reactions).reactions ?? [];
    expect(
      innerReactions.filter((r) => r === outer).length,
      'outer registered on inner more than once',
    ).toBe(1);

    // Losing the last reader must disconnect the whole chain.
    dispose();
    flushSync();
    expect((inner as unknown as Reactions).reactions, 'inner still connected').toBeNull();
    expect((src as unknown as Reactions).reactions, 'src still connected').toBeNull();
    expect((gate as unknown as Reactions).reactions, 'gate still connected').toBeNull();
  });

  it('a reconnect whose dep list did not change still registers once', () => {
    const src = state(1);
    const only = derived(() => get(src) + 1);

    let dispose = readThrough(only);
    flushSync();
    dispose();
    flushSync();

    set(src, 5);
    dispose = readThrough(only);
    flushSync();
    expect(get(only)).toBe(6);
    const reactions = (src as unknown as Reactions).reactions ?? [];
    expect(reactions.filter((r) => r === only).length).toBe(1);

    dispose();
    flushSync();
    expect((src as unknown as Reactions).reactions).toBeNull();
  });
});
