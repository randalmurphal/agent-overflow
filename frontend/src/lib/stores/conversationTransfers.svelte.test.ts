import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { grantBackendScopes, resetToLocalPage, revokeBackendScopes } from '../../test/helpers/scopes';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { noteProject, noteThread, __resetEntityIndexForTest } from '../transport/entityIndex';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { takePinnedBackend } from '../transport/backends';
import type { TransportHello } from '../transport/wsClient';
import { TransportError } from '../transport/wsClient';
import {
  computerTransfers, initConversationTransfers, openConversationTransfer, pendingConversationTransfer,
  recoverConversationTransfer, refreshComputerTransfers, resetConversationTransfersForTest,
  submitConversationTransfer, type ConversationTransfer,
} from './conversationTransfers.svelte';

vi.mock('./eventsThreadRows', () => ({ refreshSidebarProjections: vi.fn() }));
const SOURCE = '11111111-2222-4333-8444-555555555555';
const hello = (id: string) => ({ protocolVersion: 1, backendId: id, capabilities: ['conversation.transfer.v1'], backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 }) as TransportHello;
const thread = makeThread({ id: 'conversation', projectId: 'project-source' });
const row = (patch: Partial<ConversationTransfer> = {}): ConversationTransfer => ({ id: 'op-1', threadId: thread.id, targetThreadId: 'copy-id', peerBackendId: REMOTE_BACKEND_UUID, kind: 'copy', direction: 'outgoing', phase: 'preparing', needsDestination: true, ownershipEpoch: 0, createdAt: 1, updatedAt: 1, ...patch });
const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
let stop = () => {};

beforeEach(async () => {
  resetBindingMocks(); resetStagedBackends(); resetToLocalPage(); resetConversationTransfersForTest(); __resetEntityIndexForTest();
  stageBackend({ id: 'tower', backendId: SOURCE, hello: hello(SOURCE) });
  stageBackend({ hello: hello(REMOTE_BACKEND_UUID) });
  setBackendIdentityFromBootstrap(SOURCE, 'generation-a', 'Tower', 'tower');
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation-b', 'Laptop', 'laptop');
  for (const backend of ['tower', 'laptop']) await grantBackendScopes(backend, ['threads:read', 'threads:operate', 'git:operate']);
  noteThread(thread.id, 'tower', 0); noteProject('project-source', 'tower'); noteProject('project-target', 'laptop');
  setBindingMock('GetThreadTransfers', async () => { takePinnedBackend(); return []; });
});
afterEach(() => {
  stop(); stop = () => {}; resetConversationTransfersForTest(); resetStagedBackends();
  for (const backend of ['tower', 'laptop']) revokeBackendScopes(backend);
  resetToLocalPage();
});

function setupBindings() {
  const calls: string[] = [];
  const begin = setBindingMock('BeginThreadTransfer', async (threadId: string, operationId: string, destinationBackendId: string, kind: string, includeWorkspace: boolean) => {
    calls.push(`begin:${takePinnedBackend()}`);
    return { operationId, sourceBackendId: SOURCE, destinationBackendId, kind, includeWorkspace, sourceThreadId: threadId, targetThreadId: 'copy-id', provider: 'claude', runtimeMode: 'full', ownershipEpoch: 0, activationHash: 'hash' };
  });
  setBindingMock('BindThreadTransferDestination', async () => {
    calls.push(`bind:${takePinnedBackend()}`);
    return row({ id: pendingConversationTransfer()!.operationID, needsDestination: false });
  });
  return { calls, begin };
}

it('retries a lost offer with the same operation, copy mode and captured hosts', async () => {
  let lost = true;
  const { calls, begin } = setupBindings();
  setBindingMock('CreateThreadTransferOffer', async () => {
    calls.push(`offer:${takePinnedBackend()}`);
    if (lost) { lost = false; throw new Error('Connection lost'); }
    return { grant: 'private-grant' };
  });
  openConversationTransfer(thread);
  const operation = pendingConversationTransfer()!.operationID;
  await submitConversationTransfer('laptop', 'project-target', 'copy', true);
  expect(pendingConversationTransfer()?.error).toContain('Connection lost');
  await submitConversationTransfer('laptop', 'project-target', 'copy', true);
  expect(begin.mock.calls[0]).toEqual(begin.mock.calls[1]);
  expect(calls).toEqual(['begin:tower', 'offer:laptop', 'begin:tower', 'offer:laptop', 'bind:tower']);
  expect(computerTransfers('tower').rows[0].id).toBe(operation);
  expect(JSON.stringify(pendingConversationTransfer())).not.toContain('private-grant');
});

it('recovers the accepted project after the frontend lost setup state', async () => {
  setBindingMock('GetThreadTransferIntent', async () => ({ destinationBackendId: REMOTE_BACKEND_UUID, kind: 'copy', includeWorkspace: false }));
  setBindingMock('GetThreadTransferDestinationProject', async () => 'project-target');
  await recoverConversationTransfer('tower', row(), thread);
  expect(pendingConversationTransfer()).toMatchObject({ operationID: 'op-1', kind: 'copy', includeWorkspace: false, destination: 'laptop', projectID: 'project-target', submitted: true });
});

it('merges events arriving during a status read without losing other recovered operations', async () => {
  stop = initConversationTransfers(); await tick();
  let resolve!: (rows: ConversationTransfer[]) => void;
  setBindingMock('GetThreadTransfers', () => new Promise<ConversationTransfer[]>((done) => { resolve = done; }));
  const read = refreshComputerTransfers('tower');
  emitWailsEvent('thread:transfer', row({ phase: 'complete', needsDestination: false, updatedAt: 3 }), SOURCE);
  resolve([row(), row({ id: 'other-operation' })]); await read;
  expect(computerTransfers('tower').rows).toHaveLength(2);
  expect(computerTransfers('tower').rows.find((value) => value.id === 'op-1')?.phase).toBe('complete');
});

it('allows another copy once the previous operation completed', async () => {
  stop = initConversationTransfers(); await tick(); setupBindings();
  setBindingMock('CreateThreadTransferOffer', async () => ({}));
  openConversationTransfer(thread);
  const first = pendingConversationTransfer()!.operationID;
  await submitConversationTransfer('laptop', 'project-target', 'copy', true);
  emitWailsEvent('thread:transfer', row({ id: first, phase: 'complete', updatedAt: 3 }), SOURCE);
  openConversationTransfer(thread);
  expect(pendingConversationTransfer()?.operationID).not.toBe(first);
});

it('ignores transfer events from an unknown or forgotten computer', async () => {
  stop = initConversationTransfers(); await tick();
  emitWailsEvent('thread:transfer', row(), '99999999-2222-4333-8444-555555555555');
  expect(computerTransfers('').rows).toEqual([]);
  expect(computerTransfers('tower').rows).toEqual([]);
});

it('unlocks choices after a definitive first Begin refusal but preserves an uncertain request', async () => {
  setBindingMock('BeginThreadTransfer', async () => { takePinnedBackend(); throw new TransportError('method_error', 'Let the current turn finish'); });
  openConversationTransfer(thread);
  await submitConversationTransfer('laptop', 'project-target', 'copy', true);
  expect(pendingConversationTransfer()).toMatchObject({ submitted: false, error: 'Let the current turn finish.' });
  setBindingMock('BeginThreadTransfer', async () => { takePinnedBackend(); throw new Error('Connection lost'); });
  await submitConversationTransfer('laptop', 'project-target', 'move', false);
  expect(pendingConversationTransfer()).toMatchObject({ submitted: true, kind: 'move', includeWorkspace: false });
});
