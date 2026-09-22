import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { moveDraftToComputer } from './draftProjectTransfer';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID, type StagedBackend } from '../../test/helpers/backends';
import { grantBackendScopes, resetToLocalPage, revokeBackendScopes } from '../../test/helpers/scopes';
import { makeThread } from '../../test/helpers/chat';
import { noteProject, noteThread, __resetEntityIndexForTest } from '../transport/entityIndex';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { takePinnedBackend } from '../transport/backends';
import type { TransportHello } from '../transport/wsClient';
import { getToasts, removeToast } from './toast.svelte';

const sourceID = '11111111-2222-4333-8444-555555555555';
const hello = (backendId: string): TransportHello => ({
  backendId, capabilities: ['draft.project-transfer.v1', 'draft.project-transfer.v2'], protocolVersion: 1,
  backendName: 'Test', serverTimeMs: 0, clockSkewMs: 0,
  bundleId: '', bundleVersion: '', minShellBuild: 0,
});
const source = makeThread({ id: 'draft', projectId: 'source' });
const project = { id: 'target', path: '/target', name: 'Target', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
const target = { ...source, id: 'moved', projectId: project.id };
const snapshot = { content: 'prompt', attachmentIds: [], terminalChips: [], sourceProposedPlan: null };
let operation = '';
let staged: Record<string, StagedBackend>;

beforeEach(async () => {
  resetBindingMocks();
  resetStagedBackends();
  resetToLocalPage();
  __resetEntityIndexForTest();
  staged = {
    tower: stageBackend({ id: 'tower', backendId: sourceID, hello: hello(sourceID) }),
    laptop: stageBackend({ hello: hello(REMOTE_BACKEND_UUID) }),
  };
  setBackendIdentityFromBootstrap(sourceID, 'a', 'Tower', 'tower');
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'b', 'Laptop', 'laptop');
  for (const id of ['tower', 'laptop']) await grantBackendScopes(id, ['threads:read', 'threads:operate']);
  noteThread(source.id, 'tower');
  noteProject(project.id, 'laptop');
  setBindingMock('BeginDraftProjectTransfer', async (_thread: string, id: string) => {
    takePinnedBackend();
    operation = id;
    return { operationId: id };
  });
  setBindingMock('CreateThreadTransferOffer', async () => { takePinnedBackend(); return {}; });
  setBindingMock('BindThreadTransferDestination', async () => { takePinnedBackend(); return {}; });
  setBindingMock('GetThread', async () => { takePinnedBackend(); return target; });
});
afterEach(() => {
  resetStagedBackends();
  for (const id of ['tower', 'laptop']) revokeBackendScopes(id);
  resetToLocalPage();
  for (const toast of getToasts()) removeToast(toast.id);
});

it('follows the exact operation even when it is absent from recent history', async () => {
  const recent = setBindingMock('GetThreadTransfers', async () => []);
  const status = setBindingMock('GetThreadTransferStatus', async (thread: string, id: string) => {
    expect(takePinnedBackend()).toBe('tower');
    expect([thread, id]).toEqual([source.id, operation]);
    return { phase: 'complete', targetThreadId: target.id };
  });
  expect(await moveDraftToComputer(source, project, snapshot)).toEqual(target);
  expect(status).toHaveBeenCalledOnce();
  expect(recent).not.toHaveBeenCalled();
});

it('opens a completed move despite retryable cleanup and subsequent destination edits', async () => {
  setBindingMock('GetThreadTransferStatus', async () => {
    takePinnedBackend();
    return { phase: 'complete', targetThreadId: target.id, error: 'source files unavailable' };
  });
  const draft = setBindingMock('GetDraft', async () => ({ ...snapshot, content: 'newer destination text' }));
  expect(await moveDraftToComputer(source, project, snapshot)).toEqual(target);
  expect(draft).not.toHaveBeenCalled();
  expect(getToasts()).toEqual([expect.objectContaining({
    type: 'warning', message: expect.stringContaining('Source files unavailable'),
  })]);
});

it('reports a failed or canceled operation without opening the destination', async () => {
  const get = setBindingMock('GetThread', vi.fn());
  setBindingMock('GetThreadTransferStatus', async () => { takePinnedBackend(); return { phase: 'canceled' }; });
  await expect(moveDraftToComputer(source, project, snapshot)).rejects.toThrow('canceled');
  expect(get).not.toHaveBeenCalled();
});

it.each(['tower', 'laptop'])('refuses an older %s before accepting any transfer work', async (backend) => {
  staged[backend].setHello({ ...hello(backend === 'tower' ? sourceID : REMOTE_BACKEND_UUID), capabilities: ['draft.project-transfer.v1'] });
  const begin = setBindingMock('BeginDraftProjectTransfer', vi.fn());
  await expect(moveDraftToComputer(source, project, snapshot)).rejects.toThrow('Update both computers');
  expect(begin).not.toHaveBeenCalled();
});
