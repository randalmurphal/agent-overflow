// Regression test for the "destroy-pass-errors" hunk of
// frontend/patches/svelte@5.56.8.patch.
//
// Pristine svelte destroys sibling effects in a loop (keyed {#each}
// reconcile, branch teardown, component unmount). A throwing user
// `$effect` teardown aborts that loop, so every sibling still queued for
// destruction is never handed to destroy_effect: it stays subscribed to
// its dependencies and retains its detached DOM for the lifetime of the
// parent block. In a long-lived app with virtualized rows that is an
// unbounded leak — one row with a throwing cleanup poisons every
// reconcile after it.
//
// The patch collects teardown errors in an array threaded through the
// destroy call chain, so the pass always completes its structural
// cleanup, and rethrows the first error once the pass is done.
// Upstream: https://github.com/sveltejs/svelte/pull/18566
// (fixes https://github.com/sveltejs/svelte/issues/18415)
// Drop the hunk when this suite passes on an unpatched svelte release.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { get, set, state } from 'svelte/internal/client';
import ThrowingTeardownHost from './svelte-patch-fixtures/ThrowingTeardownHost.svelte';

const ITEMS = [1, 2, 3, 4];

function makeHost(throwOn: number[]) {
  const dep = state(0);
  const itemsSig = state<number[]>(ITEMS);
  const torn: number[] = [];
  const target = document.body.appendChild(document.createElement('div'));
  const app = mount(ThrowingTeardownHost, {
    target,
    props: {
      getItems: () => get(itemsSig),
      readDep: () => get(dep),
      throwOn: () => throwOn,
      onteardown: (item: number) => torn.push(item),
    },
  });
  flushSync();
  return { dep, itemsSig, torn, target, app };
}

/** Run `fn`, returning whatever it threw (or null). */
function captureThrow(fn: () => void): unknown {
  try {
    fn();
    return null;
  } catch (err) {
    return err;
  }
}

/** unmount() resolves a promise, so a throwing teardown rejects it. */
function quietUnmount(app: Record<string, unknown>): Promise<void> {
  return Promise.resolve(unmount(app)).catch(() => {});
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('svelte patch: a throwing teardown must not strand a destroy pass', () => {
  it('an {#each} reconcile destroys every removed item despite a throwing teardown', () => {
    // Silence the DEV surfacing of the secondary teardown errors; it is
    // asserted on its own below.
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const { dep, itemsSig, torn, target, app } = makeHost([1]);
    try {
      expect(dep.reactions).toHaveLength(ITEMS.length);

      // Remove every item at once. Pristine: item 1's throwing teardown
      // aborts destroy_effects, leaving 2/3/4 subscribed to `dep` and
      // holding their detached DOM. Patched: the whole loop completes.
      set(itemsSig, []);
      const thrown = captureThrow(flushSync);

      expect(thrown, 'the teardown error must still surface').toBeInstanceOf(Error);
      expect((thrown as Error).message).toContain('teardown boom 1');
      expect([...torn].sort((a, b) => a - b)).toEqual(ITEMS);
      expect(dep.reactions, 'stranded siblings stay subscribed').toBeNull();
      expect(target.textContent?.trim()).toBe('');
    } finally {
      void quietUnmount(app);
      captureThrow(flushSync);
      target.remove();
    }
  });

  it('unmount destroys every child despite a throwing teardown', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const { dep, target, torn, app } = makeHost([1]);
    try {
      // unmount() reports through its promise, so the teardown error
      // surfaces as a rejection rather than a synchronous throw.
      await expect(Promise.resolve(unmount(app))).rejects.toThrow('teardown boom 1');
      flushSync();
      expect([...torn].sort((a, b) => a - b)).toEqual(ITEMS);
      expect(dep.reactions).toBeNull();
    } finally {
      target.remove();
    }
  });

  it('only the first teardown error is rethrown; the rest are surfaced in DEV', () => {
    const logged = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { itemsSig, target, app } = makeHost(ITEMS);
    try {
      set(itemsSig, []);
      const thrown = captureThrow(flushSync);

      expect(thrown).toBeInstanceOf(Error);
      // The other three are reported rather than dropped. Svelte's own
      // error reporting can log alongside them, so assert the teardown
      // errors are present rather than pinning the exact call count.
      const surfaced = logged.mock.calls
        .flat()
        .filter((arg): arg is Error => arg instanceof Error)
        .map((err) => err.message)
        .filter((message) => message.includes('teardown boom'));
      expect(surfaced).toHaveLength(ITEMS.length - 1);
    } finally {
      void quietUnmount(app);
      captureThrow(flushSync);
      target.remove();
    }
  });

  it('a clean destroy pass still throws nothing and releases every subscriber', () => {
    // Positive control: proves the assertions above are not vacuous and
    // that the patch leaves the ordinary destroy path untouched.
    const { dep, itemsSig, torn, target, app } = makeHost([]);
    try {
      set(itemsSig, []);
      expect(captureThrow(flushSync)).toBeNull();
      expect([...torn].sort((a, b) => a - b)).toEqual(ITEMS);
      expect(dep.reactions).toBeNull();
    } finally {
      unmount(app);
      flushSync();
      target.remove();
    }
  });
});
