// Fake implementation of `@wailsio/runtime` for tests.
//
// The real module ships a Wails-specific IPC channel that only works inside a
// Wails webview. Tests run in happy-dom and never initialise that channel, so
// we replace it with a synchronous pub-sub registry that tests can drive
// directly via `emitWailsEvent()`.
//
// The Phase B production shim (`/lib/transport/runtime.ts`) exposes the same
// surface against a localhost WS transport; this mock mirrors that surface
// so tests using the generated bindings see the same module contract whether
// they bypass the WS or not.
//
// Parity rule: any change to the production shim's CancellablePromise /
// Call / Create / Events surface MUST land in this mock at the same time.
// `runtime.test.ts` enforces the static-helper surface match so accidental
// drift fails CI rather than only showing up in production.

import { __bindingMocksInternal } from './bindings-app';

/**
 * Stub of the real runtime's CancellablePromise. Mirrors every static
 * helper the production shim exposes so tests that exercise the runtime
 * surface — and the parity assertion in `runtime.test.ts` — stay in sync.
 * Only `.then()/.catch()` and `cancel()` are exercised by most tests; the
 * static helpers exist primarily to keep the surface congruent with
 * `lib/transport/runtime.ts`.
 */
export class CancellablePromise<T> extends Promise<T> {
  static get [Symbol.species](): typeof Promise {
    return Promise;
  }
  cancel(_cause?: unknown): CancellablePromise<void> {
    return CancellablePromise.resolve();
  }
  cancelOn(_signal: AbortSignal): CancellablePromise<T> {
    return this;
  }
  static resolve(): CancellablePromise<void>;
  static resolve<U>(value: U | PromiseLike<U>): CancellablePromise<Awaited<U>>;
  static resolve<U>(value?: U | PromiseLike<U>): CancellablePromise<Awaited<U> | void> {
    return wrap(Promise.resolve(value as U | PromiseLike<U> | undefined)) as CancellablePromise<
      Awaited<U> | void
    >;
  }
  static reject<U = never>(reason?: unknown): CancellablePromise<U> {
    return wrap(Promise.reject(reason)) as CancellablePromise<U>;
  }
  static all<U>(values: Iterable<U | PromiseLike<U>>): CancellablePromise<Awaited<U>[]> {
    return wrap(Promise.all(values)) as CancellablePromise<Awaited<U>[]>;
  }
  static allSettled<U>(
    values: Iterable<U | PromiseLike<U>>,
  ): CancellablePromise<PromiseSettledResult<Awaited<U>>[]> {
    return wrap(Promise.allSettled(values)) as CancellablePromise<
      PromiseSettledResult<Awaited<U>>[]
    >;
  }
  static any<U>(values: Iterable<U | PromiseLike<U>>): CancellablePromise<Awaited<U>> {
    return wrap(Promise.any(values)) as CancellablePromise<Awaited<U>>;
  }
  static race<U>(values: Iterable<U | PromiseLike<U>>): CancellablePromise<Awaited<U>> {
    return wrap(Promise.race(values)) as CancellablePromise<Awaited<U>>;
  }
  static cancel<U = never>(_cause?: unknown): CancellablePromise<U> {
    // Mirrors the production shim: resolve to undefined so generated
    // bindings that .then()-chain off cancel() never produce unhandled
    // rejections in test runs.
    return CancellablePromise.resolve() as CancellablePromise<U>;
  }
  static sleep<U = void>(milliseconds: number, value?: U): CancellablePromise<U> {
    return wrap(
      new Promise<U>((resolve) => {
        setTimeout(() => resolve(value as U), milliseconds);
      }),
    ) as CancellablePromise<U>;
  }
  static timeout<U = never>(milliseconds: number, cause?: unknown): CancellablePromise<U> {
    return wrap(
      new Promise<U>((_, reject) => {
        setTimeout(() => reject(cause ?? new Error('timeout')), milliseconds);
      }),
    ) as CancellablePromise<U>;
  }
  static withResolvers<U>(): {
    promise: CancellablePromise<U>;
    resolve: (value: U | PromiseLike<U>) => void;
    reject: (reason?: unknown) => void;
  } {
    let resolve!: (value: U | PromiseLike<U>) => void;
    let reject!: (reason?: unknown) => void;
    const promise = new CancellablePromise<U>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }
}

// wrap takes a plain Promise and returns it as a CancellablePromise
// without re-running the executor. Mirrors the production shim's helper.
function wrap<T>(p: Promise<T>): CancellablePromise<T> {
  return new CancellablePromise<T>((resolve, reject) => {
    p.then(resolve, reject);
  });
}

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
 *
 * Phase C made the production wire deliver raw payloads — frames carry
 * `data` directly with `seq` stamped on the wrapping frame, not on the
 * payload. The mock matches: `data` is forwarded verbatim. Wrapping
 * provider channels in `{seq, data}` here would hide a regression if
 * the production emitter accidentally re-introduced an envelope.
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
