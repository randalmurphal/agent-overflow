// Regression test for the "ownerless-roots" hunk of
// frontend/patches/svelte@5.56.8.patch.
//
// Pristine svelte stamps every effect — including $effect.root — with the
// component context and parent effect that happen to be live at creation
// time. A long-lived store-level root created during a row component's
// render (threadRowUiState's expansion registry is the in-app case) then
// pins that row instance's props accessors, closure scopes, and detached
// DOM for the root's entire lifetime: ~190 dead row shells in an
// overnight heap snapshot. The patch creates roots ownerless (ctx null,
// parent null), like mount()-level roots.
//
// The first test fails on pristine 5.56.3 (ctx/parent are non-null); the
// second pins that ownerless roots still behave like roots; the third
// pins the patch's own error path (try/finally restoration) and passes
// on pristine too — it exists to catch a bad future re-roll.
import { describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { active_effect, get, set, state, type EffectLike } from 'svelte/internal/client';
import RootSpawner from './svelte-patch-fixtures/RootSpawner.svelte';
import {
  spawnCapturingRoot,
  spawnReactiveRoot,
  spawnThrowingRoot,
  type CapturedRoot,
} from './svelte-patch-fixtures/ownerlessRootHelpers.svelte';

describe('svelte patch: $effect.root is ownerless', () => {
  it('a root created during component render has no ctx and no parent', () => {
    const holder: { captured: CapturedRoot<EffectLike> | null } = { captured: null };

    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(RootSpawner, {
      target,
      props: {
        title: 'spawner',
        spawn: () => {
          holder.captured = spawnCapturingRoot(() => active_effect);
        },
      },
    });
    try {
      flushSync();
      expect(holder.captured?.effect).toBeTruthy();
      // Pristine 5.56.3: ctx is the spawning component's context (when
      // the component pushes one) and parent is its render effect — the
      // retention pin. Patched: both null.
      expect(holder.captured?.effect?.ctx).toBeNull();
      expect(holder.captured?.effect?.parent).toBeNull();
    } finally {
      holder.captured?.dispose();
      unmount(app);
      flushSync();
      target.remove();
    }
  });

  it('an ownerless root survives its creating component and still reacts', () => {
    const sig = state(0);
    const seen: number[] = [];
    let disposeRoot = () => {};

    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(RootSpawner, {
      target,
      props: {
        title: 'spawner',
        spawn: () => {
          disposeRoot = spawnReactiveRoot(() => get(sig), seen);
        },
      },
    });
    flushSync();
    expect(seen).toEqual([0]);

    // The creating component goes away; the root must keep reacting.
    unmount(app);
    flushSync();
    target.remove();
    set(sig, 1);
    flushSync();
    expect(seen).toEqual([0, 1]);

    // Dispose stops it.
    disposeRoot();
    set(sig, 2);
    flushSync();
    expect(seen).toEqual([0, 1]);
  });

  it('a throwing root body restores the spawning component globals', () => {
    // The patch nulls component_context/active_effect around create_effect
    // in a try/finally. A root body that throws (root bodies run
    // synchronously inside create_effect) must propagate the error AND
    // leave the spawning component's globals restored — otherwise the
    // rest of that component's init runs ownerless and pop() crashes on
    // the nulled context.
    const sig = state(0);
    const seen: number[] = [];
    let thrown: unknown = null;
    let effectBefore: EffectLike | null = null;
    let effectAfter: EffectLike | null = null;
    let disposeRoot = () => {};

    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(RootSpawner, {
      target,
      props: {
        title: 'spawner',
        spawn: () => {
          effectBefore = active_effect;
          try {
            spawnThrowingRoot();
          } catch (err) {
            thrown = err;
          }
          effectAfter = active_effect;
          disposeRoot = spawnReactiveRoot(() => get(sig), seen);
        },
      },
    });
    try {
      flushSync();
      expect(thrown).toBeInstanceOf(Error);
      // Svelte dev mode appends a component stack to the message.
      expect((thrown as Error).message).toContain('root body throws');
      // active_effect restored exactly; component init was mid-render, so
      // it must be the same non-null effect on both sides of the throw.
      expect(effectBefore).not.toBeNull();
      expect(effectAfter).toBe(effectBefore);
      // mount() completing at all proves component_context was restored —
      // pop() dereferences it at the end of init — and the component
      // rendered normally after the throw.
      expect(target.textContent).toContain('spawner');
      // A follow-up root created after the throw behaves like the healthy
      // case.
      expect(seen).toEqual([0]);
      set(sig, 1);
      flushSync();
      expect(seen).toEqual([0, 1]);
    } finally {
      disposeRoot();
      unmount(app);
      flushSync();
      target.remove();
    }
  });
});
