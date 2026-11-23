import { expect, it, vi } from 'vitest';

type TransportStatus = typeof import('../lib/stores/transportStatus.svelte');
let loadedDuringTest: TransportStatus | null = null;

// The tests run in order: the first loads a module, the second reads it.
it('a module first loads during a test', async () => {
  vi.resetModules();
  loadedDuringTest = await import('../lib/stores/transportStatus.svelte');
  loadedDuringTest.__setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
});

it('gets the test posture before the next test', () => {
  expect(loadedDuringTest?.getTransportStatus().status).toBe('connected');
});
