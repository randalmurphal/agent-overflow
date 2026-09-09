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
import { getActiveTurn, getThreadStatus, projectTurnStarted } from './threadStatuses.svelte';
import { hasScope } from '../transport/scopes';
import type { TransportHello } from '../transport/wsClient';

vi.mock('../transport/entityScopes', async (original) => ({ ...await original<object>(), threadHasScope: () => true }));

vi.mock('./eventsThreadRows', () => ({ refreshSidebarProjections: vi.fn() }));
vi.mock('./settings.svelte', async (original) => ({ ...await original<object>(), mirrorFrontendPreferences: vi.fn(), loadSettings: vi.fn() }));
vi.mock('./workflowRuns.svelte', () => ({ refreshWorkflowRunsSoon: vi.fn(), isWorkflowOverlayLoaded: () => false, resyncWorkflowEngineState: vi.fn() }));
vi.mock('../transport/scopes', async (original) => ({ ...await original<object>(), hasScope: vi.fn(() => false) }));

let stop: (() => void) | undefined;
beforeEach(() => { resetStagedBackends(); vi.clearAllMocks(); vi.mocked(hasScope).mockReturnValue(false); });
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

it('reconciles every thread of a computer with threads:read, panes or not', async () => {
  vi.mocked(hasScope).mockImplementation((scope) => scope === 'threads:read');
  setBindingMock('GetRateLimitsSnapshots', async () => []);
  // A row with no pane: the turn this client saw start ended while it was
  // away, and only the computer's snapshot can say so.
  noteThread('gpu-ended', 'gpu');
  projectTurnStarted('gpu-ended', 'round-1', 1, 10);
  const activity = setBindingMock('ListThreadLiveActivity', async () => [
    { threadId: 'gpu-started', activeTurn: { threadId: 'gpu-started', turnId: 'round-2', turnIndex: 2, startedAt: 20 }, approvalRequestIds: [], userInputRequestIds: [] },
  ]);
  const gpu = stageBackend({ id: 'gpu', status: 'reconnecting', hello: { backendId: 'gpu' } as TransportHello });
  stop = installComputerHydration();
  await Promise.resolve();
  expect(activity).not.toHaveBeenCalled();
  gpu.setStatus('connected');
  await vi.waitFor(() => expect(getThreadStatus('gpu-started')).toBe('running'));
  expect(activity).toHaveBeenCalledTimes(1);
  expect(getActiveTurn('gpu-ended')).toBeNull();
});
