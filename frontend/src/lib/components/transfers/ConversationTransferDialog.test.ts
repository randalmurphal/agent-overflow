import { afterEach, beforeEach, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import ConversationTransferDialog from './ConversationTransferDialog.svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
import { grantBackendScopes, resetToLocalPage, revokeBackendScopes } from '../../../test/helpers/scopes';
import { makeThread } from '../../../test/helpers/chat';
import { setBackendIdentityFromBootstrap } from '../../transport/backendIdentity';
import type { TransportHello } from '../../transport/wsClient';
import { resetConversationTransfersForTest, type TransferRequest } from '../../stores/conversationTransfers.svelte';

const SOURCE = '11111111-2222-4333-8444-555555555555';
const hello = (id: string) => ({ protocolVersion: 1, backendId: id, capabilities: ['conversation.transfer.v1'], backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 }) as TransportHello;
const request = (patch: Partial<TransferRequest> = {}, agentId?: string): TransferRequest => ({
  operationID: 'op-1', thread: makeThread({ id: 'conversation', title: 'Ops', projectId: '', agentId }), source: 'tower',
  open: true, submitted: false, submitting: false, error: '', ...patch,
});
const NOTE = 'This conversation is the assistant "Ops", which stays on this computer with its home, memory, loops and watches, so it can only be copied. The copy is an ordinary conversation.';

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

function form() {
  return {
    move: screen.getByRole('radio', { name: 'Move' }) as HTMLInputElement,
    copy: screen.getByRole('radio', { name: 'Copy / fork' }) as HTMLInputElement,
    note: screen.queryByTestId('transfer-standing-agent'),
    submit: screen.queryByRole('button', { name: /^(Move|Copy) conversation$/ }),
  };
}

// An assistant stays on its computer, so its current thread is only copied:
// Move is disabled whether the source said so or, before it answers, the row
// names the assistant, and the note says why.
it.each([
  ['the source said it is an assistant', request({ standingAgent: true })],
  ['the row names the assistant before the source answers', request({}, 'agent-ops')],
])('offers only Copy when %s', async (_, value) => {
  render(ConversationTransferDialog, { request: value });
  let shown = form();
  expect(shown.move.disabled).toBe(true);
  expect(shown.copy.checked).toBe(true);
  expect(shown.note?.textContent?.trim()).toBe(NOTE);
  expect(shown.submit?.textContent?.trim()).toBe('Copy conversation');
  await fireEvent.click(shown.move);
  shown = form();
  expect(shown.copy.checked).toBe(true);
  expect(shown.submit?.textContent?.trim()).toBe('Copy conversation');
});

// The source's answer wins over a row read earlier, and an ordinary
// conversation chooses either kind.
it.each([
  ['an ordinary conversation', request({ standingAgent: false })],
  ['the source said a row naming an assistant is not one', request({ standingAgent: false }, 'agent-ops')],
])('offers Move and Copy for %s', async (_, value) => {
  render(ConversationTransferDialog, { request: value });
  let shown = form();
  expect(shown.move.disabled).toBe(false);
  expect(shown.move.checked).toBe(true);
  expect(shown.note).toBeNull();
  expect(shown.submit?.textContent?.trim()).toBe('Move conversation');
  await fireEvent.click(shown.copy);
  shown = form();
  expect(shown.copy.checked).toBe(true);
  expect(shown.submit?.textContent?.trim()).toBe('Copy conversation');
  await fireEvent.click(shown.move);
  expect(form().submit?.textContent?.trim()).toBe('Move conversation');
});
