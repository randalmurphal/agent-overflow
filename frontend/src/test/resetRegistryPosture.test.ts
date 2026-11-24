import { beforeAll, expect, it, vi } from 'vitest';

// The posture of config/testResets.ts holds for a module however it first
// loads: in a beforeAll hook, at its first import inside a test, when a
// later test reuses it, and for the fresh instance an import after
// vi.resetModules() makes.
type TransportStatus = typeof import('../lib/stores/transportStatus.svelte');
type Scopes = typeof import('../lib/transport/scopes');
let first: TransportStatus | null = null;
let hostInBeforeAll: boolean | Error = new Error('beforeAll did not run');

beforeAll(async () => {
  vi.resetModules();
  const scopes: Scopes = await import('../lib/transport/scopes');
  try {
    hostInBeforeAll = scopes.hasScope('host');
  } catch (error) {
    hostInBeforeAll = error as Error;
  }
});

it('applies to a module first imported in a beforeAll hook, during the hook', () => {
  expect(hostInBeforeAll).toBe(true);
});

// The tests run in order: each reads what the one before it left.
it('applies to a module first imported during the test', async () => {
  vi.resetModules();
  first = await import('../lib/stores/transportStatus.svelte');
  expect(first.getTransportStatus().status).toBe('connected');
  first.__setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
});

it('applies again to the module the next test reuses', () => {
  expect(first!.getTransportStatus().status).toBe('connected');
});

it('applies to the fresh instance an import after vi.resetModules() makes', async () => {
  first!.__setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
  vi.resetModules();
  const fresh = await import('../lib/stores/transportStatus.svelte');
  expect(fresh).not.toBe(first);
  expect(fresh.getTransportStatus().status).toBe('connected');
});

it('applies to a module whose step reads a module of its own import cycle', async () => {
  vi.resetModules();
  const scopes: Scopes = await import('../lib/transport/scopes');
  expect(scopes.hasScope('host')).toBe(true);
});

it('applies once the cycle loads, when the module that completes it has nothing to register', async () => {
  vi.resetModules();
  // deviceSession.ts registers nothing, and it completes the cycle that
  // scopes.ts's step reads.
  await import('../lib/transport/deviceSession');
  const scopes: Scopes = await import('../lib/transport/scopes');
  expect(scopes.hasScope('host')).toBe(true);
});
