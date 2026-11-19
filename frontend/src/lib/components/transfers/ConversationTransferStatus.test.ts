import { afterEach, beforeEach, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ConversationTransferStatus from './ConversationTransferStatus.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
import { grantBackendScopes, resetToLocalPage, revokeBackendScopes } from '../../../test/helpers/scopes';
import { setBackendIdentityFromBootstrap } from '../../transport/backendIdentity';
import { takePinnedBackend } from '../../transport/backends';
import type { TransportHello } from '../../transport/wsClient';
import { refreshComputerTransfers, resetConversationTransfersForTest, type ConversationTransfer } from '../../stores/conversationTransfers.svelte';

const SOURCE = '11111111-2222-4333-8444-555555555555';
const hello = (id: string) => ({ protocolVersion: 1, backendId: id, capabilities: ['conversation.transfer.v1'], backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 }) as TransportHello;
const row = (patch: Partial<ConversationTransfer> = {}): ConversationTransfer => ({ id: 'op-1', threadId: 'conversation', targetThreadId: 'copy-id', peerBackendId: REMOTE_BACKEND_UUID, kind: 'move', direction: 'outgoing', phase: 'preparing', needsDestination: false, ownershipEpoch: 0, createdAt: 1, updatedAt: 1, ...patch });

beforeEach(async () => {
  resetBindingMocks(); resetStagedBackends(); resetToLocalPage(); resetConversationTransfersForTest();
  stageBackend({ id: 'tower', backendId: SOURCE, hello: hello(SOURCE) });
  stageBackend({ hello: hello(REMOTE_BACKEND_UUID) });
  setBackendIdentityFromBootstrap(SOURCE, 'generation-a', 'Tower', 'tower');
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation-b', 'Laptop', 'laptop');
  for (const backend of ['tower', 'laptop']) await grantBackendScopes(backend, ['threads:read', 'threads:operate']);
});
afterEach(() => {
  resetConversationTransfersForTest(); resetStagedBackends();
  for (const backend of ['tower', 'laptop']) revokeBackendScopes(backend);
  resetToLocalPage();
});

// A row's error is the host's text for a failed attempt: a reviewed sentence,
// or a Go error string, which starts lowercase without closing punctuation.
// The row shows either as the transfer dialog does.
it('shows a Go error on the row as the sentence the dialog shows', () => {
  const view = render(ConversationTransferStatus, { backend: 'tower', row: row({ error: 'this conversation still belongs to another workflow' }) });
  expect(view.getByTestId('conversation-transfer-row-error').textContent).toBe('This conversation still belongs to another workflow.');
});

it('leaves a reviewed sentence on the row as written', () => {
  const view = render(ConversationTransferStatus, { backend: 'tower', row: row({ error: 'Reconnect the destination computer to finish setup.' }) });
  expect(view.getByTestId('conversation-transfer-row-error').textContent).toBe('Reconnect the destination computer to finish setup.');
});

it('shows the recipient computer’s error for an outgoing transfer the same way', async () => {
  setBindingMock('GetThreadTransfers', async () => {
    expect(takePinnedBackend()).toBe('laptop');
    return [row({ direction: 'incoming', error: 'this conversation belongs to a workflow. Transfer an independent conversation' })];
  });
  await refreshComputerTransfers('laptop');
  const view = render(ConversationTransferStatus, { backend: 'tower', row: row({ error: 'the source error is not shown while the recipient has one' }) });
  expect(view.getByTestId('conversation-transfer-row-error').textContent).toBe('This conversation belongs to a workflow. Transfer an independent conversation.');
});

it('shows no row error once the transfer is over', () => {
  const view = render(ConversationTransferStatus, { backend: 'tower', row: row({ phase: 'complete', error: 'this conversation still belongs to another workflow' }) });
  expect(view.queryByTestId('conversation-transfer-row-error')).toBeNull();
});
