// Fake implementation of `@wailsio/runtime` for tests.
//
// The real module ships a Wails-specific IPC channel that only works inside a
// Wails webview. Tests run in happy-dom and never initialise that channel, so
// we replace it with a synchronous pub-sub registry that tests can drive
// directly via `emitWailsEvent()`.

import { __bindingMocksInternal } from './bindings-app';

type Handler = (ev: { name: string; data: unknown }) => void;

const listeners: Map<string, Set<Handler>> = new Map();

/**
 * Mock of the Wails runtime's `Call.ByName` RPC. Routes `main.App.<Method>`
 * to the same registry that `setBindingMock` drives, so hand-written
 * bindings in lib/stores/bindings.ts that use `Call.ByName(...)` behave
 * the same as generated bindings the test harness aliases.
 */
export const Call = {
  ByName(name: string, ...args: unknown[]): Promise<unknown> {
    // Accept 'main.App.<Method>' or a bare method name — tests may use
    // either shape via setBindingMock.
    const key = name.replace(/^main\.App\./, '');
    const fn = __bindingMocksInternal.get(key);
    if (!fn) {
      return Promise.reject(
        new Error(
          `Call.ByName: no mock for ${name}. Install one via setBindingMock('${key}', impl).`,
        ),
      );
    }
    return Promise.resolve(fn(...args));
  },
};

/**
 * Mock of the Wails runtime's `Create` helpers. The generated bindings files
 * import these and call them while building up type-conversion factories
 * (e.g. `$Create.Array(ThreadMessageHit.createFrom)`). Tests don't decode
 * real Wails payloads — the mock just returns identity-like factories so
 * the generated module side-effects at import time don't blow up.
 */
export const Create = {
  Any<T = unknown>(source: unknown): T {
    return source as T;
  },
  ByteSlice(source: unknown): string {
    return typeof source === 'string' ? source : '';
  },
  Array<T = unknown>(element: (source: unknown) => T): (source: unknown) => T[] {
    return (source: unknown) => {
      if (!Array.isArray(source)) return [];
      return source.map(element);
    };
  },
  Map<V = unknown>(
    _key: (source: unknown) => string,
    value: (source: unknown) => V,
  ): (source: unknown) => Record<string, V> {
    return (source: unknown) => {
      if (!source || typeof source !== 'object') return {};
      const out: Record<string, V> = {};
      for (const [k, v] of Object.entries(source as Record<string, unknown>)) {
        out[k] = value(v);
      }
      return out;
    };
  },
  Nullable<T = unknown>(element: (source: unknown) => T): (source: unknown) => T | null {
    return (source: unknown) => (source == null ? null : element(source));
  },
  Struct(
    fields: Record<string, (source: unknown) => unknown>,
  ): <U extends Record<string, unknown> = Record<string, unknown>>(source: unknown) => U {
    return <U extends Record<string, unknown>>(source: unknown): U => {
      if (!source || typeof source !== 'object') return {} as U;
      const out: Record<string, unknown> = { ...(source as Record<string, unknown>) };
      for (const [name, factory] of Object.entries(fields)) {
        if (name in out) out[name] = factory(out[name]);
      }
      return out as U;
    };
  },
  /**
   * Mirror of the real runtime's `Events` export — a mutable map patched at
   * generation time. Tests don't drive it, but it needs to exist so modules
   * that reference `Create.Events[...]` don't throw at import.
   */
  Events: {} as Record<string, (source: unknown) => unknown>,
};

export const Events = {
  /**
   * Register a handler for a named event. Returns an unsubscribe function
   * matching the real runtime's contract.
   */
  On(name: string, handler: Handler): () => void {
    let set = listeners.get(name);
    if (!set) {
      set = new Set();
      listeners.set(name, set);
    }
    set.add(handler);
    return () => {
      const current = listeners.get(name);
      if (!current) return;
      current.delete(handler);
      if (current.size === 0) listeners.delete(name);
    };
  },

  /**
   * Emit an event into the mock bus. Not something the real runtime exposes
   * this way, but tests synthesise events through `emitWailsEvent()`.
   */
  Emit(_event: { name: string; data: unknown }): void {
    // No-op in tests unless a specific suite wants to mock-track emits.
  },
};

/**
 * Synchronously invoke every registered handler for `name`. Use this from
 * tests to drive the event router as if a real provider event arrived.
 */
export function emitWailsEvent(name: string, data: unknown): void {
  const set = listeners.get(name);
  if (!set) return;
  // Copy to avoid mutation-during-iteration if handlers unsubscribe.
  for (const handler of [...set]) {
    handler({ name, data });
  }
}

/**
 * Count of handlers currently attached to `name`. Lets tests assert that
 * cleanup functions actually unsubscribe.
 */
export function wailsListenerCount(name: string): number {
  return listeners.get(name)?.size ?? 0;
}

/**
 * Reset all listeners between tests so state doesn't leak.
 */
export function resetWailsMocks(): void {
  listeners.clear();
}
