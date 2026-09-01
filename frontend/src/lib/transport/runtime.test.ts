// runtime.ts is a thin shim over wsClient. These tests verify the
// translation layer — Call.* delegating to the right wsClient method,
// Events.On wrapping data in the {name, data} shape, the Create.*
// factories applying the right per-field shaping. The transport-level
// behaviour (reconnect, replay, gap dispatch) is covered in
// wsClient.test.ts; we don't repeat it here.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// vi.mock is hoisted above the imports of the file under test, so the
// mock factory cannot close over a top-level const declared in the same
// file. vi.hoisted() lifts the construction alongside the mock so the
// factory can safely read it.
const { mockClient } = vi.hoisted(() => ({
  mockClient: {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn<(channel: string, handler: (data: unknown) => void) => () => void>(),
    installStepUpProver: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    close: vi.fn(),
  },
}));

vi.mock('./wsClient', () => ({
  wsClient: mockClient,
  // The runtime shim doesn't import these, but other tests in the same
  // file might — re-export sensible stubs so the resolver doesn't
  // complain.
  DisconnectedError: class extends Error {},
  TransportError: class extends Error {},
  transportGapChannel: 'transport:gap',
  createWSClient: vi.fn(),
  WSClient: vi.fn(),
}));

import { Call, CancellablePromise, Create, Events } from './runtime';
import { __setHomeClientForTest } from './backends';
import { setBackendIdentityFromBootstrap } from './backendIdentity';

// `src/test/setup.ts` loads the real `wsClient` before this file's
// `vi.mock` registers, so the registry's home entry captured it. Point it
// at the fake instead — the seam exists for exactly this ordering.
__setHomeClientForTest(mockClient as unknown as Parameters<typeof __setHomeClientForTest>[0]);
import { CancellablePromise as MockCancellablePromise } from '../../test/mocks/wailsio-runtime';

beforeEach(() => {
  mockClient.callByID.mockReset();
  mockClient.callByName.mockReset();
  mockClient.subscribe.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('Call', () => {
  it('ByID delegates to wsClient.callByID and resolves with the result', async () => {
    mockClient.callByID.mockResolvedValueOnce('hello');
    const promise = Call.ByID(42, 'a', 'b');
    expect(promise).toBeInstanceOf(CancellablePromise);
    await expect(promise).resolves.toBe('hello');
    expect(mockClient.callByID).toHaveBeenCalledWith(42, ['a', 'b']);
  });

  it('ByName delegates to wsClient.callByName and rejects on transport error', async () => {
    const err = new Error('boom');
    mockClient.callByName.mockRejectedValueOnce(err);
    const promise = Call.ByName('Foo.Bar', 1);
    expect(promise).toBeInstanceOf(CancellablePromise);
    await expect(promise).rejects.toBe(err);
    expect(mockClient.callByName).toHaveBeenCalledWith('Foo.Bar', [1]);
  });
});

describe('Events.On', () => {
  function captureSubscription(): { deliver: (data: unknown) => void; unsubscribe: () => void } {
    let captured: ((data: unknown) => void) | null = null;
    const unsubscribe = vi.fn();
    mockClient.subscribe.mockImplementation((_channel, handler) => {
      captured = handler;
      return unsubscribe;
    });
    return {
      deliver: (data: unknown) => {
        if (!captured) throw new Error('nothing subscribed');
        captured(data);
      },
      unsubscribe,
    };
  }

  it('subscribes via the transport and invokes the handler with {name, data, origin}', () => {
    const subscription = captureSubscription();

    const handler = vi.fn();
    const off = Events.On('thread:updated', handler);
    expect(mockClient.subscribe).toHaveBeenCalledWith('thread:updated', expect.any(Function));

    subscription.deliver({ id: 'thread-a' });
    expect(handler).toHaveBeenCalledWith({
      name: 'thread:updated',
      data: { id: 'thread-a' },
      // No manifest identity in this test: unknown origin, not a wildcard.
      origin: { backendId: '' },
    });

    off();
    expect(subscription.unsubscribe).toHaveBeenCalled();
  });

  it('stamps each event with the backend the connection identified as', () => {
    setBackendIdentityFromBootstrap('62c8a1de-0a3f-4f4b-9d0a-2b6b1a5b0f11', 'gen-1');
    const subscription = captureSubscription();

    const handler = vi.fn<(ev: { origin?: { backendId: string } }) => void>();
    Events.On('thread:updated', handler);
    subscription.deliver({ id: 'thread-a' });

    expect(handler.mock.calls[0]?.[0].origin).toEqual({
      backendId: '62c8a1de-0a3f-4f4b-9d0a-2b6b1a5b0f11',
    });
  });

  it('hands every event of one connection the same origin object', () => {
    setBackendIdentityFromBootstrap('62c8a1de-0a3f-4f4b-9d0a-2b6b1a5b0f11', 'gen-1');
    const subscription = captureSubscription();

    const handler = vi.fn<(ev: { origin?: { backendId: string } }) => void>();
    Events.On('thread:updated', handler);
    subscription.deliver({ id: 'a' });
    subscription.deliver({ id: 'b' });

    // Stamping is a property write, not an allocation: a streaming
    // channel must not mint an origin object per frame.
    expect(handler.mock.calls[0]?.[0].origin).toBe(handler.mock.calls[1]?.[0].origin);
  });
});

describe('Events.Emit', () => {
  it('is a no-op (transport does not push events from client)', () => {
    expect(() => Events.Emit({ name: 'foo', data: 1 })).not.toThrow();
  });
});

describe('Create.Array', () => {
  it('maps each element through the factory', () => {
    const factory = (s: unknown) => Number(s) * 2;
    const wrap = Create.Array(factory);
    expect(wrap([1, 2, 3])).toEqual([2, 4, 6]);
    // Non-array input degrades to an empty array — matches the test
    // mock's identity-degrade behavior.
    expect(wrap(null)).toEqual([]);
  });
});

describe('Create.Struct', () => {
  it('applies each field factory to the named field', () => {
    const factory = Create.Struct({
      count: (s) => Number(s),
      name: (s) => `[${String(s)}]`,
    });
    const out = factory<{ count: number; name: string; raw: string }>(
      { count: '5', name: 'foo', raw: 'untouched' },
    );
    expect(out).toEqual({ count: 5, name: '[foo]', raw: 'untouched' });
  });

  it('returns an empty object for non-object input', () => {
    const factory = Create.Struct({ x: (s) => s });
    expect(factory(null)).toEqual({});
  });
});

describe('Create.Map', () => {
  it('applies the value factory to each entry', () => {
    const map = Create.Map((k) => String(k), (v) => Number(v) + 1);
    expect(map({ a: 1, b: 2 })).toEqual({ a: 2, b: 3 });
  });
});

describe('Create.Nullable', () => {
  it('passes null through and applies the factory otherwise', () => {
    const fn = Create.Nullable((s) => `[${String(s)}]`);
    expect(fn(null)).toBeNull();
    expect(fn(undefined)).toBeNull();
    expect(fn('x')).toBe('[x]');
  });
});

describe('Create.Any / Create.ByteSlice', () => {
  it('Any returns the source unchanged', () => {
    const obj = { x: 1 };
    expect(Create.Any(obj)).toBe(obj);
  });
  it('ByteSlice returns the string or empty string', () => {
    expect(Create.ByteSlice('hello')).toBe('hello');
    expect(Create.ByteSlice(null)).toBe('');
    expect(Create.ByteSlice(123)).toBe('');
  });
});

describe('CancellablePromise', () => {
  it('cancel() returns a resolved CancellablePromise', async () => {
    const p = new CancellablePromise<string>((resolve) => resolve('x'));
    const result = p.cancel();
    expect(result).toBeInstanceOf(CancellablePromise);
    await expect(result).resolves.toBeUndefined();
  });

  it('cancelOn returns the same promise (no-op)', () => {
    const p = new CancellablePromise<string>((resolve) => resolve('x'));
    const ctrl = new AbortController();
    expect(p.cancelOn(ctrl.signal)).toBe(p);
  });

  it('static helpers wrap the underlying Promise.* calls', async () => {
    await expect(CancellablePromise.resolve(7)).resolves.toBe(7);
    await expect(CancellablePromise.reject(new Error('e'))).rejects.toThrow('e');
    await expect(CancellablePromise.all([Promise.resolve(1), Promise.resolve(2)])).resolves.toEqual([1, 2]);
    await expect(CancellablePromise.race([Promise.resolve('a'), Promise.resolve('b')])).resolves.toBe('a');
  });

  it('withResolvers exposes resolve/reject and a CancellablePromise', async () => {
    const { promise, resolve } = CancellablePromise.withResolvers<number>();
    expect(promise).toBeInstanceOf(CancellablePromise);
    resolve(42);
    await expect(promise).resolves.toBe(42);
  });

  // Parity guard: any static helper added to one of these classes MUST
  // land in both. The mock at src/test/mocks/wailsio-runtime.ts and the
  // production shim at lib/transport/runtime.ts are alternate
  // implementations of the same `@wailsio/runtime` surface — drift here
  // means tests pass with shapes production won't accept (or vice
  // versa). Filter out the always-present built-ins so the assertion is
  // about CancellablePromise's own surface.
  it('mock and production CancellablePromise share static surface', () => {
    const builtIns = new Set(['length', 'name', 'prototype']);
    const filter = (s: string) => !builtIns.has(s);
    const prod = Object.getOwnPropertyNames(CancellablePromise).filter(filter).sort();
    const mock = Object.getOwnPropertyNames(MockCancellablePromise).filter(filter).sort();
    expect(mock).toEqual(prod);
  });

  // Static cancel() must resolve, not reject — generated bindings chain
  // .then() onto cancel() defensively, and a rejection there produces
  // unhandled-rejection noise in tests and (in production) hides the
  // genuine error path. See fix #15 in the Phase B review notes.
  it('static cancel() resolves to undefined rather than rejecting', async () => {
    await expect(CancellablePromise.cancel()).resolves.toBeUndefined();
    await expect(MockCancellablePromise.cancel()).resolves.toBeUndefined();
  });
});
