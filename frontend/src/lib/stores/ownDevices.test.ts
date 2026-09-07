import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installOwnDeviceSync, ownDeviceConnectionsWaiting } from './ownDevices.svelte';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { attachIntroducedBackend, awaitAttachedActivation, payloadFromLink } from '../transport/backendAttach';
import { pairedSessionId } from '../transport/deviceSession';
import { backendById, detachBackend, takePinnedBackend } from '../transport/backends';
import { ownDeviceConnectionExcluded, rememberOwnDeviceMemberships, setOwnDeviceConnectionExcluded } from '../transport/ownDeviceConnections';
import type { TransportHello } from '../transport/wsClient';

vi.mock('../native/platform', () => ({ isNativeShell: () => true, nativePlatform: () => 'android' }));
vi.mock('../transport/backendAttach', async (original) => ({
  ...await original<typeof import('../transport/backendAttach')>(),
  attachIntroducedBackend: vi.fn(),
  awaitAttachedActivation: vi.fn(async () => true),
}));
const A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const C = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const HELLO: TransportHello = { backendId: A, backendName: 'Mac', capabilities: ['own-devices.v1'], protocolVersion: 1, serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 };
const member = (id: string, removed = false, generation = 1) => ({ backendId: id, keyThumbprint: id, name: id === C ? 'GPU' : 'Computer', generation, removed, routes: [] });
const group = (ids: string[]) => ({ enabled: true, selfKeyThumbprint: A, members: ids.map((id) => member(id)), connectedBackendIds: ids, excludedBackendIds: [] });
const invite = (id: string) => ({ linkId: id, expiresAtMs: Date.now() + 60_000, url: `https://computer.example/#pair=${btoa(JSON.stringify({ v: 1, backendId: id, backendName: 'Computer', endpoint: 'https://computer.example', token: 'test', purpose: 'own-introduction' }))}` });
function pair(id: string, ownDevice = true) {
  if (backendById(id)) detachBackend(id);
  localStorage.setItem(`agent-overflow:deviceSession:${id}`, JSON.stringify({ backendId: id, sessionId: id, credential: 'test', ownDevice }));
  return stageBackend({ id, backendId: id, name: id, hello: { ...HELLO, backendId: id } });
}
let stop = () => {};
beforeEach(() => {
  resetStagedBackends(); resetBindingMocks(); localStorage.clear();
  vi.mocked(attachIntroducedBackend).mockReset();
  vi.mocked(awaitAttachedActivation).mockResolvedValue(true);
  vi.mocked(attachIntroducedBackend).mockImplementation(async (link, current) => {
    if (!current()) throw new Error('superseded');
    const id = payloadFromLink(link).backendId;
    pair(id);
    return { id, name: 'Computer', verificationNumber: '' };
  });
});
afterEach(() => { stop(); resetStagedBackends(); vi.useRealTimers(); });

describe('own device connections', () => {
  it('introduces a third computer once across sponsors, then works without the first host', async () => {
    pair(A); pair(B);
    const list = setBindingMock('ListOwnDevices', async () => group([A, B, C]));
    const introduce = setBindingMock('IntroduceOwnDevice', async (id: string) => invite(id));
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(pairedSessionId(C)).toBe(C));
    expect(introduce).toHaveBeenCalledExactlyOnceWith(C);
    expect(attachIntroducedBackend).toHaveBeenCalledOnce();
    detachBackend(A);
    emitWailsEvent('own-devices:changed', {}, B);
    await vi.waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(2));
    expect(pairedSessionId(C)).toBe(C);
    expect(introduce).toHaveBeenCalledOnce();
  });

  it('does not elevate ordinary full-access connections or unsupported hosts on its own', async () => {
    pair(A, false);
    const old = pair(B); old.setHello({ ...HELLO, backendId: B, capabilities: [] });
    const list = setBindingMock('ListOwnDevices', async () => group([C]));
    stop = installOwnDeviceSync();
    await Promise.resolve(); await Promise.resolve();
    expect(list).not.toHaveBeenCalled();
    expect(attachIntroducedBackend).not.toHaveBeenCalled();
  });

  it('can upgrade an existing ordinary destination only through an approved sponsor', async () => {
    pair(A); pair(B, false);
    setBindingMock('ListOwnDevices', async () => group([A, B]));
    const introduce = setBindingMock('IntroduceOwnDevice', async (id: string) => invite(id));
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(attachIntroducedBackend).toHaveBeenCalledOnce());
    expect(introduce).toHaveBeenCalledExactlyOnceWith(B);
  });

  it.each(['disconnect', 'remove', 'stop'] as const)('discards a late introduction after %s without another enrollment', async (reason) => {
    const sponsor = pair(A);
    setBindingMock('ListOwnDevices', async () => group([A, C]));
    let finish!: (value: ReturnType<typeof invite>) => void;
    const introduce = setBindingMock('IntroduceOwnDevice', () => new Promise<ReturnType<typeof invite>>((resolve) => { finish = resolve; }));
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(introduce).toHaveBeenCalledOnce());
    for (let i = 0; i < 10; i++) emitWailsEvent('own-devices:changed', {}, A);
    if (reason === 'disconnect') sponsor.setStatus('reconnecting');
    else if (reason === 'remove') setOwnDeviceConnectionExcluded(C, true);
    else stop();
    finish(invite(C));
    await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    expect(attachIntroducedBackend).not.toHaveBeenCalled();
    expect(introduce).toHaveBeenCalledOnce();
  });

  it('retries a sleeping target while the sponsor stays connected', async () => {
    vi.useFakeTimers(); pair(A);
    setBindingMock('ListOwnDevices', async () => group([A, C]));
    const introduce = setBindingMock('IntroduceOwnDevice', vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValue(invite(C)));
    stop = installOwnDeviceSync();
    await vi.advanceTimersByTimeAsync(1);
    expect(ownDeviceConnectionsWaiting()).toEqual(['GPU']);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(introduce).toHaveBeenCalledTimes(2);
    expect(pairedSessionId(C)).toBe(C);
  });

  it('applies removal before enrollment across stale sponsors and retains it across restart', async () => {
    pair(A); pair(B); pair(C);
    setBindingMock('ListOwnDevices', async () => takePinnedBackend() === A
      ? { ...group([A, B]), members: [member(A), member(B), member(C, true)] }
      : group([A, B, C]));
    const introduce = setBindingMock('IntroduceOwnDevice', async (id: string) => invite(id));
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(pairedSessionId(C)).toBeNull());
    expect(ownDeviceConnectionExcluded(C)).toBe(true);
    stop();
    setBindingMock('ListOwnDevices', async () => group([A, B, C]));
    stop = installOwnDeviceSync();
    await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
    expect(introduce).not.toHaveBeenCalled();
    rememberOwnDeviceMemberships([member(C, false, 2)]);
    emitWailsEvent('own-devices:changed', {}, B);
    await vi.waitFor(() => expect(pairedSessionId(C)).toBe(C));
  });

  it('does not redeem an introduction for a different identity', async () => {
    pair(A);
    setBindingMock('ListOwnDevices', async () => group([A, C]));
    setBindingMock('IntroduceOwnDevice', async () => invite(B));
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(ownDeviceConnectionsWaiting()).toEqual(['GPU']));
    expect(attachIntroducedBackend).not.toHaveBeenCalled();
  });

  it('joins two groups through the phone and delivers independent reciprocal host introductions', async () => {
    pair(A); pair(B);
    const catalogs = new Map([[A, { ...group([A]), selfKeyThumbprint: A }], [B, { ...group([B]), selfKeyThumbprint: B }]]);
    setBindingMock('ListOwnDevices', async () => catalogs.get(takePinnedBackend()!)!);
    setBindingMock('SyncOwnDevices', async (members: ReturnType<typeof member>[]) => {
      const old = catalogs.get(takePinnedBackend()!)!;
      old.members = [...new Map([...old.members, ...members].map((row) => [row.keyThumbprint, row])).values()];
      return old;
    });
    const delivered: string[][] = [];
    const minted: string[][] = [];
    setBindingMock('MintOwnDeviceIntroduction', async (key: string) => {
      const target = takePinnedBackend()!;
      minted.push([target, key]);
      return invite(target);
    });
    setBindingMock('AcceptOwnDeviceIntroduction', async (link: string) => {
      const source = takePinnedBackend()!;
      const target = payloadFromLink(link).backendId;
      delivered.push([source, target]);
      catalogs.get(source)!.connectedBackendIds.push(target);
    });
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(delivered).toHaveLength(2));
    expect(minted).toEqual([[B, A], [A, B]]);
    expect(delivered).toEqual([[A, B], [B, A]]);
    expect(catalogs.get(A)!.members).toHaveLength(2);
    expect(catalogs.get(B)!.members).toHaveLength(2);
    expect(attachIntroducedBackend).not.toHaveBeenCalled();
  });

  it('drops a disconnected sponsor catalog while another catalog read is in flight', async () => {
    pair(A); pair(B); pair(C);
    let finish!: (value: ReturnType<typeof group>) => void;
    const list = setBindingMock('ListOwnDevices', async () => {
      const id = takePinnedBackend();
      if (id === A) return { ...group([A, B]), members: [member(A), member(B), member(C, true)] };
      if (id === B) return new Promise<ReturnType<typeof group>>((resolve) => { finish = resolve; });
      return group([B, C]);
    });
    stop = installOwnDeviceSync();
    await vi.waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    detachBackend(A);
    finish(group([B, C]));
    await vi.waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(2));
    expect(pairedSessionId(C)).toBe(C);
    expect(ownDeviceConnectionExcluded(C)).toBe(false);
  });
});
