// A watched computer's answer survives a dropped connection and is read
// again when the computer comes back: on its reconnect, and after a gap in
// its settings frames.
import { afterEach, expect, it, vi } from 'vitest';
import { providerMemory, refreshProviderMemory, watchProviderMemory } from './providerMemory.svelte';
import { installComputerHydration } from './computerHydration';
import { applyTransportGap } from './eventsTransportGap';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { DisconnectedError, TransportError, type TransportHello } from '../transport/wsClient';
import type { NativeMemoryStatus } from '../types/models';

const ON: NativeMemoryStatus = { claude: { on: true, error: '' }, codex: { on: false, error: '' } };
const OFF: NativeMemoryStatus = { claude: { on: false, error: '' }, codex: { on: false, error: '' } };

let release: (() => void) | undefined;
let stop: (() => void) | undefined;
afterEach(() => {
  release?.();
  release = undefined;
  stop?.();
  stop = undefined;
});

it('keeps the answer through a dropped connection, and takes any other failure', async () => {
  setBindingMock('GetNativeMemoryStatus', async () => ON);
  release = watchProviderMemory(HOME_BACKEND);
  await vi.waitFor(() => expect(providerMemory(HOME_BACKEND)?.status).toEqual(ON));

  setBindingMock('GetNativeMemoryStatus', async () => { throw new DisconnectedError('socket closed'); });
  refreshProviderMemory(HOME_BACKEND);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(providerMemory(HOME_BACKEND)).toEqual({ status: ON, error: '' });

  setBindingMock('GetNativeMemoryStatus', async () => { throw new TransportError('method_error', 'no provider home'); });
  refreshProviderMemory(HOME_BACKEND);
  await vi.waitFor(() => expect(providerMemory(HOME_BACKEND)?.error).toContain('no provider home'));
  expect(providerMemory(HOME_BACKEND)?.status).toBeNull();
});

it('reads a watched computer again when it reconnects', async () => {
  const gpu = stageBackend({ id: 'gpu', hello: { backendId: 'gpu' } as TransportHello });
  setBindingMock('GetSettings', async () => null);
  const read = vi.fn(async () => OFF);
  setBindingMock('GetNativeMemoryStatus', read);
  release = watchProviderMemory('gpu');
  await vi.waitFor(() => expect(providerMemory('gpu')?.status).toEqual(OFF));
  stop = installComputerHydration();
  gpu.setStatus('reconnecting');
  read.mockImplementation(async () => ON);
  gpu.setStatus('connected');
  await vi.waitFor(() => expect(providerMemory('gpu')?.status).toEqual(ON));
});

it('reads a watched computer again after a gap in its settings frames', async () => {
  setBindingMock('GetSettings', async () => null);
  const read = vi.fn(async () => OFF);
  setBindingMock('GetNativeMemoryStatus', read);
  release = watchProviderMemory(HOME_BACKEND);
  await vi.waitFor(() => expect(providerMemory(HOME_BACKEND)?.status).toEqual(OFF));
  read.mockImplementation(async () => ON);
  applyTransportGap({ channel: 'settings:updated', seq: 1 });
  await vi.waitFor(() => expect(providerMemory(HOME_BACKEND)?.status).toEqual(ON));
});
