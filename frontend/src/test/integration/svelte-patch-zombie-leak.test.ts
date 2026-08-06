// Regression test for the zombie-derived leak. This was the
// "zombie-mint fix" hunk of frontend/patches/svelte@5.56.3.patch; the
// hunk was DROPPED on the 5.56.8 re-roll because upstream fixed the same
// class of leak in 5.56.5 (sveltejs/svelte#18517: update_effect leaves
// is_updating_effect false for branch/root effects). The suite stays as
// the tripwire — it must keep passing UNPATCHED.
//
// Pristine svelte 5.56.3: reading a parent's prop-expression memo during
// component init happens while the active reader is an UNCONNECTED
// derived. Because is_updating_effect was true during init, get()'s
// should_connect force-connected every derived the memo reads into its
// deps' reactions arrays — but the unconnected reader can never register
// as a subscriber, so the connection was permanent. Each mount/unmount
// cycle leaked the whole derived chain plus everything it retains (in the
// real app: per-diff-row memos pinning detached DOM, ~520 MB in a 12 h
// session). Two shapes trigger the init-time read: a prop DEFAULT
// (prop() reads the memo even if app code never does) and an explicit
// init-time read of a plain prop. Both leaked on pristine 5.56.3
// (verified externally) and both must stay clean going forward.
import { describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { get, set, state } from 'svelte/internal/client';
import Host from './svelte-patch-fixtures/Host.svelte';
import type { FixtureItem, FixtureVariant, PaneLike } from './svelte-patch-fixtures/types';

const ROWS = 5;

function makeFixture(variant: FixtureVariant) {
  const paneSig = state<PaneLike>({ thread: { workspacePath: '/repo0' } });
  const openSig = state(true);
  const items: FixtureItem[] = Array.from({ length: ROWS }, (_, i) => ({
    key: `n${i}`,
    title: `row ${i}`,
    variant,
  }));
  const seen: string[] = [];
  const target = document.body.appendChild(document.createElement('div'));
  const props = {
    getPane: () => get(paneSig),
    getItems: () => items,
    isOpen: () => get(openSig),
    onInit: (workspacePath: string) => seen.push(workspacePath),
  };
  return { paneSig, openSig, seen, target, props };
}

function runCycles(variant: FixtureVariant, cycles: number) {
  const { paneSig, openSig, seen, target, props } = makeFixture(variant);
  const app = mount(Host, { target, props });
  try {
    flushSync();
    for (let cycle = 1; cycle < cycles; cycle++) {
      set(openSig, false);
      flushSync();
      // Change the pane while the subtree is closed; the reopened
      // children must observe the fresh value through the lazy
      // (unconnected) chain.
      set(paneSig, { thread: { workspacePath: `/repo${cycle}` } });
      set(openSig, true);
      flushSync();
    }
  } finally {
    unmount(app);
    flushSync();
    target.remove();
  }
  return { paneSig, seen };
}

describe('svelte patch: init-time prop reads must not leak subscribers', () => {
  // `reactions` is null both at creation and after the last subscriber
  // is removed, so toBeNull() is the canonical "no subscribers" — and
  // unlike a `?? []` fallback it fails loudly if a svelte upgrade
  // renames the internal field out from under this suite.
  it('leak shape 1: defaulted prop (prop() init read) leaves no subscribers', () => {
    const { paneSig, seen } = runCycles('default', 3);
    expect(paneSig.reactions).toBeNull();
    expect(seen).toHaveLength(ROWS * 3);
  });

  it('leak shape 2: explicit init-time read of a plain prop leaves no subscribers', () => {
    const { paneSig, seen } = runCycles('no-default', 3);
    expect(paneSig.reactions).toBeNull();
    expect(seen).toHaveLength(ROWS * 3);
  });

  it('the lazy unconnected chain still computes fresh values per cycle', () => {
    const { seen } = runCycles('default', 3);
    expect(seen.slice(0, ROWS)).toEqual(Array(ROWS).fill('/repo0'));
    expect(seen.slice(ROWS, ROWS * 2)).toEqual(Array(ROWS).fill('/repo1'));
    expect(seen.slice(ROWS * 2)).toEqual(Array(ROWS).fill('/repo2'));
  });

  // Positive control: proves the `reactions` introspection observes real
  // subscriptions (so the null assertions above cannot pass vacuously),
  // that the patch still connects chains read by connected readers, and
  // that those chains stay live and fully disconnect on unmount.
  it('healthy path: template-read prop subscribes, updates, and disconnects', () => {
    const { paneSig, seen, target, props } = makeFixture('template-read');
    const app = mount(Host, { target, props });
    try {
      flushSync();
      expect(paneSig.reactions?.length ?? 0).toBeGreaterThan(0);
      expect(target.textContent).toContain('row 0: /repo0');
      set(paneSig, { thread: { workspacePath: '/fresh' } });
      flushSync();
      expect(target.textContent).toContain('row 0: /fresh');
      expect(seen).toHaveLength(ROWS);
    } finally {
      unmount(app);
      flushSync();
      target.remove();
    }
    expect(paneSig.reactions).toBeNull();
  });
});
