import { beforeEach, afterEach, expect, it } from 'vitest';
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte';
import ComputerActions from './ComputerActions.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { resetToLocalPage, pairViewOnly } from '../../../test/helpers/scopes';
import { saveComputerSSH, computerSSH } from '../../stores/computerSSH.svelte';
import { detachBackend } from '../../transport/backends';

beforeEach(() => { resetBindingMocks(); resetStagedBackends(); resetToLocalPage(); });
afterEach(() => { cleanup(); resetStagedBackends(); resetToLocalPage(); });

it('starts a remembered offline computer without spending another pairing', async () => {
  const remote = stageBackend({ status: 'reconnecting' });
  const view = render(ComputerActions, { backend: 'laptop' });
  expect(view.queryByText('Start over SSH')).toBeNull();
  saveComputerSSH('laptop', { target: 'gpu', binary: '/opt/ao' });
  const start = setBindingMock('StartSSHComputer', async () => {});
  const button = await view.findByText('Start over SSH');
  await fireEvent.click(button);
  await waitFor(() => expect(start).toHaveBeenCalledWith({ target: 'gpu', binary: '/opt/ao', startService: true, lan: false }));
  remote.setStatus('connected');
  await waitFor(() => expect(view.queryByText('Start over SSH')).toBeNull());
});

it('keeps desktop SSH unavailable to a paired frontend', async () => {
  stageBackend({ status: 'reconnecting' });
  saveComputerSSH('laptop', { target: 'gpu', binary: 'agent-overflow' });
  await pairViewOnly();
  const view = render(ComputerActions, { backend: 'laptop' });
  expect(view.queryByText('Start over SSH')).toBeNull();
});

it('forgets the SSH alias when the computer is removed', () => {
  stageBackend();
  saveComputerSSH('laptop', { target: 'gpu', binary: 'agent-overflow' });
  detachBackend('laptop');
  expect(computerSSH('laptop')).toBeUndefined();
});
