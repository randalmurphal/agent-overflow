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
//
// Phase 7 makes "whichever handle" a decision rather than a constant, and
// the decision is the METHOD's rather than the caller's: ./methodRoutes.ts
// (generated from the Go method table) says whether a call follows its
// thread, its project, the composer's chosen backend, the page's own, or
// every attached backend at once. `Call.ByID` reads that table, resolves
// the handle, and dispatches — the generated bindings above it are
// untouched and unregenerated.
//
// Two rules hold the fallback honest. An unknown method id, an
// unresolvable entity and a `selected` backend that has detached ALL
// resolve home, because a single-backend app must behave exactly as it did
// and a throw here would be a blank screen for a table that is merely
// incomplete. And the fallback is announced once per method id in dev, so
// a route that is genuinely missing is visible to whoever added the
// method rather than silently correct on the only machine they test on.

import { resolveTransport, type EventOrigin } from './handle';
import {
  attachedBackendCount,
  callEveryBackend,
  homeBackend,
  subscribeEveryBackend,
  takePinnedBackend,
} from './backends';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import {
  noteFamilyRowsFromCall,
  noteRowsFromCall,
  projectBackend,
  threadBackend,
} from './entityIndex';
import { METHOD_ROUTES, type MethodRoute } from './methodRoutes';
import { familyBackend } from './methodFamilies';
import { selectedBackend } from '../stores/selectedBackend.svelte';

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

// Method ids whose missing / unresolvable route has already been reported.
// Bounded by the method table, and only ever written in dev.
const warnedRoutes = new Set<number>();

function warnOnceForMethod(methodId: number, why: string): void {
  if (!import.meta.env.DEV) return;
  if (warnedRoutes.has(methodId)) return;
  warnedRoutes.add(methodId);
  console.warn(`transport: method ${methodId} ${why}; routed to the home backend`);
}

/**
 * Which backend a call goes to, from its route and its arguments.
 *
 * Returns `null` for the `all` route, which is not one backend. Every
 * other answer is a registry id, `HOME_BACKEND` included; no allocation
 * beyond the two Map lookups the entity index costs.
 */
function resolveRoute(methodId: number, args: unknown[]): BackendKey | null {
  // The ID-FAMILY table first. 59 methods are keyed by an id that is
  // neither a thread nor a project — a workflow item, an automation, a
  // terminal, a subscription — and the generated table parks all of them
  // on `home` because it has no vocabulary to infer them. Home is the
  // right fallback but the wrong answer once one of them lives on a second
  // machine, so an id the index KNOWS wins over the parked route; an id it
  // does not know falls through and lands where it always did.
  const owned = familyBackend(methodId, args);
  if (owned !== undefined) return owned;
  const route: MethodRoute | undefined = METHOD_ROUTES[methodId];
  if (route === undefined) {
    warnOnceForMethod(methodId, 'has no route');
    return HOME_BACKEND;
  }
  switch (route) {
    case 'thread': {
      const id = args[0];
      const owner = typeof id === 'string' ? threadBackend(id) : undefined;
      return owner ?? HOME_BACKEND;
    }
    case 'project': {
      const id = args[0];
      const owner = typeof id === 'string' ? projectBackend(id) : undefined;
      return owner ?? HOME_BACKEND;
    }
    case 'workspace': {
      // A WorkspaceRef: the project id inside it names the machine. The
      // zero ref (projectId '', the PR-review RPCs' "no local clone")
      // resolves home, which is the forge-API path's only sensible home.
      const ref = args[0];
      const id = ref !== null && typeof ref === 'object' ? (ref as { projectId?: unknown }).projectId : undefined;
      const owner = typeof id === 'string' ? projectBackend(id) : undefined;
      return owner ?? HOME_BACKEND;
    }
    case 'selected':
      return selectedBackend();
    case 'all':
      return null;
    case 'home':
    default:
      return HOME_BACKEND;
  }
}

// Call.ByID / Call.ByName route through the resolved transport handle.
// Generated bindings hit ByID exclusively; hand-written code paths (none
// today) can use ByName.
export const Call = {
  ByID(methodId: number, ...args: unknown[]): CancellablePromise<unknown> {
    // Drained FIRST, before anything can await: a pinned target is armed
    // for one synchronous dispatch and must not survive into the next.
    const pinned = takePinnedBackend();
    // The single-backend fast path, and the reason a client with one
    // connection pays nothing for the registry: there is no route to
    // resolve when there is only one place a call can go.
    if (attachedBackendCount() === 1) {
      return wrap(homeBackend().handle.callByID(methodId, args));
    }
    if (pinned !== null) return wrap(resolveTransport(pinned).callByID(methodId, args));
    const target = resolveRoute(methodId, args);
    if (target === null) {
      return wrap(
        callEveryBackend(methodId, args, (result, backendId) => {
          noteRowsFromCall(methodId, result, backendId);
        }),
      );
    }
    // Ids a routed call ANSWERS with are indexed too: a workflow item, a
    // terminal and a subscription are only ever learned from the call that
    // listed or minted them, and the next call about one has to know which
    // machine that was. Only on the multi-backend path — the fast path
    // above returns before any of this exists.
    return wrap(
      resolveTransport(target)
        .callByID(methodId, args)
        .then((result) => {
          noteFamilyRowsFromCall(methodId, result, target);
          return result;
        }),
    );
  },
  // ByName has no id to look up, so it takes the route every unclassified
  // call takes: the page's own backend. A PINNED target still wins, and
  // must — `withBackendTarget` is the caller saying which machine it means,
  // and a door that quietly dropped it would ask home about another
  // machine's ports and get a plausible answer. The pin is drained here
  // whether or not it is used, for the reason ByID drains it first: it is
  // armed for one synchronous dispatch and must not survive into the next.
  //
  // Anything reaching this door wants a route of its own rather than a
  // second name→route table beside the generated one; the pin is the
  // stopgap a hand-declared wrapper uses until its method is generated.
  ByName(method: string, ...args: unknown[]): CancellablePromise<unknown> {
    const pinned = takePinnedBackend();
    return wrap(resolveTransport(pinned ?? undefined).callByName(method, args));
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

// Events.On registers a subscriber on EVERY attached backend, and on every
// backend attached afterwards. The handler receives `{name, data}` to
// match Wails' real runtime contract — the events.ts store and other
// consumers expect the wrapped shape — plus `origin`, the connection the
// event arrived on.
//
// The origin comes from the DELIVERING handle rather than from one
// resolved at subscribe time, which is the whole difference between one
// backend and several: the stamp used to be a property of the app and is
// now a property of the delivery. Stamping stays free — each handle hands
// back the same origin object until its identity moves, so this adds a
// property to an envelope that was already being allocated per event.
export const Events = {
  On(
    name: string,
    handler: (ev: { name: string; data: unknown; origin?: EventOrigin }) => void,
  ): () => void {
    return subscribeEveryBackend(name, (data, transport) => {
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
