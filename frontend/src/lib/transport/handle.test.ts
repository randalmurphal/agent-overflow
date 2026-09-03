// The transport seam: what `resolveTransport()` answers with, and what
// that handle promises about the connection it stands for. The RPC and
// event paths that ride it are covered in runtime.test.ts.
import { describe, expect, it, vi } from 'vitest';

const { mockClient } = vi.hoisted(() => ({
  mockClient: {
    callByID: vi.fn<(id: number, args: unknown[]) => Promise<unknown>>(),
    callByName: vi.fn<(name: string, args: unknown[]) => Promise<unknown>>(),
    subscribe: vi.fn<(channel: string, handler: (data: unknown) => void) => () => void>(),
    installStepUpProver: vi.fn(),
    setWatchedThreads: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onStatusChange: vi.fn(() => () => undefined),
    close: vi.fn(),
  },
}));

vi.mock('./wsClient', () => ({
  wsClient: mockClient,
  DisconnectedError: class extends Error {},
  TransportError: class extends Error {},
  transportGapChannel: 'transport:gap',
  createWSClient: vi.fn(),
  WSClient: vi.fn(),
}));

import { resolveTransport } from './handle';
import { __setHomeClientForTest } from './backends';
import { setBackendIdentityFromBootstrap } from './backendIdentity';

// `src/test/setup.ts` loads the real `wsClient` before this file's
// `vi.mock` registers, so the registry's home entry captured it. Point it
// at the fake instead — the seam exists for exactly this ordering.
__setHomeClientForTest(mockClient as unknown as Parameters<typeof __setHomeClientForTest>[0]);

const BACKEND_A = '62c8a1de-0a3f-4f4b-9d0a-2b6b1a5b0f11';
const BACKEND_B = 'f0f7b0c4-6d0e-4a4a-8a3f-9c2f1a7d4e55';

describe('resolveTransport', () => {
  it('resolves the one attached connection, not a fresh handle per call', () => {
    expect(resolveTransport()).toBe(resolveTransport());
  });

  it('forwards calls and subscriptions to that connection', async () => {
    mockClient.callByID.mockResolvedValueOnce('by-id');
    mockClient.callByName.mockResolvedValueOnce('by-name');
    const unsubscribe = vi.fn();
    mockClient.subscribe.mockReturnValueOnce(unsubscribe);
    const transport = resolveTransport();

    await expect(transport.callByID(7, ['a'])).resolves.toBe('by-id');
    await expect(transport.callByName('App.Thing', [1])).resolves.toBe('by-name');
    const handler = vi.fn();
    expect(transport.subscribe('thread:updated', handler)).toBe(unsubscribe);

    expect(mockClient.callByID).toHaveBeenCalledWith(7, ['a']);
    expect(mockClient.callByName).toHaveBeenCalledWith('App.Thing', [1]);
    expect(mockClient.subscribe).toHaveBeenCalledWith('thread:updated', handler);
  });

  it('reports an unknown origin until the backend identifies itself', () => {
    expect(resolveTransport().origin).toEqual({ backendId: '' });
  });

  it('follows the backend identity, and reuses the origin until it moves', () => {
    setBackendIdentityFromBootstrap(BACKEND_A, 'gen-1');
    const first = resolveTransport().origin;
    expect(first).toEqual({ backendId: BACKEND_A });
    // A generation re-mint is not a different connection.
    setBackendIdentityFromBootstrap(BACKEND_A, 'gen-2');
    expect(resolveTransport().origin).toBe(first);

    setBackendIdentityFromBootstrap(BACKEND_B, 'gen-1');
    const second = resolveTransport().origin;
    expect(second).not.toBe(first);
    expect(second).toEqual({ backendId: BACKEND_B });
  });
});
