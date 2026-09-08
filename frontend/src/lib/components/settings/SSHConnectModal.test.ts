import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor, cleanup } from '@testing-library/svelte';
import SSHConnectModal from './SSHConnectModal.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { __resetSystemsForTest } from '../../stores/systems.svelte';
import { resetToLocalPage } from '../../../test/helpers/scopes';

beforeEach(() => {
  resetBindingMocks(); __resetSystemsForTest(); resetToLocalPage();
  setBindingMock('CancelSSHConnection', async () => {});
  setBindingMock('RemoveBackend', async () => {});
});
afterEach(cleanup);

async function start() {
  const view = render(SSHConnectModal, { onClose: vi.fn() });
  await fireEvent.input(view.getByLabelText('SSH host'), { target: { value: 'gpu' } });
  await fireEvent.click(view.getByText('Continue'));
  return view;
}
const status = { id: 'console', target: 'gpu', state: 'invitation', invitation: 'https://gpu.test/#pair=one-time' };

it('compares both halves before forwarding explicit confirmation', async () => {
  setBindingMock('StartSSHConnection', async () => status);
  let redeemed = false;
  setBindingMock('GetSSHConnection', async () => redeemed
    ? { ...status, state: 'verification', verificationNumber: '123456' } : status);
  const add = setBindingMock('AddBackend', async () => {
    redeemed = true;
    return { id: 'gpu-id', name: 'GPU', endpoint: 'https://gpu.test', verificationNumber: '123456' };
  });
  const confirm = setBindingMock('ConfirmSSHConnection', async () => {});
  const view = await start();
  await view.findByText('Connect this computer');
  expect(add).toHaveBeenCalledWith(status.invitation);
  expect(confirm).not.toHaveBeenCalled();
  expect(view.getByLabelText('SSH verification number').textContent).toContain('123456');
  await fireEvent.click(view.getByText('Connect this computer'));
  await waitFor(() => expect(confirm).toHaveBeenCalledWith('console', '123456'));
});

it('refuses a mismatch and removes its provisional profile', async () => {
  setBindingMock('StartSSHConnection', async () => status);
  setBindingMock('GetSSHConnection', async () => ({ ...status, state: 'verification', verificationNumber: '123456' }));
  setBindingMock('AddBackend', async () => ({ id: 'gpu-id', name: 'GPU', endpoint: 'https://gpu.test', verificationNumber: '999999' }));
  const confirm = setBindingMock('ConfirmSSHConnection', async () => {});
  const cancel = setBindingMock('CancelSSHConnection', async () => {});
  const remove = setBindingMock('RemoveBackend', async () => {});
  const view = await start();
  expect((await view.findByRole('alert')).textContent).toContain('do not match');
  await waitFor(() => expect(remove).toHaveBeenCalledWith('gpu-id'));
  expect(cancel).toHaveBeenCalledWith('console');
  expect(confirm).not.toHaveBeenCalled();
});

it('cancels a setup whose start reply arrives after the dialog closes', async () => {
  let reply!: (value: typeof status) => void;
  setBindingMock('StartSSHConnection', () => new Promise<typeof status>((resolve) => { reply = resolve; }));
  const cancel = setBindingMock('CancelSSHConnection', async () => {});
  const view = await start();
  view.unmount();
  reply(status);
  await waitFor(() => expect(cancel).toHaveBeenCalledWith('console'));
});

it('shows a failed remote startup and allows correction', async () => {
  setBindingMock('StartSSHConnection', async () => status);
  setBindingMock('GetSSHConnection', async () => ({ ...status, state: 'error', error: 'No service is installed; run agent-overflow service install first.' }));
  const add = setBindingMock('AddBackend', async () => { throw new Error('must not pair'); });
  const view = await start();
  expect((await view.findByRole('alert')).textContent).toContain('No service is installed');
  expect(view.getByText('Try again')).toBeTruthy();
  expect(add).not.toHaveBeenCalled();
});

const verifying = { ...status, state: 'verification', verificationNumber: '123456' };
function stageVerification() {
  setBindingMock('StartSSHConnection', async () => status);
  setBindingMock('GetSSHConnection', async () => verifying);
  setBindingMock('AddBackend', async () => ({ id: 'gpu-id', name: 'GPU', endpoint: 'https://gpu.test', verificationNumber: '123456' }));
  return { cancel: setBindingMock('CancelSSHConnection', async () => {}), remove: setBindingMock('RemoveBackend', async () => {}) };
}

it('treats a refused confirmation as unconfirmed: the failure shows and nothing is retired', async () => {
  const { cancel, remove } = stageVerification();
  setBindingMock('ConfirmSSHConnection', async () => { throw new Error('the remote declined'); });
  const view = await start();
  await fireEvent.click(await view.findByText('Connect this computer'));
  expect((await view.findByRole('alert')).textContent).toContain('the remote declined');
  // Not confirmed, so the dialog still offers Cancel rather than Close, and
  // the pending connection stays the dialog's own to cancel.
  expect(view.getByText('Cancel')).toBeTruthy();
  expect(cancel).not.toHaveBeenCalled();
  expect(remove).not.toHaveBeenCalled();
});

it('does not cancel a confirmation the remote is still answering when the dialog closes', async () => {
  const { cancel, remove } = stageVerification();
  let answer!: () => void;
  setBindingMock('ConfirmSSHConnection', () => new Promise<void>((resolve) => { answer = resolve; }));
  const view = await start();
  await fireEvent.click(await view.findByText('Connect this computer'));
  expect(view.getByText('Connecting…')).toBeTruthy();
  await fireEvent.click(view.getByText('Close'));
  answer();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(cancel).not.toHaveBeenCalled();
  expect(remove).not.toHaveBeenCalled();
});

it('retires a connection whose confirmation is refused after the dialog closed', async () => {
  const { cancel, remove } = stageVerification();
  let refuse!: (reason: Error) => void;
  setBindingMock('ConfirmSSHConnection', () => new Promise<void>((_, reject) => { refuse = reject; }));
  const view = await start();
  await fireEvent.click(await view.findByText('Connect this computer'));
  await fireEvent.click(view.getByText('Close'));
  refuse(new Error('declined'));
  await waitFor(() => expect(cancel).toHaveBeenCalledWith('console'));
  expect(remove).toHaveBeenCalledWith('gpu-id');
});
