import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { installComputerHydration } from './computerHydration';
import { refreshSidebarProjections } from './eventsThreadRows';
import { mirrorFrontendPreferences } from './settings.svelte';
import { refreshWorkflowRunsSoon } from './workflowRuns.svelte';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetPanesForTest } from './panes.svelte';
import { noteThread } from '../transport/entityIndex';
import { getActiveTurn } from './threadStatuses.svelte';
import type { TransportHello } from '../transport/wsClient';

vi.mock('../transport/entityScopes', async (original) => ({ ...await original<object>(), threadHasScope: () => true }));

vi.mock('./eventsThreadRows', () => ({ refreshSidebarProjections: vi.fn() }));
vi.mock('./settings.svelte', async (original) => ({ ...await original<object>(), mirrorFrontendPreferences: vi.fn(), loadSettings: vi.fn() }));
vi.mock('./workflowRuns.svelte', () => ({ refreshWorkflowRunsSoon: vi.fn(), isWorkflowOverlayLoaded: () => false, resyncWorkflowEngineState: vi.fn() }));
vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: () => false }));

let stop: (() => void) | undefined;
beforeEach(() => { resetStagedBackends(); vi.clearAllMocks(); });
afterEach(() => { stop?.(); stop = undefined; resetStagedBackends(); resetPanesForTest(); });

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

it('restores provider activity on ordinary reconnect even when history loaded successfully', async () => {
  installThreadPaneTestEnv();
  await buildPane(makeThread({ id: 'remote-active' }));
  noteThread('remote-active', 'gpu');
  const gpu = stageBackend({ id: 'gpu', status: 'reconnecting', hello: { backendId: 'gpu' } as TransportHello });
  const read = vi.fn(async () => ({ threadId: 'remote-active', activeTurn: { threadId: 'remote-active', turnId: 'provider-running', turnIndex: 4, startedAt: 100 } }));
  setBindingMock('GetThreadLiveState', read);
  const history = vi.fn();
  setBindingMock('SyncThreadWindow', history);
  stop = installComputerHydration();
  await Promise.resolve();
  expect(getActiveTurn('remote-active')).toBeNull();
  gpu.setStatus('connected');
  await vi.waitFor(() => expect(getActiveTurn('remote-active')?.turnId).toBe('provider-running'));
  expect(read).toHaveBeenCalledTimes(1);
  expect(history).not.toHaveBeenCalled();
});
