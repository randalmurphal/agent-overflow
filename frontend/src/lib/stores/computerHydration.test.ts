import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { installComputerHydration } from './computerHydration';
import { refreshSidebarProjections } from './eventsThreadRows';
import { mirrorFrontendPreferences } from './settings.svelte';
import { refreshWorkflowRunsSoon } from './workflowRuns.svelte';
import type { TransportHello } from '../transport/wsClient';

vi.mock('./eventsThreadRows', () => ({ refreshSidebarProjections: vi.fn() }));
vi.mock('./settings.svelte', () => ({ mirrorFrontendPreferences: vi.fn(), loadSettings: vi.fn(), getSettings: vi.fn() }));
vi.mock('./workflowRuns.svelte', () => ({ refreshWorkflowRunsSoon: vi.fn(), isWorkflowOverlayLoaded: () => false, resyncWorkflowEngineState: vi.fn() }));
vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: () => false }));

let stop: (() => void) | undefined;
beforeEach(() => { resetStagedBackends(); vi.clearAllMocks(); });
afterEach(() => { stop?.(); stop = undefined; resetStagedBackends(); });

it('refreshes a computer when it reconnects with unchanged hello metadata', async () => {
  const gpu = stageBackend({ id: 'gpu' });
  gpu.setHello({ backendId: 'gpu' } as TransportHello);
  gpu.setStatus('reconnecting');
  stop = installComputerHydration();
  await Promise.resolve();
  expect(mirrorFrontendPreferences).not.toHaveBeenCalledWith('gpu');
  gpu.setStatus('connected');
  await Promise.resolve();
  expect(mirrorFrontendPreferences).toHaveBeenCalledWith('gpu');
  expect(refreshSidebarProjections).toHaveBeenCalledTimes(1);
  expect(refreshWorkflowRunsSoon).toHaveBeenCalledTimes(1);
  gpu.setStatus('reconnecting');
  gpu.setStatus('connected');
  await Promise.resolve();
  expect(refreshSidebarProjections).toHaveBeenCalledTimes(2);
});

it('waits for the initial hello, coalesces same-tick edges, and releases scheduled hydration on teardown', async () => {
  const gpu = stageBackend({ id: 'gpu' });
  stop = installComputerHydration();
  await Promise.resolve();
  expect(mirrorFrontendPreferences).not.toHaveBeenCalledWith('gpu');
  gpu.setHello({ backendId: 'gpu' } as TransportHello);
  gpu.setStatus('reconnecting');
  gpu.setStatus('connected');
  await Promise.resolve();
  expect(refreshSidebarProjections).toHaveBeenCalledTimes(1);
  gpu.setStatus('reconnecting');
  gpu.setStatus('connected');
  stop();
  await Promise.resolve();
  expect(refreshSidebarProjections).toHaveBeenCalledTimes(1);
});
