import { tick } from 'svelte';
import { beforeEach, afterEach, expect, it } from 'vitest';
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte';
import AgentComputersSection from './AgentComputersSection.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { resetToLocalPage, grantBackendScopes } from '../../../test/helpers/scopes';
import { __setTransportHelloForTest, __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { takePinnedBackend } from '../../transport/backends';
import type { TransportHello } from '../../transport/wsClient';

const mac = '11111111-1111-4111-8111-111111111111';
const gpu = '22222222-2222-4222-8222-222222222222';
const hello = (id: string): TransportHello => ({
  backendId: id, backendName: id, capabilities: ['commands.remote.v1'], protocolVersion: 1,
  serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
});

beforeEach(() => { resetBindingMocks(); resetStagedBackends(); resetToLocalPage(); __setTransportHelloForTest(hello(mac)); __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null }); });
afterEach(() => { cleanup(); __setTransportHelloForTest(null); resetStagedBackends(); resetToLocalPage(); resetBindingMocks(); });

it('hides unsupported hosts and does not issue their new RPCs', () => {
  __setTransportHelloForTest({ ...hello(mac), capabilities: [] });
  const read = setBindingMock('ListAgentComputers', async () => []);
  const view = render(AgentComputersSection);
  expect(view.queryByText('Agent access to other computers')).toBeNull();
  expect(read).not.toHaveBeenCalled();
});

it.each([{ source: '', destination: gpu, targetID: gpu }, { source: gpu, destination: '', targetID: mac }])('pairs $source with $destination and verifies both identities before enabling', async ({ source, destination, targetID }) => {
  stageBackend({ id: gpu, backendId: gpu, name: 'GPU', hello: hello(gpu) });
  await grantBackendScopes(gpu, ['terminal:operate', 'access:admin']);
  setBindingMock('ListAgentComputers', async () => []);
  setBindingMock('MintDevicePairing', async () => { expect(takePinnedBackend()).toBe(destination); return { linkId: 'invite', url: 'private-invitation', expiresAtMs: Date.now() + 60_000 }; });
  setBindingMock('PairAgentComputer', async (url) => { expect(takePinnedBackend()).toBe(source); expect(url).toBe('private-invitation'); return { id: targetID, verificationNumber: '123456', name: 'GPU', endpoint: 'https://gpu' }; });
  setBindingMock('DevicePairingStatus', async () => { expect(takePinnedBackend()).toBe(destination); return { linkId: 'invite', state: 'redeemed', verificationNumber: '123456' }; });
  const confirm = setBindingMock('ConfirmDevicePairing', async () => { expect(takePinnedBackend()).toBe(destination); });
  const enable = setBindingMock('SetAgentComputerEnabled', async (id, enabled) => { expect(takePinnedBackend()).toBe(source); expect([id, enabled]).toEqual([targetID, true]); });
  const view = render(ComputerSettingsPage, { backend: source, Page: AgentComputersSection, needsComputer: false });
  await tick();
  const select = view.getByRole('combobox') as HTMLSelectElement;
  for (const option of select.options) option.selected = option.value === targetID;
  await fireEvent.change(select);
  expect((view.getByRole('button', { name: 'Enable access' }) as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(view.getByRole('button', { name: 'Enable access' }));
  await waitFor(() => expect(enable).toHaveBeenCalledOnce());
  expect(confirm).toHaveBeenCalledOnce();
});

it('refuses a mismatched pairing and cancels its invitation', async () => {
  stageBackend({ id: gpu, backendId: gpu, name: 'GPU', hello: hello(gpu) });
  await grantBackendScopes(gpu, ['terminal:operate', 'access:admin']);
  setBindingMock('ListAgentComputers', async () => []);
  setBindingMock('MintDevicePairing', async () => ({ linkId: 'invite', url: 'private', expiresAtMs: Date.now() + 60_000 }));
  setBindingMock('PairAgentComputer', async () => ({ id: gpu, verificationNumber: '123456' }));
  setBindingMock('DevicePairingStatus', async () => ({ verificationNumber: '654321' }));
  const confirm = setBindingMock('ConfirmDevicePairing', async () => {});
  const enable = setBindingMock('SetAgentComputerEnabled', async () => {});
  const cancel = setBindingMock('CancelDevicePairing', async () => { expect(takePinnedBackend()).toBe(gpu); });
  const view = render(AgentComputersSection);
  await tick();
  const select = view.getByRole('combobox') as HTMLSelectElement;
  for (const option of select.options) option.selected = option.value === gpu;
  await fireEvent.change(select);
  expect((view.getByRole('button', { name: 'Enable access' }) as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(view.getByRole('button', { name: 'Enable access' }));
  await waitFor(() => expect(cancel).toHaveBeenCalledOnce());
  expect(confirm).not.toHaveBeenCalled(); expect(enable).not.toHaveBeenCalled();
  expect(view.getByRole('alert').textContent).toContain('could not be verified');
});

it('keeps configuration writes on the settings computer', async () => {
  stageBackend({ id: gpu, backendId: gpu, name: 'GPU', hello: hello(gpu) });
  await grantBackendScopes(gpu, ['terminal:operate', 'access:admin']);
  let enabled = false;
  setBindingMock('ListAgentComputers', async () => { expect(takePinnedBackend()).toBe(gpu); return [{ id: mac, name: 'Mac', enabled, projects: [] }]; });
  const write = setBindingMock('SetAgentComputerEnabled', async (id, next) => { expect(takePinnedBackend()).toBe(gpu); expect(id).toBe(mac); enabled = next; });
  const view = render(ComputerSettingsPage, { backend: gpu, Page: AgentComputersSection, needsComputer: false });
  await waitFor(() => expect(view.getByRole('button', { name: 'Enable' })).toBeTruthy());
  await fireEvent.click(view.getByRole('button', { name: 'Enable' }));
  await waitFor(() => expect(write).toHaveBeenCalledOnce());
  await waitFor(() => expect(view.getByRole('button', { name: 'Enabled' }).getAttribute('aria-pressed')).toBe('true'));
});

it('offers explicit repair for an incomplete pairing without duplicating the computer in the add selector', async () => {
  stageBackend({ id: gpu, backendId: gpu, name: 'GPU', hello: hello(gpu) });
  await grantBackendScopes(gpu, ['terminal:operate', 'access:admin']);
  setBindingMock('ListAgentComputers', async () => [{ id: gpu, name: 'GPU', enabled: false, projects: [] }]);
  setBindingMock('SetAgentComputerEnabled', async () => { throw new Error('Pairing needs confirmation'); });
  const mint = setBindingMock('MintDevicePairing', async () => { throw new Error('destination is offline'); });
  const view = render(AgentComputersSection);
  await waitFor(() => expect(view.getByRole('button', { name: 'Enable' })).toBeTruthy());
  expect(view.queryByRole('combobox')).toBeNull();
  await fireEvent.click(view.getByRole('button', { name: 'Enable' }));
  await waitFor(() => expect(view.getByRole('button', { name: 'Connect again' })).toBeTruthy());
  await fireEvent.click(view.getByRole('button', { name: 'Connect again' }));
  await waitFor(() => expect(mint).toHaveBeenCalledOnce());
  await waitFor(() => expect(view.getByRole('alert').textContent).toContain('destination is offline'));
  expect(view.getByRole('button', { name: 'Connect again' })).toBeTruthy();
});
