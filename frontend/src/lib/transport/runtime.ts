// Production shim that replaces `@wailsio/runtime` at build time. The
// vite alias in vite.config.ts points every import of @wailsio/runtime
// here so the generated bindings (which `import { Call, Create,
// CancellablePromise } from '@wailsio/runtime'`) keep working without
// regeneration.
//
// Surface shape mirrors the test mock at
// src/test/mocks/wailsio-runtime.ts so the same generated code that
// passes type-check under the test alias also type-checks here.
//
// The actual transport — opening the WS, dispatching frames, reconciling
// reconnects — lives in ./wsClient.ts, and this file reaches it through
// ./handle.ts rather than importing that singleton: the connection a call
// routes over is resolved per call, which is the seam a second attached
// backend needs (docs/specs/remote-access.md §10). This file stays thin
// glue exposing a Wails-shaped API on top of whichever handle answers.

import { resolveTransport, type EventOrigin } from './handle';

// CancellablePromise is the wrapper Wails-generated bindings always
// return. The real runtime ships a complex implementation (see
// node_modules/@wailsio/runtime/types/cancellable.d.ts); for our use
// generated bindings only `.then()/.catch()` the result, so a thin
// Promise subclass with a no-op cancel suffices.
//
// The `@@species` getter is set to Promise so chaining methods (then,
// catch, finally) return plain Promises rather than CancellablePromises.
// Without this Svelte's reactivity primitives interact poorly with the
// subclass — Promise subclassing is famously fragile and most consumers
// only ever care about the first .then() boundary.
export class CancellablePromise<T> extends Promise<T> {
  static get [Symbol.species](): typeof Promise {
    return Promise;
  }

  // cancel resolves to undefined immediately. The real runtime would
  // signal an underlying operation to abort; the WS transport doesn't
  // expose per-RPC cancellation today (Phase E may add it). Returning a
  // resolved CancellablePromise keeps callers that `await cancel()`
  // working without changes.
  cancel(_cause?: unknown): CancellablePromise<void> {
    return CancellablePromise.resolve();
  }

  // cancelOn is a no-op binding to an AbortSignal. We return `this` so
  // method chains compose; the signal is ignored because the underlying
  // RPC has no cancellation channel yet.
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
    // No work in flight under this shim. The real runtime resolves with
    // a "cancelled" sentinel; generated bindings call this defensively
    // and frequently chain `.then()` onto the result. Returning a
    // pre-rejected promise produces unhandled rejections in those chains,
    // so we resolve to undefined — the call sites that care about cancel
    // semantics only run inside the real Wails runtime.
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
// without re-running the executor. Promise subclassing prefers
// .resolve, but we already have the underlying value — the cleanest
// path is to construct the subclass and forward.
function wrap<T>(p: Promise<T>): CancellablePromise<T> {
  return new CancellablePromise<T>((resolve, reject) => {
    p.then(resolve, reject);
  });
}

// Call.ByID / Call.ByName route through the resolved transport handle.
// Generated bindings hit ByID exclusively; hand-written code paths (none
// today) can use ByName.
export const Call = {
  ByID(methodId: number, ...args: unknown[]): CancellablePromise<unknown> {
    return wrap(resolveTransport().callByID(methodId, args));
  },
  ByName(method: string, ...args: unknown[]): CancellablePromise<unknown> {
    return wrap(resolveTransport().callByName(method, args));
  },
};

// Create.* are identity-like factories the binding generator emits.
// The real Wails runtime uses these to build typed payload converters
// (Create.Array(SomeStruct.createFrom), Create.Map, Create.Struct).
// Our wire data is already JSON — the generator's factories operate
// shallowly on plain objects, so the production shim mirrors the test
// mock's behaviour exactly.
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
  // Mirror of the real runtime's Events export — a mutable map the
  // binding generator monkey-patches at module load. Tests don't drive
  // it; production doesn't either, but it has to exist so generated
  // modules that reference Create.Events[...] don't throw on import.
  Events: {} as Record<string, (source: unknown) => unknown>,
};

// Events.On registers a subscriber on the resolved transport handle. The
// handler receives `{name, data}` to match Wails' real runtime contract —
// the events.ts store and other consumers expect the wrapped shape — plus
// `origin`, the connection the event arrived on.
//
// The handle is resolved ONCE, at subscribe time, because it is the
// connection: the subscription belongs to it, and so does the origin its
// events carry. Stamping is free — the handle hands back the same origin
// object until the identity moves, so this adds a property to an envelope
// that was already being allocated per event.
export const Events = {
  On(
    name: string,
    handler: (ev: { name: string; data: unknown; origin?: EventOrigin }) => void,
  ): () => void {
    const transport = resolveTransport();
    return transport.subscribe(name, (data) => {
      handler({ name, data, origin: transport.origin });
    });
  },
  Emit(_event: { name: string; data: unknown }): void {
    // The Phase B client never pushes events; this is here for API
    // compatibility with the @wailsio/runtime surface. If a code path
    // ever needs to fire a server-side event, it should go through a
    // bound RPC method rather than re-purposing Events.Emit.
  },
};
