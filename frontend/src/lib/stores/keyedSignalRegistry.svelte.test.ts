import { describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

describe('createKeyedSignalRegistry', () => {
  it('returns the empty value for unknown keys and round-trips writes', () => {
    const registry = createKeyedSignalRegistry<string>('idle');
    expect(registry.get('a')).toBe('idle');
    registry.set('a', 'running');
    expect(registry.get('a')).toBe('running');
    registry.set('a', 'idle');
    expect(registry.get('a')).toBe('idle');
  });

  it('writing the empty value to a boxless key creates nothing and wakes nobody', () => {
    const registry = createKeyedSignalRegistry<boolean>(false);
    const probe = probeReactivity(() => registry.get('a'));
    try {
      expect(probe.evaluations).toBe(1);
      registry.set('a', false);
      flushSync();
      // No box was created, so the creation version did not move.
      expect(probe.evaluations).toBe(1);
    } finally {
      probe.dispose();
    }
  });

  it("a reader re-runs only for its own key's changes once its box exists", () => {
    const registry = createKeyedSignalRegistry<number>(0);
    registry.set('mine', 1);
    const probe = probeReactivity(() => registry.get('mine'));
    try {
      expect(probe.latest).toBe(1);
      const baseline = probe.evaluations;

      // Another key's creation and churn must not reach this reader.
      registry.set('other', 5);
      flushSync();
      registry.set('other', 6);
      flushSync();
      expect(probe.evaluations).toBe(baseline);

      // Equal write on its own key: no invalidation ($state.raw ===).
      registry.set('mine', 1);
      flushSync();
      expect(probe.evaluations).toBe(baseline);

      registry.set('mine', 2);
      flushSync();
      expect(probe.latest).toBe(2);
    } finally {
      probe.dispose();
    }
  });

  it('a boxless reader re-runs on its key creation, then tracks the box; drop re-fires it once', () => {
    const registry = createKeyedSignalRegistry<string | null>(null);
    const probe = probeReactivity(() => registry.get('a'));
    try {
      expect(probe.latest).toBeNull();

      // First-ever write creates the box → one creation re-run, after
      // which the reader tracks the box directly.
      registry.set('a', 'x');
      flushSync();
      expect(probe.latest).toBe('x');

      registry.drop('a');
      flushSync();
      expect(probe.latest).toBeNull();

      // The reader re-attached through the creation version, so a
      // post-drop write still reaches it via a fresh box.
      registry.set('a', 'y');
      flushSync();
      expect(probe.latest).toBe('y');
    } finally {
      probe.dispose();
    }
  });

  it('reset empties every box and clears the registry', () => {
    const registry = createKeyedSignalRegistry<string>('');
    registry.set('a', 'x');
    registry.set('b', 'y');
    const probe = probeReactivity(() => registry.get('a'));
    try {
      expect(probe.latest).toBe('x');
      registry.reset();
      flushSync();
      expect(probe.latest).toBe('');
      expect(registry.get('b')).toBe('');
    } finally {
      probe.dispose();
    }
  });
});
