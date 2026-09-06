import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import DeviceNameField from './DeviceNameField.svelte';
import { clientDeviceName, saveClientDeviceName } from '../../stores/clientDeviceName.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import type { TransportHello } from '../../transport/wsClient';
import { setBackendIdentityFromBootstrap } from '../../transport/backendIdentity';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { stageBackend } from '../../../test/helpers/backends';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { takePinnedBackend } from '../../transport/backends';

function phone(): void {
  localStorage.setItem('agent-overflow:deviceSession', JSON.stringify({ sessionId: 'phone', credential: 'test', expiresAtMs: Date.now() + 60000 }));
}

describe('Device name field', () => {
  it('lets a phone save its own name while offline without writing host settings', async () => {
    phone();
    const view = render(DeviceNameField);
    await fireEvent.input(view.getByLabelText('Device name'), { target: { value: 'Randy’s Pixel' } });
    await fireEvent.click(view.getByRole('button', { name: 'Save' }));
    expect(clientDeviceName()).toBe('Randy’s Pixel');
    expect(getBindingMock('SetDeviceName')).toBeUndefined();
    expect(view.getByRole('status')).toHaveTextContent('Device name saved.');
  });

  it('refreshes pristine phone fields while preserving unsaved edits', async () => {
    phone();
    saveClientDeviceName('First');
    const view = render(DeviceNameField);
    const input = view.getByLabelText('Device name');
    saveClientDeviceName('Changed elsewhere');
    await tick();
    expect(input).toHaveValue('Changed elsewhere');
    await fireEvent.input(input, { target: { value: 'Still typing' } });
    saveClientDeviceName('Another change');
    await tick();
    expect(input).toHaveValue('Still typing');
  });

  it('renames the local desktop installation through its own backend', async () => {
    __setTransportHelloForTest({ protocolVersion: 1, capabilities: ['device-name.v1'], backendId: 'local', backendName: 'Mac', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    let name = 'Mac';
    setBindingMock('GetDeviceName', async () => name);
    const set = setBindingMock('SetDeviceName', (value: string) => {
      expect(takePinnedBackend()).toBe('');
      name = value;
    });
    const view = render(DeviceNameField);
    const input = view.getByLabelText('Device name');
    await waitFor(() => expect(input).toHaveValue('Mac'));
    await fireEvent.input(input, { target: { value: 'Studio Mac' } });
    await fireEvent.click(view.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(set).toHaveBeenCalledWith('Studio Mac'));
    expect(input).toHaveValue('Studio Mac');
  });

  it('accepts live host-name updates only from its own computer and keeps edits', async () => {
    __setTransportHelloForTest({ protocolVersion: 1, capabilities: ['device-name.v1'], backendId: 'local', backendName: 'Mac', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    setBackendIdentityFromBootstrap('local', 'generation', 'Mac');
    setBindingMock('GetDeviceName', async () => 'Mac');
    const view = render(DeviceNameField);
    const input = view.getByLabelText('Device name');
    await waitFor(() => expect(input).toHaveValue('Mac'));
    emitWailsEvent('backend:name-changed', { name: 'Wrong machine' }, 'other');
    await tick();
    expect(input).toHaveValue('Mac');
    emitWailsEvent('backend:name-changed', { name: 'New Mac' }, 'local');
    await tick();
    expect(input).toHaveValue('New Mac');
    await fireEvent.input(input, { target: { value: 'Typing' } });
    emitWailsEvent('backend:name-changed', { name: 'Changed again' }, 'local');
    await tick();
    expect(input).toHaveValue('Typing');
  });

  it('surfaces storage/validation failures and retains the entered name', async () => {
    phone();
    const view = render(DeviceNameField);
    const input = view.getByLabelText('Device name');
    await fireEvent.input(input, { target: { value: 'x'.repeat(81) } });
    await fireEvent.click(view.getByRole('button', { name: 'Save' }));
    expect(view.getByRole('alert')).toHaveTextContent('80');
    expect(input).toHaveValue('x'.repeat(81));
  });

  it('targets an explicitly selected host even on a paired phone', async () => {
    phone();
    stageBackend({ id: 'gpu', hello: { protocolVersion: 1, capabilities: ['device-name.v1'], backendId: 'gpu-id', backendName: 'GPU', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 } });
    setCarriedSessionScopes('gpu', ['session', 'access:admin']);
    let name = 'GPU';
    setBindingMock('GetDeviceName', async () => name);
    const set = setBindingMock('SetDeviceName', async (value: string) => {
      expect(takePinnedBackend()).toBe('gpu');
      name = value;
    });
    const view = render(DeviceNameField, { backend: 'gpu', fieldId: 'remote.device-name' });
    const input = view.getByLabelText('Device name');
    await waitFor(() => expect(input).toHaveValue('GPU'));
    await fireEvent.input(input, { target: { value: 'GPU workstation' } });
    await fireEvent.click(view.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(set).toHaveBeenCalledWith('GPU workstation'));
    expect(clientDeviceName()).not.toBe('GPU workstation');
  });

  it('shows the host name without allowing a view-only session to change it', async () => {
    stageBackend({ id: 'gpu', hello: { protocolVersion: 1, capabilities: ['device-name.v1'], backendId: 'gpu-id', backendName: 'GPU', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 } });
    setCarriedSessionScopes('gpu', ['session']);
    setBindingMock('GetDeviceName', async () => 'GPU');
    const view = render(DeviceNameField, { backend: 'gpu', fieldId: 'remote.device-name' });
    await waitFor(() => expect(view.getByLabelText('Device name')).toHaveValue('GPU'));
    expect(view.getByLabelText('Device name')).toBeDisabled();
    expect(view.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('does not call a host that lacks support', () => {
    __setTransportHelloForTest({ capabilities: [] } as unknown as TransportHello);
    const get = setBindingMock('GetDeviceName', vi.fn());
    const view = render(DeviceNameField);
    expect(view.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(get).not.toHaveBeenCalled();
  });
});
