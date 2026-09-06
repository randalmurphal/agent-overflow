import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installDeviceNameSync } from '../stores/deviceNames';
import { clientDeviceName, clientDeviceNameStatus, resetClientDeviceNameForTest, saveClientDeviceName } from '../stores/clientDeviceName.svelte';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { TransportHello } from './wsClient';
import { backendById, detachBackend } from './backends';
import { getBackendIdentity, setBackendIdentityFromBootstrap } from './backendIdentity';
import { backendDisplayName, setBackendNickname, __resetBackendNicknamesForTest } from '../stores/attachedBackends.svelte';

const HELLO: TransportHello = { protocolVersion: 1, capabilities: ['device-name.v1'], backendId: 'local', backendName: 'Mac', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 };
let stop = () => {};
function pairSlot(id: string): void {
  localStorage.setItem(`agent-overflow:deviceSession:${id}`, JSON.stringify({ sessionId: id, credential: 'test', expiresAtMs: Date.now() + 60000 }));
}

beforeEach(() => {
  resetStagedBackends();
  localStorage.clear();
  resetClientDeviceNameForTest();
  __resetBackendNicknamesForTest();
});
afterEach(() => { stop(); resetStagedBackends(); });

describe('device name publication', () => {
  it('persists a device-owned name and publishes only to JS-held pairings', async () => {
    const send = vi.fn();
    setBindingMock('UpdateClientDeviceName', send);
    pairSlot('phone-host');
    stageBackend({ id: 'phone-host', hello: HELLO });
    stageBackend({ id: 'desktop-proxy', hello: HELLO });
    saveClientDeviceName('  Pixel 9  ');
    stop = installDeviceNameSync();
    await vi.waitFor(() => expect(send).toHaveBeenCalledOnce());
    expect(send.mock.calls[0][0]).toBe('Pixel 9');
    resetClientDeviceNameForTest();
    expect(clientDeviceName()).toBe('Pixel 9');
  });

  it('keeps an offline rename and publishes it on reconnect without re-pairing', async () => {
    const send = vi.fn();
    setBindingMock('UpdateClientDeviceName', send);
    pairSlot('phone-host');
    const backend = stageBackend({ id: 'phone-host', status: 'reconnecting', hello: HELLO });
    stop = installDeviceNameSync();
    saveClientDeviceName('Phone');
    expect(send).not.toHaveBeenCalled();
    expect(clientDeviceNameStatus()).toContain('Offline');
    backend.setStatus('connected');
    await vi.waitFor(() => expect(send).toHaveBeenCalledOnce());
  });

  it('serializes rapid renames and sends the latest name last', async () => {
    let finish!: () => void;
    const send = vi.fn().mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
    setBindingMock('UpdateClientDeviceName', send);
    pairSlot('phone-host');
    stageBackend({ id: 'phone-host', hello: HELLO });
    stop = installDeviceNameSync();
    saveClientDeviceName('First');
    await vi.waitFor(() => expect(send).toHaveBeenCalledOnce());
    saveClientDeviceName('Second');
    saveClientDeviceName('Final');
    finish();
    await vi.waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls.map((args) => args[0])).toEqual(['First', 'Final']);
  });

  it('retains failures as visible state and retries on reconnect', async () => {
    const send = vi.fn().mockRejectedValueOnce(new Error('connection lost')).mockResolvedValue(undefined);
    setBindingMock('UpdateClientDeviceName', send);
    pairSlot('phone-host');
    const backend = stageBackend({ id: 'phone-host', hello: HELLO });
    saveClientDeviceName('Phone');
    stop = installDeviceNameSync();
    await vi.waitFor(() => expect(clientDeviceNameStatus()).toContain('Could not share'));
    expect(send).toHaveBeenCalledOnce();
    backend.setStatus('reconnecting');
    backend.setStatus('connected');
    await vi.waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  });

  it('does not call unsupported hosts and detaches subscriptions when removed', () => {
    const send = vi.fn();
    setBindingMock('UpdateClientDeviceName', send);
    pairSlot('old-host');
    const backend = stageBackend({ id: 'old-host', hello: { ...HELLO, capabilities: [] } });
    saveClientDeviceName('Phone');
    stop = installDeviceNameSync();
    expect(clientDeviceNameStatus()).toContain('Update older computers');
    detachBackend('old-host');
    backend.setHello(HELLO);
    saveClientDeviceName('New phone');
    expect(send).not.toHaveBeenCalled();
  });

  it('uses live advertised names while keeping a local nickname override', () => {
    stageBackend({ id: 'mac', name: 'Old Mac', nickname: '', backendId: 'mac-id' });
    setBackendIdentityFromBootstrap('mac-id', 'generation', 'Old Mac', 'mac');
    stop = installDeviceNameSync();
    const entry = backendById('mac')!;
    emitWailsEvent('backend:name-changed', { name: 'Work Mac' }, 'mac-id');
    expect(getBackendIdentity('mac').name).toBe('Work Mac');
    expect(backendDisplayName(entry)).toBe('Work Mac');
    setBackendNickname('mac', 'Mine');
    emitWailsEvent('backend:name-changed', { name: 'Renamed again' }, 'mac-id');
    expect(backendDisplayName(entry)).toBe('Mine');
  });

  it('validates names before saving or publishing', () => {
    saveClientDeviceName('💻'.repeat(80));
    expect(() => saveClientDeviceName('💻'.repeat(81))).toThrow(/80/);
    expect(() => saveClientDeviceName('bad\0name')).toThrow(/control/);
    expect(clientDeviceName()).toBe('💻'.repeat(80));
  });
});
