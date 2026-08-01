// Per-key reactive boxes for global keyed live state.
//
// The fan-out class this exists for: a store-level Map/Set that is
// either replaced wholesale on any key's change, or is a SvelteMap/Set
// read on mostly-MISSING keys. Either way every reader of every key
// subscribes to one shared version signal, so one thread's transition
// (status flip, queue change, hydration toggle) invalidates every
// sidebar row and collapsed-project rollup at event cadence. The fix,
// pioneered by threadStatuses' active-turn registry: one `$state.raw`
// box per key, so a reader re-evaluates only when ITS key's value
// changes (plus one re-run per box CREATION, via `creationVersion`).
//
// The two pitfalls that pattern solved carry over here:
//
//  - Writer-side box creation. Svelte does not register state created
//    inside the currently-running reaction as a dependency of that
//    reaction, so a reader that lazily created its own box could never
//    track it. `set` is the only box creator, and writers run from
//    event handlers, outside any reaction, where creation is safe.
//  - Creation-version tracking. A reader of a key with no box yet
//    tracks `creationVersion` instead (bumped when a box is CREATED —
//    not on every write); after the key's first-ever write creates the
//    box, that reader re-runs once and tracks the box directly.
//
// Boxes live for the session (bounded by distinct keys observed);
// `drop` releases a key when its owner is discarded. Values are held
// in `$state.raw` — replaced wholesale, never mutated in place — so
// writing the value a box already holds (`===`) does not invalidate
// its readers, mirroring a no-op Map/Set mutation.

interface ValueBox<V> {
  get current(): V;
  set current(value: V);
}

function newValueBox<V>(initial: V): ValueBox<V> {
  let current: V = $state.raw(initial);
  return {
    get current() {
      return current;
    },
    set current(value: V) {
      current = value;
    },
  };
}

export interface KeyedSignalRegistry<V> {
  /**
   * Tracked read. Subscribes the running reaction to this key only —
   * or to `creationVersion` while the key has no box yet. Returns the
   * registry's empty value for boxless keys.
   */
  get(key: string): V;
  /**
   * Writer-side upsert. Writing the empty value to a key with no box
   * is a no-op (no box created, nothing invalidated) — the equivalent
   * of deleting an absent Map/Set entry.
   */
  set(key: string, value: V): void;
  /** Empty the key's box (waking its readers) and release it. */
  drop(key: string): void;
  /** Wipe every box. Test isolation only. */
  reset(): void;
}

export function createKeyedSignalRegistry<V>(emptyValue: V): KeyedSignalRegistry<V> {
  const boxes = new Map<string, ValueBox<V>>();
  let creationVersion = $state(0);

  return {
    get(key: string): V {
      const box = boxes.get(key);
      if (!box) {
        // Track creations so this key's first-ever write re-runs the
        // reader; on that re-run the box exists and is tracked directly.
        void creationVersion;
        return emptyValue;
      }
      return box.current;
    },

    set(key: string, value: V): void {
      let box = boxes.get(key);
      if (!box) {
        if (value === emptyValue) return;
        box = newValueBox(emptyValue);
        boxes.set(key, box);
        creationVersion += 1;
      }
      box.current = value;
    },

    drop(key: string): void {
      const box = boxes.get(key);
      if (!box) return;
      // Empty the signal BEFORE dropping the box: a still-mounted
      // reader re-runs off the write, re-reads through `get` (which
      // tracks `creationVersion` while box-less and re-attaches when
      // the key's next write creates a fresh box); the orphan is GC'd
      // with the reader.
      box.current = emptyValue;
      boxes.delete(key);
    },

    reset(): void {
      for (const box of boxes.values()) box.current = emptyValue;
      boxes.clear();
      creationVersion = 0;
    },
  };
}
