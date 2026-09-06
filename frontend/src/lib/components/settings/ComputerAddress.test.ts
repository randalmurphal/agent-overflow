import { afterEach, beforeEach, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import ComputerAddress from './ComputerAddress.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { resetToLocalPage } from '../../../test/helpers/scopes';
import { detachBackend } from '../../transport/backends';

beforeEach(() => { resetBindingMocks(); resetStagedBackends(); resetToLocalPage(); });
afterEach(() => { cleanup(); resetStagedBackends(); resetToLocalPage(); });

it('verifies the captured computer and keeps a failed address editable', async () => {
  const remote = stageBackend({ status: 'reconnecting' });
  const repair = setBindingMock('RepairBackendAddress', async () => { throw new Error('Computer is not reachable'); });
  const view = render(ComputerAddress, { backend: 'laptop' });
  await fireEvent.click(view.getByRole('button', { name: 'Change address' }));
  const input = view.getByLabelText('New computer address');
  await fireEvent.input(input, { target: { value: '192.168.1.55:9443' } });
  await fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('Computer is not reachable'));
  expect(repair).toHaveBeenCalledWith('laptop', 'https://192.168.1.55:9443');
  expect(input).toHaveValue('192.168.1.55:9443');
  setBindingMock('RepairBackendAddress', async () => 'https://192.168.1.55:9443');
  await fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(view.getByRole('status')).toHaveTextContent('Verified https://192.168.1.55:9443'));
  expect(view.queryByLabelText('New computer address')).toBeNull();
  expect(remote.reconnect).toHaveBeenCalledTimes(1);
});

it('does not send a repair to a removed computer or redirect it to home', async () => {
  stageBackend({ status: 'reconnecting' });
  const repair = setBindingMock('RepairBackendAddress', async () => 'https://gpu');
  const view = render(ComputerAddress, { backend: 'laptop' });
  await fireEvent.click(view.getByRole('button', { name: 'Change address' }));
  const input = view.getByLabelText('New computer address');
  await fireEvent.input(input, { target: { value: 'gpu' } });
  detachBackend('laptop');
  await fireEvent.submit(input.closest('form')!);
  await waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('no longer connected'));
  expect(repair).not.toHaveBeenCalled();
});
